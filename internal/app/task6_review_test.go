package app

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	openai "github.com/sashabaranov/go-openai"
	bolt "go.etcd.io/bbolt"
	tele "gopkg.in/telebot.v3"
)

func TestTask6ReviewRecipientActualDelivery(t *testing.T) {
	for _, tc := range []struct {
		name, request, proposed, aux string
		onlyUserAllowed              bool
		wantRejected                 bool
		wantDM                       bool
	}{
		{"excluded_current_chat", "Tell me when the stable release arrives. Do not notify this chat.", "here", "", false, true, false},
		{"specified_other_user", "Send the stable release update to Telegram user 72.", "here", "", false, true, false},
		{"other_user_id_label", "Send the stable release update to Telegram user ID: 72.", "here", "here", false, true, false},
		{"other_user_id_affixed", "Send the stable release update to Telegram user ID=#72.", "", "", false, true, false},
		{"release_version_before_other_id", "Send the stable release 2.0 update to Telegram user ID: 72.", "here", "", false, true, false},
		{"missing_id", "Send the stable release update to Telegram user ID: unknown.", "here", "", false, true, false},
		{"multiple_ids", "Send the stable release update to Telegram user ID: 71/72.", "", "", false, true, false},
		{"comma_then_recipient_list", "Tell Telegram user 71, 11/26.", "private", "private", false, true, false},
		{"multiple_labeled_ids", "Send the stable release update to Telegram user ID: 71 or Telegram user ID: 72.", "", "", false, true, false},
		{"decimal_id", "Send the stable release update to Telegram user ID: 71.5.", "", "", false, true, false},
		{"group_origin_personally_whitelisted", "DM me when the stable release arrives.", "private", "private", true, true, false},
		{"group_and_dm_authorized", "DM me when the stable release arrives.", "private", "private", false, false, true},
		{"named_self", "Send the stable release update to Telegram user 71.", "private", "", false, false, true},
		{"self_id_label_conflicting_model", "Send the stable release update to Telegram user ID: 71.", "here", "here", false, false, true},
		{"self_id_fullwidth_separator", "Send the stable release update to Telegram user ID：71.", "", "", false, false, true},
		{"report_rewrite_then_dm", "等发行说明重写了请私聊我。", "here", "here", false, false, true},
		{"report_rewrite_explicit_dm", "等发行说明重写了请私聊我。", "private", "private", false, false, true},
		{"report_rewrite_whitespace_then_dm", "等发行说明重写了 请私聊我。", "here", "here", false, false, true},
		{"report_rewrite_whitespace_and_exclusion", "等发行说明重寫了　請\t私訊我，別通知本群。", "here", "here", false, false, true},
		{"report_rewrite_whitespace_without_source_access", "等发行说明重写了 请私聊我。", "here", "here", true, true, false},
		{"chinese_special_release", "特别版发布时请私聊我。", "", "", false, false, true},
		{"chinese_private_after_release", "特别版发布后请私聊我。", "here", "here", false, false, true},
		{"chinese_private_after_release_without_source_access", "特别版发布后请私聊我。", "here", "here", true, true, false},
		{"chinese_private_excluded_after_release", "特别版发布后请不要私聊我，只通知本群。", "private", "private", false, false, false},
		{"chinese_private_action_is_observed", "维护者私聊我后，请通知本群。", "private", "private", false, false, false},
		{"chinese_private_action_in_quote", "稳定版发布。公告提到「请私聊我」。只通知本群。", "private", "private", false, false, false},
		{"chinese_third_party_report", "稳定版发布。维护者说：请私聊我。只通知本群。", "private", "private", false, false, false},
		{"chinese_unquoted_announcement", "公告上写着请私聊我。稳定版发布时请通知本群。", "here", "here", false, false, false},
		{"chinese_reported_future_request", "维护者说他会请你私聊我的同事。稳定版发布时请通知本群。", "here", "here", false, false, false},
		{"chinese_other_private_recipient", "特别版发布后请私聊我的同事。", "here", "here", false, true, false},
		{"chinese_coordinated_recipient_conflict", "特别版发布后请私聊我并通知本群。", "here", "here", false, true, false},
		{"chinese_private_excluded", "稳定版发布时别再私聊我，请通知本群。", "", "", false, false, false},
		{"chinese_send_to_private_excluded", "稳定版发布时不要发消息到我的私聊，请通知本群。", "here", "here", false, false, false},
		{"agree_group_excludes_private", "稳定版发布时不要发消息到我的私聊，请通知本群。", "private", "private", false, false, false},
		{"chinese_private_directives_conflict", "稳定版发布时不要发消息到我的私聊，请私聊我。", "private", "private", false, true, false},
		{"chinese_private_excluded_wrong_model", "稳定版发布时别私聊我。", "private", "private", false, true, false},
		{"chinese_do_not_notify_group", "稳定版发布时请私聊我，但别通知本群。", "", "", false, false, true},
		{"chinese_group_in_condition", "如果维护者通知本群，就私聊我，条件是 stable release 到达。", "", "", false, false, true},
		{"chat_excluded_dm_authorized", "DM me when the stable release arrives. Do not notify this chat.", "private", "private", false, false, true},
		{"chat_excluded_same_sentence", "Do not notify this chat and DM me when the stable release arrives.", "private", "", false, false, true},
		{"other_group_excluded", "Tell me when the stable release arrives. Do not notify group -702.", "here", "", false, false, false},
		{"group_is_source", "When maintainers notify group -702 about the stable release, tell me.", "here", "", false, false, false},
		{"group_labeled_is_source", "When maintainers notify group ID: -702 about the stable release, tell me.", "here", "", false, false, false},
		{"ordinary_numbers", "Tell me when the stable release 2.0 arrives on 2026-11-26.", "here", "", false, false, false},
		{"ordinary_date_after_address", "Tell Telegram user ID: 71, 2026-11-26 is the stable release date.", "", "", false, false, true},
		{"slash_date_after_group_address", "Send the update to group -701, 11/26 is the stable release date.", "here", "here", false, false, false},
		{"slash_date_after_user_address", "Tell Telegram user 71, 11/26 is the stable release date.", "private", "private", false, false, true},
		{"reaccept_with_date_after_address", "Please expand this tracking. Tell Telegram user 71, 11/26 is the stable release date.", "here", "here", false, false, true},
		{"user_is_subject", "Send me the update about Telegram user ID: 72 when the stable release arrives.", "here", "", false, false, false},
		{"user_is_conditional_source", "When Telegram user ID: 72 posts a stable release, tell me.", "here", "", false, false, false},
		{"excluded_other_user", "Tell me when the stable release arrives. Do not notify Telegram user ID: 72.", "here", "", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reads atomic.Int64
			stub := server.NewMCPServer("release", "audit")
			stub.AddTool(mcp.NewTool("release", mcp.WithReadOnlyHintAnnotation(true), mcp.WithString("repository", mcp.Required())), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				reads.Add(1)
				if req.GetString("repository", "") != "upstream/project" {
					t.Errorf("wrong query: %v", req.GetArguments())
				}
				return &mcp.CallToolResult{StructuredContent: map[string]any{"events": []engagementEvent{{ID: "native-stable", Title: "Stable version 2.0", Summary: "Stable release published"}}}}, nil
			})
			protocol := server.NewTestStreamableHTTPServer(stub, server.WithStateLess(true))
			t.Cleanup(protocol.Close)
			s := newEngagementScript(t)
			enableTestEngagementMCP(t, s.b, protocol.URL)
			if tc.onlyUserAllowed {
				s.b.cfg.AllowedUsers = map[int64]bool{engagementOwner: true}
				s.b.cfg.AllowedGroups = nil
			} else {
				s.b.cfg.AllowedUsers = map[int64]bool{engagementOwner: true}
				s.b.cfg.AllowedGroups = map[int64]bool{engagementGroup: true}
			}
			s.intents.Store(tc.request, map[string]any{"recipient": tc.aux})
			args := regressionWatchArgs()
			args["recipient"] = tc.proposed
			if trigger, _, ok := strings.Cut(tc.request, "发布时"); ok {
				args["trigger"] = trigger + "发布"
			}
			switch tc.name {
			case "report_rewrite_then_dm", "report_rewrite_explicit_dm", "report_rewrite_whitespace_then_dm",
				"report_rewrite_whitespace_without_source_access":
				args["trigger"] = "发行说明重写"
			case "report_rewrite_whitespace_and_exclusion":
				args["trigger"] = "发行说明重寫"
			case "chinese_private_after_release", "chinese_private_after_release_without_source_access",
				"chinese_private_excluded_after_release", "chinese_other_private_recipient", "chinese_coordinated_recipient_conflict":
				args["trigger"] = "特别版发布"
			case "chinese_private_action_is_observed":
				args["trigger"] = "维护者私聊我"
			case "chinese_private_action_in_quote", "chinese_third_party_report", "chinese_unquoted_announcement",
				"chinese_reported_future_request":
				args["trigger"] = "稳定版发布"
			case "group_is_source":
				args["trigger"] = "When maintainers notify group -702 about the stable release"
			}
			var receipt string
			var revision EngagementChange
			switch tc.name {
			case "reaccept_with_date_after_address":
				revision = EngagementReaccept
			case "agree_group_excludes_private":
				revision = EngagementAgree
			}
			if revision == "" {
				receipt = sendRegressionAgreement(s, args, engagementGroup, engagementOwner, 899, "@testbot "+tc.request)
			} else {
				start := sendRegressionAgreement(s, regressionWatchArgs(), engagementGroup, engagementOwner, 898,
					"@testbot Tell me when the stable release arrives.")
				initial := allEngagementRows(t, s.b.engagements)
				if len(initial) != 1 || !slices.Equal(initial[0].Current.Recipients, []int64{engagementGroup}) {
					t.Fatalf("group seed not saved: %s", start)
				}
				changes := map[string]any{"change": revision, "verdict": "accept", "recipient": tc.proposed}
				if revision == EngagementReaccept {
					changes["authorization_quote"] = "Tell Telegram user 71"
				}
				s.change(initial[0].Ref, changes)
				receipt = s.send(engagementGroup, engagementOwner, 899, "@testbot "+tc.request)
			}
			if tc.onlyUserAllowed && !strings.Contains(receipt, "source chat for background observation") &&
				!strings.Contains(receipt, "后台观察所需的来源会话") {
				t.Errorf("receipt did not explain the unavailable background source: %q", receipt)
			}
			rows := allEngagementRows(t, s.b.engagements)
			if len(rows) == 0 {
				rejected := strings.Contains(receipt, "No change was saved") || strings.Contains(receipt, "未保存变更")
				if !tc.wantRejected || !rejected || reads.Load() != 0 {
					t.Errorf("incorrect acceptance receipt or read count: %q / %d", receipt, reads.Load())
				}
				return
			}
			if len(rows) != 1 {
				t.Fatalf("rows=%d", len(rows))
			}
			if revision != "" && (len(rows[0].Decisions) != 2 || rows[0].Decisions[1].Verdict != EngagementAccept ||
				rows[0].Decisions[1].Change != revision) {
				t.Errorf("accepted revision not persisted: %s", receipt)
			}
			decision := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
				if req.Stream || len(req.Tools) != 0 || !strings.Contains(fmt.Sprint(req.Messages), "Stable version 2.0") {
					t.Errorf("wrong model request: %+v", req)
				}
				_ = json.NewEncoder(w).Encode(openai.ChatCompletionResponse{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{Content: `{"matched_ids":["native-stable"]}`}}}})
			})
			s.b.ai = decision.client
			r := NewTaskRunner(s.b, nil)
			r.tick(t.Context(), engagementNow)
			waitRunner(t, r)
			item, _ := s.b.engagements.Get(rows[0].Ref.ID)
			var sent roleTestDelivery
			select {
			case sent = <-s.replies:
			default:
			}
			t.Logf("receipt=%q destinations=%v observation_calls=%d finished=%s pause=%s send=%+v", receipt, item.Current.Recipients, reads.Load(), item.EndReason, item.PauseReason, sent)
			if tc.wantRejected {
				t.Errorf("conflicting request was committed and sent")
			} else {
				wantChat := int64(engagementGroup)
				if tc.wantDM {
					wantChat = engagementOwner
				}
				if item.EndReason != EngagementCompleted || sent.chatID != wantChat || reads.Load() != 1 ||
					!slices.Equal(item.Current.Recipients, []int64{wantChat}) ||
					!strings.Contains(sent.text, "Stable version 2.0") || len(s.replies) != 0 {
					t.Errorf("saved request did not fulfill once to %d", wantChat)
				}
			}
		})
	}
}

