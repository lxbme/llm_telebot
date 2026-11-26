package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	openai "github.com/sashabaranov/go-openai"
)

func TestTrackedChangeNotifications(t *testing.T) {
	s := newEngagementScript(t)
	var now atomic.Int64
	now.Store(engagementNow.Unix())
	s.b.engagements.now = func() time.Time { return time.Unix(now.Load(), 0).UTC() }
	var stage, reads atomic.Int32
	source := server.NewMCPServer("upstream", "local")
	source.AddTool(mcp.NewTool("release", mcp.WithReadOnlyHintAnnotation(true), mcp.WithString("repository", mcp.Required())), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		reads.Add(1)
		if req.GetString("repository", "") != "upstream/project" {
			t.Errorf("source query changed: %v", req.GetArguments())
		}
		var events []map[string]any
		add := func(id, title, summary string) {
			events = append(events, map[string]any{"id": id, "title": title, "summary": summary,
				"url": "https://example.invalid/upstream/releases/" + id})
		}
		switch stage.Load() {
		case 0:
			add("merge-41", "Merged fix #41", "The change is merged into the main branch")
		case 1:
			add("v2-rc", "Preview v2 RC", "This preview includes fix #41")
		case 2:
			add("v1", "Stable v1", "This release does not contain fix #41")
		case 3:
			add("v2", "Stable v2", "Release notes confirm inclusion of fix #41")
		case 4:
			add("v3", "Stable v3", "Release notes confirm inclusion of fix #41")
		case 5, 6:
			add("v3", "Stable v3", "Release notes confirm inclusion of fix #41")
			add("v4", "Stable v4", "Release notes confirm inclusion of fix #41")
			if stage.Load() == 6 {
				add("v5", "Stable v5", "Release notes confirm inclusion of fix #41")
				add("v6", "Stable v6", "Release notes confirm inclusion of fix #41")
			}
		}
		return &mcp.CallToolResult{StructuredContent: map[string]any{"events": events}}, nil
	})
	api := server.NewTestStreamableHTTPServer(source, server.WithStateLess(true))
	defer api.Close()
	enableTestEngagementMCP(t, s.b, api.URL)

	const remark = "等包含 #41 修复的稳定版出来我就能撤补丁了"
	args := map[string]any{"kind": "tracking", "verdict": "accept", "change": "agree", "observe": "public:upstream:release",
		"trigger": "包含 #41 修复的稳定版", "recipient": "here", "once": false}
	if receipt := sendRegressionAgreement(s, args, engagementOwner, engagementOwner, 301, remark); !strings.Contains(receipt, "已") {
		t.Fatalf("no recognition of interest: %q", receipt)
	}
	items := allEngagementRows(t, s.b.engagements)
	if len(items) != 1 || items[0].Kind != EngagementTracking || len(items[0].Current.Actions) != 0 {
		t.Fatalf("no durable read-only interest: %+v", items)
	}
	_, err := s.b.engagements.setConversationPreference(
		engagementRequest(engagementOwner, engagementOwner, 302, "先观察，暂时别通知").Source, 0, 0, "task7",
		conversationPreferenceSave{Notifications: ConversationOff, NotificationUntil: engagementNow.Add(6 * time.Minute)}, engagementNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
		input := fmt.Sprint(req.Messages)
		if req.Stream || len(req.Tools) != 0 || !strings.Contains(input, "包含 #41 修复的稳定版") ||
			!strings.Contains(input, s.b.cfg.SystemPrompt) {
			t.Errorf("invalid background role input: %s", input)
		}
		ids := []string{}
		for _, id := range []string{"v2", "v3", "v4", "v5", "v6"} {
			if strings.Contains(input, "releases/"+id+`"`) {
				ids = append(ids, id)
			}
		}
		_ = json.NewEncoder(w).Encode(openai.ChatCompletionResponse{Choices: []openai.ChatCompletionChoice{
			{Message: openai.ChatCompletionMessage{Content: dialogueJSON(map[string]any{"matched_ids": ids})}},
		}})
	})
	s.b.ai = model.client
	run := func(at int, currentStage int32) {
		now.Store(engagementNow.Add(time.Duration(at) * time.Minute).Unix())
		stage.Store(currentStage)
		r := NewTaskRunner(s.b, nil)
		r.tick(t.Context(), s.b.engagements.now())
		waitTask7Runner(t, r)
	}
	for i := range 6 {
		run(i, int32(i))
		if len(s.replies) != 0 {
			t.Fatalf("unexpected delivery at stage %d: %+v", i, <-s.replies)
		}
	}
	run(6, 5)
	report := roleReceive(t, s.replies)
	if report.chatID != engagementOwner || report.path != "/botrole/sendMessage" || len(s.replies) != 0 ||
		!strings.Contains(report.text, "https://example.invalid/upstream/releases/v3") ||
		!strings.Contains(report.text, "https://example.invalid/upstream/releases/v4") ||
		strings.Contains(report.text, "Stable v2") || !strings.Contains(report.text, "fix #41") {
		t.Fatalf("expected one current report with evidence: %+v", report)
	}
	run(7, 5)
	if len(s.replies) != 0 || reads.Load() < 7 {
		t.Fatalf("duplicate delivery or observation suspended: sends=%d reads=%d", len(s.replies), reads.Load())
	}

	// A contradictory success envelope must preserve the previous delivery
	// time and both uncertain outcomes across a switch and reopen.
	contradictory, _ := newRoleTestBotWithTelegram(t, model, func(w http.ResponseWriter, _ *http.Request, d roleTestDelivery) bool {
		if d.path != "/botrole/sendMessage" {
			return false
		}
		if d.chatID != engagementOwner || !strings.Contains(d.text, "Stable v5") || !strings.Contains(d.text, "Stable v6") {
			t.Errorf("incorrect attempted combined Telegram delivery: %+v", d)
		}
		fmt.Fprintf(w, `{"ok":true,"result":{"message_id":111,"chat":{"id":%d,"type":"private"},"voice":{"file_id":"other","duration":1}}}`, d.chatID)
		return true
	})
	s.b.tg = contradictory.tg
	run(8, 6)
	ref := items[0].Ref
	assertReceipts := func() {
		t.Helper()
		row := getRunnerItem(t, s.b, ref)
		if row.UncertainSteps != 2 || !row.LastDeliveredAt.Equal(engagementNow.Add(6*time.Minute)) {
			t.Fatalf("unconfirmed attempts counted as delivery: %+v", row)
		}
		for _, work := range getRunnerWork(t, s.b, ref) {
			result := work.Results
			switch work.Observation.ChangeKey {
			case "v2":
				if len(result) != 0 {
					t.Fatalf("obsolete v2 had a delivery attempt: %+v", result)
				}
			case "v3", "v4":
				if len(result) != 1 || result[0].State != EngagementSucceeded || result[0].Attempt != 1 {
					t.Fatalf("lost confirmed combined progress: %+v", result)
				}
			case "v5", "v6":
				if len(result) != 1 || result[0].State != EngagementUnknown || result[0].Attempt != 1 {
					t.Fatalf("lost uncertain combined progress: %+v", result)
				}
			}
		}
	}
	assertReceipts()
	path := filepath.Join(t.TempDir(), "reopen.db")
	db, err := OpenChatDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateChatStores(db, s.b.store, s.b.summaries, s.b.memory, s.b.engagements); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenChatDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s.b.engagements = NewEngagementStore(db)
	s.b.engagements.now = func() time.Time { return time.Unix(now.Load(), 0).UTC() }
	fresh, freshReplies := newRoleTestBot(t, model)
	s.b.tg = fresh.tg
	run(9, 6)
	assertReceipts()
	if len(freshReplies) != 0 {
		t.Fatal("reopen resent an uncertain or previously delivered report")
	}
}

