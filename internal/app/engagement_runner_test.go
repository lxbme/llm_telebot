package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	tele "gopkg.in/telebot.v3"

	openai "github.com/sashabaranov/go-openai"
)

func TestLegacyScheduledDeliveryReportsModelAndTelegramFailure(t *testing.T) {
	for _, failModel := range []bool{true, false} {
		t.Run(fmt.Sprint("model_failure=", failModel), func(t *testing.T) {
			model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ openai.ChatCompletionRequest) {
				if failModel {
					http.Error(w, "local outage", http.StatusServiceUnavailable)
					return
				}
				engagementModelText(w, "Scheduled result.")
			})
			var editCalls atomic.Int64
			b, _ := newRoleTestBotWithTelegram(t, model, func(w http.ResponseWriter, _ *http.Request, d roleTestDelivery) bool {
				if d.path != "/botrole/editMessageText" {
					return false
				}
				editCalls.Add(1)
				fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: message can't be edited"}`)
				return true
			})
			b.tasks, b.reminders = NewTaskStore(b.chatDB), NewReminderStore(b.chatDB)
			task := ScheduledTask{ID: "daily", Prompt: "Update", CronExpr: "* * * * *", Timezone: "UTC", Enabled: true, CreatedBy: engagementOwner}
			b.tasks.Upsert(engagementOwner, task)
			NewTaskRunner(b, b.tasks).execute(engagementOwner, task)
			stored := b.tasks.List(engagementOwner)[0]
			want := "Telegram"
			if failModel {
				want = "model"
			}
			if !strings.Contains(stored.LastError, want) || editCalls.Load() == 0 {
				t.Fatalf("model=%v final edits=%d persisted failure=%q, want %q", failModel, editCalls.Load(), stored.LastError, want)
			}
		})
	}
}

type engagementLocalPoller struct{ ready chan struct{} }

func (p engagementLocalPoller) Poll(_ *tele.Bot, _ chan tele.Update, stop chan struct{}) {
	close(p.ready)
	<-stop
}

type runnerConnector struct {
	source, calendar           string
	reads, writes, applied     atomic.Int64
	event                      atomic.Int64
	events                     atomic.Pointer[[]engagementEvent]
	entered, reply             chan struct{}
	replayEntered, replayReply chan struct{}
	calendarRequests           chan map[string]any
	outcome                    atomic.Value
	keys                       sync.Map
}

func newRunnerConnector(t *testing.T, idempotent bool) *runnerConnector {
	t.Helper()
	c := &runnerConnector{}
	c.outcome.Store("created")
	c.event.Store(1)
	upstream := server.NewMCPServer("upstream", "local-test")
	upstream.AddTool(mcp.NewTool("event_list", mcp.WithReadOnlyHintAnnotation(true), mcp.WithString("repository", mcp.Required())), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		c.reads.Add(1)
		if req.GetString("repository", "") != "local/project" {
			t.Errorf("unbound observer args: %v", req.GetArguments())
		}
		i := c.event.Load()
		if i < 0 {
			return mcp.NewToolResultError("upstream temporarily unavailable"), nil
		}
		if events := c.events.Load(); events != nil {
			return &mcp.CallToolResult{StructuredContent: map[string]any{"events": *events}}, nil
		}
		if i == 0 {
			return &mcp.CallToolResult{StructuredContent: map[string]any{"events": []engagementEvent{}}}, nil
		}
		event := engagementEvent{
			ID: fmt.Sprintf("upstream-%d", i), Title: fmt.Sprintf("Release review %d", i),
			Summary: "Confirmed stable release", Start: engagementNow.Add(time.Hour), End: engagementNow.Add(2 * time.Hour),
		}
		return &mcp.CallToolResult{StructuredContent: map[string]any{"events": []engagementEvent{event}}}, nil
	})
	calendar := server.NewMCPServer("calendar", "local-test")
	options := []mcp.ToolOption{}
	for _, field := range []string{"calendarId", "title", "start", "end"} {
		options = append(options, mcp.WithString(field, mcp.Required()))
	}
	if idempotent {
		options = append(options, mcp.WithString("idempotencyKey", mcp.Required()))
	}
	argCount := len(options)
	calendar.AddTool(mcp.NewTool("insert_event", options...), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		write := c.writes.Add(1)
		key := req.GetString("idempotencyKey", "")
		if req.GetString("calendarId", "") != "work" || len(req.GetArguments()) != argCount || idempotent != (key != "") {
			t.Errorf("unbound calendar args: %v", req.GetArguments())
		}
		if c.calendarRequests != nil {
			c.calendarRequests <- req.GetArguments()
		} else {
			events := c.events.Load()
			if events == nil {
				events = &[]engagementEvent{{Title: "Release review 1", Start: engagementNow.Add(time.Hour), End: engagementNow.Add(2 * time.Hour)}}
			}
			if !slices.ContainsFunc(*events, func(event engagementEvent) bool {
				return req.GetString("title", "") == event.Title && req.GetString("start", "") == event.Start.Format(time.RFC3339) &&
					req.GetString("end", "") == event.End.Format(time.RFC3339)
			}) {
				t.Errorf("unexpected calendar event: %v", req.GetArguments())
			}
		}
		status := c.outcome.Load().(string)
		if status != "not_applied" {
			if _, repeated := c.keys.LoadOrStore(key, true); key == "" || !repeated {
				c.applied.Add(1)
			}
		}
		if c.entered != nil && write == 1 {
			close(c.entered)
			<-c.reply
		}
		if c.replayEntered != nil && write == 2 {
			close(c.replayEntered)
			<-c.replayReply
		}
		payload := map[string]string{"calendarId": "work", "idempotencyKey": key, "status": status}
		switch status {
		case "created":
			payload["id"] = "calendar-event-123"
		case "lost":
			return mcp.NewToolResultError("Unclear whether the event was created"), nil
		}
		return &mcp.CallToolResult{StructuredContent: payload, IsError: status == "not_applied"}, nil
	})
	source := server.NewTestStreamableHTTPServer(upstream, server.WithStateLess(true))
	personal := server.NewTestStreamableHTTPServer(calendar, server.WithStateLess(true))
	t.Cleanup(source.Close)
	t.Cleanup(personal.Close)
	c.source, c.calendar = source.URL, personal.URL
	return c
}

type runnerTelegram struct {
	server *httptest.Server
	sends  chan roleTestDelivery
	failTo atomic.Int64
	fail   atomic.Int64
}