func TestTask6ReviewExistingMisroutedRecipients(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		revision   bool
	}{
		{"without_provenance", "Tell me when the stable release arrives. Do not notify this chat.", false},
		{"with_inherited_provenance", "Send the stable release update to Telegram user 72.", true},
		{"labeled_without_provenance", "Send the stable release update to Telegram user ID: 72.", false},
		{"labeled_with_inherited_provenance", "Send the stable release update to Telegram user ID: 72.", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRunnerHarness(t, false)
			b, _ := h.open()
			ref := h.seed(b, engagementGroup, false, true, engagementGroup)
			if tc.revision {
				item := getRunnerItem(t, b, ref)
				ref = reviseEngagement(t, b.engagements, ref, EngagementDecision{
					Request: engagementRequest(engagementGroup, engagementOwner, 940, "Track the stable release this week"),
					Verdict: EngagementAccept, Change: EngagementAgree, At: h.clock(),
					Agreement: item.Current, Authorized: EngagementAuthorization{
						Capabilities: []EngagementCapability{item.Current.Observe}, Recipients: item.Current.Recipients,
						RecipientSources: item.Current.RecipientSources,
					},
				})
			}
			// Recreate an already committed acceptance using the older parser.
			if err := b.engagements.transact(true, func(tx *bolt.Tx) error {
				row, err := loadEngagement(tx, ref.ID)
				if err != nil {
					return err
				}
				if !tc.revision {
					row.Current.RecipientSources = nil
				}
				row.Decisions[len(row.Decisions)-1].Request.Text = tc.text
				return saveEngagement(tx, &row)
			}); err != nil {
				t.Fatal(err)
			}
			h.tick(b)
			after := getRunnerItem(t, b, ref)
			if after.Status != EngagementPaused || after.PauseReason != EngagementRouteUnverified ||
				!after.LastDeliveredAt.IsZero() || h.connector.reads.Load() != 0 || len(h.telegram.sends) != 0 {
				t.Errorf("unsafe previous route: status=%s pause=%s reads=%d sends=%d", after.Status, after.PauseReason, h.connector.reads.Load(), len(h.telegram.sends))
			}
		})
	}
}

func TestTask6ReviewLegacyDirectRecipient(t *testing.T) {
	for _, tc := range []struct {
		request    string
		withSource bool
		recipient  int64
	}{
		{"Send the stable release update to Telegram user ID: 71.", false, engagementOwner},
		{"特别版发布时请私聊我。", true, engagementOwner},
		{"特别版发布后请私聊我。", false, engagementOwner},
		{"特别版发布后请私聊我。", true, engagementOwner},
		{"等发行说明重写了请私聊我。", true, engagementOwner},
		{"等发行说明重写了请私聊我。", false, engagementOwner},
		{"等发行说明重写了 请私聊我。", true, engagementOwner},
		{"等发行说明重寫了　請\t私訊我，別通知本群。", false, engagementOwner},
		{"等发行说明重写了 请私聊我。", true, engagementGroup},
		{"等发行说明重写了 请私聊我。", false, engagementGroup},
		{"稳定版发布时不要发消息到我的私聊，请通知本群。", false, engagementGroup},
		{"公告上写着　请私聊我。稳定版发布时请通知本群。", true, engagementGroup},
		{"Tell Telegram user 71, 11/26 is the stable release date.", true, engagementOwner},
	} {
		t.Run(tc.request, func(t *testing.T) {
			h := newRunnerHarness(t, false)
			b, _ := h.open()
			create := engagementFixture(engagementGroup)
			a := create.Decision.Agreement
			a.Observe = b.snapshot().engagementCapabilities(engagementOwner, engagementGroup)["public:upstream:event_list"]
			a.Recipients = []int64{tc.recipient}
			if tc.recipient == engagementOwner {
				create.Decision.Request.Text = "DM me when the stable release arrives."
			}
			create.Decision.Authorized.Capabilities = []EngagementCapability{a.Observe}
			create.Decision.Authorized.Recipients = a.Recipients
			if tc.withSource {
				create.Decision.Authorized.RecipientSources = []EngagementRecipientSource{{tc.recipient, create.Decision.Request.Source}}
			}
			ref := createEngagement(t, b.engagements, create)
			if err := b.engagements.transact(true, func(tx *bolt.Tx) error {
				row, err := loadEngagement(tx, ref.ID)
				if err != nil {
					return err
				}
				row.Initial.Request.Text, row.Decisions[0].Request.Text = tc.request, tc.request
				return saveEngagement(tx, &row)
			}); err != nil {
				t.Fatal(err)
			}
			h.tick(b)
			row := getRunnerItem(t, b, ref)
			if row.Decisions[0].Request.Text != tc.request {
				t.Fatalf("stored source wording changed: %q", row.Decisions[0].Request.Text)
			}
			if tc.recipient == engagementGroup && strings.HasPrefix(tc.request, "等发行说明") {
				if row.PauseReason != EngagementRouteUnverified || h.connector.reads.Load() != 0 || len(h.telegram.sends) != 0 {
					t.Fatalf("old group binding used a private directive: pause=%s reads=%d sends=%d", row.PauseReason, h.connector.reads.Load(), len(h.telegram.sends))
				}
				return
			}
			if row.EndReason != EngagementCompleted || h.connector.reads.Load() != 1 || len(h.telegram.sends) != 1 {
				t.Fatalf("legacy route did not fulfill: status=%s pause=%s reads=%d sends=%d", row.Status, row.PauseReason, h.connector.reads.Load(), len(h.telegram.sends))
			}
			if sent := <-h.telegram.sends; sent.chatID != tc.recipient {
				t.Fatalf("legacy route delivered to %d, want %d", sent.chatID, tc.recipient)
			}
		})
	}
}