func TestRoleLifecycle(t *testing.T) {
	for _, scenario := range []string{"confirmed", "unconfirmed", "lookup_rate_limited"} {
		t.Run(scenario, func(t *testing.T) { testPrivateRoleFollowup(t, scenario) })
	}
}

func testPrivateRoleFollowup(t *testing.T, scenario string) {
	s := newEngagementScript(t)
	const remark = "下周我有一场手术，希望一切顺利"
	s.tool("engagement_context", map[string]any{}, func(req openai.ChatCompletionRequest) {
		if !strings.Contains(fmt.Sprint(req.Messages), s.b.cfg.SystemPrompt) {
			t.Error("missing current role")
		}
	})
	s.tool("engagement_decide", map[string]any{"kind": "tracking", "verdict": "accept", "change": "agree",
		"observe": "private:followup", "trigger": "我有一场手术", "recipient": "here"}, func(req openai.ChatCompletionRequest) {
		if !strings.Contains(engagementLastResult(req), "private:followup") {
			t.Error("the actual dialogue did not offer private follow-up")
		}
	})
	s.final("我会关心你。", nil)
	s.send(engagementOwner, engagementOwner, 401, remark)
	items := allEngagementRows(t, s.b.engagements)
	if len(items) != 1 || items[0].Current == nil {
		t.Fatalf("no saved private follow-up: %+v", items)
	}
	s.b.cfg.SystemPrompt = "You are Rowan. Be discreet."
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
		input := fmt.Sprint(req.Messages)
		if req.Stream || len(req.Tools) != 0 || !strings.Contains(input, "You are Rowan") || !strings.Contains(input, "我有一场手术") {
			t.Errorf("invalid private follow-up prompt: %s", input)
		}
		_ = json.NewEncoder(w).Encode(openai.ChatCompletionResponse{Choices: []openai.ChatCompletionChoice{
			{Message: openai.ChatCompletionMessage{Content: `{"message":"想问问你，手术之后还好吗？"}`}},
		}})
	})
	s.b.ai = model.client
	sends := s.replies
	var attempts atomic.Int32
	if scenario != "confirmed" {
		var rateLimited atomic.Bool
		wire, deliveries := newRoleTestBotWithTelegram(t, model, func(w http.ResponseWriter, _ *http.Request, d roleTestDelivery) bool {
			if scenario == "lookup_rate_limited" {
				if d.path == "/botrole/getChat" && rateLimited.CompareAndSwap(false, true) {
					w.WriteHeader(http.StatusTooManyRequests)
					fmt.Fprint(w, `{"ok":false,"error_code":429,"description":"Too Many Requests"}`)
					return true
				}
				return false
			}
			if d.path != "/botrole/sendMessage" {
				return false
			}
			attempts.Add(1)
			if d.chatID != engagementOwner || !strings.Contains(d.text, "手术之后还好吗") {
				t.Errorf("wrong attempted private follow-up: %+v", d)
			}
			fmt.Fprintf(w, `{"ok":true,"result":{"message_id":222,"chat":{"id":%d,"type":"private"}}}`, d.chatID)
			return true
		})
		s.b.tg, sends = wire.tg, deliveries
	}
	tick := func(elapsed time.Duration) {
		at := engagementNow.Add(elapsed)
		s.b.engagements.now = func() time.Time { return at }
		r := NewTaskRunner(s.b, nil)
		r.tick(t.Context(), at)
		waitTask7Runner(t, r)
	}
	tick(time.Hour)
	if len(s.replies) != 0 || model.calls.Load() != 0 {
		t.Fatal("asked before appropriate time")
	}
	due := 8 * 24 * time.Hour
	tick(due)
	ref := items[0].Ref
	if scenario == "lookup_rate_limited" {
		if got := getRunnerItem(t, s.b, ref); got.Status != EngagementActive || len(sends) != 0 || model.calls.Load() != 0 {
			t.Fatalf("temporary lookup failure changed the agreement or sent a message: %+v", got)
		}
		due += 24 * time.Hour
		tick(due)
	}
	if scenario == "unconfirmed" {
		work := getRunnerWork(t, s.b, ref)[0]
		if item := getRunnerItem(t, s.b, ref); !item.LastDeliveredAt.IsZero() || item.EndReason == EngagementCompleted ||
			attempts.Load() != 1 || len(work.Results) != 1 || work.Results[0].State != EngagementUnknown {
			t.Fatalf("private follow-up accepted an empty text receipt: %+v / %+v", item, work.Results)
		}
		if err := s.b.chatDB.Close(); err != nil {
			t.Fatal(err)
		}
		db, err := OpenChatDB(s.b.cfg.ChatDBPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		s.b.engagements = NewEngagementStore(db)
	} else {
		got := roleReceive(t, sends)
		if got.chatID != engagementOwner || !strings.Contains(got.text, "手术之后还好吗") ||
			getRunnerItem(t, s.b, ref).EndReason != EngagementCompleted {
			t.Fatalf("wrong private follow-up: %+v", got)
		}
	}
	tick(due + 24*time.Hour)
	if len(sends) != 0 || model.calls.Load() != 1 || attempts.Load() > 1 {
		t.Fatal("private result was repeatedly requested")
	}
}

