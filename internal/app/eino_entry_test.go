package app

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	tele "gopkg.in/telebot.v3"
)

func TestEinoGroupTextUsesPersistedPhotoHistory(t *testing.T) {
	const pngBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jP1sAAAAASUVORK5CYII="
	photo, err := base64.StdEncoding.DecodeString(pngBase64)
	if err != nil {
		t.Fatal(err)
	}
	files := http.NewServeMux()
	files.HandleFunc("POST /bottest-token/getFile", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req["file_id"] != "group-photo" {
			t.Errorf("getFile payload = %v, error = %v", req, err)
			http.Error(w, "unexpected file", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":{"file_id":"group-photo","file_path":"photos/group.png"}}`)
	})
	files.HandleFunc("GET /file/bottest-token/photos/group.png", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(photo)
	})
	chat := tele.Chat{ID: -100123, Type: tele.ChatSuperGroup}
	telegram := newEinoReplyTelegramForChat(t, chat, nil, files)
	answers := []string{"The photo shows a sample.", "The earlier photo is a PNG."}
	requests := make(chan openai.ChatCompletionRequest, len(answers))
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, round int, req openai.ChatCompletionRequest) {
		if round >= len(answers) || !req.Stream {
			t.Errorf("unexpected group model request %d, stream = %t", round+1, req.Stream)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		requests <- req
		einoModelSSE(w, einoReplyTextFrame(answers[round]))
		einoReplyFinish(w, "stop")
	})
	db, err := OpenChatDB(filepath.Join(t.TempDir(), "chat.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	snap := model.snapshot(3)
	snap.cfg.VisionEnabled = true
	snap.cfg.BotUsername, snap.cfg.ContextMode, snap.cfg.VisionMaxImageBytes = "@testbot", "global", 1024
	b := &Bot{
		cfg: snap.cfg, ai: snap.ai, tg: telegram.bot, store: NewHistoryStore(db),
		scheduleWizard: NewScheduleWizardStore(),
	}

	alice := &tele.User{ID: 42, Username: "alice", FirstName: "Alice"}
	photoUpdate := tele.Update{Message: &tele.Message{
		ID: 10, Chat: &chat, Sender: alice, Caption: "@TESTBOT describe this photo",
		Photo: &tele.Photo{File: tele.File{FileID: "group-photo", FileSize: int64(len(photo))}},
	}}
	if err := b.handlePhoto(b.tg.NewContext(photoUpdate)); err != nil {
		t.Fatal(err)
	}
	if edit := telegram.nextFinal(t); edit.Text != answers[0] || edit.MessageID != "1" || edit.ParseMode != "HTML" {
		t.Fatalf("photo reply = %+v", edit)
	}
	photoHistory := waitEinoReplyHistory(t, b, chat.ID, 2)
	firstRequest := <-requests
	if len(firstRequest.Messages) != 2 || !reflect.DeepEqual(firstRequest.Messages[1], photoHistory[0]) {
		t.Fatalf("photo request messages = %+v, stored photo = %+v", firstRequest.Messages, photoHistory[0])
	}
	msg := photoHistory[0]
	if msg.Role != openai.ChatMessageRoleUser || msg.Name != "user_42" || len(msg.MultiContent) != 2 {
		t.Fatalf("stored photo has an unexpected message shape: %+v", msg)
	}
	caption, image := msg.MultiContent[0], msg.MultiContent[1]
	if caption.Type != openai.ChatMessagePartTypeText ||
		!strings.HasPrefix(caption.Text, `[user_id:42 username:@alice name:"Alice" time:`) ||
		!strings.HasSuffix(caption.Text, "]\n[Photo message]\ndescribe this photo") ||
		image.Type != openai.ChatMessagePartTypeImageURL || image.ImageURL == nil ||
		image.ImageURL.URL != "data:image/png;base64,"+pngBase64 || image.ImageURL.Detail != openai.ImageURLDetailAuto {
		t.Fatalf("stored photo parts = %+v, %+v (image data: %+v)", caption, image, image.ImageURL)
	}

	b.mu.Lock()
	b.store = NewHistoryStore(db)
	b.mu.Unlock()
	bob := &tele.User{ID: 43, Username: "bob", FirstName: "Bob"}
	for i, text := range []string{"That photo is from my archive", "@testbot What format was the earlier image?"} {
		update := tele.Update{Message: &tele.Message{ID: 11 + i, Chat: &chat, Sender: bob, Text: text}}
		if err := b.handleText(b.tg.NewContext(update)); err != nil {
			t.Fatal(err)
		}
	}
	if edit := telegram.nextFinal(t); edit.Text != answers[1] || edit.MessageID != "2" || edit.ParseMode != "HTML" {
		t.Fatalf("follow-up reply = %+v", edit)
	}
	history := waitEinoReplyHistory(t, b, chat.ID, 5)
	followUp := <-requests
	if len(followUp.Messages) != 5 || !reflect.DeepEqual(followUp.Messages[1:], history[:4]) || !reflect.DeepEqual(history[:2], photoHistory) {
		t.Fatalf("follow-up lost or changed conversation context: messages = %+v, history = %+v", followUp.Messages, history)
	}
	for i, index := range []int{1, 4} {
		if msg := history[index]; msg.Role != openai.ChatMessageRoleAssistant || msg.Content != answers[i] {
			t.Errorf("stored assistant reply %d = %+v, want %q", i+1, msg, answers[i])
		}
	}
	for i, wantText := range []string{"That photo is from my archive", "What format was the earlier image?"} {
		msg := history[2+i]
		if msg.Role != openai.ChatMessageRoleUser || msg.Name != "user_43" ||
			!strings.HasPrefix(msg.Content, `[user_id:43 username:@bob name:"Bob" time:`) ||
			!strings.HasSuffix(msg.Content, "]\n"+wantText) {
			t.Errorf("group text %d = %+v", i, msg)
		}
	}
	if model.calls.Load() != 2 {
		t.Errorf("group model requests = %d, want 2", model.calls.Load())
	}
}

func TestEinoTaskRunnerUsesGlobalToolsAndRequestedContext(t *testing.T) {
	const chatID, ownerID = int64(42), int64(42)
	task := ScheduledTask{
		ID: "daily", Prompt: "Summarize the archive", CronExpr: "0 8 * * *",
		Timezone: "UTC", Context: true, Enabled: true, CreatedBy: ownerID,
	}
	rem := Reminder{ID: "archive", UserID: ownerID, ChatID: chatID, Prompt: "请复核测试单 EINO-7319。", CreatedBy: "Alice"}
	previous := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Name: "user_42", Content: "Prior archive context"}
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, round int, req openai.ChatCompletionRequest) {
		if !req.Stream || !slices.Equal(einoReplyToolNames(req), []string{"global_lookup"}) {
			t.Errorf("automated request stream = %t, tools = %v", req.Stream, einoReplyToolNames(req))
		}
		wantMessages, wantName, wantPrefix, wantPrompt := 3, "scheduler", `[scheduled_task id:"daily" cron:"0 8 * * *" timezone:"UTC" triggered_at:`, task.Prompt
		switch round {
		case 0:
			if len(req.Messages) > 1 && !reflect.DeepEqual(req.Messages[1], previous) {
				t.Errorf("schedule context = %+v, want %+v", req.Messages[1], previous)
			}
			if len(req.Messages) != 0 && strings.Contains(req.Messages[0].Content, "deliver a scheduled reminder") {
				t.Error("scheduled task received reminder delivery instructions")
			}
		case 1:
			wantMessages, wantName, wantPrefix, wantPrompt = 2, "reminder", `[reminder id:"archive" user:"Alice" triggered_at:`, rem.Prompt
			if len(req.Messages) == 0 || req.Messages[0].Role != openai.ChatMessageRoleSystem ||
				!strings.Contains(req.Messages[0].Content, "deliver a scheduled reminder") ||
				!strings.Contains(req.Messages[0].Content, "Do not perform the action") ||
				!strings.Contains(req.Messages[0].Content, "Preserve identifiers exactly") {
				t.Errorf("reminder system message does not define delivery and identifier preservation: %+v", req.Messages)
			}
		default:
			t.Errorf("unexpected automated model request %d", round+1)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		if len(req.Messages) != wantMessages {
			t.Errorf("%s request messages = %+v, want %d", wantName, req.Messages, wantMessages)
		} else if msg := req.Messages[wantMessages-1]; msg.Role != openai.ChatMessageRoleUser || msg.Name != wantName ||
			!strings.HasPrefix(msg.Content, wantPrefix) || !strings.HasSuffix(msg.Content, "]\n"+wantPrompt) {
			t.Errorf("%s request message = %+v", wantName, msg)
		}
		einoModelSSE(w, einoReplyTextFrame(wantName+" reply"))
		einoReplyFinish(w, "stop")
	})
	global, personal := NewToolRegistry(), NewToolRegistry()
	addEinoTestMCPServer(t, global, "global", einoTestMCPTool("global_lookup", "global result", ""))
	addEinoTestMCPServer(t, personal, "personal", einoTestMCPTool("personal_lookup", "personal result", ""))
	userTools := NewUserToolManager(nil, nil, nil)
	userTools.registries[ownerID] = personal

	telegram := newEinoReplyTelegramForChat(t, tele.Chat{ID: chatID, Type: tele.ChatPrivate}, nil,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/bottest-token/getChat" {
				t.Errorf("unexpected Telegram API request %s", r.URL.Path)
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"ok":true,"result":{"id":%d,"type":"private"}}`, chatID)
		}))
	snap := model.snapshot(3)
	b := &Bot{
		cfg: snap.cfg, ai: snap.ai, tg: telegram.bot, store: NewHistoryStore(nil),
		tools: global, userTools: userTools, reminders: NewReminderStore(nil),
		dashboardEvents: NewDashboardEventHub(0),
	}
	b.store.Append(chatID, previous, snap.cfg.ContextMaxMsgs)
	tasks := NewTaskStore(nil)
	tasks.Upsert(chatID, task)
	b.reminders.Insert(rem)
	runner := NewTaskRunner(b, tasks)

	runner.execute(chatID, task)
	if edit := telegram.nextFinal(t); edit.Text != "scheduler reply" || edit.MessageID != "1" || edit.ParseMode != "HTML" {
		t.Fatalf("schedule reply = %+v", edit)
	}
	waitEinoReplyHistory(t, b, chatID, 3)
	scheduled := tasks.List(chatID)
	if len(scheduled) != 1 || scheduled[0].LastRunAt.IsZero() || !scheduled[0].NextRunAt.After(scheduled[0].LastRunAt) || scheduled[0].LastError != "" {
		t.Errorf("schedule after triggering = %+v", scheduled)
	}

	runner.executeReminder(ownerID, rem)
	if edit := telegram.nextFinal(t); edit.Text != "reminder reply" || edit.MessageID != "2" || edit.ParseMode != "HTML" {
		t.Fatalf("reminder reply = %+v", edit)
	}
	waitEinoReplyHistory(t, b, chatID, 5)
	if b.reminders.HasID(ownerID, rem.ID) {
		t.Error("one-shot reminder still present after triggering")
	}
	usage := einoReplyDashboardEvents(b, DashboardEventUsageRecorded)
	if model.calls.Load() != 2 || len(usage) != 2 {
		t.Fatalf("automated model requests = %d, usage events = %+v, want one each per trigger", model.calls.Load(), usage)
	}
	for _, event := range usage {
		if event.ChatID != chatID || event.UserID != 0 || event.Summary != "main_chat prompt=7 completion=3 total=10 success=true" {
			t.Errorf("automated usage = %+v", event)
		}
	}
}
