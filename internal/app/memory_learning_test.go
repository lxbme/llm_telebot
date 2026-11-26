package app

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	tele "gopkg.in/telebot.v3"
)

func TestRoleTurnLearningRevisesAndHonorsOptOut(t *testing.T) {
	requests := make(chan openai.ChatCompletionRequest, 8)
	const first, corrected = "I prefer black tea", "I prefer green tea"
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
		requests <- req
		if req.Stream {
			einoModelSSE(w, einoReplyTextFrame("Okay."))
			einoReplyFinish(w, "stop")
			return
		}
		material := req.Messages[len(req.Messages)-1].Content
		if strings.Contains(material, "green tea") {
			if !strings.Contains(material, `"editable_conclusions_in_number_order":["I prefer black tea"]`) {
				t.Errorf("reflection did not receive its local update candidate: %s", material)
			}
			roleCompletion(w, `{"kind":"correction","conclusion":"I prefer green tea","replace":1,"cue":"trusted"}`)
			return
		}
		roleCompletion(w, `{"kind":"statement","conclusion":"I prefer black tea","cue":"familiar"}`)
	})
	b, deliveries := newRoleTestBot(t, model)
	for i, statement := range []string{first, corrected} {
		roleHandle(t, b, b.handleText, roleMessage(42, 42, i+1, statement))
		roleReceive(t, requests)
		roleReceive(t, deliveries)
		roleReceive(t, requests)
		entry := waitRoleMemory(t, b, statement)
		if entry.Ref.Version != uint64(i+1) || len(entry.Evidence) != i+1 {
			t.Fatalf("expected independent evidence on the same memory, got %+v", entry)
		}
	}
	cfg := b.currentConfig()
	cfg.ProfileEnabled = false
	if err := b.applyRuntimeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	roleHandle(t, b, b.handleText, roleMessage(42, 42, 3, "I like jasmine"))
	if req := roleReceive(t, requests); !req.Stream || !strings.Contains(fmt.Sprint(req.Messages), corrected) {
		t.Errorf("opt-out dropped prior visible memory from chat: %+v", req.Messages)
	}
	roleReceive(t, deliveries)
	// Disabled reviews are discarded synchronously before the reply starts.
	waitEinoReplyHistory(t, b, 42, 6)
	if got := model.calls.Load(); got != 5 {
		t.Fatalf("model requests after opt-out = %d, want five", got)
	}
}

