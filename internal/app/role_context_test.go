package app

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	tele "gopkg.in/telebot.v3"
)

type roleTestDelivery struct {
	path, text string
	chatID     int64
}

var testTelegramTags = regexp.MustCompile("<[^>]+>")

func testTelegramVisibleText(text, mode string) string {
	if mode == "HTML" {
		text = html.UnescapeString(testTelegramTags.ReplaceAllString(text, ""))
	}
	return strings.ReplaceAll(text, "\r\n", "\n")
}

func newRoleTestBot(t *testing.T, model *einoReplyProvider, media ...http.Handler) (*Bot, <-chan roleTestDelivery) {
	t.Helper()
	return newRoleTestBotWithTelegram(t, model, nil, media...)
}

func newRoleTestBotWithTelegram(t *testing.T, model *einoReplyProvider, hook func(http.ResponseWriter, *http.Request, roleTestDelivery) bool, media ...http.Handler) (*Bot, <-chan roleTestDelivery) {
	t.Helper()
	delivery := make(chan roleTestDelivery, 100)
	var ids atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(media) > 0 && (r.URL.Path == "/botrole/getFile" || strings.HasPrefix(r.URL.Path, "/file/botrole/")) {
			media[0].ServeHTTP(w, r)
			return
		}
		var body struct {
			ChatID  int64  `json:"-"`
			Chat    string `json:"chat_id"`
			Message string `json:"message_id"`
			Text    string `json:"text"`
			Mode    string `json:"parse_mode"`
			User    string `json:"user_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("Telegram request: %v", err)
		}
		fmt.Sscan(body.Chat, &body.ChatID)
		if hook != nil && hook(w, r, roleTestDelivery{r.URL.Path, body.Text, body.ChatID}) {
			return
		}
		chatType := tele.ChatPrivate
		if body.ChatID < 0 {
			chatType = tele.ChatSuperGroup
		}
		switch r.URL.Path {
		case "/botrole/sendMessage", "/botrole/editMessageText", "/botrole/deleteMessage":
			if body.Text != replyPlaceholderText && !strings.HasSuffix(body.Text, "▌") {
				delivery <- roleTestDelivery{r.URL.Path, body.Text, body.ChatID}
			}
			if r.URL.Path == "/botrole/deleteMessage" {
				fmt.Fprint(w, `{"ok":true,"result":true}`)
				return
			}
			if r.URL.Path == "/botrole/editMessageText" {
				visible := testTelegramVisibleText(body.Text, body.Mode)
				id, _ := strconv.Atoi(body.Message)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": tele.Message{
					ID: id, Chat: &tele.Chat{ID: body.ChatID, Type: chatType}, Text: visible,
				}})
				return
			}
		case "/botrole/getChat":
			fmt.Fprintf(w, `{"ok":true,"result":{"id":%d,"type":%q}}`, body.ChatID, chatType)
			return
		case "/botrole/getChatMember":
			status := "member"
			if body.ChatID == -100999 {
				status = "left"
			}
			fmt.Fprintf(w, `{"ok":true,"result":{"status":%q,"user":{"id":%s,"first_name":"Member"}}}`, status, body.User)
			return
		default:
			t.Errorf("unexpected Telegram call %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": tele.Message{
			ID: int(ids.Add(1)), Chat: &tele.Chat{ID: body.ChatID, Type: chatType}, Text: testTelegramVisibleText(body.Text, body.Mode),
		}})
	}))
	t.Cleanup(server.Close)
	tg, err := tele.NewBot(tele.Settings{URL: server.URL, Token: "role", Client: server.Client(), Offline: true, Synchronous: true})
	if err != nil {
		t.Fatal(err)
	}
	tg.Me = &tele.User{ID: 99, Username: "testbot", IsBot: true}
	dbPath := filepath.Join(t.TempDir(), "chat.db")
	db, err := OpenChatDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := model.snapshot(3).cfg
	cfg.ChatDBPath, cfg.OpenAIBase, cfg.OpenAIKey = dbPath, model.url+"/v1", "local"
	cfg.SystemPrompt, cfg.BotUsername = "You are Alder. Be reserved.", "@testbot"
	cfg.ProfileEnabled, cfg.ProfileExtractEvery = true, 3
	cfg.ProfileDBPath = filepath.Join(t.TempDir(), "legacy.db")
	profiles, err := NewProfileStore(cfg.ProfileDBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = profiles.Close() })
	b := &Bot{cfg: cfg, ai: model.client, tg: tg, chatDB: db, store: NewHistoryStore(db),
		memory: NewMemoryBook(db), summaries: NewSummaryStore(db), profiles: profiles,
		profileAI: model.client, profileModel: cfg.OpenAIModel, summaryAI: model.client, summaryModel: cfg.OpenAIModel,
		dashboardEvents: NewDashboardEventHub(0)}
	return b, delivery
}

func roleMessage(chatID, userID int64, id int, text string) *tele.Message {
	chatType := tele.ChatPrivate
	if chatID < 0 {
		chatType = tele.ChatSuperGroup
	}
	return &tele.Message{ID: id, Chat: &tele.Chat{ID: chatID, Type: chatType}, Sender: &tele.User{ID: userID}, Text: text}
}

func roleHandle(t *testing.T, b *Bot, handler func(tele.Context) error, m *tele.Message) {
	t.Helper()
	if err := handler(b.tg.NewContext(tele.Update{Message: m})); err != nil {
		t.Fatal(err)
	}
}

func roleReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for local protocol event")
		var zero T
		return zero
	}
}

func roleCompletion(w http.ResponseWriter, text string) {
	_ = json.NewEncoder(w).Encode(openai.ChatCompletionResponse{Choices: []openai.ChatCompletionChoice{
		{Message: openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: text}},
	}})
}

func waitRoleMemory(t *testing.T, b *Bot, want string) MemoryEntry {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := b.memory.List("", 30)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.Text == want {
				return entry
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no learned entry %q", want)
	return MemoryEntry{}
}

func TestRoleTurn(t *testing.T) {
	const privateFact = "I prefer pu-erh"
	const privateEvidence = "garage-key-raw"
	const groupInference = "Alice seems unconcerned about the book club"
	const correction = "I missed the book club because of a family emergency"
	const group = -100123
	requests := make(chan openai.ChatCompletionRequest, 16)
	var rounds atomic.Int32
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
		requests <- req
		if req.Stream {
			if rounds.Add(1) == 1 {
				einoModelSSE(w, einoReplyToolFrame("test-call", "lookup", 0))
				einoReplyFinish(w, "tool_calls")
			} else {
				einoModelSSE(w, einoReplyTextFrame("Understood."))
				einoReplyFinish(w, "stop")
			}
			return
		}
		material := fmt.Sprint(req.Messages)
		switch {
		case strings.Contains(material, privateEvidence):
			roleCompletion(w, `{"kind":"statement","conclusion":"I prefer pu-erh","cue":"familiar"}`)
		case strings.Contains(material, "I missed the reading"):
			roleCompletion(w, `{"kind":"inference","conclusion":"Alice seems unconcerned about the book club","cue":"reserved"}`)
		case strings.Contains(material, "family emergency"):
			if strings.Contains(material, groupInference) {
				t.Error("private reflection received original group inference")
			}
			roleCompletion(w, `{"kind":"correction","conclusion":"I missed the book club because of a family emergency","cue":"familiar"}`)
		default:
			roleCompletion(w, `{}`)
		}
	})
	b, deliveries := newRoleTestBot(t, model)
	var toolRuns atomic.Int32
	b.tools = newEinoReplyRegistry(t, &toolRuns)
	if err := b.profiles.Save(&UserProfile{UserID: 42, Facts: []string{"legacy-private-profile"}}); err != nil {
		t.Fatal(err)
	}

	roleHandle(t, b, b.handleText, roleMessage(42, 42, 101, privateFact+". "+privateEvidence))
	for i := 0; i < 2; i++ {
		request := roleReceive(t, requests)
		if !request.Stream || !strings.Contains(request.Messages[0].Content, "You are Alder.") ||
			strings.Contains(fmt.Sprint(request.Messages), "legacy-private-profile") {
			t.Errorf("wrong Eino system or legacy profile leaked in round %d: %+v", i, request.Messages)
		}
	}
	roleReceive(t, deliveries)
	reflection := roleReceive(t, requests)
	if reflection.Stream || !strings.Contains(reflection.Messages[0].Content, "You are Alder.") {
		t.Errorf("unexpected role reflection: %+v", reflection.Messages)
	}
	entry := waitRoleMemory(t, b, privateFact)
	if got := entry.Evidence[0].Source; got != (MemorySource{ChatID: 42, MessageID: 101, SenderID: 42}) {
		t.Fatalf("learned source = %+v", got)
	}
	if toolRuns.Load() != 1 {
		t.Fatal("test did not traverse a tool round")
	}

	cfg := b.currentConfig()
	cfg.SystemPrompt = "You are Juniper. Be direct."
	if err := b.applyRuntimeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	roleHandle(t, b, b.handleText, roleMessage(group, 42, 201, "@testbot I missed the reading"))
	request := roleReceive(t, requests)
	groupPrompt := fmt.Sprint(request.Messages)
	for _, want := range []string{"You are Juniper.", "familiar"} {
		if !strings.Contains(groupPrompt, want) {
			t.Errorf("group role context missing %q", want)
		}
	}
	for _, forbidden := range []string{privateFact, privateEvidence, "legacy-private-profile", entry.Ref.ID} {
		if strings.Contains(groupPrompt, forbidden) {
			t.Errorf("group context leaked %q", forbidden)
		}
	}
	roleReceive(t, deliveries)
	roleReceive(t, requests)
	original := waitRoleMemory(t, b, groupInference)

	m := roleMessage(42, 42, 102, "I missed it due to a family emergency")
	m.ExternalReplyInfo = &tele.ExternalReplyInfo{Chat: &tele.Chat{ID: group, Type: tele.ChatSuperGroup}, MessageID: 201,
		Origin: &tele.MessageOrigin{Type: "user", Sender: &tele.User{ID: 42}}}
	roleHandle(t, b, b.handleText, m)
	if req := roleReceive(t, requests); strings.Contains(fmt.Sprint(req.Messages), groupInference) {
		t.Error("private reply received group inference")
	}
	roleReceive(t, deliveries)
	roleReceive(t, requests)
	waitRoleMemory(t, b, correction)
	entryAfter, err := b.memory.Get(original.Ref.ID)
	if err != nil || !entryAfter.Retired {
		t.Errorf("corrected group inference is still active: %+v %v", entryAfter, err)
	}
}

func TestClearInFlight(t *testing.T) {
	replyStarted, finishReply := make(chan struct{}), make(chan struct{})
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, r *http.Request, _ int, req openai.ChatCompletionRequest) {
		if !req.Stream {
			roleCompletion(w, `{"kind":"statement","conclusion":"old memory"}`)
			return
		}
		close(replyStarted)
		select {
		case <-finishReply:
		case <-r.Context().Done():
		case <-t.Context().Done():
		}
		einoModelSSE(w, einoReplyTextFrame("old reply"))
		einoReplyFinish(w, "stop")
	})
	b, deliveries := newRoleTestBot(t, model)
	old := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "old history"}
	b.store.Append(42, old, 0)
	b.store.Append(43, old, 1)
	b.summaries.Set(42, "old summary")
	b.summaries.Set(43, "other summary")
	ref, err := b.memory.Learn(MemoryLearn{Subjects: []int64{42}, Text: "retained memory",
		Evidence: MemoryEvidence{Kind: MemoryStatement, Source: MemorySource{ChatID: 42, SenderID: 42, MessageID: 99}}})
	if err != nil {
		t.Fatal(err)
	}
	roleHandle(t, b, b.handleText, roleMessage(42, 42, 101, "clear me while in flight"))
	roleReceive(t, replyStarted)
	roleHandle(t, b, b.handleClear, roleMessage(42, 42, 102, "/clear"))
	close(finishReply)
	first, second := roleReceive(t, deliveries), roleReceive(t, deliveries)
	if second.path == "/botrole/sendMessage" {
		first, second = second, first
	}
	if !strings.Contains(first.text, "cleared") || second.path != "/botrole/deleteMessage" {
		t.Fatalf("wanted clear confirmation and stale placeholder deletion, got %+v and %+v", first, second)
	}
	if len(b.store.Get(42)) != 0 || b.store.OverflowCount(42) != 0 || b.summaries.Get(42) != "" {
		t.Fatalf("clear left history, overflow or summary for the current chat")
	}
	if len(b.store.Get(43)) != 1 || b.summaries.Get(43) != "other summary" {
		t.Fatal("clear affected a different chat")
	}
	if _, err := b.memory.Get(ref.ID); err != nil {
		t.Fatalf("clear removed the Memory Book entry: %v", err)
	}
}

func TestRoleTurnShareAndWithdraw(t *testing.T) {
	const owner, other, target, unrelated = 42, 43, -100123, -100124
	const fact, evidence = "Private favorite is pu-erh\nwith sweet rice", "RAW-EVIDENCE-8842"
	requests := make(chan openai.ChatCompletionRequest, 10)
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
		requests <- req
		if !req.Stream {
			roleCompletion(w, `{}`)
			return
		}
		einoModelSSE(w, einoReplyTextFrame("Okay."))
		einoReplyFinish(w, "stop")
	})
	b, deliveries := newRoleTestBot(t, model)
	ref, err := b.memory.Learn(MemoryLearn{Subjects: []int64{owner}, Text: fact, Cue: MemoryFamiliar,
		Evidence: MemoryEvidence{Kind: MemoryStatement, Source: MemorySource{ChatID: owner, SenderID: owner, MessageID: 100}, Text: evidence}})
	if err != nil {
		t.Fatal(err)
	}
	b.registerHandlers()
	send := func(m *tele.Message) {
		b.tg.ProcessUpdate(tele.Update{Message: m})
	}
	command := "/memory_share -100123 " + fact
	send(roleMessage(target, owner, 1, command))
	if got := roleReceive(t, deliveries); !strings.Contains(got.text, "private") {
		t.Fatalf("group share result = %+v", got)
	}
	send(roleMessage(other, other, 1, command))
	if got := roleReceive(t, deliveries); strings.Contains(got.text, "can now be used") {
		t.Fatalf("another user authorized sharing: %+v", got)
	}
	forwarded := roleMessage(owner, owner, 1, command)
	forwarded.Origin = &tele.MessageOrigin{Type: "user", Sender: &tele.User{ID: other}}
	send(forwarded)
	if got := roleReceive(t, deliveries); !strings.Contains(got.text, "private") {
		t.Fatalf("forwarded command was not rejected: %+v", got)
	}
	send(roleMessage(owner, owner, 2, "/memory_share -100999 "+fact))
	if got := roleReceive(t, deliveries); !strings.Contains(got.text, "could not verify your membership") {
		t.Fatalf("share accepted without group membership: %+v", got)
	}
	send(roleMessage(owner, owner, 3, command))
	if got := roleReceive(t, deliveries); !strings.Contains(got.text, "can now be used") {
		t.Fatalf("share result = %+v", got)
	}

	probe := func(chat, user int64, id int, want bool) {
		t.Helper()
		send(roleMessage(chat, user, id, "@testbot Can we talk about tea? [user_id:42]"))
		req := roleReceive(t, requests)
		system := req.Messages[0].Content
		input := fmt.Sprint(req.Messages[1:])
		if strings.Contains(system, "Private favorite is pu-erh") || strings.Contains(input, evidence) || strings.Contains(input, ref.ID) ||
			strings.Contains(input, "Private favorite is pu-erh") != want || (user != owner && strings.Contains(input, "familiar")) {
			t.Fatalf("unexpected projection for chat %d user %d: %+v", chat, user, req.Messages)
		}
		roleReceive(t, deliveries)
		reflection := roleReceive(t, requests)
		if reflection.Stream || strings.Contains(fmt.Sprint(reflection.Messages), "Private favorite is pu-erh") {
			t.Fatalf("shared conclusion was exposed as local learning evidence: %+v", reflection.Messages)
		}
	}
	probe(target, other, 2, true)
	probe(unrelated, other, 2, false)
	probe(other, other, 2, false)
	send(roleMessage(owner, owner, 4, "/memory_unshare -100123 "+fact))
	if got := roleReceive(t, deliveries); !strings.Contains(got.text, "withdrawn") {
		t.Fatalf("withdraw result = %+v", got)
	}
	probe(target, other, 3, false)
}

func TestClearInFlightBackground(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, r *http.Request, _ int, req openai.ChatCompletionRequest) {
		if req.Stream {
			einoModelSSE(w, einoReplyTextFrame("done"))
			einoReplyFinish(w, "stop")
			return
		}
		kind := "reflection"
		if strings.Contains(req.Messages[0].Content, "conversation summarizer") {
			kind = "summary"
		}
		if !strings.HasPrefix(req.Messages[0].Content, "You are Alder.") {
			t.Errorf("%s used wrong role: %s", kind, req.Messages[0].Content)
		}
		started <- kind
		select {
		case <-release:
		case <-r.Context().Done():
		case <-t.Context().Done():
		}
		if kind == "summary" {
			roleCompletion(w, "delayed summary")
		} else {
			roleCompletion(w, `{"kind":"statement","conclusion":"delayed memory"}`)
		}
	})
	b, deliveries := newRoleTestBot(t, model)
	b.cfg.SummaryEnabled, b.cfg.SummaryMinOverflow, b.cfg.ContextMaxMsgs = true, 1, 1
	b.summaries.Set(42, "existing summary")
	b.summaries.Set(43, "other chat")
	oldTurn := b.beginRoleTurn(RoleTurn{ChatID: 42})
	roleHandle(t, b, b.handleText, roleMessage(42, 42, 1, "old event"))
	roleReceive(t, deliveries)
	if a, z := roleReceive(t, started), roleReceive(t, started); a == z {
		t.Fatalf("observed two %s workers", a)
	}
	roleHandle(t, b, b.handleClear, roleMessage(42, 42, 2, "/clear"))
	close(release)
	roleReceive(t, deliveries)
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, busy := b.summaries.summarizing.Load(oldTurn.generation)
		if !busy && b.memoryReviews.Load() == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("old background work did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	entries, err := b.memory.List("", 10)
	if err != nil || len(entries) != 0 || b.summaries.Get(42) != "" || b.summaries.Get(43) != "other chat" {
		t.Fatalf("late worker crossed clear boundary: entries=%v summary=%q err=%v", entries, b.summaries.Get(42), err)
	}
	if db := b.snapshot().chatDB; len(db.LoadAllHistory()[42]) != 0 ||
		len(db.LoadAllOverflow()[42]) != 0 || db.LoadAllSummaries()[42] != "" {
		t.Fatal("context was not cleared in the persisted database")
	}
}

func TestRoleTurnNoPersonalToolsInGroup(t *testing.T) {
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
		if req.Stream {
			if got := einoReplyToolNames(req); len(got) != 0 {
				t.Errorf("group received private connector tools: %v", got)
			}
			einoModelSSE(w, einoReplyTextFrame("done"))
			einoReplyFinish(w, "stop")
		} else {
			roleCompletion(w, `{}`)
		}
	})
	b, deliveries := newRoleTestBot(t, model)
	b.cfg.ProfileEnabled = false
	personal := NewToolRegistry()
	addEinoTestMCPServer(t, personal, "private", einoTestMCPTool("private_lookup", "private data", ""))
	b.userTools = NewUserToolManager(nil, nil, nil)
	b.userTools.registries[42] = personal
	b.tools = NewToolRegistry()
	roleHandle(t, b, b.handleText, roleMessage(-100123, 42, 1, "@testbot tell me about my data"))
	roleReceive(t, deliveries)
	waitEinoReplyHistory(t, b, -100123, 2)
}

func TestRoleTurnDifferentDeploymentsAndRelevance(t *testing.T) {
	requests := make(chan openai.ChatCompletionRequest, 4)
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
		requests <- req
		if !req.Stream {
			roleCompletion(w, "YES")
			return
		}
		einoModelSSE(w, einoReplyTextFrame("Answer"))
		einoReplyFinish(w, "stop")
	})
	const group, user = -100123, 42
	for _, persona := range []string{"You are Sand. Keep it brief.", "You are River. Ask questions."} {
		b, deliveries := newRoleTestBot(t, model)
		b.cfg.SystemPrompt, b.cfg.ProfileEnabled, b.cfg.AutoDetect = persona, false, true
		b.detectorAI, b.detectorModel = model.client, b.cfg.OpenAIModel
		if _, err := b.memory.Learn(MemoryLearn{Subjects: []int64{user}, Text: "private counterexample", Cue: MemoryFamiliar,
			Evidence: MemoryEvidence{Kind: MemoryStatement, Source: MemorySource{ChatID: user, SenderID: user, MessageID: 1}}}); err != nil {
			t.Fatal(err)
		}
		roleHandle(t, b, b.handleText, roleMessage(group, user, 2, "Ready for the next step?"))
		for _, stream := range []bool{false, true} {
			request := roleReceive(t, requests)
			input := fmt.Sprint(request.Messages)
			if request.Stream != stream || !strings.HasPrefix(request.Messages[0].Content, persona) ||
				!strings.Contains(input, "familiar") || strings.Contains(input, "private counterexample") {
				t.Errorf("wrong deployment persona or audience in request: %s", input)
			}
		}
		roleReceive(t, deliveries)
		waitEinoReplyHistory(t, b, group, 2)
	}
}