func TestTask6ReviewLegacyEmptyModel(t *testing.T) {
	for _, tc := range []struct {
		name, tool, text, visible string
		wantError                 bool
	}{
		{name: "no choices", visible: "empty response", wantError: true},
		{name: "whitespace", text: "   ", visible: "empty response", wantError: true},
		{name: "tool error only", tool: "missing", visible: "unknown tool", wantError: true},
		{name: "usable tool fallback", tool: "lookup", visible: "result 1"},
		{name: "ordinary answer", text: "The due result.", visible: "The due result."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, round int, _ openai.ChatCompletionRequest) {
				if tc.tool != "" && round == 0 {
					einoModelSSE(w, einoReplyToolFrame("audit-tool", tc.tool, 0))
					einoReplyFinish(w, "tool_calls")
					return
				}
				if tc.text != "" {
					engagementModelText(w, tc.text)
					return
				}
				einoModelSSE(w, einoReplyUsageFrame, "[DONE]")
			})
			b, replies := newRoleTestBot(t, provider)
			b.tasks = NewTaskStore(b.chatDB)
			var toolRuns atomic.Int32
			b.tools = newEinoReplyRegistry(t, &toolRuns)
			task := ScheduledTask{ID: "audit", Prompt: "The due result", CronExpr: "* * * * *", Timezone: "UTC", Enabled: true, CreatedBy: engagementOwner, NextRunAt: engagementNow}
			b.tasks.Upsert(engagementGroup, task)
			r := NewTaskRunner(b, b.tasks)
			r.runDue(engagementNow)
			sent := roleReceive(t, replies)
			auditWaitLegacy(t, r, fmt.Sprintf("%d:%s", engagementGroup, task.ID))
			row := b.tasks.List(engagementGroup)[0]
			if sent.chatID != engagementGroup || !strings.Contains(sent.text, tc.visible) ||
				row.LastRunAt.IsZero() || (row.LastError != "") != tc.wantError ||
				(tc.wantError && !strings.Contains(row.LastError, "model")) {
				t.Fatalf("unexpected Telegram delivery or run result: %+v, %+v", sent, row)
			}
			if (toolRuns.Load() == 1) != (tc.tool == "lookup") {
				t.Errorf("unexpected tool calls: %d", toolRuns.Load())
			}
		})
	}
}

func TestTask6ReviewLegacyMCPPayload(t *testing.T) {
	const log = "error: API worker restarted at 02:03; deployment recovered"
	const indented = "    [release]: https://release.example\n        next command"
	compactDiagnostic := mcp.NewToolResultError("lookup refused" + strings.Repeat("\n", 4300) + "Retry with current credentials")
	entityLog := "worker reported: " + strings.Repeat("x<&> ", 480)
	for _, tc := range []struct {
		name      string
		results   []*mcp.CallToolResult
		final     string
		visible   []string
		wantError bool
	}{
		{"successful log", []*mcp.CallToolResult{mcp.NewToolResultText(log)}, "", []string{log}, false},
		{"empty envelope", []*mcp.CallToolResult{{}}, "", []string{"(no output)"}, true},
		{"blank text", []*mcp.CallToolResult{mcp.NewToolResultText(" \t")}, "", []string{"empty response"}, true},
		{"literal placeholder payload", []*mcp.CallToolResult{mcp.NewToolResultText("(no output)")}, "", []string{"(no output)"}, false},
		{"MCP error", []*mcp.CallToolResult{mcp.NewToolResultError("lookup refused")}, "", []string{"lookup refused"}, true},
		{"discovery then MCP error", []*mcp.CallToolResult{mcp.NewToolResultError("lookup refused " + strings.Repeat("x", 4900))}, "", []string{"lookup refused"}, true},
		{"success in earlier round", []*mcp.CallToolResult{mcp.NewToolResultText(log), mcp.NewToolResultError("later lookup refused")}, "", []string{log, "later lookup refused"}, false},
		{"diagnostic exceeds delivery budget before success", []*mcp.CallToolResult{mcp.NewToolResultError("lookup refused " + strings.Repeat("x", 4900)), mcp.NewToolResultText(log)}, "", []string{log}, false},
		{"diagnostic exceeds delivery budget without success", []*mcp.CallToolResult{mcp.NewToolResultError("lookup refused " + strings.Repeat("x", 4900)), {}}, "", []string{"lookup refused"}, true},
		{"compact diagnostic with literal log", []*mcp.CallToolResult{compactDiagnostic, mcp.NewToolResultText(log)}, "", []string{"lookup refused", "Retry with current credentials", log}, false},
		{"compact diagnostic with preserved indent", []*mcp.CallToolResult{compactDiagnostic, mcp.NewToolResultText(indented)}, "", []string{"lookup refused", "Retry with current credentials", indented}, false},
		{"compact diagnostic with escaped entities", []*mcp.CallToolResult{compactDiagnostic, mcp.NewToolResultText(entityLog)}, "", []string{"lookup refused", "Retry with current credentials", entityLog}, false},
		{"complete literal lead space", []*mcp.CallToolResult{mcp.NewToolResultText("   **worker**   started  ")}, "", []string{"   **worker**   started  "}, false},
		{"final model answer", []*mcp.CallToolResult{{}}, "No incidents reported.", []string{"No incidents reported."}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			discover := tc.name == "discovery then MCP error"
			toolRounds := len(tc.results)
			if discover {
				toolRounds++
			}
			var calls atomic.Int32
			tool := einoTestMCPTool("lookup", "Fetch logs", "")
			tool.run = func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				i := int(calls.Add(1)) - 1
				if i >= len(tc.results) {
					t.Errorf("unexpected MCP call %d", i+1)
					return nil, errors.New("unexpected call")
				}
				return tc.results[i], nil
			}
			provider := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, round int, _ openai.ChatCompletionRequest) {
				if round < toolRounds {
					frame := einoReplyToolFrame("reused-id", "lookup", 0)
					if discover && round == 0 {
						frame = einoReplyToolFrame("discover", "get_tool_details", 0)
						frame = strings.Replace(frame, `"arguments":"{}"`, fmt.Sprintf(`"arguments":%q`, `{"groups":["global"]}`), 1)
					}
					einoModelSSE(w, frame)
					einoReplyFinish(w, "tool_calls")
				} else if tc.final != "" {
					engagementModelText(w, tc.final)
				} else {
					einoModelSSE(w, einoReplyUsageFrame, "[DONE]")
				}
			})
			b, replies := newRoleTestBot(t, provider)
			b.tools = NewToolRegistry()
			addEinoTestMCPServer(t, b.tools, "global", tool)
			if discover {
				b.cfg.ToolsSummaryEnabled = true
				b.toolSummaryStore = NewMCPSummaryStore(b.chatDB.db)
			}
			b.tasks = NewTaskStore(b.chatDB)
			task := ScheduledTask{ID: "payload", Prompt: "Report exact service status", CronExpr: "* * * * *", Timezone: "UTC", Enabled: true, CreatedBy: engagementOwner, NextRunAt: engagementNow}
			b.tasks.Upsert(engagementGroup, task)
			r := NewTaskRunner(b, b.tasks)
			r.runDue(engagementNow)
			sent := roleReceive(t, replies)
			auditWaitLegacy(t, r, fmt.Sprintf("%d:%s", engagementGroup, task.ID))
			row := NewTaskStore(b.chatDB).List(engagementGroup)[0]
			for _, content := range tc.visible {
				if !strings.Contains(html.UnescapeString(sent.text), content) {
					t.Errorf("Telegram reply of %d runes begins %q and is missing %q", len([]rune(sent.text)), truncate(sent.text, 180), content)
				}
			}
			if sent.path != "/botrole/editMessageText" || sent.chatID != engagementGroup ||
				calls.Load() != int32(len(tc.results)) || provider.calls.Load() != int32(toolRounds+1) || row.LastRunAt.IsZero() ||
				(row.LastError != "") != tc.wantError {
				t.Fatalf("Telegram=%s/%d MCP calls=%d model calls=%d result=%+v", sent.path, sent.chatID, calls.Load(), provider.calls.Load(), row)
			}
			if got := len(einoReplyDashboardEvents(b, DashboardEventUsageRecorded)); got != toolRounds+1 {
				t.Errorf("recorded usage for %d of %d model rounds", got, toolRounds+1)
			}
		})
	}
}

