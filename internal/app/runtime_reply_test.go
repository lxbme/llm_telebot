package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	openai "github.com/sashabaranov/go-openai"
	tele "gopkg.in/telebot.v3"
)

func TestReplyKeepsMCPGenerationAcrossReload(t *testing.T) {
	for _, tt := range []struct {
		name            string
		eino            bool
		reuseDB         bool
		failPlaceholder bool
	}{
		{name: "Eino same personal database", eino: true, reuseDB: true},
		{name: "legacy new personal database"},
		{name: "failed placeholder", eino: true, failPlaceholder: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			oldGlobal := newReplyMCPServer(t, "old_global")
			newGlobal := newReplyMCPServer(t, "new_global")
			oldPersonal := newReplyMCPServer(t, "old_personal")
			newPersonal := newReplyMCPServer(t, "new_personal")
			const userID = 42

			cfg := Config{
				OpenAIKey: "test-key", OpenAIModel: "old-model", SystemPrompt: "old prompt",
				ChatDBPath: filepath.Join(dir, "chat.db"), StickerRulesPath: filepath.Join(dir, "sticker.json"),
				ToolsEnabled: true, ToolsMaxIterations: 1, EinoChatEnabled: tt.eino, ContextMaxMsgs: 20,
				MCPConfigPath: filepath.Join(dir, "old-mcp.json"), UserMCPDBPath: filepath.Join(dir, "old-users.db"),
			}
			writeReplyMCPConfig(t, cfg.MCPConfigPath, oldGlobal.url)
			seedReplyUserMCP(t, cfg.UserMCPDBPath, userID, oldPersonal.url)
			next := cfg
			next.OpenAIModel, next.SystemPrompt, next.EinoChatEnabled = "new-model", "new prompt", true
			next.MCPConfigPath = filepath.Join(dir, "new-mcp.json")
			writeReplyMCPConfig(t, next.MCPConfigPath, newGlobal.url)
			newPersonalName := "old_personal"
			if !tt.reuseDB {
				next.UserMCPDBPath = filepath.Join(dir, "new-users.db")
				seedReplyUserMCP(t, next.UserMCPDBPath, userID, newPersonal.url)
				newPersonalName = "new_personal"
			}

			oldProvider := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, round int, req openai.ChatCompletionRequest) {
				if tt.failPlaceholder || req.Model != "old-model" || req.Messages[0].Content != "old prompt" {
					t.Errorf("unexpected old-model request: %+v", req)
				}
				switch round {
				case 0:
					assertReplyMCPNames(t, req, "old_global", "old_personal")
					if req.Stream != tt.eino {
						t.Errorf("old-model stream = %t, want %t", req.Stream, tt.eino)
					}
					if tt.eino {
						einoModelSSE(w, einoReplyToolFrame("call-g", "old_global", 0), einoReplyToolFrame("call-p", "old_personal", 1))
						einoReplyFinish(w, "tool_calls")
					} else {
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call-g","type":"function","function":{"name":"old_global","arguments":"{}"}},{"id":"call-p","type":"function","function":{"name":"old_personal","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
					}
				case 1:
					var results []string
					for _, message := range req.Messages {
						if message.Role == openai.ChatMessageRoleTool {
							results = append(results, message.Content)
						}
					}
					if !slices.Equal(results, []string{"old_global result", "old_personal result"}) {
						t.Errorf("old-model MCP results = %q", results)
					}
					einoModelSSE(w, einoReplyTextFrame("old reply"))
					einoReplyFinish(w, "stop")
				default:
					t.Errorf("unexpected old-model request %d", round)
					http.Error(w, "unexpected round", http.StatusBadRequest)
				}
			})
			newProvider := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
				if req.Model != "new-model" || req.Messages[0].Content != "new prompt" || !req.Stream {
					t.Errorf("unexpected new-model request: %+v", req)
				}
				assertReplyMCPNames(t, req, "new_global", newPersonalName)
				einoModelSSE(w, einoReplyTextFrame("new reply"))
				einoReplyFinish(w, "stop")
			})
			cfg.OpenAIBase, next.OpenAIBase = oldProvider.url+"/v1", newProvider.url+"/v1"

			placeholderStarted := make(chan struct{})
			unblock := make(chan struct{})
			telegram := newEinoReplyTelegram(t, func(id int) error {
				if id == 1 {
					close(placeholderStarted)
					<-unblock
					if tt.failPlaceholder || t.Context().Err() != nil {
						return errors.New("simulated placeholder failure")
					}
				}
				return nil
			})
			var unblockOnce sync.Once
			releasePlaceholder := func() { unblockOnce.Do(func() { close(unblock) }) }
			t.Cleanup(releasePlaceholder)

			b := &Bot{cfg: Config{ChatDBPath: cfg.ChatDBPath}, store: NewHistoryStore(nil), tg: telegram.bot}
			if err := b.applyRuntimeConfig(cfg); err != nil {
				t.Fatal(err)
			}
			var reloadDone chan struct{}
			t.Cleanup(func() {
				releasePlaceholder()
				if reloadDone != nil {
					waitReplySignal(t, reloadDone, "configuration reload cleanup")
				}
				current := b.snapshot()
				if current.userTools != nil {
					current.userTools.Close()
				}
				if current.mcpClients != nil {
					current.mcpClients.Close()
				}
			})
			chat := &tele.Chat{ID: 41, Type: tele.ChatPrivate}
			user := &tele.User{ID: userID}
			message := newEinoReplyMessages()[1]
			oldSend := make(chan error, 1)
			go func() { oldSend <- b.startChatFlow(chat, user, message, false) }()
			waitReplySignal(t, placeholderStarted, "first placeholder request")

			reloadResult := make(chan error, 1)
			reloadDone = make(chan struct{})
			go func() {
				defer close(reloadDone)
				reloadResult <- b.applyRuntimeConfig(next)
			}()
			select {
			case err := <-reloadResult:
				if err != nil {
					t.Fatalf("reload while the placeholder is blocked: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("configuration reload waited for the blocked placeholder")
			}
			if err := b.startChatFlow(chat, user, message, false); err != nil {
				t.Fatal(err)
			}
			if edit := telegram.nextFinal(t); edit.Text != "new reply" || edit.MessageID != "2" {
				t.Fatalf("new reply while old placeholder is blocked = %+v", edit)
			}
			waitEinoReplyHistory(t, b, chat.ID, 2)
			for name, closed := range map[string]<-chan struct{}{
				"global": oldGlobal.closed, "personal": oldPersonal.closed,
			} {
				select {
				case <-closed:
					t.Errorf("retired %s MCP closed before the old reply ran", name)
				default:
				}
			}

			releasePlaceholder()
			select {
			case err := <-oldSend:
				if tt.failPlaceholder && (err == nil || !strings.Contains(err.Error(), "simulated placeholder failure")) {
					t.Fatalf("placeholder error = %v", err)
				}
				if !tt.failPlaceholder && err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("old placeholder did not finish")
			}
			if !tt.failPlaceholder {
				if edit := telegram.nextFinal(t); edit.Text != "old reply" || edit.MessageID != "1" {
					t.Fatalf("old reply = %+v", edit)
				}
				waitEinoReplyHistory(t, b, chat.ID, 4)
			}
			waitReplySignal(t, oldGlobal.closed, "retired global MCP close")
			waitReplySignal(t, oldPersonal.closed, "retired personal MCP close")
			servers, err := b.snapshot().userTools.ListServers(userID)
			if err != nil || len(servers) != 1 || servers[0].ToolCount != 1 {
				t.Errorf("personal MCP servers after retirement = %+v, %v", servers, err)
			}
			if tt.failPlaceholder && oldProvider.calls.Load() != 0 {
				t.Errorf("failed placeholder made %d model calls", oldProvider.calls.Load())
			}
		})
	}
}

