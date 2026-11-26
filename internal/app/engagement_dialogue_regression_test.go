package app

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	openai "github.com/sashabaranov/go-openai"
)

func regressionWatchArgs() map[string]any {
	return map[string]any{"kind": "delegation", "verdict": "accept", "change": "agree", "observe": "public:upstream:release", "trigger": "stable release", "recipient": "here"}
}

func sendRegressionAgreement(s *engagementScript, args map[string]any, chat, actor int64, id int, text string) string {
	s.tool("engagement_decide", args, nil)
	s.final("Saved.", nil)
	return s.send(chat, actor, id, text)
}

func TestEngagementDialogueRegressionCalendarExplicitTarget(t *testing.T) {
	s := newEngagementScript(t)
	enableTestEngagementMCP(t, s.b, newEngagementMCP(t, "release", true))
	users, err := NewUserMCPStore(filepath.Join(t.TempDir(), "personal.db"))
	if err != nil {
		t.Fatal(err)
	}
	manager := NewUserToolManager(users, nil, nil)
	t.Cleanup(func() { manager.Close(); _ = users.Close() })
	s.b.userTools = manager
	url := newEngagementMCP(t, "insert_event", false)
	enrollment := fmt.Sprintf(`{"mcpServers":{"calendar":{"url":%q,"background":{"insert_event":{"operation":"calendar.create","target":"work","arguments":{"calendarId":"work"}}}}}}`, url)
	s.sendImport(engagementOwner, engagementOwner, 510, enrollment)
	args := regressionWatchArgs()
	args["actions"], args["authorization_quote"] = []string{"personal:calendar:insert_event"}, "add a review to my family calendar"
	args["trigger"] = "When the stable release arrives"
	request := "When the stable release arrives, add a review to my family calendar."
	s.intents.Store(request, map[string]any{"calendar": map[string]any{"target": "family", "quote": "add a review to my family calendar", "trigger": "When the stable release arrives", "once": true}})
	got := sendRegressionAgreement(s, args, engagementOwner, engagementOwner, 511, request)
	items, err := s.b.engagements.Visible(EngagementViewer{ChatID: engagementOwner, ActorID: engagementOwner}, "", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) > 0 && items[0].Status == EngagementActive {
		t.Errorf("wrong calendar accepted: %s", got)
	}

	args = maps.Clone(args)
	args["trigger"] = "稳定版一发布"
	args["authorization_quote"] = "不用把复查加到我的日历"
	request = "稳定版一发布，不用把复查加到我的日历，只告诉我。"
	// Even an affirmative assessment cannot override a negated quotation.
	s.intents.Store(request, map[string]any{"calendar": map[string]any{
		"quote": args["authorization_quote"], "trigger": args["trigger"], "once": true,
	}})
	got = sendRegressionAgreement(s, args, engagementOwner, engagementOwner, 512, request)
	if strings.Contains(got, "calendar.create") {
		t.Errorf("negated calendar write accepted: %s", got)
	}
	request = "When the stable release arrives and the owner has approved it, add a review to my work calendar."
	args["trigger"], args["authorization_quote"] = "When the stable release arrives", "add a review to my work calendar"
	s.intents.Store(request, map[string]any{"calendar": map[string]any{
		"target": "work", "quote": args["authorization_quote"], "trigger": "When the stable release arrives and the owner has approved it", "once": true,
	}})
	got = sendRegressionAgreement(s, args, engagementOwner, engagementOwner, 513, request)
	if strings.Contains(got, "calendar.create") {
		t.Errorf("shortened write trigger accepted: %s", got)
	}
	items, err = s.b.engagements.Visible(EngagementViewer{ChatID: engagementOwner, ActorID: engagementOwner}, "", 8)
	if err != nil || len(items) != 0 {
		t.Fatalf("invalid calendar decisions persisted: %+v %v", items, err)
	}
}

