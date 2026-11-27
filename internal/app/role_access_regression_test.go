package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	openai "github.com/sashabaranov/go-openai"
	tele "gopkg.in/telebot.v3"
)

const regressionGroup = -100123

func seedRoleFact(t *testing.T, b *Bot, fact string) MemoryRef {
	t.Helper()
	ref, err := b.memory.Learn(MemoryLearn{Subjects: []int64{42}, Text: fact,
		Evidence: MemoryEvidence{Kind: MemoryStatement, Source: MemorySource{ChatID: 42, SenderID: 42, MessageID: 99}}})
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func dispatchRoleText(b *Bot, chat, sender int64, id int, text string) {
	b.tg.ProcessUpdate(tele.Update{Message: roleMessage(chat, sender, id, text)})
}

func TestRoleAccessWithdrawStopsInFlightGroupReply(t *testing.T) {
	const fact = "my nickname is Pine"
	toolStarted, release := make(chan struct{}), make(chan struct{})
	var laterToolCalls atomic.Int32
	requests := make(chan openai.ChatCompletionRequest, 6)
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, round int, req openai.ChatCompletionRequest) {
		requests <- req
		if round == 0 {
			einoModelSSE(w, einoReplyToolFrame("call-one", "pause", 0), einoReplyToolFrame("call-two", "after_pause", 1))
			einoReplyFinish(w, "tool_calls")
			return
		}
		einoModelSSE(w, einoReplyTextFrame(fact))
		einoReplyFinish(w, "stop")
	})
	b, deliveries := newRoleTestBot(t, model)
	b.cfg.ProfileEnabled = false
	b.tools = NewToolRegistry()
	pause := einoTestMCPTool("pause", "pause this reply", "")
	pause.run = func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		close(toolStarted)
		select {
		case <-release:
		case <-ctx.Done():
		case <-t.Context().Done():
		}
		return mcp.NewToolResultText("completed"), nil
	}
	later := einoTestMCPTool("after_pause", "run after the first lookup", "")
	later.run = func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		laterToolCalls.Add(1)
		return mcp.NewToolResultText("second tool completed"), nil
	}
	addEinoTestMCPServer(t, b.tools, "global", pause, later)
	seedRoleFact(t, b, fact)
	b.registerHandlers()
	dispatchRoleText(b, 42, 42, 1, "/memory_share -100123 "+fact)
	roleReceive(t, deliveries)
	dispatchRoleText(b, regressionGroup, 43, 2, "@testbot discuss nicknames")
	if req := roleReceive(t, requests); !strings.Contains(fmt.Sprint(req.Messages), fact) {
		t.Fatal("first model request missed the approved conclusion")
	}
	roleReceive(t, toolStarted)
	dispatchRoleText(b, 42, 42, 3, "/memory_unshare -100123 "+fact)
	if got := roleReceive(t, deliveries); !strings.Contains(got.text, "withdrawn") {
		t.Fatalf("withdrawal = %+v", got)
	}
	close(release)
	if got := roleReceive(t, deliveries); got.path != "/botrole/deleteMessage" {
		t.Errorf("stale reply was delivered: %+v", got)
	}
	if laterToolCalls.Load() != 0 {
		t.Error("a later call in the same tool batch started after withdrawal")
	}
	select {
	case req := <-requests:
		t.Errorf("model request after withdrawal: %+v", req.Messages)
	default:
	}
}

