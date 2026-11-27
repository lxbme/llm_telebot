package app

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	openai "github.com/sashabaranov/go-openai"
)

func enableTestEngagementMCP(t *testing.T, b *Bot, url string) {
	t.Helper()
	cfg, ok := TryParseMCPConfig(`{"mcpServers":{"upstream":{"url":` + strconv.Quote(url) + `,"backgroundIdentity":"stable-upstream-v1","background":{"release":{"operation":"release.list","target":"upstream/releases","arguments":{"repository":"upstream/project"}}}}}}`)
	if !ok {
		t.Fatal("invalid MCP test configuration")
	}
	b.tools, b.mcpClients = NewToolRegistry(), NewMCPClientManager()
	if failures := b.mcpClients.ConnectAll(cfg, b.tools); len(failures) != 0 {
		t.Fatalf("MCP connections: %v", failures)
	}
	t.Cleanup(b.mcpClients.Close)
}

func newEngagementMCP(t *testing.T, toolName string, readOnly bool) string {
	t.Helper()
	stub := server.NewMCPServer(toolName, "test")
	tool := einoTestMCPTool(toolName, toolName+" result", "")
	tool.info.Annotations.ReadOnlyHint = &readOnly
	if readOnly {
		mcp.WithString("repository", mcp.Required())(&tool.info)
	} else {
		for _, name := range []string{"calendarId", "title", "start", "end"} {
			mcp.WithString(name, mcp.Required())(&tool.info)
		}
	}
	stub.AddTool(tool.info, tool.run)
	httpServer := server.NewTestStreamableHTTPServer(stub, server.WithStateLess(true))
	t.Cleanup(httpServer.Close)
	return httpServer.URL
}

func TestEngagementCapabilityLivePaginationAndSchemaChange(t *testing.T) {
	s := newEngagementScript(t)
	stub := server.NewMCPServer("release", "test", server.WithPaginationLimit(1))
	other := einoTestMCPTool("alpha", "other result", "")
	stub.AddTool(other.info, other.run)
	addRelease := func(schema string) {
		registered := einoTestMCPTool("release", "release result", "")
		info := mcp.NewToolWithRawSchema("release", "release result", []byte(schema))
		stub.AddTool(info, registered.run)
	}
	addRelease(`{"type":"object","properties":{"repository":{"type":"string"}},"required":["repository"]}`)
	protocol := server.NewTestStreamableHTTPServer(stub, server.WithStateLess(true))
	t.Cleanup(protocol.Close)
	enableTestEngagementMCP(t, s.b, protocol.URL)
	request := "Tell me when the stable release arrives."
	s.tool("engagement_context", struct{}{}, nil)
	s.tool("engagement_decide", regressionWatchArgs(), func(req openai.ChatCompletionRequest) {
		if !strings.Contains(engagementLastResult(req), "public:upstream:release") {
			t.Error("enrolled tool on a later page did not appear in the role's context")
		}
		addRelease(`{"type":"object","properties":{"repository":{"type":"string"}},"required":["repository"],"maxProperties":0}`)
	})
	s.final("Saved.", nil)
	if got := s.send(engagementOwner, engagementOwner, 671, request); !strings.Contains(got, "No change was saved") {
		t.Fatal(got)
	}
	s.tool("engagement_context", struct{}{}, nil)
	s.final("No connector is available.", func(req openai.ChatCompletionRequest) {
		if strings.Contains(engagementLastResult(req), "public:upstream:release") {
			t.Error("tool with changed input constraints remained in the live options")
		}
	})
	s.send(engagementOwner, engagementOwner, 672, request)
}