func newRunnerTelegram(t *testing.T) *runnerTelegram {
	t.Helper()
	tg := &runnerTelegram{sends: make(chan roleTestDelivery, 32)}
	var ids atomic.Int64
	tg.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/botrole/getMe" {
			fmt.Fprint(w, `{"ok":true,"result":{"id":99,"username":"testbot","is_bot":true}}`)
			return
		}
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode Telegram request: %v", err)
		}
		chatID, _ := strconv.ParseInt(req["chat_id"], 10, 64)
		chatType := "private"
		if chatID < 0 {
			chatType = "supergroup"
		}
		switch r.URL.Path {
		case "/botrole/getChat":
			fmt.Fprintf(w, `{"ok":true,"result":{"id":%d,"type":%q}}`, chatID, chatType)
		case "/botrole/getChatMember":
			fmt.Fprintf(w, `{"ok":true,"result":{"status":"member","user":{"id":%s,"first_name":"A"}}}`, req["user_id"])
		case "/botrole/sendMessage":
			tg.sends <- roleTestDelivery{path: "sendMessage", chatID: chatID, text: req["text"]}
			if code := int(tg.fail.Load()); chatID == tg.failTo.Load() && code != 0 {
				w.WriteHeader(code)
				fmt.Fprintf(w, `{"ok":false,"error_code":%d}`, code)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": tele.Message{
				ID: int(ids.Add(1)), Chat: &tele.Chat{ID: chatID, Type: tele.ChatType(chatType)}, Text: req["text"],
			}})
		default:
			t.Errorf("unexpected Telegram API %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(tg.server.Close)
	return tg
}

type runnerHarness struct {
	t         *testing.T
	cfg       Config
	connector *runnerConnector
	telegram  *runnerTelegram
	now       atomic.Int64
	model     *einoReplyProvider
	decision  atomic.Value
}