func TestRoleAccessSharesOnlyTheApprovedClause(t *testing.T) {
	const public, reason = "Call me Pine", "because the witness lives at Secret Place"
	requests := make(chan openai.ChatCompletionRequest, 2)
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
		requests <- req
		einoModelSSE(w, einoReplyTextFrame("done"))
		einoReplyFinish(w, "stop")
	})
	b, deliveries := newRoleTestBot(t, model)
	b.cfg.ProfileEnabled = false
	seedRoleFact(t, b, public+" "+reason)
	b.registerHandlers()
	dispatchRoleText(b, 42, 42, 1, "/memory_share -100123 "+public)
	if got := roleReceive(t, deliveries); !strings.Contains(got.text, "can now be used") {
		t.Errorf("clause approval failed: %+v", got)
	}
	dispatchRoleText(b, regressionGroup, 43, 2, "@testbot what is the nickname?")
	got := fmt.Sprint(roleReceive(t, requests).Messages)
	if !strings.Contains(got, public) || strings.Contains(got, reason) {
		t.Errorf("group prompt did not contain only the approved clause: %s", got)
	}
	roleReceive(t, deliveries)
	waitEinoReplyHistory(t, b, regressionGroup, 2)
}

func TestRoleAccessReflectionCannotLaunderPersonalToolHistory(t *testing.T) {
	const secret = "private invoice value 818358"
	reflections := make(chan string, 4)
	var replies atomic.Int32
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
		material := fmt.Sprint(req.Messages)
		if !req.Stream {
			reflections <- material
			if strings.Contains(material, secret) {
				roleCompletion(w, `{"kind":"statement","conclusion":"private invoice value 818358"}`)
			} else {
				roleCompletion(w, `{}`)
			}
			return
		}
		switch replies.Add(1) {
		case 1:
			einoModelSSE(w, einoReplyToolFrame("invoice", "invoice", 0))
			einoReplyFinish(w, "tool_calls")
			return
		case 2, 3:
			if !strings.Contains(material, secret) {
				t.Error("private reply lost the connector result from tool or chat history")
			}
			einoModelSSE(w, einoReplyTextFrame(secret))
		default:
			if strings.Contains(material, secret) {
				t.Errorf("group model received a restricted fact: %s", material)
			}
			einoModelSSE(w, einoReplyTextFrame("no approved invoice data"))
		}
		einoReplyFinish(w, "stop")
	})
	b, deliveries := newRoleTestBot(t, model)
	b.tools = NewToolRegistry()
	b.userTools = NewUserToolManager(nil, nil, nil)
	personal := NewToolRegistry()
	invoice := einoTestMCPTool("invoice", "retrieve an invoice", "")
	invoice.run = func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText(secret), nil
	}
	addEinoTestMCPServer(t, personal, "billing", invoice)
	b.userTools.registries[42] = personal
	b.registerHandlers()
	for i, statement := range []string{"look up my invoice", "remember the result from earlier"} {
		dispatchRoleText(b, 42, 42, i+1, statement)
		roleReceive(t, deliveries)
		if input := roleReceive(t, reflections); strings.Contains(input, secret) {
			t.Errorf("reflection %d got a private tool-derived fact: %s", i+1, input)
		}
		waitEinoReplyHistory(t, b, 42, 2*(i+1))
		waitRoleReviewsIdle(t, b)
	}
	entries, err := b.memory.List("", 20)
	if err != nil || len(entries) != 0 {
		t.Errorf("reflection persisted a restricted fact: %+v (%v)", entries, err)
	}
	dispatchRoleText(b, 42, 42, 3, "/memory_share -100123 "+secret)
	if got := roleReceive(t, deliveries); strings.Contains(got.text, "can now be used") {
		t.Errorf("unrestricted sharing was granted: %+v", got)
	}
	dispatchRoleText(b, regressionGroup, 42, 4, "@testbot what is my invoice value?")
	roleReceive(t, deliveries)
	roleReceive(t, reflections)
	waitRoleReviewsIdle(t, b)
}

func waitRoleReviewsIdle(t *testing.T, b *Bot) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for b.memoryReviews.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if b.memoryReviews.Load() != 0 {
		t.Fatal("reflection did not finish")
	}
}