func TestMCPClearWaitsForSamePathReload(t *testing.T) {
	const userID = 42
	restoreStarted := make(chan struct{})
	resumeRestore := make(chan struct{})
	releaseRestore := sync.OnceFunc(func() { close(resumeRestore) })
	var lists atomic.Int32
	personal := newReplyMCPServer(t, "personal_tool", server.WithToolFilter(func(ctx context.Context, tools []mcp.Tool) []mcp.Tool {
		if lists.Add(1) == 2 {
			close(restoreStarted)
			select {
			case <-resumeRestore:
			case <-ctx.Done():
			}
		}
		return tools
	}))
	provider := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
		assertReplyMCPNames(t, req, "personal_tool")
		einoModelSSE(w, einoReplyTextFrame("reply during reload"))
		einoReplyFinish(w, "stop")
	})
	telegram := newEinoReplyTelegram(t, nil)
	dir := t.TempDir()
	cfg := Config{
		OpenAIKey: "test-key", OpenAIModel: "test-model", OpenAIBase: provider.url + "/v1",
		ChatDBPath: filepath.Join(dir, "chat.db"), StickerRulesPath: filepath.Join(dir, "stickers.json"),
		UserMCPDBPath: filepath.Join(dir, "users.db"), ToolsEnabled: true, ToolsMaxIterations: 1,
		EinoChatEnabled: true, ContextMaxMsgs: 20,
	}
	seedReplyUserMCP(t, cfg.UserMCPDBPath, userID, personal.url)
	b := &Bot{cfg: Config{ChatDBPath: cfg.ChatDBPath}, store: NewHistoryStore(nil), tg: telegram.bot}
	if err := b.applyRuntimeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	var reloadDone, clearDone, sendDone chan struct{}
	t.Cleanup(func() {
		releaseRestore()
		for name, done := range map[string]chan struct{}{"reload": reloadDone, "clear": clearDone, "send": sendDone} {
			if done != nil {
				waitReplySignal(t, done, name+" cleanup")
			}
		}
		current := b.snapshot()
		closeToolResources(current.mcpClients, current.userTools)
	})
	next := cfg
	next.MCPConfigPath = filepath.Join(dir, "new-mcp.json")
	if err := os.WriteFile(next.MCPConfigPath, []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	var reloadErr, clearErr, sendErr error
	reloadDone = make(chan struct{})
	go func() {
		defer close(reloadDone)
		reloadErr = b.applyRuntimeConfig(next)
	}()
	waitReplySignal(t, restoreStarted, "personal MCP restore after reading the saved config")

	chat := &tele.Chat{ID: 41, Type: tele.ChatPrivate}
	user := &tele.User{ID: userID}
	sendDone = make(chan struct{})
	go func() {
		defer close(sendDone)
		sendErr = b.startChatFlow(chat, user, newEinoReplyMessages()[1], false)
	}()
	waitReplySignal(t, sendDone, "reply start during reload")
	if sendErr != nil {
		t.Fatal(sendErr)
	}
	if edit := telegram.nextFinal(t); edit.Text != "reply during reload" {
		t.Fatalf("unexpected reply during reload: %+v", edit)
	}
	waitEinoReplyHistory(t, b, chat.ID, 2)

	cmd := telegram.bot.NewContext(tele.Update{Message: &tele.Message{ID: 2, Chat: chat, Sender: user, Text: "/mcp_clear"}})
	clearStarted := make(chan struct{})
	clearDone = make(chan struct{})
	go func() {
		defer close(clearDone)
		close(clearStarted)
		clearErr = b.handleMCPClear(cmd)
	}()
	waitReplySignal(t, clearStarted, "/mcp_clear start")
	select {
	case <-clearDone:
		t.Error("/mcp_clear completed before the restored manager was published")
	case <-time.After(100 * time.Millisecond):
	}
	releaseRestore()
	waitReplySignal(t, reloadDone, "configuration reload")
	waitReplySignal(t, clearDone, "/mcp_clear completion")
	if reloadErr != nil || clearErr != nil {
		t.Fatalf("reload error = %v, clear error = %v", reloadErr, clearErr)
	}
	manager := b.snapshot().userTools
	if reg := manager.GetRegistry(userID); reg != nil {
		t.Errorf("current registry still contains %d tools after /mcp_clear", reg.Count())
	}
	if persisted, err := manager.store.Get(userID); err != nil || persisted != nil {
		t.Errorf("persisted configuration after /mcp_clear = %+v, %v", persisted, err)
	}
}