func newRunnerHarness(t *testing.T, idempotent bool) *runnerHarness {
	t.Helper()
	h := &runnerHarness{t: t, connector: newRunnerConnector(t, idempotent), telegram: newRunnerTelegram(t)}
	h.now.Store(engagementNow.Unix())
	h.decision.Store("")
	h.model = newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
		if req.Stream || len(req.Tools) != 0 || !strings.Contains(req.Messages[0].Content, "Alder") ||
			!strings.Contains(req.Messages[0].Content, engagementCheckInstruction) ||
			strings.Contains(fmt.Sprint(req.Messages), "calendarId") || !strings.Contains(req.Messages[1].Content, "Confirmed stable release") {
			t.Errorf("unbounded background model request: %+v", req)
		}
		content := h.decision.Load().(string)
		if content == "" {
			content = fmt.Sprintf(`{"matched_ids":["upstream-%d"]}`, h.connector.event.Load())
		}
		_ = json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{
				Role: openai.ChatMessageRoleAssistant, Content: content,
			}}},
			Usage: openai.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10},
		})
	})
	cfg := h.model.snapshot(3).cfg
	dir := t.TempDir()
	cfg.SystemPrompt, cfg.TelegramToken = "You are Alder. Be reserved.", "role"
	cfg.OpenAIBase, cfg.OpenAIKey = h.model.url+"/v1", "local"
	cfg.ToolsEnabled = true
	cfg.ChatDBPath, cfg.UserMCPDBPath = filepath.Join(dir, "chat.db"), filepath.Join(dir, "mcp.db")
	cfg.MCPConfigPath, cfg.StickerRulesPath = filepath.Join(dir, "tools.json"), filepath.Join(dir, "stickers.json")
	cfg.AllowedUsers = map[int64]bool{engagementOwner: true}
	cfg.AllowedGroups = map[int64]bool{engagementGroup: true}
	cfg.quotaResetLocation = time.UTC
	raw := fmt.Sprintf(`{"mcpServers":{"upstream":{"url":%q,"backgroundIdentity":"local-upstream-v1","background":{"event_list":{"operation":"event.list","target":"local/project","arguments":{"repository":"local/project"}}}}}}`, h.connector.source)
	if err := os.WriteFile(cfg.MCPConfigPath, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	h.cfg = cfg
	users, err := NewUserMCPStore(cfg.UserMCPDBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer users.Close()
	personal, ok := TryParseMCPConfig(fmt.Sprintf(`{"mcpServers":{"calendar":{"url":%q,"background":{"insert_event":{"operation":"calendar.create","target":"work","arguments":{"calendarId":"work"},"idempotent":%t}}}}}`, h.connector.calendar, idempotent))
	if !ok {
		t.Fatal("calendar fixture did not parse")
	}
	if _, err := users.AddServers(engagementOwner, personal.MCPServers); err != nil {
		t.Fatal(err)
	}
	if err := users.Close(); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *runnerHarness) clock() time.Time { return time.Unix(h.now.Load(), 0).UTC() }

func (h *runnerHarness) advance(d time.Duration) { h.now.Add(int64(d / time.Second)) }

func (h *runnerHarness) open() (*Bot, engagementLocalPoller) {
	h.t.Helper()
	poller := engagementLocalPoller{ready: make(chan struct{})}
	b, err := newBotWithTelegram(h.cfg, tele.Settings{URL: h.telegram.server.URL, Client: h.telegram.server.Client(), Poller: poller})
	if err != nil {
		h.t.Fatal(err)
	}
	b.engagements.now = h.clock
	h.t.Cleanup(func() {
		closeToolResources(b.mcpClients, b.userTools)
		_ = b.stats.Close()
		_ = b.closeChatDB()
	})
	return b, poller
}

func (h *runnerHarness) seed(b *Bot, chatID int64, write, once bool, recipients ...int64) EngagementRef {
	h.t.Helper()
	create := engagementFixture(chatID)
	a := create.Decision.Agreement
	bindings := b.snapshot().engagementCapabilities(engagementOwner, chatID)
	a.Observe = bindings["public:upstream:event_list"]
	if a.Observe.Operation != engagementEventList {
		h.t.Fatalf("production observer was not enrolled: %+v", bindings)
	}
	a.Once, a.Recipients = once, recipients
	create.Decision.Authorized.Capabilities = []EngagementCapability{a.Observe}
	create.Decision.Authorized.Recipients = recipients
	for _, id := range recipients {
		create.Decision.Authorized.RecipientSources = append(create.Decision.Authorized.RecipientSources,
			EngagementRecipientSource{id, create.Decision.Request.Source})
	}
	if len(recipients) == 2 {
		create.Decision.Request.Text = "Notify this chat and DM me about the stable release."
	}
	if write {
		cap := bindings["personal:calendar:insert_event"]
		if cap.Operation != engagementCalendarCreate {
			h.t.Fatalf("production personal account was not restored: %+v", bindings)
		}
		create.Kind = EngagementDelegation
		a.Actions = []EngagementCapability{cap}
		create.Decision.Authorized.Capabilities = append(create.Decision.Authorized.Capabilities, cap)
	}
	return createEngagement(h.t, b.engagements, create)
}

func waitRunner(t *testing.T, r *TaskRunner) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, busy := r.running.Load("engagement:batch"); !busy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("production engagement tick did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func getRunnerItem(t *testing.T, b *Bot, ref EngagementRef) Engagement {
	t.Helper()
	item, err := b.engagements.Get(ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func getRunnerWork(t *testing.T, b *Bot, ref EngagementRef) []EngagementWork {
	t.Helper()
	work, err := b.engagements.Work(ref.ID, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	return work
}

func TestEngagementRunnerStartupConcurrentAndRestartDelivery(t *testing.T) {
	h := newRunnerHarness(t, false)
	b, poller := h.open()
	ref := h.seed(b, engagementOwner, true, true, engagementOwner)
	h.telegram.failTo.Store(engagementOwner)
	h.telegram.fail.Store(http.StatusTooManyRequests)
	h.connector.entered, h.connector.reply = make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	release := func() { unblock.Do(func() { close(h.connector.reply) }) }
	defer release()
	stopped := make(chan struct{})
	go func() { b.Run(); close(stopped) }()
	roleReceive(t, poller.ready)
	defer func() {
		if stopped != nil {
			b.tg.Stop()
			roleReceive(t, stopped)
		}
	}()
	roleReceive(t, h.connector.entered)
	second := NewTaskRunner(b, b.tasks)
	second.tick(t.Context(), h.clock())
	waitRunner(t, second)
	release()
	delivery := roleReceive(t, h.telegram.sends)
	waitRunner(t, b.scheduler)
	works := getRunnerWork(t, b, ref)
	if len(works) != 1 || len(works[0].Results) != 2 || works[0].Results[0].State != EngagementSucceeded ||
		works[0].Results[1].State != EngagementNoEffect || !getRunnerItem(t, b, ref).LastDeliveredAt.IsZero() {
		t.Fatalf("partial result: %+v", works)
	}
	if delivery.chatID != engagementOwner || !strings.Contains(delivery.text, "added to your calendar") ||
		h.connector.reads.Load() != 1 || h.connector.writes.Load() != 1 {
		t.Fatalf("wrong startup delivery or duplicate calls: %+v", delivery)
	}
	if quota := b.stats.GetQuotaBucket(UsageScopeUser, 0, engagementOwner, h.clock().Format("20060102")); quota.QuotaTokens != 10 {
		t.Fatalf("initiator quota = %+v", quota)
	}
	if quota := b.stats.GetQuotaBucket(UsageScopeChat, engagementOwner, 0, h.clock().Format("20060102")); quota.QuotaTokens != 10 {
		t.Fatalf("execution chat quota = %+v", quota)
	}
	b.tg.Stop()
	roleReceive(t, stopped)
	stopped = nil
	if err := b.stats.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.closeChatDB(); err != nil {
		t.Fatal(err)
	}

	closeToolResources(b.mcpClients, b.userTools)
	h.advance(time.Minute)
	h.telegram.fail.Store(0)
	h.connector.event.Store(0)
	restarted, _ := h.open()
	restarted.scheduler.tick(t.Context(), h.clock())
	waitRunner(t, restarted.scheduler)
	delivery = roleReceive(t, h.telegram.sends)
	after := getRunnerItem(t, restarted, ref)
	works = getRunnerWork(t, restarted, ref)
	if after.EndReason != EngagementCompleted || !after.LastDeliveredAt.Equal(h.clock()) ||
		works[0].Results[0].Receipt != "calendar event calendar-event-123" ||
		works[0].Results[1].State != EngagementSucceeded || works[0].Results[1].Attempt != 2 ||
		delivery.chatID != engagementOwner || h.model.calls.Load() != 1 || h.connector.reads.Load() != 1 || h.connector.writes.Load() != 1 {
		t.Fatalf("restart failed to resume solely delivery: item=%+v work=%+v", after, works)
	}
}

func (h *runnerHarness) tick(b *Bot) {
	h.t.Helper()
	b.scheduler.tick(h.t.Context(), h.clock())
	waitRunner(h.t, b.scheduler)
}

func (h *runnerHarness) restart(b *Bot) *Bot {
	h.t.Helper()
	closeToolResources(b.mcpClients, b.userTools)
	if err := b.stats.Close(); err != nil {
		h.t.Fatal(err)
	}
	if err := b.closeChatDB(); err != nil {
		h.t.Fatal(err)
	}
	restarted, _ := h.open()
	return restarted
}

func TestEngagementRunnerExpiredMatchDoesNotBlockNotice(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(fmt.Sprintf("end_condition=%t", stop), func(t *testing.T) {
			h := newRunnerHarness(t, false)
			h.model = newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
				var input struct {
					Events      []engagementEvent
					EligibleIDs []string
				}
				if err := json.Unmarshal([]byte(req.Messages[1].Content), &input); err != nil || len(input.Events) != 2 {
					t.Errorf("missing full source snapshot: %+v (%v)", input, err)
					http.Error(w, "missing observations", http.StatusBadRequest)
					return
				}
				matches := []string{}
				endID := ""
				for _, event := range input.Events {
					if len(matches) == 0 && (input.EligibleIDs == nil || slices.Contains(input.EligibleIDs, event.ID)) {
						matches = append(matches, event.ID)
					}
					if event.Summary == "The organizer closed the series" {
						endID = event.ID
					}
				}
				roleCompletion(w, dialogueJSON(map[string]any{"matched_ids": matches, "end_id": endID}))
			})
			h.cfg.OpenAIBase = h.model.url + "/v1"
			b, _ := h.open()
			create := engagementFixture(engagementOwner)
			a := create.Decision.Agreement
			a.Observe = b.snapshot().engagementCapabilities(engagementOwner, engagementOwner)["public:upstream:event_list"]
			a.EndCondition = EngagementText{ChatID: engagementOwner, Text: "Stop when the organizer closes the series"}
			create.Decision.Authorized.Capabilities = []EngagementCapability{a.Observe}
			ref := createEngagement(t, b.engagements, create)
			_, err := b.engagements.setConversationPreference(
				engagementRequest(engagementOwner, engagementOwner, 883, "Mute notifications for now.").Source, 0, 0, "test",
				conversationPreferenceSave{Notifications: ConversationOff, NotificationUntil: h.clock().Add(engagementPoll)}, h.clock())
			if err != nil {
				t.Fatal(err)
			}
			first := engagementEvent{ID: "A", Title: "Early release", Start: h.clock().Add(-time.Minute), End: h.clock().Add(engagementPoll)}
			second := engagementEvent{ID: "B", Title: "Later release"}
			h.connector.events.Store(&[]engagementEvent{first, second})
			h.tick(b)
			if getRunnerItem(t, b, ref).OnceChangeKey == "" || len(h.telegram.sends) != 0 {
				t.Fatal("expected an unsent one-shot binding before expiry")
			}
			b = h.restart(b)
			h.advance(engagementPoll)
			if stop {
				first.Summary = "The organizer closed the series"
				h.connector.events.Store(&[]engagementEvent{first, second})
			}
			h.tick(b)
			item := getRunnerItem(t, b, ref)
			if stop {
				if item.EndReason != EngagementConditionEnded || len(h.telegram.sends) != 0 {
					t.Fatal("expired evidence did not end the agreement before delivery")
				}
				return
			}
			if item.EndReason != EngagementCompleted || len(h.telegram.sends) != 1 {
				t.Fatalf("live alternative was skipped: end=%q, sends=%d", item.EndReason, len(h.telegram.sends))
			}
			if send := <-h.telegram.sends; send.chatID != engagementOwner || !strings.Contains(send.text, second.Title) {
				t.Fatalf("wrong delivery: %+v", send)
			}
		})
	}
}

func TestEngagementRunnerRechecksBoundNotice(t *testing.T) {
	for _, tc := range []struct {
		name      string
		group     EngagementResultState
		finalFact string
		wantTitle string
	}{
		{"confirmed_group", EngagementSucceeded, "Confirmed stable release with corrected notes", "Release review 1"},
		{"unknown_group", EngagementUnknown, "Confirmed stable release with corrected notes", "Release review 1"},
		{"no_effect_can_replace", "", "Preview only", "Release review 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRunnerHarness(t, false)
			h.model = newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, call int, req openai.ChatCompletionRequest) {
				var input struct {
					Trigger, BoundID string
					Once             bool
					Events           []engagementEvent
				}
				if len(req.Messages) != 2 || json.Unmarshal([]byte(req.Messages[1].Content), &input) != nil ||
					req.Stream || len(req.Tools) != 0 || input.Trigger != "stable releases" || !input.Once ||
					!strings.Contains(req.Messages[0].Content, "You are Alder.") {
					t.Errorf("invalid background request: %+v", req)
				}
				ids := make([]string, 0, len(input.Events))
				for _, event := range input.Events {
					ids = append(ids, event.ID)
				}
				wantOrder := []string{"upstream-2", "upstream-1"}
				if call == 0 {
					wantOrder = []string{"upstream-1", "upstream-2"}
				}
				if !slices.Equal(ids, wantOrder) {
					t.Errorf("source ordering changed in model input: %v", ids)
				}
				if call == 0 && input.BoundID != "" {
					t.Errorf("initial selection unexpectedly scoped to %q", input.BoundID)
				}
				matches := []string{}
				rejected := false
				if input.BoundID != "" {
					if !strings.Contains(req.Messages[0].Content, "independently of its position or other matches") ||
						!strings.Contains(req.Messages[0].Content, "bound_rejected") {
						t.Error("bound request did not describe independent verification")
					}
					i := slices.Index(ids, input.BoundID)
					if i < 0 || input.BoundID != "upstream-1" {
						t.Errorf("model asked to verify wrong binding: %q in %v", input.BoundID, ids)
					} else {
						fact := input.Events[i].Summary
						if strings.HasPrefix(fact, "Confirmed stable release") {
							matches = append(matches, input.BoundID)
						}
						rejected = fact == "Preview only"
					}
				}
				if input.BoundID == "" || rejected {
					for _, event := range input.Events {
						if strings.HasPrefix(event.Summary, "Confirmed stable release") {
							matches = append(matches, event.ID)
							break
						}
					}
				}
				t.Logf("model input candidates=%v bound=%q, matched=%v rejected=%t", ids, input.BoundID, matches, rejected)
				content := map[string]any{"matched_ids": matches}
				if rejected {
					content["bound_rejected"] = true
				}
				_ = json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
					Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{Content: dialogueJSON(content)}}},
					Usage:   openai.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10},
				})
			})
			h.cfg.OpenAIBase = h.model.url + "/v1"
			b, _ := h.open()
			chatID := engagementOwner
			recipients := []int64{engagementOwner}
			if tc.group != "" {
				chatID = engagementGroup
				recipients = []int64{engagementGroup, engagementOwner}
			}
			ref := h.seed(b, chatID, false, true, recipients...)
			_, err := b.engagements.setConversationPreference(
				engagementRequest(engagementOwner, engagementOwner, 883, "Keep my notifications quiet.").Source, 0, 0, "task7",
				conversationPreferenceSave{Notifications: ConversationOff, NotificationUntil: h.clock().Add(engagementPoll)}, h.clock(),
			)
			if err != nil {
				t.Fatal(err)
			}
			if tc.group == EngagementUnknown {
				h.telegram.failTo.Store(engagementGroup)
				h.telegram.fail.Store(http.StatusBadGateway)
			}
			h.connector.events.Store(&[]engagementEvent{
				{ID: "upstream-1", Title: "Release review 1", Summary: "Confirmed stable release"},
				{ID: "upstream-2", Title: "Release review 2", Summary: "Confirmed stable release"},
			})
			h.tick(b)
			var groupReceipt EngagementResult
			if tc.group != "" {
				first := roleReceive(t, h.telegram.sends)
				groupReceipt = getRunnerWork(t, b, ref)[0].Results[0]
				if first.chatID != engagementGroup || !strings.Contains(first.text, "Release review 1") || groupReceipt.State != tc.group {
					t.Fatalf("wrong initial group delivery: %+v, result=%+v", first, groupReceipt)
				}
			}
			initial := getRunnerItem(t, b, ref)
			if initial.OnceChangeKey == "" || len(h.telegram.sends) != 0 {
				t.Fatalf("initial owner delivery was not held: %+v", initial)
			}

			b = h.restart(b)
			h.telegram.fail.Store(0)
			h.connector.event.Store(-1)
			h.advance(engagementPoll)
			h.tick(b)
			if failed := getRunnerItem(t, b, ref); failed.OnceChangeKey != initial.OnceChangeKey ||
				!failed.LastObservedAt.Equal(initial.LastObservedAt) || len(h.telegram.sends) != 0 || h.model.calls.Load() != 1 {
				t.Fatalf("source failure advanced recovery: %+v", failed)
			}
			h.connector.event.Store(1)
			setEvidence := func(fact string) {
				h.connector.events.Store(&[]engagementEvent{
					{ID: "upstream-2", Title: "Release review 2", Summary: "Confirmed stable release"},
					{ID: "upstream-1", Title: "Release review 1", Summary: fact},
				})
			}
			// Evidence against the old match must not move a sent or uncertain
			// result. Inconclusive evidence must not release an unsent binding.
			if tc.group != "" {
				setEvidence("Preview only")
			} else {
				setEvidence("Status unconfirmed")
			}
			h.advance(engagementPoll)
			h.tick(b)
			if held := getRunnerItem(t, b, ref); held.OnceChangeKey != initial.OnceChangeKey ||
				!held.LastObservedAt.Equal(h.clock()) || !held.LastDeliveredAt.Equal(initial.LastDeliveredAt) || len(h.telegram.sends) != 0 {
				t.Fatalf("old evidence did not protect the binding or delivery: %+v, sends=%d", held, len(h.telegram.sends))
			}

			setEvidence(tc.finalFact)
			h.advance(engagementPoll)
			h.tick(b)
			if len(h.telegram.sends) != 1 {
				t.Fatalf("want one recovery delivery, got %d", len(h.telegram.sends))
			}
			delivery := <-h.telegram.sends
			after := getRunnerItem(t, b, ref)
			if delivery.chatID != engagementOwner || !strings.Contains(delivery.text, tc.wantTitle) ||
				(tc.group != "" && !strings.Contains(delivery.text, tc.finalFact)) || !after.DeliveredTo[engagementOwner].Equal(h.clock()) {
				t.Fatalf("wrong current evidence or recipient: %+v / %+v", delivery, after)
			}
			if tc.group != "" {
				works := getRunnerWork(t, b, ref)
				if len(works) != 1 || len(works[0].Results) != 2 || works[0].Results[0] != groupReceipt ||
					works[0].Results[1].State != EngagementSucceeded || after.OnceChangeKey != initial.OnceChangeKey {
					t.Fatalf("original binding or group attempt changed: %+v / %+v", after, works)
				}
			} else if after.OnceChangeKey == initial.OnceChangeKey {
				t.Fatal("explicitly rejected unsent result did not release its binding")
			}
			if tc.group == EngagementUnknown {
				if after.Status != EngagementActive || after.UncertainSteps != 1 || !after.DeliveredTo[engagementGroup].IsZero() {
					t.Fatalf("unknown attempt counted as confirmed: %+v", after)
				}
			} else if after.EndReason != EngagementCompleted {
				t.Fatalf("settled agreement did not complete: %+v", after)
			}
			b = h.restart(b)
			h.advance(engagementPoll)
			h.tick(b)
			if len(h.telegram.sends) != 0 || h.connector.reads.Load() != 4 || h.model.calls.Load() != 3 {
				t.Fatalf("wrong recovery calls: MCP=%d model=%d Telegram duplicates=%d", h.connector.reads.Load(), h.model.calls.Load(), len(h.telegram.sends))
			}
			date := h.clock().Format("20060102")
			wantOwner := int64(0)
			if chatID == engagementOwner {
				wantOwner = 30
			}
			if chat, owner := b.stats.GetQuotaBucket(UsageScopeChat, chatID, 0, date).QuotaTokens,
				b.stats.GetQuotaBucket(UsageScopeUser, 0, engagementOwner, date).QuotaTokens; chat != 30 || owner != wantOwner {
				t.Fatalf("wrong recovery quota chat=%d owner=%d", chat, owner)
			}
		})
	}
}