func TestTask6ReviewLegacyConfirmedMCPPayload(t *testing.T) {
	const payload = "worker restarted after release"
	for _, tc := range []struct {
		name, lastError string
		edits           int32
	}{
		{"diagnostic only", "no text or usable tool results", 1},
		{"HTML diagnostic with a plain proof prefix", "no text or usable tool results", 1},
		{"normalized line endings", "", 1},
		{"missing Message.text", "Telegram delivery is unconfirmed", 1},
		{"message is not modified", "", 1},
		{"plain text after HTML rejection", "", 2},
		{"unconfirmed edits", "Telegram delivery is unconfirmed", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			business := payload
			diagnostic := "unavailable: " + payload
			if tc.name == "HTML diagnostic with a plain proof prefix" {
				business = "error: " + payload
				diagnostic = payload + strings.Repeat("\n", 4300) + "Try refreshing credentials"
			}
			if tc.name == "normalized line endings" {
				business += "\r\n  health check passed\r\nhealthy"
			}
			var calls atomic.Int32
			tool := einoTestMCPTool("lookup", "Fetch logs", "")
			tool.run = func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				if calls.Add(1) == 1 {
					return mcp.NewToolResultError(diagnostic), nil
				}
				return mcp.NewToolResultText(business), nil
			}
			provider := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, round int, _ openai.ChatCompletionRequest) {
				if round < 2 {
					einoModelSSE(w, einoReplyToolFrame("reused-id", "lookup", 0))
					einoReplyFinish(w, "tool_calls")
					return
				}
				einoModelSSE(w, einoReplyUsageFrame, "[DONE]")
			})
			var edits atomic.Int32
			b, _ := newRoleTestBotWithTelegram(t, provider, func(w http.ResponseWriter, _ *http.Request, d roleTestDelivery) bool {
				if d.path != "/botrole/editMessageText" {
					return false
				}
				edit := edits.Add(1)
				if d.chatID != engagementGroup || strings.Count(d.text, payload) != 2 {
					t.Errorf("scheduled edit did not attempt to deliver the log to the group: %+v", d)
				}
				confirmed := testTelegramVisibleText(d.text, "HTML")
				switch tc.name {
				case "diagnostic only":
					// The failed step mentioned the payload, but Telegram only
					// confirms the diagnostic and not the later successful result.
					header, _, _ := strings.Cut(d.text, "\n\nerror:")
					confirmed = header + "\n\nerror: unavailable: " + payload
				case "HTML diagnostic with a plain proof prefix":
					var ok bool
					confirmed, _, ok = strings.Cut(confirmed, "\n\n———\n\n"+business)
					if !ok {
						t.Error("HTML edit did not contain the business block")
					}
				case "missing Message.text":
					fmt.Fprintf(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":%d}}}`, d.chatID)
					return true
				case "message is not modified":
					fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: message is not modified"}`)
					return true
				case "plain text after HTML rejection":
					if edit == 1 {
						fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`)
						return true
					}
				case "unconfirmed edits":
					http.Error(w, "lost Telegram confirmation", http.StatusBadGateway)
					return true
				}
				fmt.Fprintf(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":%d},"text":%q}}`, d.chatID, confirmed)
				return true
			})
			b.tools = NewToolRegistry()
			addEinoTestMCPServer(t, b.tools, "global", tool)
			b.tasks = NewTaskStore(b.chatDB)
			task := ScheduledTask{ID: "confirmed-payload", Prompt: "Report service status", CronExpr: "* * * * *", Timezone: "UTC", Enabled: true, CreatedBy: engagementOwner, NextRunAt: engagementNow}
			b.tasks.Upsert(engagementGroup, task)
			r := NewTaskRunner(b, b.tasks)
			r.runDue(engagementNow)
			auditWaitLegacy(t, r, fmt.Sprintf("%d:%s", engagementGroup, task.ID))
			row := NewTaskStore(b.chatDB).List(engagementGroup)[0]
			if edits.Load() != tc.edits || calls.Load() != 2 || provider.calls.Load() != 3 || row.LastRunAt.IsZero() ||
				(row.LastError != "") != (tc.lastError != "") || !strings.Contains(row.LastError, tc.lastError) {
				t.Fatalf("edits=%d MCP=%d model=%d persisted=%+v", edits.Load(), calls.Load(), provider.calls.Load(), row)
			}
		})
	}
}