func TestRoleAccessContextScheduleRechecksAudience(t *testing.T) {
	const fact = "Pine has an approved group nickname"
	requests := make(chan openai.ChatCompletionRequest, 6)
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
		requests <- req
		einoModelSSE(w, einoReplyTextFrame("scheduled reply"))
		einoReplyFinish(w, "stop")
	})
	b, deliveries := newRoleTestBot(t, model)
	ref := seedRoleFact(t, b, fact)
	if _, err := b.memory.Share(ref, 42, regressionGroup, fact); err != nil {
		t.Fatal(err)
	}
	task := ScheduledTask{ID: "old", Prompt: "check nickname", Context: true, CronExpr: "0 8 * * *", Timezone: "UTC", Enabled: true, CreatedBy: 42}
	tasks := NewTaskStore(nil)
	tasks.Upsert(regressionGroup, task)
	runner := NewTaskRunner(b, tasks)
	runner.execute(regressionGroup, task)
	if req := roleReceive(t, requests); !strings.Contains(fmt.Sprint(req.Messages), fact) {
		t.Error("valid old schedule lost its group memory")
	}
	roleReceive(t, deliveries)
	waitEinoReplyHistory(t, b, regressionGroup, 2)
	cfg := b.currentConfig()
	cfg.AllowedGroups = map[int64]bool{-100999: true}
	cfg.AllowedUsers = map[int64]bool{42: true} // The old creator keeps private access.
	if err := b.applyRuntimeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	runner.execute(regressionGroup, tasks.List(regressionGroup)[0])
	if got := tasks.List(regressionGroup); len(got) != 1 || got[0].LastError == "" {
		t.Errorf("denied context schedule was marked successful: %+v", got)
	}
	select {
	case req := <-requests:
		t.Errorf("schedule issued a new request after access change: %+v", req.Messages)
	default:
	}
}

