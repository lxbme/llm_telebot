package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

func TestEngagementTextReceipt(t *testing.T) {
	model := newEinoReplyProvider(t, func(http.ResponseWriter, *http.Request, int, openai.ChatCompletionRequest) {
		t.Error("text receipt validation called the model")
	})
	const request = "Line one\r\nLine two"
	const normalized = "Line one\nLine two"
	long := strings.Repeat("界", telegramMessageLimit+5)
	for _, tc := range []struct {
		name, input, sent, result string
		chatID                    int64
		extra                     map[string]any
		want                      EngagementResultState
	}{
		{name: "actual normalized text", input: request, sent: request, result: normalized, chatID: engagementOwner, want: EngagementSucceeded},
		{name: "actual truncated text", input: long, sent: truncateTelegramMessage(long), result: truncateTelegramMessage(long), chatID: engagementOwner, want: EngagementSucceeded},
		{name: "different text", input: request, sent: request, result: "Different body", chatID: engagementOwner, want: EngagementUnknown},
		{name: "missing text", input: request, sent: request, chatID: engagementOwner, want: EngagementUnknown},
		{name: "mixed text and voice", input: request, sent: request, result: request, chatID: engagementOwner,
			extra: map[string]any{"voice": map[string]any{"file_id": "voice", "duration": 1}}, want: EngagementUnknown},
		{name: "mixed text and invoice", input: request, sent: request, result: request, chatID: engagementOwner,
			extra: map[string]any{"invoice": map[string]any{"title": "Tea", "description": "Tea order", "start_parameter": "tea", "currency": "USD", "total_amount": 500}}, want: EngagementUnknown},
		{name: "mixed text and payment", input: request, sent: request, result: request, chatID: engagementOwner,
			extra: map[string]any{"successful_payment": map[string]any{"currency": "USD", "total_amount": 500, "invoice_payload": "tea"}}, want: EngagementUnknown},
		{name: "different owner", input: request, sent: request, result: request, chatID: engagementPeer, want: EngagementUnknown},
		{name: "different chat", input: request, sent: request, result: request, chatID: engagementGroup, want: EngagementUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := newRoleTestBotWithTelegram(t, model, func(w http.ResponseWriter, _ *http.Request, d roleTestDelivery) bool {
				if d.path != "/botrole/sendMessage" || d.chatID != engagementOwner || d.text != tc.sent {
					t.Errorf("unexpected request %s to %d (text runes=%d)", d.path, d.chatID, len([]rune(d.text)))
				}
				msg := map[string]any{"message_id": 42, "chat": map[string]any{"id": tc.chatID}}
				if tc.result != "" {
					msg["text"] = tc.result
				}
				for key, value := range tc.extra {
					msg[key] = value
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": msg})
				return true
			})
			state, _, text := b.snapshot().sendEngagementText(t.Context(), engagementOwner, tc.input)
			wantText := ""
			if tc.want == EngagementSucceeded {
				wantText = tc.result
			}
			if state != tc.want || text != wantText {
				t.Fatalf("state=%s want=%s, confirmed text runes=%d want=%d", state, tc.want, len([]rune(text)), len([]rune(wantText)))
			}
		})
	}
}