func TestMemoryAdminDeleteWinsInFlightReflection(t *testing.T) {
	reflectionStarted, release := make(chan struct{}), make(chan struct{})
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, r *http.Request, _ int, req openai.ChatCompletionRequest) {
		if req.Stream {
			einoModelSSE(w, einoReplyTextFrame("Okay."))
			einoReplyFinish(w, "stop")
			return
		}
		close(reflectionStarted)
		select {
		case <-release:
		case <-r.Context().Done():
		case <-t.Context().Done():
		}
		// Even if a stale model proposes a new row instead of updating the old
		// one, the pre-delete reflection must not restore deleted material.
		roleCompletion(w, `{"kind":"statement","conclusion":"deleted material"}`)
	})
	b, deliveries := newRoleTestBot(t, model)
	b.cfg.AdminIDs = map[int64]bool{42: true}
	ref, err := b.memory.Learn(MemoryLearn{Subjects: []int64{42}, Text: "deleted material",
		Evidence: MemoryEvidence{Kind: MemoryStatement, Source: MemorySource{ChatID: 42, SenderID: 42, MessageID: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	b.registerHandlers()
	b.tg.ProcessUpdate(tele.Update{Message: roleMessage(42, 42, 2, "reflect on this")})
	roleReceive(t, deliveries)
	roleReceive(t, reflectionStarted)
	b.tg.ProcessUpdate(tele.Update{Message: roleMessage(42, 42, 3, fmt.Sprintf("/memory delete %s %d", ref.ID, ref.Version))})
	if got := roleReceive(t, deliveries); got.text != "Memory deleted." {
		t.Fatalf("delete reply = %+v", got)
	}
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for b.memoryReviews.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	entries, err := b.memory.List("", 10)
	if b.memoryReviews.Load() != 0 || err != nil || len(entries) != 0 {
		t.Fatalf("stale reflection restored deleted material: entries=%v err=%v", entries, err)
	}
}

func TestRoleTurnMediaSource(t *testing.T) {
	const pngBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jP1sAAAAASUVORK5CYII="
	png, err := base64.StdEncoding.DecodeString(pngBase64)
	if err != nil {
		t.Fatal(err)
	}
	files := http.NewServeMux()
	files.HandleFunc("POST /botrole/getFile", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("invalid file request: %v", err)
		}
		fmt.Fprintf(w, `{"ok":true,"result":{"file_id":%q,"file_path":%q}}`, body["file_id"], body["file_id"])
	})
	files.HandleFunc("GET /file/botrole/photo", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(png)
	})
	files.HandleFunc("GET /file/botrole/voice", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "Local audio fixture")
	})
	files.HandleFunc("GET /file/botrole/doc", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ATTACHMENT-ONLY-SECRET")
	})

	stt := newLocalTranscriber(t)
	requests := make(chan openai.ChatCompletionRequest, 6)
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
		requests <- req
		if req.Stream {
			einoModelSSE(w, einoReplyTextFrame("Media reply"))
			einoReplyFinish(w, "stop")
			return
		}
		var data struct {
			Statement string `json:"latest_sender_message"`
		}
		raw := req.Messages[len(req.Messages)-1].Content
		if err := json.Unmarshal([]byte(raw), &data); err != nil || strings.Contains(raw, "ATTACHMENT-ONLY-SECRET") {
			t.Errorf("invalid reflection input: %s (%v)", raw, err)
		}
		reply, _ := json.Marshal(map[string]string{"kind": "statement", "conclusion": data.Statement})
		roleCompletion(w, string(reply))
	})
	b, deliveries := newRoleTestBot(t, model, files)
	b.stt = stt
	b.cfg.VisionEnabled, b.cfg.VisionMaxImageBytes = true, 1024
	b.cfg.DocumentEnabled, b.cfg.DocumentMaxBytes, b.cfg.DocumentMaxTextChars = true, 1024, 1024
	b.cfg.DocumentAllowedExts = "txt"
	voice := roleMessage(42, 42, 10, "")
	voice.Voice = &tele.Voice{File: tele.File{FileID: "voice"}}
	photo := roleMessage(42, 42, 11, "")
	photo.Photo, photo.Caption = &tele.Photo{File: tele.File{FileID: "photo"}}, "Photo caption"
	doc := roleMessage(42, 42, 12, "")
	doc.Document, doc.Caption = &tele.Document{File: tele.File{FileID: "doc"}, FileName: "archive.txt"}, "Document caption"
	for _, test := range []struct {
		message  *tele.Message
		handler  func(tele.Context) error
		evidence string
	}{
		{voice, b.handleVoice, "Voice transcript"},
		{photo, b.handlePhoto, "Photo caption"},
		{doc, b.handleDocument, "Document caption"},
	} {
		roleHandle(t, b, test.handler, test.message)
		req := roleReceive(t, requests)
		current := req.Messages[len(req.Messages)-1]
		if !req.Stream || current.Name != "user_42" || !strings.Contains(userMessageText(current), test.evidence) {
			t.Errorf("media source did not reach Eino: %+v", current)
		}
		roleReceive(t, deliveries)
		roleReceive(t, requests)
		entry := waitRoleMemory(t, b, test.evidence)
		got := entry.Evidence[0].Source
		if got.ChatID != 42 || got.SenderID != 42 || got.MessageID != int64(test.message.ID) {
			t.Errorf("media evidence source = %+v", got)
		}
	}
}

func newLocalTranscriber(t *testing.T) *STTClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			t.Errorf("unexpected STT path %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"text":"Voice transcript"}`)
	}))
	t.Cleanup(server.Close)
	return NewSTTClient(Config{STTEnabled: true, STTAPIBase: server.URL + "/v1", STTAPIKey: "local"})
}