func TestEngagementRunnerRecurringRecipientsAndQuiet(t *testing.T) {
	h := newRunnerHarness(t, false)
	b, _ := h.open()
	ref := h.seed(b, engagementGroup, false, false, engagementGroup, engagementOwner)
	h.telegram.failTo.Store(engagementOwner)
	h.telegram.fail.Store(http.StatusTooManyRequests)
	h.tick(b)
	group, private := roleReceive(t, h.telegram.sends), roleReceive(t, h.telegram.sends)
	if group.chatID != engagementGroup || private.chatID != engagementOwner {
		t.Fatalf("unexpected recipients: %+v %+v", group, private)
	}
	h.advance(time.Minute)
	h.telegram.fail.Store(0)
	h.tick(b)
	if retry := roleReceive(t, h.telegram.sends); retry.chatID != engagementOwner {
		t.Fatalf("resent a settled recipient: %+v", retry)
	}
	works := getRunnerWork(t, b, ref)
	if len(works) != 1 || len(works[0].Results) != 2 || works[0].Results[0].Attempt != 1 || works[0].Results[1].Attempt != 2 ||
		getRunnerItem(t, b, ref).Status != EngagementActive || len(h.telegram.sends) != 0 {
		t.Fatalf("recurring fulfillment: %+v", works)
	}
	_, err := b.engagements.setConversationPreference(
		engagementRequest(engagementGroup, engagementOwner, 754, "Mute group notifications").Source, 0, 0, "quiet",
		conversationPreferenceSave{Notifications: ConversationOff, NotificationUntil: h.clock().Add(2 * time.Minute)}, h.clock(),
	)
	if err != nil {
		t.Fatal(err)
	}
	h.connector.event.Store(2)
	h.advance(time.Minute)
	h.tick(b)
	if len(h.telegram.sends) == 0 {
		t.Fatalf("expected a private update, item=%+v, work=%+v, reads=%d", getRunnerItem(t, b, ref), getRunnerWork(t, b, ref), h.connector.reads.Load())
	}
	if private := roleReceive(t, h.telegram.sends); private.chatID != engagementOwner || !strings.Contains(private.text, "Release review 2") {
		t.Fatalf("live preference sent to wrong audience: %+v", private)
	}
	if len(h.telegram.sends) != 0 || len(getRunnerWork(t, b, ref)) != 2 {
		t.Fatal("group was notified while quiet or the change was not recorded")
	}
	h.advance(time.Minute)
	h.tick(b)
	if retry := roleReceive(t, h.telegram.sends); retry.chatID != engagementGroup || !strings.Contains(retry.text, "Release review 2") {
		t.Fatalf("group did not receive pending update after quiet: %+v", retry)
	}
	if len(h.telegram.sends) != 0 || !getRunnerItem(t, b, ref).NextCheck.After(h.clock()) {
		t.Fatal("recurring work failed to advance once per event and recipient")
	}
}

