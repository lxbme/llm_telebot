package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
		if err := set.addTool(getToolDetailsVirtualTool, nil); err != nil {
			return nil, err
		}
	}

	for _, name := range names {
		if summaryMode && name == getToolDetailsVirtualTool.Function.Name {
			continue
		}
		registered := canonical[name]
		if remote, ok := registered.(*MCPRemoteTool); summaryMode && ok {
			set.summaryGroups[name] = remote.serverName
		}
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
	registered MCPTool // nil for application-local tools
	local      func(context.Context, string) (string, error)
}

func (t *einoTool) Info(context.Context) (*schema.ToolInfo, error) {
	return t.info, nil
}

type einoToolPayloadsKey struct{}

type einoToolPayloads struct {
	mu         sync.Mutex
	scheduled  bool
	hasPayload bool
	results    []mcpToolOutput
}

func (p *einoToolPayloads) add(output string, hasPayload bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hasPayload = p.hasPayload || hasPayload
	if p.scheduled && strings.TrimSpace(output) != "" {
		p.results = append(p.results, mcpToolOutput{text: output, hasPayload: hasPayload})
	}
}

func (p *einoToolPayloads) snapshot() (bool, []mcpToolOutput) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hasPayload, slices.Clone(p.results)
}

func (t *einoTool) InvokableRun(ctx context.Context, args string, _ ...tool.Option) (output string, runErr error) {
	if err := checkRoleReplyUse(ctx); err != nil {
		return "", err
	}
	name := t.info.Name
	t.set.mu.Lock()
	allowed := t.set.allowed[name]
	t.set.mu.Unlock()
	if !allowed {
		return unknownEinoTool(ctx, name, args)
	}
	var hasPayload bool
	if payloads, ok := ctx.Value(einoToolPayloadsKey{}).(*einoToolPayloads); ok && (!payloads.scheduled || t.registered != nil) {
		// Evaluate after command revocation and cancellation have had a
		// chance to replace the result.
		defer func() {
			if runErr == nil && ctx.Err() == nil {
				payloads.add(output, hasPayload)
			}
		}()
	}
	if t.local != nil {
		output, runErr = t.local(ctx, args)
		hasPayload = strings.TrimSpace(output) != ""
		return output, runErr
	}
	if t.registered == nil {
		hasPayload = true
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

	if err := checkRoleReplyUse(ctx); err != nil {
		return "", err
	}
	if run, ok := ctx.Value(roleReplyRunKey{}).(*roleReplyRun); ok {
		if grant := run.turn.groupTool; grant != nil && grant.key.name == name {
			if parsed, ok := groupToolArguments(args); !ok || parsed != grant.args {
				return "error: the arguments do not match the account's approval for this group", nil
			}
			// Use the approved canonical payload after comparing it with the
			// model request so only the authorized RPC reaches the server.
			args = grant.args
		}
	}

	var result mcpToolOutput
	var err error
	if remote, ok := t.registered.(*MCPRemoteTool); ok {
		if command := remote.command; command != nil {
			call := command.beginCall(ctx)
			if call == nil {
				err = errCommandMCPAccess
			} else {
				// The completion observer runs before this result is delivered.
				// Keep revocation effective until InvokableRun is ready to return.
				defer func() {
					authorized := command.finishCall(call)
					cancelErr := ctx.Err()
					switch {
					case cancelErr != nil:
						hasPayload = false
						output, runErr = "", cancelErr
					case !authorized:
						hasPayload = false
						output, runErr = "error: "+errCommandMCPAccess.Error(), nil
					}
				}()
				result, err = remote.executeRPC(call.ctx, args, true)
			}
		} else {
			result, err = remote.executeRPC(ctx, args, true)
		}
	} else {
		result.text, err = t.registered.Execute(args)
		result.hasPayload = strings.TrimSpace(result.text) != ""
	}
	if observer.record != nil {
		event.Type = DashboardEventToolCallFinished
		event.LatencyMs = time.Since(started).Milliseconds()
		event.Success = err == nil && ctx.Err() == nil
		event.Summary = "tool " + name + " failed"
		if event.Success {
			event.Summary = "tool " + name + " completed"
		}
		observer.record(event)
	}

	if err := checkRoleReplyUse(ctx); err != nil {
		return "", err
	}
	if errors.Is(err, context.Canceled) {
		return "", err
	}
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	hasPayload = result.hasPayload
	return result.text, nil
}

func unknownEinoTool(ctx context.Context, name, _ string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	output := fmt.Sprintf("error: unknown tool %q", name)
	if payloads, ok := ctx.Value(einoToolPayloadsKey{}).(*einoToolPayloads); ok && payloads.scheduled {
		payloads.add(output, false)
	}
	return output, nil
}

type einoToolEventsKey struct{}

type einoToolEvents struct {
	usage  UsageContext
	record func(DashboardEvent)
}

func withEinoToolEvents(ctx context.Context, usage UsageContext, record func(DashboardEvent)) context.Context {
	return context.WithValue(ctx, einoToolEventsKey{}, einoToolEvents{usage: usage, record: record})
}

// addEngagementDialogue installs role tools beside the foreground tools. The
// local tools are directly visible even when remote schemas use lazy discovery.
func (s *einoToolSet) addEngagementDialogue(d *engagementDialogue) error {
	if s.rawTools == nil {
		s.rawTools = make(map[string]openai.Tool)
	}
	for _, tool := range []struct {
		name, description, schema string
		run                       func(context.Context, string) (string, error)
	}{
		{"engagement_context", "Read the speaker's visible items, agreement versions, available background capabilities and conversation preferences. Use an empty object to list or supply an item id. Use the next cursor to read another page.",
			`{"type":"object","properties":{"id":{"type":"string"},"after":{"type":"string"}}}`, d.read},
		{"engagement_decide", "Propose the role's decision regarding a lasting tracking item or delegation. Supply id and its visible version on revision. A new item uses kind and change agree. Use the returned capability IDs. Authorization and management quotes must be verbatim from the current speaker and basis_message_id identifies an actual visible approving statement. Omit unchanged terms on revision. Supply declined quotations for partially accepted requests.",
			`{"type":"object","required":["verdict"],"properties":{"id":{"type":"string"},"version":{"type":"integer"},"kind":{"enum":["tracking","delegation"]},"verdict":{"enum":["accept","reject","clarify"]},"change":{"enum":["agree","reaccept","pause","resume","end","manage","mute","unmute"]},"observe":{"type":"string"},"actions":{"type":"array","items":{"type":"string"}},"recipient":{"enum":["here","private"]},"trigger":{"type":"string"},"end_condition":{"type":"string"},"once":{"type":"boolean"},"check_after":{"type":"string","description":"Earliest RFC3339 date for an initial private:followup only"},"duration":{"type":"string","description":"ongoing, this_week, or explicit RFC3339 expiry"},"reply":{"type":"string","description":"One focused clarification or reason for declining, safe for this chat"},"declined":{"type":"array","items":{"type":"string"}},"authorization_quote":{"type":"string"},"management_quote":{"type":"string"},"basis_message_id":{"type":"integer"}}}`, d.decide},
		{"conversation_preference", "Decide a request for proactive chatter and tracking notifications. Use the current version from engagement_context. Set either channel independently. An explicit reminder is independent of these preferences.",
			`{"type":"object","required":["scope","verdict"],"properties":{"scope":{"enum":["private","group","self"]},"version":{"type":"integer"},"verdict":{"enum":["accept","reject","clarify"]},"chatter":{"enum":["on","off","reduce","default"]},"notifications":{"enum":["on","off","reduce","default"]},"duration":{"type":"string","description":"ongoing, this_week, or explicit RFC3339 expiry"},"chatter_duration":{"type":"string"},"notification_duration":{"type":"string"},"reply":{"type":"string"}}}`, d.changePreference},
	} {
		// These names are reserved locally in ordinary role dialogue. A
		// conflicting remote definition must not receive local arguments.
		s.tools = slices.DeleteFunc(s.tools, func(existing *einoTool) bool { return existing.info.Name == tool.name })
		err := s.addTool(openai.Tool{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{
			Name: tool.name, Description: tool.description, Parameters: json.RawMessage(tool.schema),
		}}, nil)
		if err != nil {
			return err
		}
		s.tools[len(s.tools)-1].local = tool.run
	}
	return nil
}