func TestTask6ReviewLegacyVoiceFallback(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required for Telegram voice encoding")
	}
	const log = "Worker restarted after release."
	const empty = "no text or usable tool results"
	for _, tc := range []struct {
		name, payload, spoken, lastError    string
		voiceHTTP                           int
		failSpeech, omitVoice, textAndVoice bool
	}{
		{name: "voice only", payload: log, spoken: log},
		{name: "definite voice rejection uses text", payload: log, spoken: log, voiceHTTP: 403},
		{name: "voice 502 after upload", payload: log, spoken: log, voiceHTTP: 502, lastError: "Telegram delivery is unconfirmed"},
		{name: "payload cut by speech limit", payload: strings.Repeat("界", 1300), spoken: "。", lastError: empty},
		{name: "payload stripped by speech formatting", payload: "```\n" + log + "\n```", lastError: empty},
		{name: "voice response omits attachment", payload: log, spoken: log, omitVoice: true, lastError: "Telegram delivery is unconfirmed"},
		{name: "synthesis failure uses confirmed literal text", payload: "`[log]` " + log, spoken: "[log] " + log, failSpeech: true},
		{name: "text and voice", payload: log, spoken: log, textAndVoice: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wav := append([]byte("RIFF\x00\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x80\x3e\x00\x00\x00\x7d\x00\x00\x02\x00\x10\x00data\x00\x00\x00\x00"), make([]byte, 3200)...)
			binary.LittleEndian.PutUint32(wav[4:8], uint32(len(wav)-8))
			binary.LittleEndian.PutUint32(wav[40:44], uint32(len(wav)-44))
			var calls, voices, deletes atomic.Int32
			var spoken atomic.Value
			tts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Params struct{ Text string } `json:"req_params"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				ttsInput := req.Params.Text
				spoken.Store(ttsInput)
				if tc.spoken != "" && !strings.HasSuffix(ttsInput, tc.spoken) ||
					tc.lastError == empty && strings.Contains(ttsInput, log) {
					t.Errorf("unexpected TTS input: %q", ttsInput)
				}
				if tc.failSpeech {
					http.Error(w, "synthesis failed", http.StatusServiceUnavailable)
					return
				}
				fmt.Fprintf(w, "event: TTSResponse\ndata: {\"code\":0,\"data\":%q}\n\nevent: SessionFinish\ndata: {\"code\":0}\n\n", base64.StdEncoding.EncodeToString(wav))
			}))
			t.Cleanup(tts.Close)
			chat := tele.Chat{ID: engagementGroup, Type: tele.ChatSuperGroup}
			telegram := newEinoReplyTelegramForChat(t, chat, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/bottest-token/sendVoice":
					voices.Add(1)
					if err := r.ParseMultipartForm(1 << 20); err != nil || r.FormValue("chat_id") != chat.Recipient() || !strings.HasPrefix(r.FormValue("voice"), "OggS") {
						t.Errorf("unexpected voice upload: %v", err)
					}
					if tc.voiceHTTP != 0 {
						// Even a rejection-shaped body cannot prove no upload
						// when the proxy responded with a 5xx transport error.
						w.WriteHeader(tc.voiceHTTP)
						fmt.Fprint(w, `{"ok":false,"error_code":403,"description":"Forbidden: voice messages are forbidden"}`)
						return
					}
					msg := tele.Message{ID: 10, Chat: &chat}
					if !tc.omitVoice {
						msg.Voice = &tele.Voice{File: tele.File{FileID: "confirmed"}}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": msg})
				case "/bottest-token/deleteMessage":
					deletes.Add(1)
					fmt.Fprint(w, `{"ok":true,"result":true}`)
				default:
					t.Errorf("unexpected Telegram operation %s", r.URL.Path)
				}
			}))
			model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, round int, _ openai.ChatCompletionRequest) {
				if round == 0 {
					einoModelSSE(w, einoReplyToolFrame("lookup-1", "lookup", 0))
					einoReplyFinish(w, "tool_calls")
					return
				}
				einoModelSSE(w, einoReplyUsageFrame, "[DONE]")
			})
			b, _ := newRoleTestBot(t, model)
			b.tg = telegram.bot
			b.cfg.VolcengineTTSSendText = tc.textAndVoice
			b.speechModes = NewSpeechModeStore()
			b.speechModes.Set(chat.ID, true)
			b.tts = &VolcengineTTSClient{url: tts.URL, httpClient: tts.Client(), audioFormat: "wav"}
			b.tools = NewToolRegistry()
			tool := einoTestMCPTool("lookup", "Fetch log", "")
			tool.run = func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				calls.Add(1)
				return mcp.NewToolResultText(tc.payload), nil
			}
			addEinoTestMCPServer(t, b.tools, "global", tool)
			b.tasks = NewTaskStore(b.chatDB)
			task := ScheduledTask{ID: "voice", Prompt: "Report service status", CronExpr: "* * * * *", Timezone: "UTC", Enabled: true, CreatedBy: engagementOwner, NextRunAt: engagementNow}
			b.tasks.Upsert(chat.ID, task)
			runner := NewTaskRunner(b, b.tasks)
			runner.runDue(engagementNow)
			auditWaitLegacy(t, runner, fmt.Sprintf("%d:%s", chat.ID, task.ID))
			row := NewTaskStore(b.chatDB).List(chat.ID)[0]
			telegram.mu.Lock()
			edits := slices.Clone(telegram.edits)
			telegram.mu.Unlock()
			var visible string
			for _, edit := range edits {
				if edit.Text != "🔊 正在发送语音…" {
					visible = testTelegramVisibleText(edit.Text, edit.ParseMode)
				}
			}
			wantText := tc.failSpeech || tc.textAndVoice || tc.voiceHTTP == 403
			wantDelete := !wantText && !tc.omitVoice && tc.voiceHTTP == 0
			t.Logf("TTS=%t sends=%d deletes=%d edits=%d business text=%t LastError=%q MCP/model=%d/%d",
				spoken.Load() != nil, voices.Load(), deletes.Load(), len(edits), visible != "", row.LastError, calls.Load(), model.calls.Load())
			if spoken.Load() == nil || (voices.Load() == 1) != !tc.failSpeech || (deletes.Load() == 1) != wantDelete ||
				(visible != "") != wantText || wantText && !strings.Contains(visible, tc.payload) ||
				(row.LastError != "") != (tc.lastError != "") || !strings.Contains(row.LastError, tc.lastError) ||
				row.LastRunAt.IsZero() || calls.Load() != 1 || model.calls.Load() != 2 {
				t.Error("wrong persisted outcome or delivery for the selected speech mode")
			}
		})
	}
}

func auditWaitLegacy(t *testing.T, r *TaskRunner, key string) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for {
		if _, busy := r.running.Load(key); !busy {
			return
		}
		if time.Now().After(until) {
			t.Fatal("legacy runner did not settle")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTask6ReviewLegacyOccurrenceRestart(t *testing.T) {
	for _, cron := range []bool{false, true} {
		t.Run(fmt.Sprintf("cron=%t", cron), func(t *testing.T) {
			provider := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ openai.ChatCompletionRequest) {
				engagementModelText(w, "Deliver the due report.")
			})
			applied := make(chan roleTestDelivery, 4)
			reply := make(chan struct{})
			var edits atomic.Int64
			var once sync.Once
			release := func() { once.Do(func() { close(reply) }) }
			b, deliveries := newRoleTestBotWithTelegram(t, provider, func(w http.ResponseWriter, _ *http.Request, d roleTestDelivery) bool {
				if !strings.HasSuffix(d.path, "editMessageText") || d.text != "Deliver the due report." {
					return false
				}
				n := edits.Add(1)
				applied <- d
				if n == 1 {
					<-reply
				}
				fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d,"chat":{"id":%d,"type":"private"}}}`, n, d.chatID)
				return true
			})
			defer release()
			b.tasks, b.reminders = NewTaskStore(b.chatDB), NewReminderStore(b.chatDB)
			if cron {
				b.tasks.Upsert(engagementOwner, ScheduledTask{ID: "audit", Prompt: "Report", CronExpr: "* * * * *", Timezone: "UTC", Enabled: true, CreatedBy: engagementOwner, NextRunAt: engagementNow})
			} else {
				b.reminders.Insert(Reminder{ID: "audit", UserID: engagementOwner, ChatID: engagementOwner, Prompt: "Report", FireAt: engagementNow, Timezone: "UTC"})
			}
			run := func(r *TaskRunner) string {
				if cron {
					r.runDue(engagementNow)
					return fmt.Sprintf("%d:audit", engagementOwner)
				}
				r.runDueReminders(engagementNow)
				return fmt.Sprintf("rem:%d:audit", engagementOwner)
			}
			r := NewTaskRunner(b, b.tasks)
			key := run(r)
			_ = roleReceive(t, applied)
			if !cron {
				_, inMemory := b.reminders.Get(engagementOwner, "audit")
				onDisk := len(b.chatDB.LoadAllReminders()[engagementOwner])
				t.Logf("first Telegram result applied, before reply: reminder in memory=%t disk rows=%d", inMemory, onDisk)
			}
			if err := b.chatDB.Close(); err != nil {
				t.Fatal(err)
			}
			release()
			auditWaitLegacy(t, r, key)
			db, err := OpenChatDB(b.cfg.ChatDBPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			b.chatDB, b.store = db, NewHistoryStore(db)
			b.tasks, b.reminders = NewTaskStore(db), NewReminderStore(db)
			r = NewTaskRunner(b, b.tasks)
			key = run(r)
			auditWaitLegacy(t, r, key)
			t.Logf("applied Telegram results=%d, model_calls=%d", edits.Load(), provider.calls.Load())
			if edits.Load() != 1 || provider.calls.Load() != 1 {
				t.Error("same due occurrence published again after bbolt reopening")
			}
			if !cron {
				roleHandle(t, b, b.handleRemindersCommand, roleMessage(engagementOwner, engagementOwner, 938, "/reminders"))
				visible := roleReceive(t, deliveries)
				if !strings.Contains(visible.text, "delivery unconfirmed") || !strings.Contains(visible.text, "audit") {
					t.Errorf("owner reminder status after restart: %+v", visible)
				}
			}
		})
	}
}