func TestRoleLifecycleGroupAgreement(t *testing.T) {
	s := newEngagementScript(t)
	const oldFact = "I prefer the private black-tea blend"
	const newFact = "I prefer the private green-tea blend"
	reflection := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
		input := fmt.Sprint(req.Messages)
		if req.Stream || !strings.Contains(input, "Alder. Be reserved.") {
			t.Errorf("reflection used the wrong role: %s", input)
		}
		switch {
		case strings.Contains(input, "Correction, my preferred tea"):
			if !strings.Contains(input, oldFact) {
				t.Error("correction did not receive its memory candidate")
			}
			roleCompletion(w, dialogueJSON(map[string]any{"kind": "correction", "replace": 1, "conclusion": newFact, "cue": "familiar"}))
		case strings.Contains(input, oldFact):
			roleCompletion(w, dialogueJSON(map[string]any{"kind": "statement", "conclusion": oldFact, "cue": "familiar"}))
		default:
			roleCompletion(w, `{"kind":"skip"}`)
		}
	})
	s.b.cfg.ProfileEnabled, s.b.profileAI = true, reflection.client
	s.final("Got it.", nil)
	s.send(engagementOwner, engagementOwner, 501, oldFact+".")
	initial := waitRoleMemory(t, s.b, oldFact)
	s.final("Thanks for the correction.", nil)
	s.send(engagementOwner, engagementOwner, 502, "Correction, my preferred tea is the private green-tea blend.")
	corrected := waitRoleMemory(t, s.b, newFact)
	if corrected.Ref.ID != initial.Ref.ID || corrected.Ref.Version != initial.Ref.Version+1 {
		t.Fatalf("memory did not revise the original entry: %+v", corrected)
	}

	var reads atomic.Int32
	source := server.NewMCPServer("upstream", "local")
	source.AddTool(mcp.NewTool("release", mcp.WithReadOnlyHintAnnotation(true), mcp.WithString("repository", mcp.Required())), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		reads.Add(1)
		if req.GetString("repository", "") != "upstream/project" {
			t.Errorf("unexpected query: %+v", req.GetArguments())
		}
		return &mcp.CallToolResult{StructuredContent: map[string]any{"events": []engagementEvent{
			{ID: "v6", Title: "Stable releases v6", Summary: "Published on the stable channel"},
		}}}, nil
	})
	api := server.NewTestStreamableHTTPServer(source, server.WithStateLess(true))
	defer api.Close()
	enableTestEngagementMCP(t, s.b, api.URL)
	stats := NewStatsManager(s.b.chatDB)
	s.b.mu.Lock()
	s.b.stats = stats
	s.b.mu.Unlock()
	defer stats.Close()
	args := regressionWatchArgs()
	args["trigger"] = "releases"
	args["declined"] = []string{"run verification in a sandbox"}
	s.tool("engagement_decide", args, func(req openai.ChatCompletionRequest) {
		input := fmt.Sprint(req.Messages)
		if !strings.Contains(input, "Alder. Be reserved.") || !strings.Contains(input, "familiar") ||
			strings.Contains(input, oldFact) || strings.Contains(input, newFact) {
			t.Errorf("incorrect group memory projection: %s", input)
		}
	})
	s.final("I ran the sandbox already.", nil)
	if receipt := s.send(engagementGroup, engagementOwner, 503,
		"@testbot Watch releases for this group and run verification in a sandbox."); !strings.Contains(receipt, "run verification in a sandbox") ||
		strings.Contains(receipt, "I ran the sandbox already") {
		t.Fatalf("partial agreement receipt: %q", receipt)
	}
	rows := allEngagementRows(t, s.b.engagements)
	if len(rows) != 1 || len(rows[0].Current.Actions) != 0 {
		t.Fatalf("unsupported action was granted: %+v", rows)
	}
	ref := rows[0].Ref
	s.change(ref, map[string]any{"verdict": "accept", "change": "agree", "trigger": "stable releases"})
	s.send(engagementGroup, engagementPeer, 504, "@testbot Only report stable releases.")
	item := getRunnerItem(t, s.b, ref)
	if item.Current.Trigger.Text != "stable releases" || item.Origin.SenderID != engagementOwner ||
		item.Decisions[1].Request.Source.SenderID != engagementPeer {
		t.Fatalf("revision changed the original initiator or failed to apply: %+v", item)
	}
	s.change(item.Ref, map[string]any{"verdict": "accept", "change": "pause"})
	s.send(engagementGroup, engagementOwner, 505, "@testbot Pause this task.")
	runner := NewTaskRunner(s.b, nil)
	runner.tick(t.Context(), engagementNow)
	waitTask7Runner(t, runner)
	if reads.Load() != 0 || len(s.replies) != 0 {
		t.Fatal("paused agreement ran")
	}
	item = getRunnerItem(t, s.b, ref)
	s.change(item.Ref, map[string]any{"verdict": "accept", "change": "resume"})
	s.send(engagementGroup, engagementOwner, 506, "@testbot Resume this task.")
	background := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
		input := fmt.Sprint(req.Messages)
		if req.Stream || len(req.Tools) != 0 || !strings.Contains(input, s.b.cfg.SystemPrompt) ||
			!strings.Contains(input, "stable releases") || !strings.Contains(input, "Published on the stable channel") ||
			strings.Contains(input, newFact) {
			t.Errorf("incorrect verification input: %s", input)
		}
		_ = json.NewEncoder(w).Encode(openai.ChatCompletionResponse{Model: "background-test", Choices: []openai.ChatCompletionChoice{
			{Message: openai.ChatCompletionMessage{Content: `{"matched_ids":["v6"]}`}},
		}, Usage: openai.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}})
	})
	date := engagementNow.Format("20060102")
	counts := func() (group, owner, peer int64) {
		return s.b.stats.GetQuotaBucket(UsageScopeChat, engagementGroup, 0, date).QuotaTokens,
			s.b.stats.GetQuotaBucket(UsageScopeUser, 0, engagementOwner, date).QuotaTokens,
			s.b.stats.GetQuotaBucket(UsageScopeUser, 0, engagementPeer, date).QuotaTokens
	}
	groupBefore, ownerBefore, peerBefore := counts()
	s.b.mu.Lock()
	s.b.ai = background.client
	s.b.mu.Unlock()
	runner.tick(t.Context(), engagementNow)
	waitTask7Runner(t, runner)
	delivery := roleReceive(t, s.replies)
	item = getRunnerItem(t, s.b, ref)
	work := getRunnerWork(t, s.b, ref)[0]
	if delivery.path != "/botrole/sendMessage" || delivery.chatID != engagementGroup || !strings.Contains(delivery.text, "Stable releases v6") ||
		item.EndReason != EngagementCompleted || work.Results[0].State != EngagementSucceeded || reads.Load() != 1 {
		t.Fatalf("agreement did not deliver once: %+v, %+v, %+v", delivery, item, work)
	}
	if group, owner, peer := counts(); group-groupBefore != 10 || owner-ownerBefore != 10 || peer-peerBefore != 0 {
		t.Fatalf("wrong background charge: group=%d initiator=%d editor=%d", group-groupBefore, owner-ownerBefore, peer-peerBefore)
	}
}

