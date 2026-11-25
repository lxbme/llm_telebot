package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	openai "github.com/sashabaranov/go-openai"
)

// einoToolSet belongs to a single reply. The available tools are fixed for the
// reply, while expansion requests can change visibility for the next round.
type einoToolSet struct {
	rawTools      map[string]openai.Tool
	tools         []*einoTool
	summaryMode   bool
	serverNames   []string
	summaryGroups map[string]string // canonical tool name to server group

	mu       sync.Mutex
	expanded map[string]bool
	allowed  map[string]bool
}

func newEinoTools(view *MergedToolView, summaryMode bool) (*einoToolSet, error) {
	set := &einoToolSet{}
	if view == nil {
		return set, nil
	}
	names, canonical := snapshotEinoTools(view)
	if len(names) == 0 {
		return set, nil
	}
	set.rawTools = make(map[string]openai.Tool, len(names))
	set.summaryMode = summaryMode

	if summaryMode {
		set.expanded = make(map[string]bool)
		set.serverNames = view.ServerNames()
		set.summaryGroups = make(map[string]string)
		for _, group := range set.serverNames {
			for _, definition := range view.OpenAIToolsForServer(group) {
				name := definition.Function.Name
				// An identically named personal tool may belong to a different group.
				if remote, ok := canonical[name].(*MCPRemoteTool); ok && remote.serverName == group {
					set.summaryGroups[name] = group
				}
			}
		}
		if err := set.addTool(getToolDetailsVirtualTool, nil); err != nil {
			return nil, err
		}
	}

	for _, name := range names {
		if summaryMode && name == getToolDetailsVirtualTool.Function.Name {
			continue
		}
		registered := canonical[name]
		definition := openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        name,
				Description: registered.Description(),
				Parameters:  registered.Parameters(),
			},
		}
		if err := set.addTool(definition, registered); err != nil {
			return nil, err
		}
	}
	return set, nil
}

func snapshotEinoTools(view *MergedToolView) ([]string, map[string]MCPTool) {
	var names []string
	canonical := make(map[string]MCPTool)
	// MergedToolView gives the personal registry priority. Take the schema
	// and executor from the same tool, including when a name is shadowed.
	for _, registry := range []*ToolRegistry{view.global, view.user} {
		if registry == nil {
			continue
		}
		registry.mu.RLock()
		for _, name := range registry.order {
			if _, exists := canonical[name]; !exists {
				names = append(names, name)
			}
			canonical[name] = registry.tools[name]
		}
		registry.mu.RUnlock()
	}
	return names, canonical
}

func (s *einoToolSet) addTool(definition openai.Tool, registered MCPTool) error {
	function := *definition.Function
	params, err := json.Marshal(function.Parameters)
	if err != nil {
		return fmt.Errorf("encode parameters for Eino tool %q: %w", function.Name, err)
	}
	function.Parameters = json.RawMessage(params)
	definition.Function = &function
	s.rawTools[function.Name] = definition

	// The model adapter takes the provider schema from rawTools by name.
	// Keep it out of Eino's schema type so provider extensions survive.
	s.tools = append(s.tools, &einoTool{
		set:        s,
		info:       &schema.ToolInfo{Name: function.Name, Desc: function.Description},
		registered: registered,
	})
	return nil
}

func (s *einoToolSet) ToolsNodeConfig() compose.ToolsNodeConfig {
	config := compose.ToolsNodeConfig{
		ExecuteSequentially: true,
		UnknownToolsHandler: unknownEinoTool,
	}
	if s != nil {
		config.Tools = make([]tool.BaseTool, 0, len(s.tools))
		for _, tool := range s.tools {
			config.Tools = append(config.Tools, tool)
		}
	}
	return config
}

// RoundToolInfos installs the exact allowlist for the upcoming model call
// and its tool batch. Call it before each model call, after the prior batch.
func (s *einoToolSet) RoundToolInfos() []*schema.ToolInfo {
	if s == nil || len(s.tools) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.allowed = make(map[string]bool, len(s.tools))
	var infos []*schema.ToolInfo
	for _, tool := range s.tools {
		name := tool.info.Name
		if s.summaryMode && tool.registered != nil {
			group, ok := s.summaryGroups[name]
			if !ok || !s.expanded[group] {
				continue
			}
		}
		s.allowed[name] = true
		infos = append(infos, tool.info)
	}
	return infos
}

func (s *einoToolSet) expandGroups(input string) string {
	var args struct {
		Groups []string `json:"groups"`
	}
	// Preserve the existing virtual tool's output and case-insensitive lookup.
	_ = json.Unmarshal([]byte(input), &args)
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, group := range args.Groups {
		for _, actual := range s.serverNames {
			if strings.EqualFold(actual, group) {
				s.expanded[actual] = true
				break
			}
		}
	}
	return "Tool schemas loaded for: " + strings.Join(args.Groups, ", ") + ". You can now call those tools."
}

type einoTool struct {
	set        *einoToolSet
	info       *schema.ToolInfo
	registered MCPTool // nil for the virtual group expansion tool
}

func (t *einoTool) Info(context.Context) (*schema.ToolInfo, error) {
	return t.info, nil
}

func (t *einoTool) InvokableRun(ctx context.Context, args string, _ ...tool.Option) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	name := t.info.Name
	t.set.mu.Lock()
	allowed := t.set.allowed[name]
	t.set.mu.Unlock()
	if !allowed {
		return unknownEinoTool(ctx, name, args)
	}
	if t.registered == nil {
		return t.set.expandGroups(args), nil
	}

	observer, _ := ctx.Value(einoToolEventsKey{}).(einoToolEvents)
	event := DashboardEvent{
		Type:      DashboardEventToolCallStarted,
		ChatID:    observer.usage.ChatID,
		UserID:    observer.usage.UserID,
		RequestID: observer.usage.RequestID,
		ToolName:  name,
		Summary:   "executing tool " + name,
	}
	if id := compose.GetToolCallID(ctx); id != "" {
		event.Detail = "call " + id
	}
	started := time.Now()
	if observer.record != nil {
		observer.record(event)
	}

	var result string
	var err error
	if remote, ok := t.registered.(*MCPRemoteTool); ok {
		result, err = remote.ExecuteContext(ctx, args)
	} else {
		result, err = t.registered.Execute(args)
	}
	cancelErr := ctx.Err()
	if observer.record != nil {
		event.Type = DashboardEventToolCallFinished
		event.LatencyMs = time.Since(started).Milliseconds()
		event.Success = err == nil && cancelErr == nil
		event.Summary = "tool " + name + " failed"
		if event.Success {
			event.Summary = "tool " + name + " completed"
		}
		observer.record(event)
	}

	if cancelErr != nil {
		return "", cancelErr
	}
	if errors.Is(err, context.Canceled) {
		return "", err
	}
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	return result, nil
}

func unknownEinoTool(ctx context.Context, name, _ string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return fmt.Sprintf("error: unknown tool %q", name), nil
}

type einoToolEventsKey struct{}

type einoToolEvents struct {
	usage  UsageContext
	record func(DashboardEvent)
}

func withEinoToolEvents(ctx context.Context, usage UsageContext, record func(DashboardEvent)) context.Context {
	return context.WithValue(ctx, einoToolEventsKey{}, einoToolEvents{usage: usage, record: record})
}