func TestTask6ReviewUnknownAfterStop(t *testing.T) {
	h := newRunnerHarness(t, false)
	b, _ := h.open()
	ref := h.seed(b, engagementOwner, true, true, engagementOwner)
	h.connector.outcome.Store("lost")
	h.connector.entered, h.connector.reply = make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(h.connector.reply) }) }
	defer release()
	b.scheduler.tick(t.Context(), h.clock())
	roleReceive(t, h.connector.entered)
	ref = reviseEngagement(t, b.engagements, ref, EngagementDecision{
		Request: engagementRequest(engagementOwner, engagementOwner, 822, "Stop this task"),
		Verdict: EngagementAccept, Change: EngagementEnd, At: h.clock(),
	})
	release()
	waitRunner(t, b.scheduler)
	w := getRunnerWork(t, b, ref)[0]
	viewer := EngagementViewer{ChatID: engagementOwner, ActorID: engagementOwner, DataAccess: map[MemoryDataAccess]bool{
		w.Results[0].Step.Action.Access: true,
	}}
	p, err := b.engagements.ReadVisible(ref.ID, viewer)
	t.Logf("status=%s external applied=%d step=%+v unknown in view=%v sends=%d", p.Status, h.connector.applied.Load(), w.Results[0], p.OutcomeUncertain, len(h.telegram.sends))
	if err != nil || p.Status != EngagementEnded || len(h.telegram.sends) != 0 || h.connector.writes.Load() != 1 {
		t.Fatalf("incorrect stop: %v", err)
	}
	if !p.OutcomeUncertain || w.Results[0].State != EngagementUnknown || h.connector.applied.Load() != 1 {
		t.Fatal("user projection conceals the ambiguous in-flight write after stopping")
	}
	if _, err := b.engagements.ReadVisible(ref.ID, EngagementViewer{ChatID: engagementPeer, ActorID: engagementPeer}); !errors.Is(err, ErrEngagementDenied) {
		t.Fatalf("private uncertain receipt exposed to another user: %v", err)
	}
	withoutAccount, err := b.engagements.ReadVisible(ref.ID, EngagementViewer{ChatID: engagementOwner, ActorID: engagementOwner})
	if err != nil || withoutAccount.OutcomeUncertain {
		t.Fatalf("private action status was exposed without its account grant: %+v, %v", withoutAccount, err)
	}
	result := w.Results[0]
	h.advance(time.Minute)
	if err := b.engagements.ArchiveOutcome(result.AttemptRef, h.clock(), EngagementUnknown, "same outcome after restart"); err != nil {
		t.Fatal(err)
	}
	if got := getRunnerWork(t, b, ref)[0].Results[0]; got != result {
		t.Fatalf("repeated unknown changed original result: %+v", got)
	}
	receipt := "verified calendar event calendar-event-123"
	if err := b.engagements.ArchiveOutcome(result.AttemptRef, h.clock(), EngagementSucceeded, receipt); err != nil {
		t.Fatal(err)
	}
	if err := b.engagements.ArchiveOutcome(result.AttemptRef, h.clock(), EngagementUnknown, "older result arrived again"); !errors.Is(err, ErrEngagementConflict) {
		t.Fatalf("unknown result could downgrade verified delivery: %v", err)
	}
	h.tick(b)
	p, err = b.engagements.ReadVisible(ref.ID, viewer)
	after := getRunnerItem(t, b, ref)
	if err != nil || p.OutcomeUncertain || p.Status != EngagementEnded || after.Ref != ref || h.connector.writes.Load() != 1 ||
		len(h.telegram.sends) != 0 || getRunnerWork(t, b, ref)[0].Results[0].Receipt != receipt {
		t.Fatalf("reconciliation reactivated or obscured a stopped item: status=%s version=%v uncertain=%v err=%v", p.Status, after.Ref, p.OutcomeUncertain, err)
	}
}