func TestEngagementCapabilitySamePathMCPConfigReload(t *testing.T) {
	s := newEngagementScript(t)
	url := newEngagementMCP(t, "release", true)
	dir := t.TempDir()
	cfg := s.b.currentConfig()
	cfg.ToolsEnabled = true
	cfg.MCPConfigPath = filepath.Join(dir, "global.json")
	cfg.StickerRulesPath = filepath.Join(dir, "stickers.json")
	cfg.UserMCPDBPath = filepath.Join(dir, "users.db")
	writeConfig := func(target string) {
		raw := fmt.Sprintf(`{"mcpServers":{"upstream":{"url":%q,"backgroundIdentity":"same-enrollment","background":{"release":{"operation":"release.list","target":%q,"arguments":{"repository":%q}}}}}}`, url, target, target)
		if err := os.WriteFile(cfg.MCPConfigPath, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	apply := func() {
		if err := s.b.applyRuntimeConfig(cfg); err != nil {
			t.Fatal(err)
		}
	}
	binding := func() EngagementCapability {
		return s.b.snapshot().engagementCapabilities(engagementOwner, engagementOwner)["public:upstream:release"]
	}
	writeConfig("org/old")
	apply()
	t.Cleanup(func() {
		snap := s.b.snapshot()
		closeToolResources(snap.mcpClients, snap.userTools)
	})
	original := binding()
	if original.Target != "org/old" {
		t.Fatalf("startup target = %+v", original)
	}

	writeConfig("org/new")
	cfg.ToolsMaxIterations++
	apply()
	if current := binding(); current != original {
		t.Fatalf("unrelated reload unexpectedly rebuilt the MCP connection: %+v", current)
	}
	cfg.ToolsEnabled = false
	apply()
	cfg.ToolsEnabled = true
	apply()
	if current := binding(); current.Target != "org/new" || current.Access == original.Access {
		t.Fatalf("connection rebuild did not read the new file: %+v", current)
	}
}

func TestEngagementCapabilityUntypedNestedArguments(t *testing.T) {
	for _, tc := range []struct {
		name, fixed string
		allowed     bool
	}{
		{"matching value", "{\"id\":7}", true},
		{"wrong nested type", "{\"id\":\"7\"}", false},
		{"required field missing", "{}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newEngagementScript(t)
			stub := server.NewMCPServer("release", "test")
			register := func(nestedSchema string) {
				tool := einoTestMCPTool("release", "release result", "")
				schema := fmt.Sprintf(`{"type":"object","properties":{"repository":%s},"required":["repository"]}`, nestedSchema)
				info := mcp.NewToolWithRawSchema("release", "release result", []byte(schema))
				stub.AddTool(info, tool.run)
			}
			register(`{"properties":{"id":{"type":"number"}},"required":["id"]}`)
			protocol := server.NewTestStreamableHTTPServer(stub, server.WithStateLess(true))
			t.Cleanup(protocol.Close)
			cfg, ok := TryParseMCPConfig(fmt.Sprintf(`{"mcpServers":{"upstream":{"url":%q,"backgroundIdentity":"upstream-test","background":{"release":{"operation":"release.list","target":"project","arguments":{"repository":%s}}}}}}`, protocol.URL, tc.fixed))
			if !ok {
				t.Fatal("invalid MCP test configuration")
			}
			s.b.tools, s.b.mcpClients = NewToolRegistry(), NewMCPClientManager()
			t.Cleanup(s.b.mcpClients.Close)
			if failed := s.b.mcpClients.ConnectAll(cfg, s.b.tools); len(failed) != 0 {
				t.Fatalf("MCP connection: %v", failed)
			}
			s.tool("engagement_context", struct{}{}, nil)
			s.tool("engagement_decide", regressionWatchArgs(), func(req openai.ChatCompletionRequest) {
				if found := strings.Contains(engagementLastResult(req), "public:upstream:release"); found != tc.allowed {
					t.Errorf("live discovery allowed=%v, want %v", found, tc.allowed)
				}
			})
			s.final("Saved.", nil)
			got := s.send(engagementOwner, engagementOwner, 722, "Tell me when the stable release arrives.")
			viewer := EngagementViewer{ChatID: engagementOwner, ActorID: engagementOwner}
			rows, err := s.b.engagements.Visible(viewer, "", 8)
			if err != nil || (len(rows) == 1) != tc.allowed || strings.Contains(got, "Agreement saved") != tc.allowed {
				t.Fatalf("receipt = %q; rows = %v, %v", got, rows, err)
			}
			if !tc.allowed {
				return
			}
			item, err := s.b.engagements.Get(rows[0].Ref.ID)
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := s.b.verifyEngagementCapability(t.Context(), item, item.Current.Observe)
			if err != nil {
				t.Fatal(err)
			}
			if args, err := resolved.arguments(nil); args != `{"repository":{"id":7}}` || err != nil {
				t.Fatalf("verified payload = %q, %v", args, err)
			}
			register(`{"properties":{"id":{"type":"number"}},"required":["id","region"]}`)
			if _, err := s.b.verifyEngagementCapability(t.Context(), item, item.Current.Observe); err == nil {
				t.Fatal("verify accepted a payload that misses a new nested requirement")
			}
		})
	}
}

func TestEngagementCapabilityExactNumbers(t *testing.T) {
	const args = `{"repository":{"id":9007199254740993,"threshold":0.3}}`
	const valid = `"type":"integer","const":9007199254740993.0,"enum":[9.007199254740993e15]`
	for _, tc := range []struct {
		name, idSchema string
		allowed        bool
	}{
		{"equal numbers in different notation", valid, true},
		{"neighboring const", `"type":"integer","const":9007199254740992`, false},
		{"neighboring enum", `"type":"integer","enum":[9007199254740992]`, false},
		{"neighboring bound", `"type":"integer","maximum":9007199254740992`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newEngagementScript(t)
			stub := server.NewMCPServer("release", "test")
			register := func(idSchema string) {
				bounds := ""
				if tc.allowed {
					bounds = `,"minimum":0.1,"multipleOf":0.1`
				}
				contract := fmt.Sprintf(`{"type":"object","properties":{"repository":{"type":"object","properties":{"id":{%s},"threshold":{"type":"number"%s}},"required":["id","threshold"]}},"required":["repository"]}`, idSchema, bounds)
				stub.AddTool(mcp.NewToolWithRawSchema("release", "release", []byte(contract)), func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					t.Error("verification must not execute the background tool")
					return mcp.NewToolResultText("unused"), nil
				})
			}
			register(tc.idSchema)
			protocol := server.NewTestStreamableHTTPServer(stub, server.WithStateLess(true))
			t.Cleanup(protocol.Close)
			path := filepath.Join(t.TempDir(), "mcp.json")
			if err := os.WriteFile(path, fmt.Appendf(nil, `{"mcpServers":{"upstream":{"url":%q,"backgroundIdentity":"numeric-test","background":{"release":{"operation":"release.list","target":"project","arguments":%s}}}}}`, protocol.URL, args), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadMCPConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			s.b.tools, s.b.mcpClients = NewToolRegistry(), NewMCPClientManager()
			t.Cleanup(s.b.mcpClients.Close)
			if failures := s.b.mcpClients.ConnectAll(cfg, s.b.tools); len(failures) != 0 {
				t.Fatal(failures)
			}
			got := sendRegressionAgreement(s, regressionWatchArgs(), engagementOwner, engagementOwner, 726, "Tell me when the stable release arrives.")
			rows, err := s.b.engagements.Visible(EngagementViewer{ChatID: engagementOwner, ActorID: engagementOwner}, "", 8)
			if err != nil || (len(rows) == 1) != tc.allowed || strings.Contains(got, "Agreement saved") != tc.allowed {
				t.Fatalf("receipt = %q; rows = %v, %v", got, rows, err)
			}
			if !tc.allowed {
				return
			}
			item, err := s.b.engagements.Get(rows[0].Ref.ID)
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := s.b.verifyEngagementCapability(t.Context(), item, item.Current.Observe)
			if err != nil {
				t.Fatal(err)
			}
			if payload, err := resolved.arguments(nil); payload != args || err != nil {
				t.Fatalf("verified payload = %q, %v", payload, err)
			}
			// A saved digest from the third-round format included the raw
			// schema when the SDK projection rounded its numeric constraints.
			const projected = `{"properties":{"repository":{"properties":{"id":{"const":9007199254740992,"enum":[9007199254740992],"type":"integer"},"threshold":{"minimum":0.1,"multipleOf":0.1,"type":"number"}},"required":["id","threshold"],"type":"object"}},"required":["repository"],"type":"object"}`
			legacy := func(messageID int64, projected, full, payload string) Engagement {
				identity := fmt.Sprintf(`["numeric-test","upstream","streamablehttp",%q,null,"release",%s,%s,{"operation":"release.list","target":"project","arguments":%s}]`,
					protocol.URL, projected, full, payload)
				create := engagementFixture(engagementOwner)
				capability := item.Current.Observe
				capability.Access.Key = fmt.Sprintf("engagement:mcp:%x", sha256.Sum256([]byte(identity)))
				create.Decision.Request.Source.MessageID = messageID
				create.Decision.Agreement.Observe = capability
				create.Decision.Agreement.Trigger.Access = capability.Access
				create.Decision.Authorized.Capabilities = []EngagementCapability{capability}
				create.Decision.Authorized.DataAccess = []MemoryDataAccess{capability.Access}
				ref := createEngagement(t, s.b.engagements, create)
				saved, err := s.b.engagements.Get(ref.ID)
				if err != nil {
					t.Fatal(err)
				}
				return saved
			}
			full := strings.NewReplacer(`"const":9007199254740992`, `"const":9007199254740993.0`,
				`"enum":[9007199254740992]`, `"enum":[9.007199254740993e15]`).Replace(projected)
			exactLegacy := legacy(727, projected, full, args)
			if _, err := s.b.verifyEngagementCapability(t.Context(), exactLegacy, exactLegacy.Current.Observe); err != nil {
				t.Fatalf("an exact legacy digest no longer verifies: %v", err)
			}
			d := engagementDialogue{turn: RoleTurn{ChatID: engagementOwner, SenderID: engagementOwner}}
			visible, err := s.b.engagements.ReadVisible(exactLegacy.Ref.ID, d.viewer(s.b.snapshot()))
			if err != nil || visible.Agreement == nil || visible.Agreement.Trigger == "" {
				t.Fatalf("an exact legacy agreement is no longer visible: %+v, %v", visible, err)
			}
			register(`"type":"integer","const":9007199254740993,"enum":[9007199254740993.00]`)
			if _, err := s.b.verifyEngagementCapability(t.Context(), item, item.Current.Observe); err != nil {
				t.Fatalf("equivalent live numeric notation invalidated the contract: %v", err)
			}
			reconnect := func() {
				s.b.mcpClients.Close()
				s.b.tools, s.b.mcpClients = NewToolRegistry(), NewMCPClientManager()
				if failures := s.b.mcpClients.ConnectAll(cfg, s.b.tools); len(failures) != 0 {
					t.Fatal(failures)
				}
			}
			t.Cleanup(func() { s.b.mcpClients.Close() })
			reconnect()
			if _, err := s.b.verifyEngagementCapability(t.Context(), item, item.Current.Observe); err != nil {
				t.Errorf("equivalent contract changed the saved capability after reconnect: %v", err)
			}
			register(`"type":"integer","const":9007199254740992`)
			if _, err := s.b.verifyEngagementCapability(t.Context(), item, item.Current.Observe); err == nil {
				t.Fatal("neighboring live constant still matched the approved payload")
			}
			// Re-enroll a valid neighboring contract and payload on the same server.
			serverConfig := cfg.MCPServers["upstream"]
			policy := serverConfig.Background["release"]
			neighbor := strings.Replace(args, "9007199254740993", "9007199254740992", 1)
			policy.Arguments = []byte(neighbor)
			serverConfig.Background["release"] = policy
			reconnect()
			if _, err := s.b.verifyEngagementCapability(t.Context(), item, item.Current.Observe); err == nil {
				t.Fatal("neighboring enrollment resolved to the old saved capability")
			}
			// A projected legacy digest without the raw schema could describe
			// either large neighbor on an earlier, rounding release.
			rounded := legacy(728, strings.ReplaceAll(projected, `,"enum":[9007199254740992]`, ""), "null", neighbor)
			if _, err := s.b.verifyEngagementCapability(t.Context(), rounded, rounded.Current.Observe); err == nil {
				t.Fatal("ambiguous numeric legacy binding was restored")
			}
			// Restore the exact contract and payload but change the credential.
			register(valid)
			policy.Arguments = []byte(args)
			serverConfig.Background["release"] = policy
			serverConfig.Headers = map[string]string{"Authorization": "Bearer local-other-account"}
			cfg.MCPServers["upstream"] = serverConfig
			reconnect()
			if _, err := s.b.verifyEngagementCapability(t.Context(), item, item.Current.Observe); err == nil {
				t.Fatal("new credential resolved to the old saved capability")
			}
		})
	}
}