func TestRoleAccessGroupToolGrantAndSenderBinding(t *testing.T) {
	const secret = "approved invoice 7"
	requests := make(chan openai.ChatCompletionRequest, 10)
	var mode atomic.Int32
	var toolCalls atomic.Int32
	begun, release := make(chan struct{}), make(chan struct{})
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
		requests <- req
		if mode.Load() > 0 && len(einoReplyToolNames(req)) > 0 && !strings.Contains(fmt.Sprint(req.Messages), "call-invoice") {
			frame := einoReplyToolFrame("call-invoice", "invoice", 0)
			if mode.Load() >= 2 {
				frame = strings.Replace(frame, `"arguments":"{}"`, `"arguments":"{\"id\":7}"`, 1)
			}
			einoModelSSE(w, frame)
			einoReplyFinish(w, "tool_calls")
			return
		}
		reply := "response"
		if strings.Contains(fmt.Sprint(req.Messages), secret) {
			reply = secret
		}
		einoModelSSE(w, einoReplyTextFrame(reply))
		einoReplyFinish(w, "stop")
	})
	b, deliveries := newRoleTestBot(t, model)
	b.cfg.ProfileEnabled = false
	b.tools = NewToolRegistry()
	b.userTools = NewUserToolManager(nil, nil, nil)
	personal := NewToolRegistry()
	invoice := einoTestMCPTool("invoice", "look up a selected invoice", "")
	invoice.run = func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		toolCalls.Add(1)
		if mode.Load() == 3 {
			close(begun)
			select {
			case <-release:
			case <-ctx.Done():
			case <-t.Context().Done():
			}
		}
		return mcp.NewToolResultText(secret), nil
	}
	addEinoTestMCPServer(t, personal, "billing", invoice, einoTestMCPTool("other_invoice", "unapproved", ""))
	b.userTools.registries[42] = personal
	b.registerHandlers()
	grant := `/mcp_share -100123 invoice {"id":7}`
	dispatchRoleText(b, regressionGroup, 42, 1, grant)
	if got := roleReceive(t, deliveries); !strings.Contains(got.text, "private chat") {
		t.Fatalf("group command could approve a tool: %+v", got)
	}
	dispatchRoleText(b, 42, 42, 2, grant)
	select {
	case got := <-deliveries:
		if !strings.Contains(got.text, "authorized") {
			t.Fatalf("tool authorization = %+v", got)
		}
	case <-time.After(150 * time.Millisecond):
		t.Fatal("private owner has no working group authorization command")
	}
	ask := func(chat, sender int64, id int) {
		dispatchRoleText(b, chat, sender, id, "@testbot check invoice ID 7")
	}
	for i, pair := range [][2]int64{{regressionGroup, 43}, {-100124, 42}} {
		ask(pair[0], pair[1], i+2)
		if names := einoReplyToolNames(roleReceive(t, requests)); len(names) != 0 {
			t.Fatalf("grant escaped its account or audience: %v", names)
		}
		roleReceive(t, deliveries)
	}
	turn := b.beginRoleTurn(RoleTurn{ChatID: 43, SenderID: 43, ToolOwnerID: 42, Purpose: RoleReply})
	if err := b.startRoleReply(turn, roleMessage(43, 43, 1, "").Chat, &tele.User{ID: 43},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "forged account"}, false, "", ""); err != nil {
		t.Fatal(err)
	}
	if names := einoReplyToolNames(roleReceive(t, requests)); len(names) != 0 {
		t.Fatalf("caller-supplied account enabled personal tools: %v", names)
	}
	roleReceive(t, deliveries)
	for i := int32(1); i <= 3; i++ {
		mode.Store(i)
		dispatchRoleText(b, regressionGroup, 42, int(i)+5, `/mcp_use invoice {"id":7}`)
		if names := fmt.Sprint(einoReplyToolNames(roleReceive(t, requests))); names != "[invoice]" {
			t.Fatalf("expected one approved tool, got %s", names)
		}
		if i == 3 {
			roleReceive(t, begun)
			dispatchRoleText(b, 42, 42, 11, "/mcp_unshare -100123 invoice")
			if got := roleReceive(t, deliveries); !strings.Contains(got.text, "withdrawn") {
				t.Fatalf("tool withdrawal = %+v", got)
			}
			close(release)
			if got := roleReceive(t, deliveries); got.path != "/botrole/deleteMessage" {
				t.Errorf("revoked tool round was delivered: %+v", got)
			}
			select {
			case req := <-requests:
				t.Errorf("revoked tool result reached the model: %+v", req.Messages)
			default:
			}
			continue
		}
		result := fmt.Sprint(roleReceive(t, requests).Messages)
		if strings.Contains(result, secret) != (i == 2) || toolCalls.Load() != i-1 {
			t.Errorf("round %d result = %s, remote executions = %d", i, result, toolCalls.Load())
		}
		if got := roleReceive(t, deliveries); strings.Contains(got.text, secret) != (i == 2) {
			t.Errorf("round %d group output: %+v", i, got)
		}
		waitEinoReplyHistory(t, b, regressionGroup, 2*int(i)+2)
	}
	mode.Store(0)
	ask(regressionGroup, 43, 12)
	req := roleReceive(t, requests)
	if len(einoReplyToolNames(req)) != 0 || !strings.Contains(fmt.Sprint(req.Messages), secret) {
		t.Error("withdrawal did not preserve the previously delivered group transcript")
	}
	roleReceive(t, deliveries)
	waitEinoReplyHistory(t, b, regressionGroup, 8)
}