func TestEngagementRunnerUnknownAndDefiniteCalendarOutcomes(t *testing.T) {
	for _, status := range []string{"lost", "not_applied"} {
		t.Run(status, func(t *testing.T) {
			h := newRunnerHarness(t, false)
			b, _ := h.open()
			ref := h.seed(b, engagementOwner, true, true, engagementOwner)
			h.connector.outcome.Store(status)
			h.tick(b)
			work := getRunnerWork(t, b, ref)[0]
			if status == "lost" {
				delivery := roleReceive(t, h.telegram.sends)
				if work.Results[0].State != EngagementUnknown || !work.Results[1].Step.UncertainNotice ||
					!strings.Contains(delivery.text, "couldn't confirm") || !getRunnerItem(t, b, ref).LastDeliveredAt.IsZero() {
					t.Fatalf("unknown write was not preserved and reported separately: %+v %+v", work.Results, delivery)
				}
			} else if work.Results[0].State != EngagementNoEffect || len(h.telegram.sends) != 0 {
				t.Fatalf("known rejection was reported as completion: %+v", work)
			}
			b = h.restart(b)
			if status == "lost" {
				h.connector.event.Store(0)
			}
			h.connector.outcome.Store("created")
			h.advance(time.Minute)
			h.tick(b)
			item := getRunnerItem(t, b, ref)
			if status == "lost" {
				if h.connector.writes.Load() != 1 || h.connector.applied.Load() != 1 || h.connector.reads.Load() != 1 ||
					item.Status != EngagementActive || len(h.telegram.sends) != 0 || getRunnerWork(t, b, ref)[0].Results[0] != work.Results[0] {
					t.Fatalf("unknown write was repeated or completed: %+v", item)
				}
			} else {
				roleReceive(t, h.telegram.sends)
				if h.connector.writes.Load() != 2 || h.connector.applied.Load() != 1 || h.connector.reads.Load() != 2 || item.EndReason != EngagementCompleted {
					t.Fatalf("confirmed non-execution was not recoverable: %+v", item)
				}
			}
		})
	}
}

