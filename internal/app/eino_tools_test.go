package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func TestEinoToolVisibility(t *testing.T) {
	const personalSchema = `{"type":"object","properties":{"pairs":{"type":"array","items":[{"type":"integer"},{"type":"string"}],"x-provider":true}},"required":["pairs"]}`

	global := NewToolRegistry()
	addEinoTestMCPServer(t, global, "Core",
		einoTestMCPTool("lookup", "global lookup", `{"type":"object","additionalProperties":false}`),
		einoTestMCPTool("global_only", "global only", ""))
	alice := NewToolRegistry()
	personalCalls := make(chan struct{}, 3)
	lookup := einoTestMCPTool("lookup", "personal lookup", personalSchema)
	lookup.run = func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		personalCalls <- struct{}{}
		return mcp.NewToolResultText("personal lookup"), nil
	}
	addEinoTestMCPServer(t, alice, "Library",
		lookup,
		einoTestMCPTool("alice_only", "Alice only", ""))
	bob := NewToolRegistry()
	addEinoTestMCPServer(t, bob, "Research", einoTestMCPTool("bob_only", "Bob only", ""))

	manager := &UserToolManager{registries: map[int64]*ToolRegistry{10: alice, 20: bob}}

	t.Run("empty", func(t *testing.T) {
		for _, view := range []*MergedToolView{nil, NewMergedToolView(nil, nil), NewMergedToolView(NewToolRegistry(), nil)} {
			set, node := newEinoTestToolNode(t, view, true)
			assertEinoToolNames(t, set)
			msgs := invokeEinoTestTools(t, context.Background(), node, einoTestCall("empty", "get_tool_details", "{}"))
			assertEinoToolMessage(t, msgs[0], "empty", "get_tool_details", `error: unknown tool "get_tool_details"`)
		}
		var set *einoToolSet
		assertEinoToolNames(t, set)
		if cfg := set.ToolsNodeConfig(); len(cfg.Tools) != 0 {
			t.Fatalf("nil set registered tools: %v", cfg.Tools)
		}
	})

	t.Run("global view", func(t *testing.T) {
		set, node := newEinoTestToolNode(t, NewMergedToolView(global, nil), false)
		assertEinoToolNames(t, set, "lookup", "global_only")
		if len(set.rawTools) != 2 || set.rawTools["lookup"].Function.Description != "global lookup" {
			t.Fatalf("incorrect global definitions: %v", set.rawTools)
		}
		msgs := invokeEinoTestTools(t, context.Background(), node, einoTestCall("global", "lookup", "{}"))
		assertEinoToolMessage(t, msgs[0], "global", "lookup", "global lookup")
	})

	t.Run("Alice", func(t *testing.T) {
		t.Parallel()
		view := NewMergedToolView(global, manager.GetRegistry(10))
		set, node := newEinoTestToolNode(t, view, true)
		assertEinoToolNames(t, set, "get_tool_details")
		definition := set.rawTools["lookup"].Function
		if definition.Description != "personal lookup" {
			t.Fatalf("personal tool did not shadow the global schema: %q", definition.Description)
		}
		gotSchema, err := json.Marshal(definition.Parameters)
		if err != nil {
			t.Fatal(err)
		}
		var got, want any
		if err := json.Unmarshal(gotSchema, &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(personalSchema), &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("raw parameters = %s, want %s", gotSchema, personalSchema)
		}

		var events []DashboardEvent
		ctx := withEinoToolEvents(context.Background(), UsageContext{}, func(event DashboardEvent) {
			events = append(events, event)
		})
		msgs := invokeEinoTestTools(t, ctx, node,
			einoTestCall("expand", "get_tool_details", `{"groups":["lIBrAry","Research"]}`),
			einoTestCall("hidden", "lookup", "{}"),
			einoTestCall("unknown", "bob_only", "{}"),
		)
		assertEinoToolMessage(t, msgs[0], "expand", "get_tool_details", "Tool schemas loaded for: lIBrAry, Research. You can now call those tools.")
		assertEinoToolMessage(t, msgs[1], "hidden", "lookup", `error: unknown tool "lookup"`)
		assertEinoToolMessage(t, msgs[2], "unknown", "bob_only", `error: unknown tool "bob_only"`)
		if len(events) != 0 || len(personalCalls) != 0 {
			t.Fatalf("expected no executions, got %d calls and %d events", len(personalCalls), len(events))
		}

		assertEinoToolNames(t, set, "get_tool_details", "lookup", "alice_only")
		msgs = invokeEinoTestTools(t, ctx, node, einoTestCall("personal", "lookup", "{}"))
		assertEinoToolMessage(t, msgs[0], "personal", "lookup", "personal lookup")

		otherRequest, otherNode := newEinoTestToolNode(t, view, true)
		assertEinoToolNames(t, otherRequest, "get_tool_details")
		invokeEinoTestTools(t, ctx, otherNode, einoTestCall("core", "get_tool_details", `{"groups":["Core"]}`))
		assertEinoToolNames(t, otherRequest, "get_tool_details", "global_only")
		msgs = invokeEinoTestTools(t, ctx, otherNode, einoTestCall("still-hidden", "lookup", "{}"))
		assertEinoToolMessage(t, msgs[0], "still-hidden", "lookup", `error: unknown tool "lookup"`)
		if len(events) != 2 || len(personalCalls) != 1 {
			t.Fatalf("expected one real execution, got %d calls and %d events", len(personalCalls), len(events))
		}
	})

	t.Run("Bob", func(t *testing.T) {
		t.Parallel()
		set, node := newEinoTestToolNode(t, NewMergedToolView(global, manager.GetRegistry(20)), true)
		assertEinoToolNames(t, set, "get_tool_details")
		invokeEinoTestTools(t, context.Background(), node,
			einoTestCall("expand", "get_tool_details", `{"groups":["core","research","library"]}`))
		assertEinoToolNames(t, set, "get_tool_details", "lookup", "global_only", "bob_only")
		if len(set.rawTools) != 4 {
			t.Fatalf("Bob received unexpected canonical definitions: %v", set.rawTools)
		}
		msgs := invokeEinoTestTools(t, context.Background(), node, einoTestCall("shared", "lookup", "{}"))
		assertEinoToolMessage(t, msgs[0], "shared", "lookup", "global lookup")
	})
}