func TestEngagementProtocolRecoveryAcrossSwitchAndRestart(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	calendarKey := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls[r.Method+" "+r.URL.Path]++
		switch r.Method + " " + r.URL.Path {
		case "POST /calendar":
			calendarKey = r.Header.Get("Idempotency-Key")
			w.WriteHeader(http.StatusBadGateway)
			io.WriteString(w, "response lost after calendar write")
		case "GET /calendar":
			if calendarKey == "" || calendarKey != r.Header.Get("Idempotency-Key") {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, "event 9")
		case "POST /telegram/71":
			io.WriteString(w, "message 11")
		case "POST /telegram/-701":
			if calls["POST /telegram/-701"] == 1 {
				w.WriteHeader(http.StatusTooManyRequests)
				io.WriteString(w, "rejected before send")
			} else {
				io.WriteString(w, "message 12")
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	call := func(method, path, key string, expected int) string {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Idempotency-Key", key)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != expected {
			t.Fatalf("%s %s returned %d", method, path, resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}

	dir := t.TempDir()
	a := openMemoryTestDB(t, filepath.Join(dir, "a.db"))
	bPath := filepath.Join(dir, "b.db")
	b := openMemoryTestDB(t, bPath)
	store := NewEngagementStore(a)
	create := engagementFixture(engagementOwner)
	action := EngagementStep{Action: addEngagementAction(&create)}
	create.Decision.Agreement.Recipients = []int64{engagementOwner, engagementGroup}
	create.Decision.Authorized.Recipients = []int64{engagementOwner, engagementGroup}
	ref := createEngagement(t, store, create)
	now := engagementNow
	claim := engagementClaim(t, store, now)
	observed, _, err := store.Observe(claim, now, EngagementObservation{ChangeKey: "event/9"})
	if err != nil {
		t.Fatal(err)
	}
	begin := func(step EngagementStep, wantStart bool, wantState EngagementResultState) EngagementResult {
		t.Helper()
		r, start, err := store.BeginStep(claim, now, observed.ChangeKey, step)
		if err != nil || start != wantStart || r.State != wantState {
			t.Fatalf("begin %v: %v %v %v", step, r, start, err)
		}
		return r
	}
	settle := func(r EngagementResult, state EngagementResultState, body string) {
		t.Helper()
		if err := store.RecordOutcome(claim, now, observed.ChangeKey, r.Step, r.Attempt, state, body); err != nil {
			t.Fatal(err)
		}
	}
	reserved := begin(action, true, EngagementReserved)
	settle(reserved, EngagementUnknown, call("POST", "/calendar", reserved.Key, 502))
	if err := migrateChatStores(b, nil, nil, nil, store); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	b = openMemoryTestDB(t, bPath)
	store = NewEngagementStore(b)
	now = now.Add(time.Minute)
	claim = engagementClaim(t, store, now)
	pending := begin(action, false, EngagementUnknown)
	if pending.Key != reserved.Key {
		t.Fatal("changed idempotency key across rebind/reopen")
	}
	if err := store.ArchiveOutcome(pending.AttemptRef, now, EngagementSucceeded, call("GET", "/calendar", pending.Key, 200)); err != nil {
		t.Fatal(err)
	}
	var rejected EngagementAttemptRef
	for _, recipient := range []int64{engagementOwner, engagementGroup} {
		r := begin(EngagementStep{Recipient: recipient}, true, EngagementReserved)
		path := fmt.Sprintf("/telegram/%d", recipient)
		if recipient == engagementOwner {
			settle(r, EngagementSucceeded, call("POST", path, r.Key, 200))
		} else {
			rejected = r.AttemptRef
			settle(r, EngagementNoEffect, call("POST", path, r.Key, 429))
		}
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	store = NewEngagementStore(openMemoryTestDB(t, bPath))
	now = now.Add(time.Minute)
	claim = engagementClaim(t, store, now)
	begin(action, false, EngagementSucceeded)
	begin(EngagementStep{Recipient: engagementOwner}, false, EngagementSucceeded)
	retry := begin(EngagementStep{Recipient: engagementGroup}, true, EngagementReserved)
	if err := store.ArchiveOutcome(rejected, now, EngagementSucceeded, "late prior attempt"); !errors.Is(err, ErrEngagementConflict) {
		t.Fatalf("archived prior attempt over a retry: %v", err)
	}
	if err := store.ArchiveOutcome(EngagementAttemptRef{}, now, EngagementSucceeded, "no reservation"); !errors.Is(err, ErrEngagementInvalid) {
		t.Fatalf("archived without a reservation: %v", err)
	}
	settle(retry, EngagementSucceeded, call("POST", "/telegram/-701", retry.Key, 200))
	if retry.Attempt != 2 {
		t.Fatal(retry)
	}
	if err := store.FinishCheck(claim, now, EngagementCheck{CompletedChange: observed.ChangeKey}); err != nil {
		t.Fatal(err)
	}
	if item, err := store.Get(ref.ID); err != nil || item.EndReason != EngagementCompleted {
		t.Fatalf("ended engagement: %v %v", item, err)
	}
	mu.Lock()
	defer mu.Unlock()
	for path, want := range map[string]int{"POST /calendar": 1, "GET /calendar": 1, "POST /telegram/71": 1, "POST /telegram/-701": 2} {
		if calls[path] != want {
			t.Errorf("%s called %d; expected %d", path, calls[path], want)
		}
	}
}