func TestEngagementRunnerCalendarRevalidation(t *testing.T) {
	for _, scenario := range []string{"updated", "unchanged", "canceled", "elapsed", "ended", "quota", "revoked", "elapsed at write"} {
		t.Run(scenario, func(t *testing.T) {
			h := newRunnerHarness(t, true)
			h.connector.calendarRequests = make(chan map[string]any, 4)
			first := engagementEvent{ID: "A", Title: "Original meeting", Summary: "Confirmed stable release", Start: h.clock().Add(time.Hour), End: h.clock().Add(2 * time.Hour)}
			if scenario == "elapsed at write" {
				first.Start = h.clock().Add(75 * time.Second)
			}
			var checkedBound atomic.Value
			h.model = newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, call int, req openai.ChatCompletionRequest) {
				var input struct {
					Trigger, BoundID string
					Once             bool
					Events           []engagementEvent
					EligibleIDs      *[]string
				}
				if len(req.Messages) != 2 || json.Unmarshal([]byte(req.Messages[1].Content), &input) != nil ||
					!input.Once || input.Trigger != "stable releases" || len(req.Tools) != 0 || !strings.Contains(req.Messages[0].Content, "You are Alder") {
					t.Errorf("unexpected classifier request: %+v", req)
					http.Error(w, "invalid input", http.StatusBadRequest)
					return
				}
				if call == 0 && input.BoundID != "" {
					t.Error("initial source check already bound")
				}
				eligible := func(e engagementEvent) bool {
					return (input.EligibleIDs == nil || slices.Contains(*input.EligibleIDs, e.ID)) && e.Summary == first.Summary
				}
				matches := []string{}
				endID := ""
				for _, e := range input.Events {
					if e.ID == input.BoundID && eligible(e) {
						matches = append(matches, e.ID)
					}
					if e.Summary == "The series ended" {
						endID = e.ID
					}
				}
				rejected := input.BoundID != "" && len(matches) == 0
				if len(matches) == 0 {
					for _, e := range input.Events {
						if eligible(e) {
							matches = append(matches, e.ID)
							break
						}
					}
				}
				checkedBound.Store(input.BoundID)
				if call == 1 && scenario == "elapsed at write" {
					h.advance(15 * time.Second)
				}
				roleCompletion(w, dialogueJSON(map[string]any{"matched_ids": matches, "bound_rejected": rejected, "end_id": endID}))
			})
			h.cfg.OpenAIBase = h.model.url + "/v1"
			b, _ := h.open()
			if scenario == "quota" {
				h.cfg.UserDailyTokenLimit = 20
				if err := b.applyRuntimeConfig(h.cfg); err != nil {
					t.Fatal(err)
				}
			}
			ref := h.seed(b, engagementOwner, true, true, engagementOwner)
			if scenario == "ended" {
				current := *getRunnerItem(t, b, ref).Current
				current.EndCondition = EngagementText{ChatID: engagementOwner, Text: "The series ended"}
				ref = reviseEngagement(t, b.engagements, ref, EngagementDecision{
					Request: engagementRequest(engagementOwner, engagementOwner, 42, "Stop when The series ended"),
					Verdict: EngagementAccept, Change: EngagementAgree, At: h.clock(), Agreement: &current,
					Authorized: EngagementAuthorization{Capabilities: append([]EngagementCapability{current.Observe}, current.Actions...),
						Recipients: current.Recipients, RecipientSources: current.RecipientSources},
				})
			}
			h.connector.events.Store(&[]engagementEvent{first})
			h.connector.outcome.Store("not_applied")
			h.tick(b)
			prior := getRunnerWork(t, b, ref)[0]
			original := roleReceive(t, h.connector.calendarRequests)
			if prior.Results[0].State != EngagementNoEffect || prior.Results[0].Key != original["idempotencyKey"] || len(h.telegram.sends) != 0 {
				t.Fatalf("first operation did not persist a definitive rejection: %+v %v", prior.Results, original)
			}
			b = h.restart(b)
			h.advance(engagementPoll)
			current, next := first, first
			next.ID, next.Title = "B", "Later meeting"
			next.Start, next.End = first.Start.Add(3*time.Hour), first.End.Add(3*time.Hour)
			switch scenario {
			case "updated":
				current.Title, current.Start, current.End = "Rescheduled meeting", next.Start, next.End
			case "canceled":
				current.Summary = "Organizer canceled the meeting"
			case "elapsed", "ended":
				h.advance(time.Hour)
				if scenario == "ended" {
					current.Summary = "The series ended"
				}
			case "quota":
				b.stats.ApplyQuotaDelta(UsageEvent{ChatID: engagementOwner, UserID: engagementOwner, TotalTokens: 20}, h.clock().Format("20060102"), true)
			case "revoked":
				cfg, err := b.userTools.store.Get(engagementOwner)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := b.userTools.AddServers(engagementOwner, cfg.MCPServers); err != nil {
					t.Fatal(err)
				}
			}
			snapshot := []engagementEvent{current}
			if scenario == "updated" {
				snapshot = []engagementEvent{next, current}
			} else if scenario == "elapsed" || scenario == "ended" {
				snapshot = []engagementEvent{current, next}
			}
			h.connector.events.Store(&snapshot)
			h.connector.outcome.Store("created")
			h.tick(b)
			item := getRunnerItem(t, b, ref)
			works := getRunnerWork(t, b, ref)
			if scenario == "quota" {
				if h.connector.reads.Load() != 1 || h.model.calls.Load() != 1 || h.connector.writes.Load() != 1 || len(h.telegram.sends) != 0 || item.Status != EngagementActive {
					t.Fatalf("quota did not defer revalidation: %+v", item)
				}
				h.cfg.UserDailyTokenLimit = 0
				if err := b.applyRuntimeConfig(h.cfg); err != nil {
					t.Fatal(err)
				}
				h.advance(engagementPoll)
				h.tick(b)
				item, works = getRunnerItem(t, b, ref), getRunnerWork(t, b, ref)
			}
			if h.connector.reads.Load() != 2 || h.model.calls.Load() != 2 {
				t.Fatalf("expected to re-read and classify at retry: reads=%d calls=%d", h.connector.reads.Load(), h.model.calls.Load())
			}
			wantBound := first.ID
			if scenario == "elapsed" || scenario == "ended" {
				wantBound = ""
			}
			if got := checkedBound.Load(); got != wantBound {
				t.Fatalf("classifier bound = %q, want %q", got, wantBound)
			}
			switch scenario {
			case "canceled", "ended", "revoked", "elapsed at write":
				if h.connector.writes.Load() != 1 || h.connector.applied.Load() != 0 || len(works) != 1 || works[0].Results[0] != prior.Results[0] {
					t.Fatalf("unsafe retry or changed rejection: %+v", works)
				}
				if scenario == "revoked" {
					if n := roleReceive(t, h.telegram.sends); n.chatID != engagementOwner || !strings.Contains(n.text, "paused") || item.PauseReason != EngagementAccessLost {
						t.Fatalf("wrong response to revoked account: %+v, %+v", item, n)
					}
				} else if len(h.telegram.sends) != 0 || item.PauseReason != "" || (item.EndReason == EngagementConditionEnded) != (scenario == "ended") {
					t.Fatalf("false report or terminal reason: %+v", item)
				}
			default:
				want := current
				if scenario == "elapsed" {
					want = next
					if len(works) != 2 || !slices.ContainsFunc(works, func(w EngagementWork) bool { return w.Superseded && w.Results[0] == prior.Results[0] }) {
						t.Fatalf("lost prior rejected binding: %+v", works)
					}
				}
				request := roleReceive(t, h.connector.calendarRequests)
				notice := roleReceive(t, h.telegram.sends)
				if h.connector.writes.Load() != 2 || h.connector.applied.Load() != 1 || item.EndReason != EngagementCompleted ||
					request["title"] != want.Title || request["start"] != want.Start.Format(time.RFC3339) || request["end"] != want.End.Format(time.RFC3339) ||
					(request["idempotencyKey"] == original["idempotencyKey"]) != (scenario == "unchanged" || scenario == "quota") ||
					notice.chatID != engagementOwner || !strings.Contains(notice.text, want.Title) || !strings.Contains(notice.text, "added to your calendar") {
					t.Fatalf("wrong successful result: %+v %v %+v", item, request, notice)
				}
			}
		})
	}
}