func TestEngagementDialogueRegressionPrivateManagementShareAppliesConclusion(t *testing.T) {
	s := newEngagementScript(t)
	enableTestEngagementMCP(t, s.b, newEngagementMCP(t, "release", true))
	sendRegressionAgreement(s, regressionWatchArgs(), engagementGroup, engagementOwner, 520, "@testbot Watch for the stable release for the group.")
	viewer := EngagementViewer{ChatID: engagementGroup, ActorID: engagementPeer}
	items, err := s.b.engagements.Visible(viewer, "", 8)
	if err != nil || len(items) != 1 {
		t.Fatalf("lookup: %+v %v", items, err)
	}
	ref := items[0].Ref
	share := fmt.Sprintf("把这项任务的管理结论分享给群 %d：只有发起人可以修改", engagementGroup)
	s.change(ref, map[string]any{"verdict": "accept", "change": "manage", "management_quote": share})
	privateReceipt := s.send(engagementOwner, engagementOwner, 521, share)
	p, err := s.b.engagements.ReadVisible(ref.ID, viewer)
	if err != nil || p.Management == nil {
		t.Fatalf("lookup after share: %+v %v", p, err)
	}
	s.tool("engagement_context", map[string]string{"id": ref.ID}, nil)
	s.tool("engagement_decide", map[string]any{"id": ref.ID, "version": p.Ref.Version, "verdict": "reject", "reply": "Ask the initiator to change it."}, func(req openai.ChatCompletionRequest) {
		view := engagementLastResult(req)
		if !strings.Contains(view, `"InitiatorOnly":true`) || !strings.Contains(view, "只有发起人可以修改") ||
			strings.Contains(view, "分享给群") || strings.Contains(view, `"MessageID":521`) {
			t.Errorf("incorrect shared projection: %s", view)
		}
	})
	s.final("Stopped.", nil)
	got := s.send(engagementGroup, engagementPeer, 522, "@testbot End this task.")
	item, err := s.b.engagements.Get(ref.ID)
	if err != nil || item.Status == EngagementEnded || !p.InitiatorOnly || !strings.Contains(got, "Ask the initiator") {
		t.Errorf("private receipt=%s; summary=%q; only=%v; peer receipt=%s; status=%s err=%v", privateReceipt, p.Management.Summary, p.InitiatorOnly, got, item.Status, err)
	}
}

func TestEngagementDialogueRegressionPrivateManagementShareRequiresConsent(t *testing.T) {
	s := newEngagementScript(t)
	enableTestEngagementMCP(t, s.b, newEngagementMCP(t, "release", true))
	sendRegressionAgreement(s, regressionWatchArgs(), engagementGroup, engagementOwner, 530, "@testbot Watch for the stable release for the group.")
	viewer := EngagementViewer{ChatID: engagementGroup, ActorID: engagementPeer}
	items, err := s.b.engagements.Visible(viewer, "", 8)
	if err != nil || len(items) != 1 {
		t.Fatalf("lookup: %+v %v", items, err)
	}
	ref := items[0].Ref
	private := "Only I can edit this task."
	s.change(ref, map[string]any{"verdict": "accept", "change": "manage", "management_quote": private})
	s.send(engagementOwner, engagementOwner, 531, private)
	p, err := s.b.engagements.ReadVisible(ref.ID, viewer)
	if err != nil || p.Management != nil {
		t.Fatalf("unexpected management term: %+v; %v", p.Management, err)
	}
	s.tool("engagement_context", map[string]string{"id": ref.ID}, nil)
	s.tool("engagement_decide", map[string]any{"id": ref.ID, "version": p.Ref.Version, "verdict": "accept", "change": "end"}, func(req openai.ChatCompletionRequest) {
		view := engagementLastResult(req)
		if strings.Contains(view, private) || strings.Contains(view, `"InitiatorOnly":true`) || strings.Contains(view, `"MessageID":531`) {
			t.Errorf("private management leaked in ordinary group context: %s", view)
		}
	})
	s.final("Stopped.", nil)
	reply := s.send(engagementGroup, engagementPeer, 532, "@testbot End this task.")
	if strings.Contains(reply, private) || strings.Contains(reply, "Stopped.") || !strings.Contains(reply, "No agreement change") {
		t.Errorf("unexpected private management receipt: %s", reply)
	}
}