func TestTask6ReviewUnknownDeliveryAudienceAfterReopen(t *testing.T) {
	s := newEngagementScript(t)
	const otherGroup int64 = -702
	create := engagementFixture(engagementGroup)
	action := addEngagementAction(&create)
	create.ManageChats = []int64{engagementOwner, otherGroup}
	agreement := create.Decision.Agreement
	agreement.Recipients = []int64{engagementGroup, engagementOwner}
	create.Decision.Authorized.Recipients = agreement.Recipients
	create.Decision.Request.Text = "Notify this chat and DM me when the stable release arrives."
	ref := createEngagement(t, s.b.engagements, create)
	claims, err := s.b.engagements.ClaimDue(engagementNow, time.Minute, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim: %v, %v", claims, err)
	}
	work, _, err := s.b.engagements.Observe(claims[0], engagementNow, EngagementObservation{
		ChangeKey: "legacy-source-event", Evidence: EngagementText{ChatID: engagementGroup, Text: "private fixture observation"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []EngagementStep{{Recipient: engagementOwner}, {Action: action}} {
		result, started, err := s.b.engagements.BeginStep(claims[0], engagementNow, work.ChangeKey, step)
		if err != nil || !started {
			t.Fatalf("reserve: %v, %v", started, err)
		}
		if err := s.b.engagements.ArchiveOutcome(result.AttemptRef, engagementNow, EngagementUnknown, "private receipt contents"); err != nil {
			t.Fatal(err)
		}
	}
	ref = reviseEngagement(t, s.b.engagements, ref, EngagementDecision{
		Request: engagementRequest(engagementGroup, engagementOwner, 1220, "Stop this task."),
		Verdict: EngagementAccept, Change: EngagementEnd, At: engagementNow,
	})
	// Recreate pre-scoped work keys and a row without a summary count.
	legacyKey := legacyEngagementChange("legacy-source-event")
	if err := s.b.engagements.transact(true, func(tx *bolt.Tx) error {
		row, err := loadEngagement(tx, ref.ID)
		if err != nil {
			return err
		}
		bucket := tx.Bucket(engagementWorkBucket)
		scoped := engagementWorkKey(ref.ID, work.ChangeKey)
		if err := bucket.Put(engagementWorkKey(ref.ID, legacyKey), slices.Clone(bucket.Get(scoped))); err != nil {
			return err
		}
		if err := bucket.Delete(scoped); err != nil {
			return err
		}
		row.ScopedWorkKeys, row.UncertainSteps, row.OnceChangeKey = false, 0, "legacy-source-event"
		return putEngagementJSON(tx.Bucket(engagementBucket), []byte(ref.ID), row)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.b.chatDB.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := OpenChatDB(s.b.cfg.ChatDBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := NewEngagementStore(db)
	s.b.chatDB, s.b.store, s.b.engagements = db, NewHistoryStore(db), store
	s.b.memory, s.b.summaries = NewMemoryBook(db), NewSummaryStore(db)
	check := func(chat, actor int64, message int, list, wantUncertain bool) {
		t.Helper()
		out := make(chan string, 1)
		args := map[string]string{}
		if !list {
			args["id"] = ref.ID
		}
		s.tool("engagement_context", args, nil)
		s.final("Here is the status.", func(req openai.ChatCompletionRequest) { out <- engagementLastResult(req) })
		s.send(chat, actor, message, "@testbot Show tracking status.")
		raw := roleReceive(t, out)
		if strings.Contains(raw, `"OutcomeUncertain":true`) != wantUncertain ||
			strings.Contains(raw, "private receipt contents") || strings.Contains(raw, "private fixture observation") ||
			strings.Contains(raw, "UncertainSteps") || strings.Contains(raw, action.Access.Key) {
			t.Errorf("chat=%d actor=%d unexpected context: %s", chat, actor, raw)
		}
	}
	check(engagementOwner, engagementOwner, 1221, false, true)
	check(engagementOwner, engagementOwner, 1222, true, true)
	check(engagementGroup, engagementOwner, 1223, false, false)
	check(engagementGroup, engagementPeer, 1224, true, false)
	check(otherGroup, engagementPeer, 1225, false, false)
	check(engagementPeer, engagementPeer, 1226, false, false)
	archived := getRunnerWork(t, s.b, ref)
	if len(archived) != 1 || archived[0].ChangeKey != legacyKey || len(archived[0].Results) != 2 {
		t.Fatalf("old work not loaded: %+v", archived)
	}
	if err := store.ArchiveOutcome(archived[0].Results[0].AttemptRef, engagementNow.Add(time.Minute), EngagementSucceeded, "confirmed Telegram receipt"); err != nil {
		t.Fatal(err)
	}
	check(engagementOwner, engagementOwner, 1227, false, false)
	withAccount, err := store.ReadVisible(ref.ID, EngagementViewer{ChatID: engagementOwner, ActorID: engagementOwner, DataAccess: map[MemoryDataAccess]bool{action.Access: true}})
	if err != nil || !withAccount.OutcomeUncertain || withAccount.Ref != ref || withAccount.EndReason != EngagementStopped {
		t.Fatalf("delivery receipt changed the separate private action or stopped item: %+v, %v", withAccount, err)
	}
}

func TestTask6ReviewUnknownDeliveryAfterObserverChange(t *testing.T) {
	const otherGroup int64 = -702
	h := newRunnerHarness(t, false)
	entered, release := make(chan int64, 1), make(chan struct{})
	var unblock sync.Once
	done := func() { unblock.Do(func() { close(release) }) }
	original := h.telegram.server.Config.Handler
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/botrole/sendMessage" {
			original.ServeHTTP(w, req)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Errorf("Telegram send: %v", err)
		}
		var chat int64
		fmt.Sscan(body["chat_id"], &chat)
		entered <- chat
		select {
		case <-release:
		case <-req.Context().Done():
		}
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"ok":false,"error_code":502,"description":"upstream timed out"}`)
	}))
	t.Cleanup(stub.Close)
	defer done()
	h.telegram.server = stub
	b, _ := h.open()
	create := engagementFixture(engagementGroup)
	create.ManageChats = []int64{engagementOwner, otherGroup}
	create.Decision.Agreement.Observe = b.snapshot().engagementCapabilities(engagementOwner, engagementGroup)["public:upstream:event_list"]
	create.Decision.Authorized.Capabilities = []EngagementCapability{create.Decision.Agreement.Observe}
	ref := createEngagement(t, b.engagements, create)
	run := engagementRun{bot: b, store: b.engagements, claim: engagementClaim(t, b.engagements, h.clock())}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		run.execute(t.Context())
	}()
	if chat := roleReceive(t, entered); chat != engagementGroup {
		t.Fatalf("original Telegram destination = %d", chat)
	}
	revision := create.Decision
	revision.Change = EngagementReaccept
	revision.Request = engagementRequest(engagementGroup, engagementOwner, 1240, "Expand tracking to the replacement release feed.")
	revision.Request.ExplicitReaccept = true
	next := *revision.Agreement
	next.Observe.Target = "upstream/new-feed"
	revision.Agreement = &next
	revision.Authorized.Capabilities = []EngagementCapability{next.Observe}
	ref = reviseEngagement(t, b.engagements, ref, revision)
	done()
	roleReceive(t, finished)

	work := getRunnerWork(t, b, ref)[0]
	if len(work.Results) != 1 || work.Results[0].State != EngagementUnknown || work.Results[0].Step.Recipient != engagementGroup ||
		work.Observe == next.Observe {
		t.Fatalf("late Telegram result was not archived against the original work: %+v", work)
	}
	s := newEngagementScript(t)
	s.b.engagements = b.engagements
	check := func(chat int64, message int, list, want bool) {
		t.Helper()
		args := map[string]string{}
		if !list {
			args["id"] = ref.ID
		}
		out := make(chan string, 1)
		s.tool("engagement_context", args, nil)
		s.final("Here is the status.", func(req openai.ChatCompletionRequest) { out <- engagementLastResult(req) })
		s.send(chat, engagementOwner, message, "@testbot Show tracking status.")
		raw := roleReceive(t, out)
		if !strings.Contains(raw, ref.ID) || strings.Contains(raw, `"OutcomeUncertain":true`) != want ||
			strings.Contains(raw, "UncertainSteps") || strings.Contains(raw, work.Results[0].Receipt) {
			t.Errorf("chat=%d list=%t want uncertainty=%t, got %s", chat, list, want, raw)
		}
	}
	check(engagementGroup, 1241, false, true)
	check(engagementGroup, 1242, true, true)
	check(otherGroup, 1243, false, false)
	check(engagementOwner, 1244, false, false)

	current := engagementClaim(t, b.engagements, revision.At)
	if pending, err := b.engagements.ClaimWork(current, revision.At, "", 8); err != nil || len(pending) != 0 {
		t.Fatalf("new claim listed old work for execution: %v, %v", pending, err)
	}
	if _, started, err := b.engagements.BeginStep(current, revision.At, work.ChangeKey, work.Results[0].Step); started || !errors.Is(err, ErrEngagementDenied) {
		t.Fatalf("new claim could start old work: started=%v error=%v", started, err)
	}
	revision.At = revision.At.Add(time.Second)
	revision.Request = engagementRequest(engagementGroup, engagementOwner, 1246, "Reaccept this tracking and DM me instead.")
	revision.Request.ExplicitReaccept = true
	next.Recipients = []int64{engagementOwner}
	revision.Authorized.Recipients = next.Recipients
	ref = reviseEngagement(t, b.engagements, ref, revision)
	check(engagementGroup, 1247, false, false)
	check(engagementOwner, 1248, false, false)
	if err := b.engagements.ArchiveOutcome(work.Results[0].AttemptRef, revision.At.Add(time.Second), EngagementSucceeded, "verified original delivery"); err != nil {
		t.Fatal(err)
	}
	if item := getRunnerItem(t, b, ref); item.Ref != ref || item.Status != EngagementActive ||
		h.connector.reads.Load() != 1 || len(entered) != 0 {
		t.Fatalf("queries or settlement changed the agreement or sent again: %+v", item)
	}
}

func TestTask6ReviewSettledWorkClosesWithNoQuota(t *testing.T) {
	for _, tc := range []struct {
		name        string
		once, write bool
	}{
		{"once_with_calendar", true, true},
		{"recurring", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRunnerHarness(t, false)
			b, _ := h.open()
			h.cfg.UserDailyTokenLimit = 20
			if err := b.applyRuntimeConfig(h.cfg); err != nil {
				t.Fatal(err)
			}
			ref := h.seed(b, engagementOwner, tc.write, tc.once, engagementOwner)
			claims, err := b.engagements.ClaimDue(h.clock(), engagementLease, 1)
			if err != nil || len(claims) != 1 {
				t.Fatalf("claims=%v err=%v", claims, err)
			}
			// Run the real check and its external calls, then interrupt before FinishCheck.
			item := getRunnerItem(t, b, ref)
			run := engagementRun{bot: b, store: b.engagements, claim: claims[0], item: item, snap: b.snapshot()}
			check, err := run.check(t.Context())
			works := getRunnerWork(t, b, ref)
			if err != nil || len(works) != 1 || check.CompletedChange != works[0].ChangeKey ||
				!works[0].completed(*item.Current) {
				t.Fatalf("initial production receipts missing: %+v, %v, %+v", check, err, works)
			}
			_ = roleReceive(t, h.telegram.sends)
			b = h.restart(b)
			day := h.clock().Format("20060102")
			usage := func() int64 {
				return b.stats.GetQuotaBucket(UsageScopeUser, 0, engagementOwner, day).QuotaTokens
			}
			b.stats.ApplyQuotaDelta(UsageEvent{ChatID: engagementOwner, UserID: engagementOwner, TotalTokens: 20}, day, true)
			tokens := usage()
			h.advance(engagementLease)
			h.tick(b)
			after := getRunnerItem(t, b, ref)
			if tc.once {
				if after.EndReason != EngagementCompleted || after.CompletedChangeKey != check.CompletedChange {
					t.Errorf("persisted one-shot could not settle without fresh quota: %s / %s", after.Status, after.EndReason)
				}
			} else if after.Status != EngagementActive || !after.NextCheck.Equal(h.clock().Add(engagementPoll)) {
				t.Errorf("recurrence was not advanced: %s / %s", after.Status, after.NextCheck)
			}
			if h.connector.reads.Load() != 1 || (h.connector.applied.Load() == 1) != tc.write ||
				h.model.calls.Load() != 1 || usage() != tokens || len(h.telegram.sends) != 0 {
				t.Errorf("local settlement called or charged external work: reads=%d writes=%d model=%d tokens=%d before=%d sends=%d",
					h.connector.reads.Load(), h.connector.writes.Load(), h.model.calls.Load(), usage(), tokens, len(h.telegram.sends))
			}
			if !tc.once {
				h.cfg.UserDailyTokenLimit = 0
				if err := b.applyRuntimeConfig(h.cfg); err != nil {
					t.Fatal(err)
				}
				h.advance(engagementPoll)
				h.tick(b)
				works = getRunnerWork(t, b, ref)
				if h.connector.reads.Load() != 2 || len(works) != 1 || len(h.telegram.sends) != 0 {
					t.Errorf("already delivered event repeated after quota restored: reads=%d work=%d sends=%d", h.connector.reads.Load(), len(works), len(h.telegram.sends))
				}
			}
		})
	}
}

func TestTask6ReviewReminderMigration(t *testing.T) {
	h := newRunnerHarness(t, false)
	b, _ := h.open()
	future := Reminder{ID: "audit", ChatID: engagementOwner, UserID: engagementOwner, Prompt: "Future", FireAt: h.clock().Add(time.Hour), Timezone: "UTC"}
	b.reminders.Insert(future)
	h.cfg.ChatDBPath = filepath.Join(t.TempDir(), "replacement.db")
	if err := b.applyRuntimeConfig(h.cfg); err != nil {
		t.Fatal(err)
	}
	_, inMemory := b.reminders.Get(engagementOwner, future.ID)
	b = h.restart(b)
	_, onRestart := b.reminders.Get(engagementOwner, future.ID)
	t.Logf("in live memory after switch=%t after reopen=%t", inMemory, onRestart)
	if !onRestart {
		t.Error("future reminder absent after hot switch and restart")
	}
}

func TestTask6ReviewReminderMigrationInFlight(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprintf("conflict=%t", conflict), func(t *testing.T) {
			model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ openai.ChatCompletionRequest) {
				engagementModelText(w, "Your due report.")
			})
			applied := make(chan struct{})
			reply := make(chan struct{})
			var edits atomic.Int64
			var once sync.Once
			release := func() { once.Do(func() { close(reply) }) }
			b, _ := newRoleTestBotWithTelegram(t, model, func(w http.ResponseWriter, _ *http.Request, d roleTestDelivery) bool {
				if !strings.HasSuffix(d.path, "editMessageText") || d.text != "Your due report." {
					return false
				}
				if edits.Add(1) == 1 {
					close(applied)
					<-reply
				}
				fmt.Fprintf(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":%d,"type":"private"}}}`, d.chatID)
				return true
			})
			defer release()
			b.reminders = NewReminderStore(b.chatDB)
			original := b.cfg.ChatDBPath
			next := b.cfg
			next.ChatDBPath = filepath.Join(t.TempDir(), "replacement.db")
			rem := Reminder{ID: "audit", UserID: engagementOwner, ChatID: engagementOwner, Prompt: "Report", FireAt: engagementNow, Timezone: "UTC"}
			if err := b.reminders.Insert(rem); err != nil {
				t.Fatal(err)
			}
			if conflict {
				destination, err := OpenChatDB(next.ChatDBPath)
				if err != nil {
					t.Fatal(err)
				}
				unrelated := rem
				unrelated.Prompt = "An independent reminder"
				if err := NewReminderStore(destination).Insert(unrelated); err != nil {
					t.Fatal(err)
				}
				if err := destination.Close(); err != nil {
					t.Fatal(err)
				}
			}

			r := NewTaskRunner(b, nil)
			r.runDueReminders(engagementNow)
			roleReceive(t, applied)
			if err := b.applyRuntimeConfig(next); (err != nil) != conflict {
				t.Fatalf("apply replacement config: %v", err)
			}
			expectFile := next.ChatDBPath
			if conflict {
				expectFile = original
			}
			snap := b.snapshot()
			if snap.cfg.ChatDBPath != expectFile || len(snap.chatDB.LoadAllReminders()[engagementOwner]) != 1 {
				t.Fatalf("in-flight row did not follow live config: %s", snap.cfg.ChatDBPath)
			}
			if !conflict {
				if !sameReminder(snap.chatDB.LoadAllReminders()[engagementOwner][0], mustReminder(t, b.reminders, rem)) {
					t.Fatal("copy lost current attempt")
				}
			}
			// A separate runner using the same store must not reissue an in-flight reminder.
			other := NewTaskRunner(b, nil)
			key := fmt.Sprintf("rem:%d:%s", engagementOwner, rem.ID)
			other.runDueReminders(engagementNow)
			auditWaitLegacy(t, other, key)
			future := rem
			future.ID, future.FireAt = "new", engagementNow.Add(time.Hour)
			if err := b.reminders.Insert(future); err != nil {
				t.Fatal(err)
			}
			release()
			auditWaitLegacy(t, r, key)
			if model.calls.Load() != 1 || edits.Load() != 1 {
				t.Fatalf("concurrent runner duplicated the reminder: %d model calls, %d edits", model.calls.Load(), edits.Load())
			}
			if err := b.closeChatDB(); err != nil {
				t.Fatal(err)
			}
			assertDatabase := func(path string, want, absent string) {
				t.Helper()
				db, err := OpenChatDB(path)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				items := db.LoadAllReminders()[engagementOwner]
				if len(items) != 1 || items[0].ID != want || strings.Contains(items[0].Prompt, absent) {
					t.Fatalf("wrong reminder rows in %s: %+v", path, items)
				}
			}
			assertDatabase(expectFile, "new", "independent")
			if conflict {
				assertDatabase(next.ChatDBPath, "audit", "Report")
			} else {
				assertDatabase(original, "audit", "independent")
			}
		})
	}
}