func TestRoleAccessCorrectionRechecksConcurrentLearning(t *testing.T) {
	const original, earlier, correction = "I prefer tea", "I prefer black tea", "I prefer green tea"
	oldStarted, newStarted := make(chan struct{}), make(chan struct{})
	oldRelease, newRelease := make(chan struct{}), make(chan struct{})
	var correcting atomic.Int32
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, r *http.Request, _ int, req openai.ChatCompletionRequest) {
		if req.Stream {
			einoModelSSE(w, einoReplyTextFrame("understood"))
			einoReplyFinish(w, "stop")
			return
		}
		material := req.Messages[len(req.Messages)-1].Content
		if strings.Contains(material, `"latest_sender_message":"The update`) {
			close(oldStarted)
			select {
			case <-oldRelease:
			case <-r.Context().Done():
			}
			roleCompletion(w, `{"kind":"statement","conclusion":"`+earlier+`","replace":1}`)
			return
		}
		if correcting.Add(1) == 1 {
			close(newStarted)
			select {
			case <-newRelease:
			case <-r.Context().Done():
			}
		} else if !strings.Contains(material, earlier) {
			t.Errorf("correction retry did not re-read current candidate: %s", material)
		}
		roleCompletion(w, `{"kind":"correction","conclusion":"`+correction+`","replace":1}`)
	})
	b, deliveries := newRoleTestBot(t, model)
	ref := seedRoleFact(t, b, original)
	roleHandle(t, b, b.handleText, roleMessage(42, 42, 101, "The update is I prefer black tea"))
	roleReceive(t, deliveries)
	roleReceive(t, oldStarted)
	roleHandle(t, b, b.handleText, roleMessage(42, 42, 102, "Correction: I prefer green tea now"))
	roleReceive(t, deliveries)
	roleReceive(t, newStarted)
	close(oldRelease)
	waitRoleMemory(t, b, earlier)
	close(newRelease)
	waitRoleReviewsIdle(t, b)
	entry, err := b.memory.Get(ref.ID)
	if err != nil || entry.Text != correction || len(entry.Evidence) != 3 {
		t.Fatalf("latest correction was lost: %+v (%v)", entry, err)
	}
}