func TestEngagementDialogueRegressionLiveMCPToolRemoval(t *testing.T) {
	s := newEngagementScript(t)
	stub := server.NewMCPServer("release", "test")
	tool := einoTestMCPTool("release", "available", "")
	readOnly := true
	tool.info.Annotations.ReadOnlyHint = &readOnly
	mcp.WithString("repository", mcp.Required())(&tool.info)
	stub.AddTool(tool.info, tool.run)
	protocol := server.NewTestStreamableHTTPServer(stub, server.WithStateLess(true))
	t.Cleanup(protocol.Close)
	enableTestEngagementMCP(t, s.b, protocol.URL)
	stub.DeleteTools("release")
	got := sendRegressionAgreement(s, regressionWatchArgs(), engagementOwner, engagementOwner, 540, "Tell me when the stable release arrives.")
	if strings.Contains(got, "Agreement saved") {
		t.Errorf("removed tool accepted although MCP still responds to ping: %s", got)
	}
}

func TestEngagementDialogueRegressionPreferenceNaturalGroupScope(t *testing.T) {
	s := newEngagementScript(t)
	s.b.cfg.AdminWildcard = true
	s.tool("conversation_preference", map[string]any{"scope": "group", "verdict": "accept", "chatter": "off", "duration": "this_week"}, nil)
	s.final("Saved.", nil)
	request := "群里这周别主动开话题，跟踪通知照常。"
	s.intents.Store(request, map[string]string{"preference": "group"})
	got := s.send(engagementGroup, engagementPeer, 550, "@testbot "+request)
	p, err := s.b.engagements.ConversationPreferenceAt(engagementGroup, 0, engagementNow)
	if err != nil || p.Chatter != ConversationOff {
		t.Errorf("explicit group request denied: %s; %+v; %v", got, p, err)
	}
}

func TestEngagementDialogueRegressionTwoDecisionsOneMessage(t *testing.T) {
	s := newEngagementScript(t)
	s.b.cfg.AdminWildcard = true
	ref := createEngagement(t, s.b.engagements, engagementFixture(engagementGroup))
	s.tool("engagement_context", map[string]any{"id": ref.ID}, nil)
	s.tool("conversation_preference", map[string]any{"scope": "group", "verdict": "accept", "chatter": "off", "duration": "this_week"}, nil)
	s.tool("engagement_decide", map[string]any{"id": ref.ID, "version": ref.Version, "verdict": "accept", "change": "mute", "duration": "this_week"}, nil)
	s.final("Both saved.", nil)
	request := "本群这周别主动闲聊，这项跟踪这周也先静音。"
	s.intents.Store(request, map[string]any{"preference": "group", "task": true})
	got := s.send(engagementGroup, engagementOwner, 560, "@testbot "+request)
	item, err := s.b.engagements.Get(ref.ID)
	if err != nil || !item.NotificationsMutedAt(engagementNow) || !strings.Contains(got, "全群") || !strings.Contains(got, "静音") {
		t.Errorf("second decision not recorded: %s; muted=%v; %v", got, item.Muted, err)
	}
}

func TestEngagementDialogueRegressionReacceptDoesNotMultiplyCalendarWrites(t *testing.T) {
	s := newEngagementScript(t)
	enableTestEngagementMCP(t, s.b, newEngagementMCP(t, "release", true))
	users, err := NewUserMCPStore(filepath.Join(t.TempDir(), "personal.db"))
	if err != nil {
		t.Fatal(err)
	}
	manager := NewUserToolManager(users, nil, nil)
	t.Cleanup(func() { manager.Close(); _ = users.Close() })
	s.b.userTools = manager
	url := newEngagementMCP(t, "insert_event", false)
	enrollment := fmt.Sprintf(`{"mcpServers":{"calendar":{"url":%q,"background":{"insert_event":{"operation":"calendar.create","target":"work","arguments":{"calendarId":"work"}}}}}}`, url)
	s.sendImport(engagementOwner, engagementOwner, 570, enrollment)
	args := regressionWatchArgs()
	args["actions"], args["authorization_quote"] = []string{"personal:calendar:insert_event"}, "add a review to my work calendar"
	args["trigger"] = "When the stable release arrives"
	request := "When the stable release arrives, add a review to my work calendar."
	s.intents.Store(request, map[string]any{"calendar": map[string]any{"target": "work", "quote": "add a review to my work calendar", "trigger": "When the stable release arrives", "once": true}})
	got := sendRegressionAgreement(s, args, engagementOwner, engagementOwner, 571, request)
	if !strings.Contains(got, "calendar.create") {
		t.Fatalf("initial acceptance: %s", got)
	}
	viewer := EngagementViewer{ChatID: engagementOwner, ActorID: engagementOwner}
	items, err := s.b.engagements.Visible(viewer, "", 8)
	if err != nil || len(items) != 1 {
		t.Fatalf("lookup: %+v %v", items, err)
	}
	ref := items[0].Ref
	s.change(ref, map[string]any{"verdict": "accept", "change": "reaccept", "once": false, "authorization_quote": "keep notifying me about future releases"})
	got = s.send(engagementOwner, engagementOwner, 572, "Please expand this tracking to keep notifying me about future releases.")
	item, err := s.b.engagements.Get(ref.ID)
	if err != nil || (!item.Current.Once && len(item.Current.Actions) > 0) {
		t.Errorf("calendar was retained in the ongoing agreement: %s; %v", got, err)
	}
}

