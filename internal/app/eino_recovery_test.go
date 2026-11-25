package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	tele "gopkg.in/telebot.v3"
)

const einoErrorSecret = "sk-provider-error-secret"

func einoTestProviderError(w http.ResponseWriter, status int) {
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":{"message":%q}}`, einoErrorSecret)
}

func einoRecoveryConfig(t *testing.T, retry, backup string) Config {
	t.Helper()
	t.Setenv("CONFIG_FILE", filepath.Join(t.TempDir(), "config.yaml"))
	t.Setenv("EINO_CHAT_ENABLED", "true")
	t.Setenv("EINO_MODEL_RETRY_ENABLED", retry)
	t.Setenv("EINO_FALLBACK_MODEL", backup)
	t.Setenv("OPENAI_MODEL", "test-model")
	t.Setenv("MAX_TOKENS", "321")
	return loadConfig()
}

func TestEinoReplyRecoveryPolicy(t *testing.T) {
	for _, tc := range []struct {
		name          string
		retry, backup string
		statuses      []int
		models        []string
	}{
		{name: "default single request", statuses: []int{0}, models: []string{"test-model"}},
		{name: "default does not retry", statuses: []int{429}, models: []string{"test-model"}},
		{name: "retry rate limit", retry: " TRUE ", statuses: []int{429, 0}, models: []string{"test-model", "test-model"}},
		{name: "only one retry", retry: "true", statuses: []int{429, 429}, models: []string{"test-model", "test-model"}},
		{name: "backup after 503", backup: " gpt-5-mini ", statuses: []int{503, 0}, models: []string{"test-model", "gpt-5-mini"}},
		{name: "retry then backup", retry: "true", backup: "gpt-5-mini", statuses: []int{503, 429, 503}, models: []string{"test-model", "test-model", "gpt-5-mini"}},
		{name: "backup duplicates primary", backup: "test-model", statuses: []int{503}, models: []string{"test-model"}},
		{name: "hard failure on primary", retry: "true", backup: "gpt-5-mini", statuses: []int{401}, models: []string{"test-model"}},
		{name: "hard failure on retry", retry: "true", backup: "gpt-5-mini", statuses: []int{503, 400}, models: []string{"test-model", "test-model"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := einoRecoveryConfig(t, tc.retry, tc.backup)
			b := &Bot{dashboardEvents: NewDashboardEventHub(0)}
			server := newEinoReplyProvider(t, func(w http.ResponseWriter, r *http.Request, i int, req openai.ChatCompletionRequest) {
				if i >= len(tc.models) {
					t.Errorf("unexpected attempt %d", i+1)
					einoTestProviderError(w, http.StatusBadRequest)
					return
				}
				if req.Model != tc.models[i] || !req.Stream || r.Header.Get("Authorization") != "Bearer test-key" {
					t.Errorf("attempt %d model = %q, stream = %t, wrong authorization = %t", i+1, req.Model, req.Stream, r.Header.Get("Authorization") != "Bearer test-key")
				}
				if req.Model == "gpt-5-mini" && (req.MaxCompletionTokens != 321 || req.MaxTokens != 0) ||
					req.Model == "test-model" && (req.MaxTokens != 321 || req.MaxCompletionTokens != 0) {
					t.Errorf("attempt %d has incorrect token limit for %q", i+1, req.Model)
				}
				if tc.statuses[i] != 0 {
					einoTestProviderError(w, tc.statuses[i])
					return
				}
				einoModelSSE(w, einoReplyTextFrame("answer"))
				einoReplyFinish(w, "stop")
			})
			snap := server.snapshot(0)
			snap.cfg = cfg
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			reply, err := b.runEinoReply(ctx, snap, newEinoReplyMessages(), nil, einoReplyUsageContext(), nil, nil)
			if tc.statuses[len(tc.statuses)-1] == 0 {
				if reply != "answer" || err != nil {
					t.Errorf("reply = %q, error = %v, want recovered answer", reply, err)
				}
			} else if reply != "" || err == nil || strings.Contains(fmt.Sprint(err), einoErrorSecret) {
				t.Error("expected a sanitized terminal error and an empty reply")
			}
			if got := int(server.calls.Load()); got != len(tc.models) {
				t.Errorf("model requests = %d, want %d", got, len(tc.models))
			}
			events := einoReplyDashboardEvents(b, DashboardEventUsageRecorded)
			if len(events) != len(tc.models) {
				t.Fatalf("usage events = %+v, want %d", events, len(tc.models))
			}
			for i, event := range events {
				model, summary := tc.models[i], "main_chat prompt=0 completion=0 total=0 success=false"
				if tc.statuses[i] == 0 {
					model, summary = "provider-model", "main_chat prompt=7 completion=3 total=10 success=true"
				}
				if event.Model != model || event.Summary != summary || event.RequestID != "reply-test" {
					t.Errorf("usage event %d = %+v, want %q for %q", i, event, summary, model)
				}
			}
		})
	}
}