func assertReplyMCPNames(t *testing.T, req openai.ChatCompletionRequest, want ...string) {
	t.Helper()
	got := einoReplyToolNames(req)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("model %q tools = %v, want %v", req.Model, got, want)
	}
}

type replyMCPServer struct {
	url    string
	closed chan struct{}
}

func newReplyMCPServer(t *testing.T, toolName string, opts ...server.ServerOption) replyMCPServer {
	t.Helper()
	s := server.NewMCPServer(toolName, "test", opts...)
	tool := einoTestMCPTool(toolName, toolName+" result", "")
	s.AddTool(tool.info, tool.run)
	handler := server.NewStreamableHTTPServer(s, server.WithStateful(true))
	closed := make(chan struct{})
	var closeOnce sync.Once
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
		if r.Method == http.MethodDelete {
			closeOnce.Do(func() { close(closed) })
		}
	}))
	t.Cleanup(ts.Close)
	return replyMCPServer{url: ts.URL, closed: closed}
}

func writeReplyMCPConfig(t *testing.T, path, url string) {
	t.Helper()
	cfg := MCPConfig{MCPServers: map[string]MCPServerConfig{"global": {URL: url}}}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func seedReplyUserMCP(t *testing.T, path string, userID int64, url string) {
	t.Helper()
	store, err := NewUserMCPStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Save(userID, &MCPConfig{MCPServers: map[string]MCPServerConfig{"personal": {URL: url}}}); err != nil {
		t.Fatal(err)
	}
}

func waitReplySignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func TestReplyFinalEditCancelsBlockedPreview(t *testing.T) {
	previewStarted := make(chan struct{})
	previewCanceled := make(chan struct{})
	releaseHandler := make(chan struct{})
	finalEdit := make(chan einoReplyEdit, 1)
	var previews, resets atomic.Int32
	var finalized atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/botpreview-test-token/sendMessage":
		case "/botpreview-test-token/editMessageText":
			var edit einoReplyEdit
			if err := json.NewDecoder(r.Body).Decode(&edit); err != nil {
				t.Errorf("decode Telegram edit: %v", err)
				http.Error(w, "invalid edit", http.StatusBadRequest)
				return
			}
			if edit.ParseMode == "" {
				if edit.Text == replyPlaceholderText {
					if edit.MessageID != "1" || finalized.Load() || resets.Add(1) != 1 {
						t.Errorf("unexpected reset edit: %+v", edit)
					}
					break
				}
				if previews.Add(1) != 1 {
					t.Errorf("unexpected edit after the blocked preview: %+v", edit)
					http.Error(w, "unexpected edit", http.StatusBadRequest)
					return
				}
				if edit.MessageID != "1" || edit.Text != strings.Repeat("界", 4093)+"…" {
					t.Errorf("preview message id = %q, length = %d", edit.MessageID, len([]rune(edit.Text)))
				}
				close(previewStarted)
				select {
				case <-r.Context().Done():
					close(previewCanceled)
				case <-releaseHandler:
				}
				return
			}
			finalized.Store(true)
			finalEdit <- edit
		default:
			t.Errorf("unexpected Telegram route %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":41,"type":"private"}}}`)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(releaseHandler) })
	tg, err := tele.NewBot(tele.Settings{URL: server.URL, Token: "preview-test-token", Client: server.Client(), Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	provider := newEinoReplyProvider(t, func(w http.ResponseWriter, r *http.Request, round int, _ openai.ChatCompletionRequest) {
		if round == 1 {
			einoModelSSE(w, einoReplyTextFrame("**final**"))
			einoReplyFinish(w, "stop")
			return
		}
		einoModelSSE(w, einoReplyTextFrame(strings.Repeat("界", 4100)))
		select {
		case <-previewStarted:
		case <-r.Context().Done():
			return
		}
		einoModelSSE(w, einoReplyToolFrame("call-a", "lookup", 0))
		einoReplyFinish(w, "tool_calls")
	})
	snap := provider.snapshot(3)
	snap.cfg.EinoChatEnabled = true
	var toolRuns atomic.Int32
	b := &Bot{
		cfg: snap.cfg, ai: snap.ai, tg: tg, telegramHTTP: server.Client(),
		store: NewHistoryStore(nil), tools: newEinoReplyRegistry(t, &toolRuns),
	}
	chat := &tele.Chat{ID: 41, Type: tele.ChatPrivate}
	if err := b.startChatFlow(chat, nil, newEinoReplyMessages()[1], false); err != nil {
		t.Fatal(err)
	}
	waitReplySignal(t, previewStarted, "blocked preview")
	select {
	case <-previewCanceled:
	case <-time.After(time.Second):
		t.Fatal("finishing the model stream did not cancel the blocked preview")
	}
	select {
	case edit := <-finalEdit:
		if edit.MessageID != "1" || edit.ParseMode != "HTML" || edit.Text != "<b>final</b>" {
			t.Errorf("final edit = %+v", edit)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked preview prevented the final edit")
	}
	waitEinoReplyHistory(t, b, chat.ID, 2)
}

func TestChatHistoryPersistsAcrossConcurrentMigration(t *testing.T) {
	const migrations, chatID, maxMessages = 12, 41, 3
	dir := t.TempDir()
	cfg := Config{
		ChatDBPath: filepath.Join(dir, "0.db"), StickerRulesPath: filepath.Join(dir, "stickers.json"),
		OpenAIKey: "test-key", OpenAIModel: "test-model",
	}
	db, err := OpenChatDB(cfg.ChatDBPath)
	if err != nil {
		t.Fatal(err)
	}
	b := &Bot{cfg: cfg, chatDB: db, store: NewHistoryStore(db)}
	t.Cleanup(func() { _ = b.snapshot().chatDB.Close() })
	msg := func(n int) openai.ChatCompletionMessage {
		return openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: fmt.Sprint(n)}
	}
	for i := range 8 {
		b.store.Append(chatID, msg(i), maxMessages)
	}
	for i := 1; i <= migrations; i++ {
		next := cfg
		next.ChatDBPath = filepath.Join(dir, fmt.Sprintf("%d.db", i))
		start := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			<-start
			for n := range 10 {
				b.store.AppendBatch(chatID, []openai.ChatCompletionMessage{msg(i*100 + n)}, maxMessages)
			}
		}()
		close(start)
		err := b.applyRuntimeConfig(next)
		<-done
		if err != nil {
			t.Fatal(err)
		}
	}
	last := b.snapshot().chatDB
	history := last.LoadAllHistory()[chatID]
	overflow := last.LoadAllOverflow()[chatID]
	var want []openai.ChatCompletionMessage
	for i := range 8 {
		want = append(want, msg(i))
	}
	for i := 1; i <= migrations; i++ {
		for n := range 10 {
			want = append(want, msg(i*100+n))
		}
	}
	if !reflect.DeepEqual(append(overflow, history...), want) || len(history) != maxMessages {
		t.Fatalf("persisted history/overflow lengths %d/%d or message order differ from the %d expected messages",
			len(history), len(overflow), len(want))
	}
}

func TestChatHistoryRemovalsSurviveMigrationRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name     string
		restartB bool
	}{
		{name: "live rebind"},
		{name: "restart before rebind", restartB: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const cleared, appended, drained, destinationOnly = 41, 42, 43, 44
			dir := t.TempDir()
			pathA, pathB := filepath.Join(dir, "a.db"), filepath.Join(dir, "b.db")
			open := func(path string) *ChatDB {
				t.Helper()
				db, err := OpenChatDB(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
				return db
			}
			closeDB := func(db *ChatDB) {
				t.Helper()
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			msg := func(content string) openai.ChatCompletionMessage {
				return openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: content}
			}
			oldOverflow, oldHistory := msg("old overflow"), msg("old history")
			newHistory := msg("new history")
			dbA, dbB := open(pathA), open(pathB)
			store := NewHistoryStore(dbA)
			for _, id := range []int64{cleared, appended, drained} {
				store.AppendBatch(id, []openai.ChatCompletionMessage{oldOverflow, oldHistory}, 1)
			}
			NewHistoryStore(dbB).AppendBatch(destinationOnly, []openai.ChatCompletionMessage{oldOverflow, oldHistory}, 1)
			if err := store.migrateDB(dbB); err != nil {
				t.Fatal(err)
			}
			closeDB(dbA)
			h, o := dbB.LoadAllHistory()[destinationOnly], dbB.LoadAllOverflow()[destinationOnly]
			if !reflect.DeepEqual(h, []openai.ChatCompletionMessage{oldHistory}) ||
				!reflect.DeepEqual(o, []openai.ChatCompletionMessage{oldOverflow}) {
				t.Errorf("destination-only chat lost messages: history = %v, overflow = %v", h, o)
			}

			store.Clear(cleared)
			store.Clear(appended)
			store.Append(appended, newHistory, 1)
			if got := store.DrainOverflow(drained); !reflect.DeepEqual(got, []openai.ChatCompletionMessage{oldOverflow}) {
				t.Fatalf("drained overflow = %v, want one old message", got)
			}
			if tc.restartB {
				closeDB(dbB)
				dbB = open(pathB)
				store = NewHistoryStore(dbB)
			}
			dbA = open(pathA)
			if err := store.migrateDB(dbA); err != nil {
				t.Fatal(err)
			}
			closeDB(dbB)
			closeDB(dbA)
			store = NewHistoryStore(open(pathA))

			if got := store.Get(cleared); len(got) != 0 {
				t.Errorf("cleared history reappeared: %v", got)
			}
			if got := store.Get(appended); !reflect.DeepEqual(got, []openai.ChatCompletionMessage{newHistory}) {
				t.Errorf("history after clear and append = %v, want only the new message", got)
			}
			if got := store.Get(drained); !reflect.DeepEqual(got, []openai.ChatCompletionMessage{oldHistory}) {
				t.Errorf("draining overflow changed history: %v", got)
			}
			for _, id := range []int64{cleared, appended, drained} {
				if got := store.OverflowCount(id); got != 0 {
					t.Errorf("chat %d resurrected %d overflow messages", id, got)
				}
			}
			store.Append(appended, msg("another message"), 1)
			if got := store.DrainOverflow(appended); !reflect.DeepEqual(got, []openai.ChatCompletionMessage{newHistory}) {
				t.Errorf("fresh overflow = %v, want only the new message", got)
			}
		})
	}
}