func TestEngagementRunnerReservedAfterCrashUsesPinnedIdempotency(t *testing.T) {
	h := newRunnerHarness(t, true)
	b, _ := h.open()
	ref := h.seed(b, engagementOwner, true, true, engagementOwner)
	h.connector.entered, h.connector.reply = make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	release := func() { unblock.Do(func() { close(h.connector.reply) }) }
	defer release()
	b.scheduler.tick(t.Context(), h.clock())
	roleReceive(t, h.connector.entered)
	before := getRunnerWork(t, b, ref)[0]
	if before.Results[0].State != EngagementReserved || before.Results[0].Key == "" {
		t.Fatalf("external request preceded durable reservation: %+v", before)
	}
	// Make the response impossible to archive in this process. The persisted
	// state at the instant of interruption is reopened by the next worker.
	if err := b.closeChatDB(); err != nil {
		t.Fatal(err)
	}
	release()
	waitRunner(t, b.scheduler)
	closeToolResources(b.mcpClients, b.userTools)
	_ = b.stats.Close()
	h.advance(engagementLease)
	h.connector.event.Store(0)
	restarted, _ := h.open()
	h.tick(restarted)
	roleReceive(t, h.telegram.sends)
	after := getRunnerWork(t, restarted, ref)[0]
	if h.connector.writes.Load() != 2 || h.connector.applied.Load() != 1 || h.connector.reads.Load() != 1 ||
		after.Results[0].State != EngagementSucceeded || after.Results[0].Key != before.Results[0].Key ||
		after.Results[0].Attempt != before.Results[0].Attempt || getRunnerItem(t, restarted, ref).EndReason != EngagementCompleted {
		t.Fatalf("failed to reconcile original attempt: before=%+v after=%+v", before, after)
	}
}

func TestEngagementRunnerHotSwitchArchivesInFlightReceipt(t *testing.T) {
	h := newRunnerHarness(t, false)
	b, _ := h.open()
	ref := h.seed(b, engagementOwner, true, true, engagementOwner)
	h.connector.entered, h.connector.reply = make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	release := func() { unblock.Do(func() { close(h.connector.reply) }) }
	defer release()
	b.scheduler.tick(t.Context(), h.clock())
	roleReceive(t, h.connector.entered)
	h.cfg.ChatDBPath = filepath.Join(t.TempDir(), "replacement.db")
	h.cfg.SystemPrompt = "You are Alder with updated instructions."
	if err := b.applyRuntimeConfig(h.cfg); err != nil {
		t.Fatal(err)
	}
	release()
	waitRunner(t, b.scheduler)
	work := getRunnerWork(t, b, ref)[0]
	if len(work.Results) != 1 || work.Results[0].State != EngagementSucceeded || len(h.telegram.sends) != 0 {
		t.Fatalf("late receipt didn't survive hot switch or retired runtime delivered: %+v", work)
	}
	h.advance(time.Minute)
	h.tick(b)
	roleReceive(t, h.telegram.sends)
	if h.connector.writes.Load() != 1 || h.connector.reads.Load() != 1 || getRunnerItem(t, b, ref).EndReason != EngagementCompleted {
		t.Fatal("hot switch repeated fulfilled work")
	}
}

func TestEngagementRunnerBlockUnauthorizedScope(t *testing.T) {
	for _, scenario := range []string{"legacy route", "paused", "stopped", "expired", "quota", "replaced account", "recipient revoked"} {
		t.Run(scenario, func(t *testing.T) {
			h := newRunnerHarness(t, false)
			b, _ := h.open()
			var ref EngagementRef
			switch scenario {
			case "legacy route":
				old := engagementFixture(engagementGroup)
				cap := b.snapshot().engagementCapabilities(engagementOwner, engagementGroup)["public:upstream:event_list"]
				old.Decision.Agreement.Observe = cap
				old.Decision.Authorized.Capabilities = []EngagementCapability{cap}
				old.Decision.Request.Text = "Send the update to group -702 when the stable release arrives."
				ref = createEngagement(t, b.engagements, old)
				b = h.restart(b)
			case "paused", "stopped":
				ref = h.seed(b, engagementOwner, true, true, engagementOwner)
				change := EngagementPause
				if scenario == "stopped" {
					change = EngagementEnd
				}
				ref = reviseEngagement(t, b.engagements, ref, EngagementDecision{
					Request: engagementRequest(engagementOwner, engagementOwner, 102, scenario),
					Verdict: EngagementAccept, Change: change, At: h.clock(),
				})
			case "expired":
				create := engagementFixture(engagementOwner)
				cap := b.snapshot().engagementCapabilities(engagementOwner, engagementOwner)["public:upstream:event_list"]
				create.Decision.Agreement.Observe = cap
				create.Decision.Agreement.ExpiresAt = h.clock().Add(time.Minute)
				create.Decision.Authorized.Capabilities = []EngagementCapability{cap}
				ref = createEngagement(t, b.engagements, create)
				h.advance(time.Minute)
			case "quota":
				h.cfg.UserDailyTokenLimit = 10
				if err := b.applyRuntimeConfig(h.cfg); err != nil {
					t.Fatal(err)
				}
				ref = h.seed(b, engagementOwner, true, true, engagementOwner)
				b.stats.ApplyQuotaDelta(UsageEvent{ChatID: engagementOwner, UserID: engagementOwner, TotalTokens: 10}, h.clock().Format("20060102"), true)
			case "replaced account":
				ref = h.seed(b, engagementOwner, true, true, engagementOwner)
				// Importing the same name and even the same endpoint creates
				// a new account binding; the old agreement cannot use it.
				cfg, err := b.userTools.store.Get(engagementOwner)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := b.userTools.AddServers(engagementOwner, cfg.MCPServers); err != nil {
					t.Fatal(err)
				}
			case "recipient revoked":
				ref = h.seed(b, engagementGroup, false, true, engagementGroup, engagementOwner)
				h.telegram.failTo.Store(engagementGroup)
				h.telegram.fail.Store(http.StatusTooManyRequests)
				h.tick(b)
				roleReceive(t, h.telegram.sends)
				roleReceive(t, h.telegram.sends)
				h.telegram.fail.Store(0)
				h.cfg.AllowedGroups = map[int64]bool{-702: true}
				if err := b.applyRuntimeConfig(h.cfg); err != nil {
					t.Fatal(err)
				}
				h.advance(time.Minute)
			}
			h.tick(b)
			item := getRunnerItem(t, b, ref)
			if h.connector.writes.Load() != 0 {
				t.Fatal("unauthorized calendar action executed")
			}
			switch scenario {
			case "legacy route":
				if item.Status != EngagementPaused || item.PauseReason != EngagementRouteUnverified || h.connector.reads.Load() != 0 || len(h.telegram.sends) != 0 {
					t.Fatalf("legacy ambiguity was not quarantined: %+v", item)
				}
			case "paused":
				if item.Status != EngagementPaused || h.connector.reads.Load() != 0 || len(h.telegram.sends) != 0 {
					t.Fatalf("paused item ran: %+v", item)
				}
			case "stopped", "expired":
				want := EngagementStopped
				if scenario == "expired" {
					want = EngagementExpired
				}
				if item.EndReason != want || h.connector.reads.Load() != 0 || len(h.telegram.sends) != 0 {
					t.Fatalf("terminal work ran: %+v", item)
				}
			case "quota":
				if item.Status != EngagementActive || !item.NextCheck.After(h.clock()) || h.connector.reads.Load() != 0 ||
					h.model.calls.Load() != 0 || len(h.telegram.sends) != 0 {
					t.Fatalf("quota should defer without invoking the model: %+v", item)
				}
			case "replaced account":
				note := roleReceive(t, h.telegram.sends)
				if item.Status != EngagementPaused || item.PauseReason != EngagementAccessLost || note.chatID != engagementOwner ||
					!strings.Contains(note.text, "paused") || len(item.PauseNotices) != 1 ||
					item.PauseNotices[0].State != EngagementSucceeded {
					t.Fatalf("account mismatch was not paused and communicated: %+v %+v", item, note)
				}
			case "recipient revoked":
				work := getRunnerWork(t, b, ref)[0]
				if item.EndReason == EngagementCompleted || len(h.telegram.sends) != 0 ||
					work.Results[0].State != EngagementNoEffect || work.Results[0].Attempt != 1 ||
					work.Results[1].State != EngagementSucceeded {
					t.Fatalf("revoked recipient received a retry: %+v", work)
				}
			}
		})
	}
}