func TestEngagementDialogueRegressionIncorrectMCPFixedArgumentSchema(t *testing.T) {
	s := newEngagementScript(t)
	stub := server.NewMCPServer("release", "test")
	tool := einoTestMCPTool("release", "available", "")
	readOnly := true
	tool.info.Annotations.ReadOnlyHint = &readOnly
	mcp.WithNumber("repository", mcp.Required())(&tool.info)
	stub.AddTool(tool.info, tool.run)
	protocol := server.NewTestStreamableHTTPServer(stub, server.WithStateLess(true))
	t.Cleanup(protocol.Close)
	enableTestEngagementMCP(t, s.b, protocol.URL)
	got := sendRegressionAgreement(s, regressionWatchArgs(), engagementOwner, engagementOwner, 580, "Tell me when the stable release arrives.")
	if strings.Contains(got, "Agreement saved") {
		t.Errorf("invalid string fixed argument accepted against number schema: %s", got)
	}
}

func TestEngagementDialogueRegressionModelOnlyPromise(t *testing.T) {
	s := newEngagementScript(t)
	enableTestEngagementMCP(t, s.b, newEngagementMCP(t, "release", true))
	request := "Please track and tell me when the stable release arrives."
	s.reviews.Store(request, map[string]bool{"allow": false})
	s.final("I've saved the tracking and will notify you.", nil)
	got := s.send(engagementOwner, engagementOwner, 590, request)
	items, err := s.b.engagements.Visible(EngagementViewer{ChatID: engagementOwner, ActorID: engagementOwner}, "", 8)
	if err != nil || (len(items) == 0 && strings.Contains(got, "I've saved")) {
		t.Errorf("Telegram delivered unverified save claim: %s; records=%d; %v", got, len(items), err)
	}
}