func TestEinoReplyRecoveryClearsTelegramAndKeepsToolSideEffectOnce(t *testing.T) {
	cfg := einoRecoveryConfig(t, "false", "backup-model")
	cfg.ToolsMaxIterations, cfg.ToolsEnabled = 2, true
	cfg.SummaryEnabled, cfg.ProfileEnabled = false, false
	var toolRuns atomic.Int32
	registry := newEinoReplyRegistry(t, &toolRuns)
	telegram := newEinoReplyTelegram(t, nil)
	// A failed round contains a partial tool call, while the prior successful
	// round already executed a tool. Neither should cause another execution.
	server := newEinoReplyProvider(t, func(w http.ResponseWriter, r *http.Request, i int, req openai.ChatCompletionRequest) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("provider authorization changed during recovery")
		}
		switch i {
		case 0:
			einoModelSSE(w, einoReplyToolFrame("call-a", "lookup", 0))
			einoReplyFinish(w, "tool_calls")
		case 1:
			assertEinoReplyTranscript(t, req.Messages, []string{"call-a"}, false)
			w.Header().Set("Content-Length", "4096")
			einoModelSSE(w, `{"model":"interrupted-model","choices":[{"delta":{"content":"draft"}}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`)
			select {
			case <-telegram.previewSeen:
			case <-r.Context().Done():
				return
			case <-time.After(4 * time.Second):
				t.Error("Telegram did not show the provisional draft")
				return
			}
			einoModelSSE(w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"discarded","type":"function","function":{"name":"lookup","arguments":"{"}}]}}]}`)
		case 2:
			if req.Model != "backup-model" || !reflect.DeepEqual(einoReplyToolNames(req), []string{"lookup"}) {
				t.Errorf("recovered round on %q has tools %v", req.Model, einoReplyToolNames(req))
			}
			assertEinoReplyTranscript(t, req.Messages, []string{"call-a"}, false)
			einoModelSSE(w, einoReplyToolFrame("call-b", "lookup", 0))
			einoReplyFinish(w, "tool_calls")
		case 3:
			if req.Model != "test-model" || len(req.Tools) != 0 {
				t.Errorf("final round on %q has tools %v", req.Model, einoReplyToolNames(req))
			}
			assertEinoReplyTranscript(t, req.Messages, []string{"call-a", "call-b"}, true)
			einoModelSSE(w, einoReplyTextFrame("**recovered**"))
			einoReplyFinish(w, "stop")
		default:
			t.Errorf("unexpected model request %d", i+1)
			einoTestProviderError(w, http.StatusBadRequest)
		}
	})
	b := &Bot{
		cfg: cfg, ai: server.client, tg: telegram.bot, tools: registry,
		store: NewHistoryStore(nil), dashboardEvents: NewDashboardEventHub(0),
	}
	chat := &tele.Chat{ID: 41, Type: tele.ChatPrivate}
	input := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "question"}
	if err := b.startChatFlow(chat, &tele.User{ID: 42}, input, false); err != nil {
		t.Fatal(err)
	}
	if edit := telegram.nextFinal(t); edit.Text != "<b>recovered</b>" {
		t.Fatalf("Telegram final edit = %+v", edit)
	}
	history := waitEinoReplyHistory(t, b, chat.ID, 2)
	if history[0].Content != "question" || history[1].Content != "**recovered**" {
		t.Errorf("persisted history = %+v", history)
	}
	telegram.mu.Lock()
	defer telegram.mu.Unlock()
	wantEdits := []einoReplyEdit{
		{MessageID: "1", Text: "draft▌"},
		{MessageID: "1", Text: "⏳ Thinking…"},
		{MessageID: "1", Text: "<b>recovered</b>", ParseMode: "HTML"},
	}
	if !reflect.DeepEqual(telegram.edits, wantEdits) {
		t.Errorf("Telegram edits = %+v, want %+v", telegram.edits, wantEdits)
	}
	if toolRuns.Load() != 2 || server.calls.Load() != 4 {
		t.Errorf("MCP calls = %d, model requests = %d, want 2 and 4", toolRuns.Load(), server.calls.Load())
	}
	events := einoReplyDashboardEvents(b, DashboardEventUsageRecorded)
	if len(events) != 4 {
		t.Fatalf("usage = %+v, want four attempts", events)
	}
	for i, want := range []string{
		"tool_round prompt=7 completion=3 total=10 success=true",
		"tool_round prompt=7 completion=2 total=9 success=false",
		"tool_round prompt=7 completion=3 total=10 success=true",
		"main_chat prompt=7 completion=3 total=10 success=true",
	} {
		model := "provider-model"
		if i == 1 {
			model = "interrupted-model"
		}
		if events[i].Summary != want || events[i].Model != model || events[i].RequestID != events[0].RequestID {
			t.Errorf("usage %d = %+v, want %s for %s", i, events[i], want, model)
		}
	}
}

func TestEinoReplyRecoveryStopsAtCancellationOrProtocolFailure(t *testing.T) {
	for _, tc := range []struct {
		name         string
		afterPreview string
		cancel       bool
		interrupted  bool
	}{
		{name: "caller cancellation", cancel: true},
		{name: "malformed stream", afterPreview: `{"choices":{"sk-provider-error-secret":true}}`},
		{name: "provider error payload", afterPreview: `{"error":{"message":"sk-provider-error-secret"}}`},
		{name: "clean truncated protocol"},
		{name: "invalid role", afterPreview: `{"choices":[{"delta":{"role":"sk-provider-error-secret","content":"bad"},"finish_reason":"stop"}]}`},
		{name: "conflicting deltas before transport failure", interrupted: true, afterPreview: einoReplyToolFrame("a", "lookup", 0)},
		{name: "invalid tool arguments", afterPreview: `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"invalid","type":"function","function":{"name":"lookup","arguments":"{"}}]},"finish_reason":"tool_calls"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := einoRecoveryConfig(t, "true", "backup-model")
			var toolRuns atomic.Int32
			tools, err := newEinoTools(NewMergedToolView(newEinoReplyRegistry(t, &toolRuns), nil), false)
			if err != nil {
				t.Fatal(err)
			}
			server := newEinoReplyProvider(t, func(w http.ResponseWriter, r *http.Request, i int, _ openai.ChatCompletionRequest) {
				if i != 0 {
					t.Error("cancellation or invalid protocol caused recovery")
					einoTestProviderError(w, http.StatusBadRequest)
					return
				}
				if tc.interrupted {
					w.Header().Set("Content-Length", "4096")
				}
				einoModelSSE(w, einoReplyTextFrame("draft"))
				if tc.cancel {
					<-r.Context().Done()
				} else if tc.interrupted {
					einoModelSSE(w, tc.afterPreview, einoReplyToolFrame("b", "lookup", 0))
				} else if tc.afterPreview != "" {
					einoModelSSE(w, tc.afterPreview, einoReplyUsageFrame, "[DONE]")
				}
			})
			var logs bytes.Buffer
			logOutput := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(logOutput) })
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			b := &Bot{dashboardEvents: NewDashboardEventHub(0)}
			snap := server.snapshot(2)
			snap.cfg = cfg
			var previews []string
			reply, err := b.runEinoReply(ctx, snap, newEinoReplyMessages(), tools, einoReplyUsageContext(), func(text string) {
				previews = append(previews, text)
				if tc.cancel && text == "draft" {
					cancel()
				}
			}, nil)
			if reply != "" || err == nil || tc.cancel && !errors.Is(err, context.Canceled) {
				t.Errorf("reply = %q, error = %v, want terminal failure", reply, err)
			}
			if strings.Contains(fmt.Sprint(err), einoErrorSecret) || strings.Contains(logs.String(), einoErrorSecret) {
				t.Error("upstream error payload escaped")
			}
			if !reflect.DeepEqual(previews, []string{"draft", ""}) {
				t.Errorf("previews = %q, want draft followed by a reset", previews)
			}
			if toolRuns.Load() != 0 || server.calls.Load() != 1 {
				t.Errorf("MCP calls = %d, model requests = %d, want 0 and 1", toolRuns.Load(), server.calls.Load())
			}
		})
	}
}