func TestEngagementRunnerUnknownTelegramDoesNotResend(t *testing.T) {
	h := newRunnerHarness(t, false)
	b, _ := h.open()
	ref := h.seed(b, engagementOwner, false, true, engagementOwner)
	h.telegram.failTo.Store(engagementOwner)
	h.telegram.fail.Store(http.StatusBadGateway)
	h.tick(b)
	roleReceive(t, h.telegram.sends)
	b = h.restart(b)
	h.advance(time.Minute)
	h.telegram.fail.Store(0)
	h.tick(b)
	work := getRunnerWork(t, b, ref)[0]
	if len(h.telegram.sends) != 0 || work.Results[0].State != EngagementUnknown || work.Results[0].Attempt != 1 ||
		getRunnerItem(t, b, ref).EndReason == EngagementCompleted {
		t.Fatalf("unknown Telegram send was repeated or settled: %+v", work.Results)
	}
}

func TestLegacyScheduledDeliveryReminderIgnoresSocialMute(t *testing.T) {
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ openai.ChatCompletionRequest) {
		engagementModelText(w, "Remember to file the report.")
	})
	b, sent := newRoleTestBot(t, model)
	b.tasks, b.reminders, b.engagements = NewTaskStore(b.chatDB), NewReminderStore(b.chatDB), NewEngagementStore(b.chatDB)
	b.engagements.now = func() time.Time { return engagementNow }
	_, err := b.engagements.setConversationPreference(
		engagementRequest(engagementGroup, engagementOwner, 750, "Quiet please").Source, 0, 0, "quiet",
		conversationPreferenceSave{Chatter: ConversationOff, Notifications: ConversationOff}, engagementNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	rem := Reminder{ID: "report", UserID: engagementOwner, ChatID: engagementGroup, Prompt: "File the report", FireAt: engagementNow, Timezone: "UTC"}
	b.reminders.Insert(rem)
	runner := NewTaskRunner(b, b.tasks)
	runner.tick(t.Context(), engagementNow)
	if got := roleReceive(t, sent); got.chatID != engagementGroup || got.text != "Remember to file the report." {
		t.Fatalf("explicit reminder suppressed by group notification preference: %+v", got)
	}
	waitEinoReplyHistory(t, b, engagementGroup, 2)
	waitRunner(t, runner)
}

func TestEngagementRunnerEndConditionRequiresObservedEvent(t *testing.T) {
	h := newRunnerHarness(t, false)
	b, _ := h.open()
	create := engagementFixture(engagementOwner)
	a := create.Decision.Agreement
	a.Observe = b.snapshot().engagementCapabilities(engagementOwner, engagementOwner)["public:upstream:event_list"]
	a.Once = false
	a.EndCondition = EngagementText{ChatID: engagementOwner, Text: "The last release is published"}
	create.Decision.Authorized.Capabilities = []EngagementCapability{a.Observe}
	ref := createEngagement(t, b.engagements, create)
	h.decision.Store(`{"matched_ids":["model-invented-id"],"end_id":"model-invented-id"}`)
	h.tick(b)
	item := getRunnerItem(t, b, ref)
	if item.Status != EngagementActive || !item.LastObservedAt.IsZero() || len(getRunnerWork(t, b, ref)) != 0 ||
		len(h.telegram.sends) != 0 {
		t.Fatalf("model-created evidence reached fulfillment: %+v", item)
	}
	h.advance(engagementPoll)
	h.decision.Store(`{"matched_ids":[],"end_id":"upstream-1"}`)
	h.tick(b)
	item = getRunnerItem(t, b, ref)
	if item.Status != EngagementEnded || item.EndReason != EngagementConditionEnded ||
		item.LastObservedAt.IsZero() || len(h.telegram.sends) != 0 {
		t.Fatalf("verified end condition failed to settle: %+v", item)
	}
}

func TestEngagementRunnerItemMuteDoesNotSuppressAction(t *testing.T) {
	h := newRunnerHarness(t, false)
	b, _ := h.open()
	ref := h.seed(b, engagementOwner, true, true, engagementOwner)
	ref = reviseEngagement(t, b.engagements, ref, EngagementDecision{
		Request: engagementRequest(engagementOwner, engagementOwner, 745, "Quiet this item for now"),
		Verdict: EngagementAccept, Change: EngagementMute, MuteUntil: h.clock().Add(engagementPoll), At: h.clock(),
	})
	h.tick(b)
	work := getRunnerWork(t, b, ref)[0]
	if len(work.Results) != 1 || work.Results[0].State != EngagementSucceeded || len(h.telegram.sends) != 0 ||
		getRunnerItem(t, b, ref).Status != EngagementActive {
		t.Fatalf("quiet halted the action or confirmed delivery: %+v", work)
	}
	h.advance(engagementPoll)
	h.tick(b)
	roleReceive(t, h.telegram.sends)
	if h.connector.writes.Load() != 1 || h.connector.reads.Load() != 1 ||
		getRunnerItem(t, b, ref).EndReason != EngagementCompleted {
		t.Fatal("quiet recovery did more than deliver the pending result")
	}
}