func TestEngagementDialogueRegressionReceiptFallbackAfterEditFailure(t *testing.T) {
	provider := newEngagementReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, round int, _ openai.ChatCompletionRequest) {
		if round == 0 {
			engagementToolFrame(w, "engagement_decide", regressionWatchArgs())
			return
		}
		engagementModelText(w, "Saved.")
	}, nil)
	observedEdits := make(chan struct{}, 1)
	b, sent := newRoleTestBotWithTelegram(t, provider, func(w http.ResponseWriter, r *http.Request, d roleTestDelivery) bool {
		if strings.HasSuffix(r.URL.Path, "editMessageText") && strings.Contains(d.text, "Agreement saved") {
			fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`)
			observedEdits <- struct{}{}
			return true
		}
		return false
	})
	b.cfg.ProfileEnabled = false
	b.engagements = NewEngagementStore(b.chatDB)
	enableTestEngagementMCP(t, b, newEngagementMCP(t, "release", true))
	roleHandle(t, b, b.handleText, roleMessage(engagementOwner, engagementOwner, 600, "Tell me when the stable release arrives."))
	roleReceive(t, observedEdits)
	items, err := b.engagements.Visible(EngagementViewer{ChatID: engagementOwner, ActorID: engagementOwner}, "", 8)
	if err != nil || len(items) != 1 {
		t.Fatalf("agreement did not really save: %+v %v", items, err)
	}
	select {
	case msg := <-sent:
		if !strings.Contains(msg.text, "Agreement saved") || !strings.HasSuffix(msg.path, "/sendMessage") {
			t.Errorf("wrong fallback delivery: %+v", msg)
		}
	case <-time.After(250 * time.Millisecond):
		t.Errorf("agreement persisted, both edits rejected, no new sendMessage even though the local Telegram accepted sends")
	}
}

func TestEngagementDialogueRegressionRecipientMustMatchExplicitRequest(t *testing.T) {
	for _, tc := range []struct {
		name, request, proposed, interpreted string
		chat, wantRecipient                  int64
	}{
		{"auxiliary omitted", "Tell only group -702 when the stable release arrives.", "here", "", engagementGroup, 0},
		{"auxiliary misread", "Tell only group -702 when the stable release arrives.", "here", "here", engagementGroup, 0},
		{"auxiliary recognized", "Tell only group -702 when the stable release arrives.", "here", "other", engagementGroup, 0},
		{"misread as private", "Send updates only to chat ID -702 when the stable release arrives.", "private", "private", engagementGroup, 0},
		{"multiple recipients", "Notify group -701 and group -702 when the stable release arrives.", "here", "here", engagementGroup, 0},
		{"Chinese destination", "稳定版发布时只通知群 -702。", "here", "", engagementGroup, 0},
		{"explicit current group", "Tell group -701 when the stable release arrives.", "", "", engagementGroup, engagementGroup},
		{"group is observation source", "Watch group -702 and tell me when the stable release arrives.", "", "", engagementGroup, engagementGroup},
		{"group is conditional source", "Tell me when the stable release arrives in group -702.", "", "", engagementGroup, engagementGroup},
		{"default reply here", "Tell me when the stable release arrives.", "", "", engagementGroup, engagementGroup},
		{"explicit reply here", "Notify this chat when the stable release arrives.", "here", "here", engagementGroup, engagementGroup},
		{"explicit private delivery", "Notify me privately when the stable release arrives.", "private", "private", engagementGroup, engagementOwner},
		{"default private conversation", "Tell me when the stable release arrives.", "", "", engagementOwner, engagementOwner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newEngagementScript(t)
			enableTestEngagementMCP(t, s.b, newEngagementMCP(t, "release", true))
			s.intents.Store(tc.request, map[string]string{"recipient": tc.interpreted})
			args := regressionWatchArgs()
			args["recipient"] = tc.proposed
			if tc.name == "Chinese destination" {
				args["trigger"] = "稳定版"
			}
			message := tc.request
			if tc.chat == engagementGroup {
				message = "@testbot " + message
			}
			got := sendRegressionAgreement(s, args, tc.chat, engagementOwner, 620, message)
			rows, err := s.b.engagements.Visible(EngagementViewer{ChatID: tc.chat, ActorID: engagementOwner}, "", 8)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantRecipient == 0 {
				if len(rows) != 0 || !(strings.Contains(got, "No change was saved") || strings.Contains(got, "未保存变更")) {
					t.Fatalf("unavailable recipient: rows=%+v receipt=%q", rows, got)
				}
				t.Logf("rows=0 receipt=%q", got)
				return
			}
			if len(rows) != 1 || !strings.Contains(got, "Agreement saved") {
				t.Fatalf("expected one saved agreement: rows=%+v receipt=%q", rows, got)
			}
			item, err := s.b.engagements.Get(rows[0].Ref.ID)
			if err != nil || item.Status != EngagementActive || item.Current == nil ||
				!slices.Equal(item.Current.Recipients, []int64{tc.wantRecipient}) {
				t.Fatalf("active agreement has wrong recipient: %+v err=%v", item.Current, err)
			}
			t.Logf("status=%s recipients=%v receipt=%q", item.Status, item.Current.Recipients, got)
		})
	}
}

func TestEngagementDialogueRegressionUnverifiedRecipientPreservesExistingAgreement(t *testing.T) {
	s := newEngagementScript(t)
	enableTestEngagementMCP(t, s.b, newEngagementMCP(t, "release", true))
	got := sendRegressionAgreement(s, regressionWatchArgs(), engagementGroup, engagementOwner, 621,
		"@testbot Tell me when the stable release arrives.")
	rows, err := s.b.engagements.Visible(EngagementViewer{ChatID: engagementGroup, ActorID: engagementOwner}, "", 8)
	if err != nil || len(rows) != 1 || !strings.Contains(got, "Agreement saved") {
		t.Fatalf("seed agreement: rows=%+v receipt=%q err=%v", rows, got, err)
	}
	ref := rows[0].Ref
	s.change(ref, map[string]any{"verdict": "accept", "change": "agree", "recipient": "here", "duration": "this_week"})
	got = s.send(engagementGroup, engagementOwner, 622, "@testbot Send updates only to group -702 this week.")
	item, err := s.b.engagements.Get(ref.ID)
	if err != nil || item.Ref != ref || item.Status != EngagementActive || item.Current == nil ||
		!slices.Equal(item.Current.Recipients, []int64{engagementGroup}) || !item.Current.ExpiresAt.IsZero() ||
		!strings.Contains(got, "No change was saved") {
		t.Fatalf("scope change altered original agreement: item=%+v receipt=%q err=%v", item, got, err)
	}
	t.Logf("version=%d recipients=%v receipt=%q", item.Ref.Version, item.Current.Recipients, got)
}

func TestEngagementDialogueRegressionSelfPreferenceDoesNotChangeGroup(t *testing.T) {
	s := newEngagementScript(t)
	s.b.cfg.AdminWildcard = true
	s.tool("conversation_preference", map[string]any{"scope": "group", "verdict": "accept", "chatter": "off", "duration": "this_week"}, nil)
	s.final("Saved.", nil)
	request := "我这周在本群很忙，只对我少一些主动闲聊。"
	s.intents.Store(request, map[string]string{"preference": "self"})
	got := s.send(engagementGroup, engagementPeer, 630, "@testbot "+request)
	p, err := s.b.engagements.ConversationPreferenceAt(engagementGroup, 0, engagementNow)
	if err != nil || p.Chatter != ConversationDefault {
		t.Errorf("self-only request accepted as group preference: %s; %+v; %v", got, p, err)
	}
	s.tool("conversation_preference", map[string]any{"scope": "self", "verdict": "accept", "chatter": "reduce", "duration": "this_week"}, nil)
	s.final("Saved.", nil)
	got = s.send(engagementGroup, engagementPeer, 631, "@testbot "+request)
	self, err := s.b.engagements.ConversationPreferenceAt(engagementGroup, engagementPeer, engagementNow)
	group, groupErr := s.b.engagements.ConversationPreferenceAt(engagementGroup, 0, engagementNow)
	if err != nil || groupErr != nil || self.Chatter != ConversationReduce || group.Chatter != ConversationDefault ||
		!strings.Contains(got, "你在本群") {
		t.Errorf("personal preference: %s; %+v; %+v; %v %v", got, self, group, err, groupErr)
	}

}

func TestEngagementDialogueRegressionCompoundToolsAndReplay(t *testing.T) {
	s := newEngagementScript(t)
	s.b.cfg.AdminWildcard = true
	ref := createEngagement(t, s.b.engagements, engagementFixture(engagementGroup))
	request := "Please stop starting conversations here, and mute notifications for this tracking."
	s.intents.Store(request, map[string]any{"preference": "group", "task": true})
	pref := map[string]any{"scope": "group", "verdict": "accept", "chatter": "off"}
	change := map[string]any{"id": ref.ID, "version": ref.Version, "verdict": "accept", "change": "mute"}
	run := func(version uint64) string {
		change["version"] = version
		s.tool("engagement_context", map[string]any{"id": ref.ID}, nil)
		s.calls <- func(w http.ResponseWriter, req openai.ChatCompletionRequest) {
			for i, call := range []struct {
				name string
				args any
			}{{"conversation_preference", pref}, {"engagement_decide", change}, {"conversation_preference", pref}} {
				einoModelSSE(w, fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":%d,"id":"decision-%d","type":"function","function":{"name":%q,"arguments":%q}}]}}]}`, i, i, call.name, dialogueJSON(call.args)))
			}
			einoReplyFinish(w, "tool_calls")
		}
		s.final("The two changes were saved.", nil)
		return s.send(engagementGroup, engagementOwner, 641, "@testbot "+request)
	}
	first := run(ref.Version)
	item, err := s.b.engagements.Get(ref.ID)
	if err != nil || !item.Muted || item.Ref.Version != ref.Version+1 || !strings.Contains(first, "muted") || strings.Count(first, "Conversation preference saved") != 1 {
		t.Fatalf("compound result %q, ref %+v, muted %v, err %v", first, item.Ref, item.Muted, err)
	}
	second := run(ref.Version + 1)
	item, err = s.b.engagements.Get(ref.ID)
	p, prefErr := s.b.engagements.conversationPreference(engagementGroup, 0)
	if err != nil || prefErr != nil || p.Version != 1 || item.Ref.Version != ref.Version+1 || strings.Count(second, "already processed") != 2 {
		t.Fatalf("replayed result %q, ref %+v, pref version %d, errors %v %v", second, item.Ref, p.Version, err, prefErr)
	}
}