func TestEinoToolExecution(t *testing.T) {
	t.Run("ordered calls and errors", func(t *testing.T) {
		registry := NewToolRegistry()
		wireCalls := make(chan string, 5)
		observe := func(name string, result *mcp.CallToolResult, err error) einoTestTool {
			tool := einoTestMCPTool(name, "", "")
			tool.run = func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				var args struct {
					Value string
				}
				if bindErr := request.BindArguments(&args); bindErr != nil {
					return nil, bindErr
				}
				wireCalls <- name + ":" + args.Value
				return result, err
			}
			return tool
		}
		addEinoTestMCPServer(t, registry, "Tools",
			observe("save", mcp.NewToolResultText("saved {private data}"), nil),
			observe("reject", mcp.NewToolResultError("server refused"), nil),
			observe("broken", nil, errors.New("broken wire call")),
		)
		set, node := newEinoTestToolNode(t, NewMergedToolView(registry, nil), false)
		if !set.ToolsNodeConfig().ExecuteSequentially {
			t.Fatal("tool node permits parallel execution")
		}
		assertEinoToolNames(t, set, "save", "reject", "broken")
		usage := UsageContext{ChatID: 50, UserID: 12, RequestID: "reply-7"}
		events := make(chan DashboardEvent, 12)
		ctx := withEinoToolEvents(context.Background(), usage, func(event DashboardEvent) {
			events <- event
		})

		msgs := invokeEinoTestTools(t, ctx, node,
			einoTestCall("first", "save", `{"value":"one"}`),
			einoTestCall("second", "reject", `{"value":"two"}`),
			einoTestCall("hallucination", "invented", "{}"),
			einoTestCall("third", "broken", `{"value":"three"}`),
			einoTestCall("fourth", "save", `{"value":"four"}`),
		)
		assertEinoToolMessage(t, msgs[0], "first", "save", "saved {private data}")
		assertEinoToolMessage(t, msgs[1], "second", "reject", "error: server refused")
		assertEinoToolMessage(t, msgs[2], "hallucination", "invented", `error: unknown tool "invented"`)
		if msg := msgs[3]; msg.ToolCallID != "third" || msg.ToolName != "broken" || !strings.HasPrefix(msg.Content, "error: ") || !strings.Contains(msg.Content, "broken wire call") {
			t.Fatalf("incorrect protocol error result: %+v", msg)
		}
		assertEinoToolMessage(t, msgs[4], "fourth", "save", "saved {private data}")

		for _, want := range []string{"save:one", "reject:two", "broken:three", "save:four"} {
			select {
			case got := <-wireCalls:
				if got != want {
					t.Fatalf("wire call = %q, want %q", got, want)
				}
			default:
				t.Fatalf("missing wire call %q", want)
			}
		}
		if len(wireCalls) != 0 || len(events) != 8 {
			t.Fatalf("extra wire calls or incorrect event count: %d extra calls, %d events", len(wireCalls), len(events))
		}
		for _, want := range []struct {
			id, name string
			success  bool
		}{{"first", "save", true}, {"second", "reject", false}, {"third", "broken", false}, {"fourth", "save", true}} {
			start, finish := <-events, <-events
			if start.Type != DashboardEventToolCallStarted || finish.Type != DashboardEventToolCallFinished || finish.Success != want.success {
				t.Fatalf("incorrect event pair for %q: %+v %+v", want.id, start, finish)
			}
			for _, event := range []DashboardEvent{start, finish} {
				if event.ToolName != want.name || event.ChatID != usage.ChatID || event.UserID != usage.UserID || event.RequestID != usage.RequestID || event.Detail != "call "+want.id {
					t.Fatalf("incorrect event metadata for %q: %+v", want.id, event)
				}
			}
		}

		if got, err := registry.Get("reject").Execute(`{"value":"legacy"}`); err != nil || got != "server refused" {
			t.Fatalf("legacy Execute changed its MCP IsError policy: %q, %v", got, err)
		}
	})

	t.Run("cancel in flight", func(t *testing.T) {
		registry := NewToolRegistry()
		started, canceled := make(chan struct{}), make(chan struct{})
		later := make(chan struct{}, 1)
		block := einoTestMCPTool("wait", "", "")
		block.run = func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			close(started)
			select {
			case <-ctx.Done():
				close(canceled)
				return nil, ctx.Err()
			case <-time.After(5 * time.Second):
				return nil, errors.New("test MCP handler timed out")
			}
		}
		next := einoTestMCPTool("later", "", "")
		next.run = func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			later <- struct{}{}
			return mcp.NewToolResultText("should not run"), nil
		}
		addEinoTestMCPServer(t, registry, "Tools", block, next)
		set, node := newEinoTestToolNode(t, NewMergedToolView(registry, nil), false)
		assertEinoToolNames(t, set, "wait", "later")
		events := make(chan DashboardEvent, 4)
		ctx, cancel := context.WithCancel(withEinoToolEvents(context.Background(), UsageContext{}, func(event DashboardEvent) {
			events <- event
		}))
		defer cancel()
		errCh := make(chan error, 1)
		go func() {
			_, err := node.Invoke(ctx, schema.AssistantMessage("", []schema.ToolCall{
				einoTestCall("wait-1", "wait", "{}"),
				einoTestCall("later-2", "later", "{}"),
			}))
			errCh <- err
		}()
		select {
		case <-started:
			cancel()
		case <-time.After(3 * time.Second):
			t.Fatal("MCP handler did not start")
		}
		select {
		case err := <-errCh:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("invoke returned %v, want context.Canceled", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("tool node did not return after cancellation")
		}
		select {
		case <-canceled:
		case <-time.After(3 * time.Second):
			t.Fatal("MCP HTTP request remained active after cancellation")
		}
		if len(later) != 0 || len(events) != 2 {
			t.Fatalf("cancellation produced unexpected calls or events: %d later calls, %d events", len(later), len(events))
		}
		start, finish := <-events, <-events
		if start.Type != DashboardEventToolCallStarted || finish.Type != DashboardEventToolCallFinished || finish.Success || finish.Detail != "call wait-1" {
			t.Fatalf("incorrect cancellation events: %+v %+v", start, finish)
		}
	})
}