func mustReminder(t *testing.T, store *ReminderStore, expected Reminder) Reminder {
	t.Helper()
	rem, ok := store.Get(expected.UserID, expected.ID)
	if !ok {
		t.Fatalf("missing reminder %s", expected.ID)
	}
	return rem
}

func TestTask6ReviewReminderReservationFailure(t *testing.T) {
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ openai.ChatCompletionRequest) {
		engagementModelText(w, "Due.")
	})
	b, deliveries := newRoleTestBot(t, model)
	b.reminders = NewReminderStore(b.chatDB)
	rem := Reminder{ID: "audit", UserID: engagementOwner, ChatID: engagementOwner, Prompt: "Report", FireAt: engagementNow, Timezone: "UTC"}
	if err := b.reminders.Insert(rem); err != nil {
		t.Fatal(err)
	}
	if err := b.closeChatDB(); err != nil {
		t.Fatal(err)
	}
	r := NewTaskRunner(b, nil)
	r.runDueReminders(engagementNow)
	auditWaitLegacy(t, r, fmt.Sprintf("rem:%d:%s", engagementOwner, rem.ID))
	if model.calls.Load() != 0 || len(deliveries) != 0 || !sameReminder(mustReminder(t, b.reminders, rem), rem) {
		t.Fatal("missing reservation allowed a model call, Telegram delivery or reminder mutation")
	}
}

func TestTask6ReviewRecurringReminderResult(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusTooManyRequests, http.StatusBadGateway} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ openai.ChatCompletionRequest) {
				engagementModelText(w, "Reminder on schedule.")
			})
			b, deliveries := newRoleTestBotWithTelegram(t, model, func(w http.ResponseWriter, _ *http.Request, d roleTestDelivery) bool {
				if d.path != "/botrole/editMessageText" || status == http.StatusOK {
					return false
				}
				w.WriteHeader(status)
				fmt.Fprintf(w, `{"ok":false,"error_code":%d,"description":"local refusal"}`, status)
				return true
			})
			b.reminders = NewReminderStore(b.chatDB)
			b.reminders.now = func() time.Time { return engagementNow }
			rem := Reminder{ID: "audit", UserID: engagementOwner, ChatID: engagementOwner, Prompt: "Report", FireAt: engagementNow, CronExpr: "* * * * *", Timezone: "UTC"}
			if err := b.reminders.Insert(rem); err != nil {
				t.Fatal(err)
			}
			r := NewTaskRunner(b, nil)
			r.runDueReminders(engagementNow)
			auditWaitLegacy(t, r, fmt.Sprintf("rem:%d:%s", rem.UserID, rem.ID))
			wantTime := rem.FireAt
			if status != http.StatusTooManyRequests {
				wantTime = wantTime.Add(time.Minute)
			}
			persisted := b.chatDB.LoadAllReminders()[engagementOwner]
			if len(persisted) != 1 || !persisted[0].FireAt.Equal(wantTime) {
				t.Fatalf("incorrect recurrence after Telegram status %d: %+v", status, persisted)
			}
			failures := einoReplyDashboardEvents(b, DashboardEventScheduleFailed)
			switch status {
			case http.StatusOK:
				if got := roleReceive(t, deliveries); got.text != "Reminder on schedule." || len(failures) != 0 {
					t.Fatalf("confirmed result: %+v, failures=%+v", got, failures)
				}
			default:
				if len(failures) != 1 || strings.Contains(failures[0].Detail, "unconfirmed") != (status == http.StatusBadGateway) {
					t.Fatalf("ambiguous and definite failure not distinguished: %+v", failures)
				}
			}
		})
	}
}