func TestRoleAccessPlaceholderPrecedesClearConfirmation(t *testing.T) {
	firstSend, release, accepted := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var hold atomic.Bool
	hold.Store(true)
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, r *http.Request, _ int, req openai.ChatCompletionRequest) {
		input := req.Messages[len(req.Messages)-1].Content
		if strings.HasSuffix(input, "old turn") {
			select {
			case <-r.Context().Done():
			case <-t.Context().Done():
			}
			return
		}
		einoModelSSE(w, einoReplyTextFrame(strings.TrimSpace(input[strings.LastIndex(input, "\n")+1:])))
		einoReplyFinish(w, "stop")
	})
	b, deliveries := newRoleTestBotWithTelegram(t, model, func(w http.ResponseWriter, r *http.Request, d roleTestDelivery) bool {
		if d.path == "/botrole/sendMessage" && d.text == replyPlaceholderText && d.chatID == 42 && hold.Swap(false) {
			close(firstSend)
			select {
			case <-release:
			case <-r.Context().Done():
			case <-t.Context().Done():
			}
			close(accepted)
		}
		if d.path == "/botrole/deleteMessage" {
			fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"simulated delete failure"}`)
			return true
		}
		return false
	})
	b.cfg.ProfileEnabled = false
	b.registerHandlers()
	oldDone, clearDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(oldDone)
		dispatchRoleText(b, 42, 42, 1, "old turn")
	}()
	roleReceive(t, firstSend)
	go func() {
		defer close(clearDone)
		dispatchRoleText(b, 42, 42, 2, "/clear")
	}()
	dispatchRoleText(b, 43, 43, 1, "other chat proceeds")
	if d := roleReceive(t, deliveries); d.chatID != 43 || d.text != "other chat proceeds" {
		t.Errorf("the first send affected another chat or let clear overtake it: %+v", d)
	}
	select {
	case d := <-deliveries:
		t.Errorf("Telegram confirmed clear before completing the first send: %+v", d)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	roleReceive(t, accepted)
	roleReceive(t, oldDone)
	roleReceive(t, clearDone)
	if d := roleReceive(t, deliveries); d.chatID != 42 || !strings.Contains(d.text, "cleared") {
		t.Fatalf("expected the same-chat clear confirmation after the old send: %+v", d)
	}
	dispatchRoleText(b, 42, 42, 3, "new turn")
	if d := roleReceive(t, deliveries); d.path != "/botrole/editMessageText" || d.text != "new turn" {
		t.Fatalf("new turn failed after stale placeholder deletion failed: %+v", d)
	}
	waitEinoReplyHistory(t, b, 42, 2)
}

func TestRoleAccessGroupToolLazyDiscovery(t *testing.T) {
	const result = "the authorized ledger result"
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, round int, req openai.ChatCompletionRequest) {
		input := fmt.Sprint(req.Messages)
		if strings.Contains(input, "private-server") || strings.Contains(input, "private schema wording") ||
			strings.Contains(input, "unapproved_ledger") {
			t.Errorf("group tool discovery exposed private server metadata: %s", input)
		}
		switch round {
		case 0:
			assertReplyMCPNames(t, req, "get_tool_details")
			if strings.Count(input, "**personal_ledger**") != 2 {
				t.Errorf("no discoverable scoped tool in request: %s", input)
			}
			frame := einoReplyToolFrame("discover", "get_tool_details", 0)
			einoModelSSE(w, strings.Replace(frame, `"arguments":"{}"`, `"arguments":"{\"groups\":[\"personal_ledger\"]}"`, 1))
			einoReplyFinish(w, "tool_calls")
		case 1:
			assertReplyMCPNames(t, req, "get_tool_details", "public_rates", "ledger")
			if strings.Contains(fmt.Sprint(req.Tools), "private schema wording") {
				t.Error("scope leaked the private tool description")
			}
			einoModelSSE(w, einoReplyToolFrame("ledger", "ledger", 0), einoReplyToolFrame("rates", "public_rates", 1))
			einoReplyFinish(w, "tool_calls")
		case 2:
			if !strings.Contains(input, result) || !strings.Contains(input, "public exchange rates") {
				t.Error("approved result did not reach the next model round")
			}
			einoModelSSE(w, einoReplyTextFrame(result))
			einoReplyFinish(w, "stop")
		default:
			t.Errorf("unexpected model round %d", round)
			http.Error(w, "unexpected model round", http.StatusBadRequest)
		}
	})
	b, deliveries := newRoleTestBot(t, model)
	b.cfg.ProfileEnabled, b.cfg.ToolsSummaryEnabled = false, true
	b.toolSummaryStore = NewMCPSummaryStore(b.chatDB.db)
	b.tools = NewToolRegistry()
	addEinoTestMCPServer(t, b.tools, "personal_ledger", einoTestMCPTool("public_rates", "public exchange rates", ""))
	b.userTools = NewUserToolManager(nil, nil, nil)
	personal := NewToolRegistry()
	ledger := einoTestMCPTool("ledger", "private schema wording", "")
	ledger.run = func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText(result), nil
	}
	addEinoTestMCPServer(t, personal, "private-server", ledger, einoTestMCPTool("unapproved_ledger", "other data", ""))
	b.userTools.registries[42] = personal
	b.registerHandlers()
	dispatchRoleText(b, 42, 42, 1, "/mcp_share -100123 ledger {}")
	roleReceive(t, deliveries)
	dispatchRoleText(b, regressionGroup, 42, 2, "/mcp_use ledger {}")
	if got := roleReceive(t, deliveries); got.text != result || got.chatID != regressionGroup {
		t.Errorf("scoped lookup did not reach the authorized group: %+v", got)
	}
	waitEinoReplyHistory(t, b, regressionGroup, 2)
}

func TestRoleAccessAdminRevisionBeforeFallbackEdit(t *testing.T) {
	const secret = "sensitive local conclusion"
	editStarted, releaseEdit := make(chan struct{}), make(chan struct{})
	var edits atomic.Int32
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ openai.ChatCompletionRequest) {
		einoModelSSE(w, einoReplyTextFrame(secret))
		einoReplyFinish(w, "stop")
	})
	b, deliveries := newRoleTestBotWithTelegram(t, model, func(w http.ResponseWriter, r *http.Request, d roleTestDelivery) bool {
		if d.path != "/botrole/editMessageText" || d.text != secret {
			return false
		}
		if edits.Add(1) != 1 {
			t.Error("the stale reply reached Telegram's fallback edit")
			return false
		}
		close(editStarted)
		select {
		case <-releaseEdit:
		case <-r.Context().Done():
		case <-t.Context().Done():
		}
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"simulated edit failure"}`)
		return true
	})
	b.cfg.ProfileEnabled = false
	b.cfg.AdminIDs = map[int64]bool{43: true}
	ref := seedRoleFact(t, b, secret)
	b.registerHandlers()
	dispatchRoleText(b, 42, 42, 1, "tell me the existing conclusion")
	roleReceive(t, editStarted)
	dispatchRoleText(b, 43, 43, 2, fmt.Sprintf("/memory edit %s %d new conclusion", ref.ID, ref.Version))
	if d := roleReceive(t, deliveries); !strings.Contains(d.text, "updated") {
		t.Fatalf("admin edit failed: %+v", d)
	}
	close(releaseEdit)
	if d := roleReceive(t, deliveries); d.path != "/botrole/deleteMessage" {
		t.Fatalf("stale draft survived: %+v", d)
	}
	if len(b.store.Get(42)) != 0 {
		t.Error("an undelivered stale reply entered chat history")
	}
}