type einoTestTool struct {
	info mcp.Tool
	run  server.ToolHandlerFunc
}

func einoTestMCPTool(name, description, params string) einoTestTool {
	info := mcp.NewTool(name, mcp.WithDescription(description))
	if params != "" {
		info = mcp.NewToolWithRawSchema(name, description, json.RawMessage(params))
	}
	return einoTestTool{
		info: info,
		run: func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText(description), nil
		},
	}
}

func addEinoTestMCPServer(t *testing.T, registry *ToolRegistry, group string, tools ...einoTestTool) {
	t.Helper()
	mcpServer := server.NewMCPServer(group, "test")
	for _, tool := range tools {
		mcpServer.AddTool(tool.info, tool.run)
	}
	httpServer := server.NewTestStreamableHTTPServer(mcpServer, server.WithStateLess(true))
	t.Cleanup(httpServer.Close)
	manager := NewMCPClientManager()
	t.Cleanup(manager.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := manager.connectOne(ctx, group, MCPServerConfig{URL: httpServer.URL}, registry); err != nil {
		t.Fatal(err)
	}
}

func newEinoTestToolNode(t *testing.T, view *MergedToolView, summaryMode bool) (*einoToolSet, *compose.ToolsNode) {
	t.Helper()
	set, err := newEinoTools(view, summaryMode)
	if err != nil {
		t.Fatal(err)
	}
	config := set.ToolsNodeConfig()
	node, err := compose.NewToolNode(context.Background(), &config)
	if err != nil {
		t.Fatal(err)
	}
	return set, node
}

func assertEinoToolNames(t *testing.T, set *einoToolSet, want ...string) {
	t.Helper()
	var got []string
	for _, info := range set.RoundToolInfos() {
		got = append(got, info.Name)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("round tools = %v, want %v", got, want)
	}
}

func einoTestCall(id, name, args string) schema.ToolCall {
	return schema.ToolCall{ID: id, Function: schema.FunctionCall{Name: name, Arguments: args}}
}

func invokeEinoTestTools(t *testing.T, ctx context.Context, node *compose.ToolsNode, calls ...schema.ToolCall) []*schema.Message {
	t.Helper()
	msgs, err := node.Invoke(ctx, schema.AssistantMessage("", calls))
	if err != nil || len(msgs) != len(calls) {
		t.Fatalf("invoke returned %d tool results, want %d (%v)", len(msgs), len(calls), err)
	}
	return msgs
}

func assertEinoToolMessage(t *testing.T, got *schema.Message, id, name, content string) {
	t.Helper()
	if got.Role != schema.Tool || got.ToolCallID != id || got.ToolName != name || got.Content != content {
		t.Fatalf("tool result = %+v, want %s/%s %q", got, id, name, content)
	}
}