func TestEngagementDialogueRegressionReportsMissingCompoundPart(t *testing.T) {
	s := newEngagementScript(t)
	request := "Please don't start conversations in this group, and mute this tracking task."
	s.intents.Store(request, map[string]any{"preference": "group", "task": true})
	s.tool("conversation_preference", map[string]any{"scope": "group", "verdict": "accept", "chatter": "off"}, nil)
	s.final("Both saved.", nil)
	got := s.send(engagementGroup, engagementOwner, 650, "@testbot "+request)
	if !strings.Contains(got, "Conversation preference saved") || !strings.Contains(got, "No decision was recorded for the tracking request") ||
		strings.Contains(got, "Both saved") {
		t.Fatal(got)
	}
}

func TestEngagementDialogueRegressionUnknownEditResultDoesNotResend(t *testing.T) {
	provider := newEngagementReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, round int, _ openai.ChatCompletionRequest) {
		if round == 0 {
			engagementToolFrame(w, "engagement_decide", regressionWatchArgs())
			return
		}
		engagementModelText(w, "Saved.")
	}, nil)
	edits := make(chan struct{}, 1)
	b, sent := newRoleTestBotWithTelegram(t, provider, func(w http.ResponseWriter, r *http.Request, d roleTestDelivery) bool {
		if strings.HasSuffix(r.URL.Path, "editMessageText") && strings.Contains(d.text, "Agreement saved") {
			http.Error(w, "upstream reply lost", http.StatusBadGateway)
			edits <- struct{}{}
			return true
		}
		return false
	})
	b.cfg.ProfileEnabled = false
	b.engagements = NewEngagementStore(b.chatDB)
	enableTestEngagementMCP(t, b, newEngagementMCP(t, "release", true))
	roleHandle(t, b, b.handleText, roleMessage(engagementOwner, engagementOwner, 660, "Tell me when the stable release arrives."))
	roleReceive(t, edits)
	select {
	case msg := <-sent:
		t.Fatalf("unexpected second delivery after an uncertain edit result: %+v", msg)
	case <-time.After(80 * time.Millisecond):
	}
	items, err := b.engagements.Visible(EngagementViewer{ChatID: engagementOwner, ActorID: engagementOwner}, "", 8)
	if err != nil || len(items) != 1 || len(b.store.Get(engagementOwner)) != 0 {
		t.Fatalf("unexpected persistence or delivery evidence: %+v; %v", items, err)
	}
}