func TestRoleAccessWithdrawStopsStreamedPreview(t *testing.T) {
	const secret = "currently approved group code"
	modelStarted, writeDraft, draftWritten, finish := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	previews := make(chan string, 2)
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, r *http.Request, _ int, req openai.ChatCompletionRequest) {
		if !strings.Contains(fmt.Sprint(req.Messages), secret) {
			t.Error("model lacked the conclusion before withdrawal")
		}
		close(modelStarted)
		select {
		case <-writeDraft:
		case <-r.Context().Done():
			return
		case <-t.Context().Done():
			return
		}
		einoModelSSE(w, einoReplyTextFrame(secret))
		close(draftWritten)
		select {
		case <-finish:
		case <-r.Context().Done():
			return
		case <-t.Context().Done():
			return
		}
		einoReplyFinish(w, "stop")
	})
	b, deliveries := newRoleTestBotWithTelegram(t, model, func(_ http.ResponseWriter, _ *http.Request, d roleTestDelivery) bool {
		if d.path == "/botrole/editMessageText" && strings.HasSuffix(d.text, "▌") {
			previews <- d.text
		}
		return false
	})
	b.cfg.ProfileEnabled = false
	seedRoleFact(t, b, secret)
	b.registerHandlers()
	dispatchRoleText(b, 42, 42, 1, "/memory_share -100123 "+secret)
	roleReceive(t, deliveries)
	dispatchRoleText(b, regressionGroup, 43, 2, "@testbot what is the code?")
	roleReceive(t, modelStarted)
	dispatchRoleText(b, 42, 42, 3, "/memory_unshare -100123 "+secret)
	if d := roleReceive(t, deliveries); !strings.Contains(d.text, "withdrawn") {
		t.Fatalf("withdrawal failed: %+v", d)
	}
	close(writeDraft)
	roleReceive(t, draftWritten)
	// Keep the final frame blocked through the first preview tick.
	select {
	case preview := <-previews:
		t.Errorf("Telegram received a stale preview: %q", preview)
	case <-time.After(1700 * time.Millisecond):
	}
	close(finish)
	if d := roleReceive(t, deliveries); d.path != "/botrole/deleteMessage" {
		t.Fatalf("stale draft was delivered: %+v", d)
	}
}