func TestTrackedChangeNotificationsOneShotQuiet(t *testing.T) {
	h := newRunnerHarness(t, false)
	b, _ := h.open()
	ref := h.seed(b, engagementOwner, false, true, engagementOwner)
	_, err := b.engagements.Decide(ref, EngagementDecision{
		Request: engagementRequest(engagementOwner, engagementOwner, 880, "Keep watching but do not notify me yet."),
		Verdict: EngagementAccept, Change: EngagementMute, MuteUntil: h.clock().Add(3 * engagementPoll), At: h.clock(),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.tick(b)
	h.advance(engagementPoll)
	h.tick(b)
	if h.connector.reads.Load() != 2 || len(h.telegram.sends) != 0 {
		t.Fatalf("one-shot observation stopped during notification mute: reads=%d sends=%d", h.connector.reads.Load(), len(h.telegram.sends))
	}
	h.advance(2 * engagementPoll)
	h.tick(b)
	if got := roleReceive(t, h.telegram.sends); got.chatID != engagementOwner ||
		getRunnerItem(t, b, ref).EndReason != EngagementCompleted {
		t.Fatalf("bound one-shot did not deliver after mute: %+v", got)
	}
}

func TestTrackedChangeNotificationsOneShotRevalidates(t *testing.T) {
	for _, replacement := range []int64{1, 3} {
		t.Run(fmt.Sprintf("next_match=%d", replacement), func(t *testing.T) {
			h := newRunnerHarness(t, false)
			b, _ := h.open()
			ref := h.seed(b, engagementOwner, false, true, engagementOwner)
			ref = reviseEngagement(t, b.engagements, ref, EngagementDecision{
				Request: engagementRequest(engagementOwner, engagementOwner, 882, "Keep watching quietly."),
				Verdict: EngagementAccept, Change: EngagementMute, MuteUntil: h.clock().Add(engagementPoll), At: h.clock(),
			})
			h.tick(b)
			old := getRunnerItem(t, b, ref)
			if old.OnceChangeKey == "" || len(h.telegram.sends) != 0 {
				t.Fatalf("initial match was not held: %+v", old)
			}

			h.advance(engagementPoll)
			h.connector.event.Store(-1)
			h.tick(b)
			failed := getRunnerItem(t, b, ref)
			if failed.OnceChangeKey != old.OnceChangeKey || !failed.LastObservedAt.Equal(old.LastObservedAt) ||
				len(h.telegram.sends) != 0 {
				t.Fatalf("temporary failure sent or displaced the stored match: %+v", failed)
			}

			h.advance(engagementPoll)
			h.connector.event.Store(0)
			h.tick(b)
			empty := getRunnerItem(t, b, ref)
			if empty.OnceChangeKey != "" || empty.Status != EngagementActive || !empty.LastObservedAt.Equal(h.clock()) ||
				len(h.telegram.sends) != 0 {
				t.Fatalf("authoritative empty snapshot did not reopen the wait: %+v", empty)
			}
			b = h.restart(b)
			h.advance(engagementPoll)
			h.connector.event.Store(replacement)
			h.tick(b)
			got := roleReceive(t, h.telegram.sends)
			done := getRunnerItem(t, b, ref)
			if got.chatID != engagementOwner || !strings.Contains(got.text, fmt.Sprintf("Release review %d", replacement)) ||
				len(h.telegram.sends) != 0 || done.EndReason != EngagementCompleted || !done.LastDeliveredAt.Equal(h.clock()) ||
				h.connector.reads.Load() != 4 {
				t.Fatalf("wrong replacement delivery: %+v / %+v (reads=%d)", got, done, h.connector.reads.Load())
			}
		})
	}
}

func TestTrackedChangeNotificationsOneShotKeepsPartialReceipts(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprintf("unknown=%t", unknown), func(t *testing.T) {
			h := newRunnerHarness(t, false)
			b, _ := h.open()
			ref := h.seed(b, engagementGroup, false, true, engagementGroup, engagementOwner)
			_, err := b.engagements.setConversationPreference(
				engagementRequest(engagementOwner, engagementOwner, 883, "Keep my notifications quiet.").Source, 0, 0, "task7",
				conversationPreferenceSave{Notifications: ConversationOff, NotificationUntil: h.clock().Add(engagementPoll)}, h.clock(),
			)
			if err != nil {
				t.Fatal(err)
			}
			want := EngagementSucceeded
			if unknown {
				h.telegram.failTo.Store(engagementGroup)
				h.telegram.fail.Store(http.StatusBadGateway)
				want = EngagementUnknown
			}
			h.tick(b)
			if first := roleReceive(t, h.telegram.sends); first.chatID != engagementGroup {
				t.Fatalf("wrong first recipient: %+v", first)
			}
			first := getRunnerItem(t, b, ref)

			b = h.restart(b)
			h.advance(engagementPoll)
			h.connector.event.Store(2)
			h.telegram.fail.Store(0)
			h.tick(b)
			current := getRunnerItem(t, b, ref)
			works := getRunnerWork(t, b, ref)
			if len(h.telegram.sends) != 0 || current.OnceChangeKey != first.OnceChangeKey ||
				!current.LastDeliveredAt.Equal(first.LastDeliveredAt) || len(works) != 1 ||
				len(works[0].Results) != 1 || works[0].Results[0].State != want {
				t.Fatalf("replacement crossed a settled or uncertain one-shot: %+v / %+v", current, works)
			}
			h.advance(engagementPoll)
			h.connector.event.Store(1)
			h.tick(b)
			if next := roleReceive(t, h.telegram.sends); next.chatID != engagementOwner || len(h.telegram.sends) != 0 {
				t.Fatalf("retried a settled recipient or missed the pending one: %+v", next)
			}
		})
	}
}