func TestEngagementDialogueRegressionDoesNotShowUnreviewedDraft(t *testing.T) {
	request := "Please keep tracking the stable release."
	const draft = "Leave it with me."
	modelStarted := make(chan struct{})
	provider := newEngagementReplyProvider(t, func(w http.ResponseWriter, r *http.Request, _ int, _ openai.ChatCompletionRequest) {
		einoModelSSE(w, einoReplyTextFrame(draft))
		close(modelStarted)
		select {
		case <-time.After(1650 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		einoReplyFinish(w, "stop")
	}, func(message, reply string) any {
		if message != request || reply != draft {
			t.Errorf("unexpected reviewer input %q; %q", message, reply)
		}
		return map[string]bool{"allow": false}
	})
	drafts := make(chan string, 2)
	b, sent := newRoleTestBotWithTelegram(t, provider, func(_ http.ResponseWriter, r *http.Request, d roleTestDelivery) bool {
		if strings.HasSuffix(r.URL.Path, "editMessageText") && strings.HasSuffix(d.text, "▌") {
			drafts <- d.text
		}
		return false
	})
	b.cfg.ProfileEnabled = false
	b.engagements = NewEngagementStore(b.chatDB)
	roleHandle(t, b, b.handleText, roleMessage(engagementOwner, engagementOwner, 665, request))
	roleReceive(t, modelStarted)
	reply := roleReceive(t, sent)
	if !strings.Contains(reply.text, "No tracking agreement") || strings.Contains(reply.text, draft) {
		t.Fatalf("unexpected final reply %q", reply.text)
	}
	waitEinoReplyHistory(t, b, engagementOwner, 2)
	select {
	case draft := <-drafts:
		t.Fatalf("unreviewed text reached Telegram: %q", draft)
	default:
	}
}

func TestEngagementDialogueRegressionSpeechAndStickerDelivery(t *testing.T) {
	provider := newEngagementReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, round int, req openai.ChatCompletionRequest) {
		switch round {
		case 0:
			if !strings.Contains(req.Messages[0].Content, "Speech Mode Instruction") {
				t.Error("voice mode instruction was omitted")
			}
			engagementModelText(w, "**ordinary answer**")
		case 1:
			engagementToolFrame(w, "engagement_decide", regressionWatchArgs())
		case 2:
			engagementModelText(w, "Saved.")
		}
	}, nil)
	stickers := make(chan struct{}, 2)
	b, delivered := newRoleTestBotWithTelegram(t, provider, func(w http.ResponseWriter, r *http.Request, d roleTestDelivery) bool {
		if r.URL.Path == "/botrole/sendSticker" {
			stickers <- struct{}{}
			fmt.Fprintf(w, `{"ok":true,"result":{"message_id":20,"chat":{"id":%d,"type":"private"},"sticker":{"file_id":"fake-sticker"}}}`, engagementOwner)
			return true
		}
		return false
	})
	b.cfg.ProfileEnabled = false
	b.engagements = NewEngagementStore(b.chatDB)
	enableTestEngagementMCP(t, b, newEngagementMCP(t, "release", true))
	var syntheses atomic.Int64
	speech := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		syntheses.Add(1)
		var req struct {
			Params struct{ Text string } `json:"req_params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Params.Text != "ordinary answer" {
			t.Errorf("unexpected speech payload: %+v; %v", req, err)
		}
		http.Error(w, "synthesis unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(speech.Close)
	b.speechModes = NewSpeechModeStore()
	b.speechModes.Set(engagementOwner, true)
	b.tts = &VolcengineTTSClient{url: speech.URL, httpClient: speech.Client()}
	b.cfg.StickerEnabled, b.cfg.StickerWithSpeech, b.cfg.StickerSendProbability = true, true, 1
	b.cfg.StickerMode = "append"
	b.stickers = &StickerEngine{rules: StickerRules{
		KeywordToLabel: map[string]string{"ordinary answer": "ordinary", "agreement saved": "ordinary"},
		LabelStickers:  map[string][]string{"ordinary": {"fake-sticker"}},
	}}

	roleHandle(t, b, b.handleText, roleMessage(engagementOwner, engagementOwner, 680, "Answer an ordinary question."))
	if msg := roleReceive(t, delivered); msg.text != "🔊 正在发送语音…" {
		t.Fatal(msg)
	}
	if msg := roleReceive(t, delivered); msg.text != "<b>ordinary answer</b>" {
		t.Fatal(msg)
	}
	roleReceive(t, delivered) // TTS failure notice
	roleReceive(t, stickers)

	roleHandle(t, b, b.handleText, roleMessage(engagementOwner, engagementOwner, 681, "Tell me when the stable release arrives."))
	if msg := roleReceive(t, delivered); !strings.Contains(msg.text, "Agreement saved") {
		t.Fatal(msg)
	}
	waitEinoReplyHistory(t, b, engagementOwner, 4)
	if calls := syntheses.Load(); calls != 1 {
		t.Fatalf("ordinary response and receipt triggered %d voice requests", calls)
	}
	select {
	case <-stickers:
		t.Fatal("agreement receipt triggered a sticker")
	default:
	}
}
