package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	openai "github.com/sashabaranov/go-openai"
	tele "gopkg.in/telebot.v3"
)

const (
	einoReplyUsageFrame  = `{"model":"provider-model","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`
	einoReplyLimitPrompt = "(You have reached the maximum number of tool calls. Based on the information gathered so far, please provide the best answer you can. If some information is uncertain or incomplete, clearly say so.)"
)

func TestEinoReplyRounds(t *testing.T) {
	for _, tt := range []struct {
		name     string
		max      int
		calls    [][]string
		final    string
		capped   bool
		rejected bool
	}{
		{name: "direct", max: 3, final: "**Answer**"},
		{name: "one tool then answer", max: 3, calls: [][]string{{"call-a"}}, final: "**Answer**"},
		{name: "one round allows a batch", max: 1, calls: [][]string{{"call-a", "call-b"}}, final: "**Answer**", capped: true},
		{name: "multiple rounds", max: 2, calls: [][]string{{"call-a"}, {"call-b"}}, final: "**Answer**", capped: true},
		{name: "reject calls after the cap", max: 1, calls: [][]string{{"call-a"}}, capped: true, rejected: true},
		{name: "empty final uses tool results", max: 3, calls: [][]string{{"call-a"}}},
		{name: "empty direct answer", max: 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var toolRuns atomic.Int32
			registry := newEinoReplyRegistry(t, &toolRuns)
			set, err := newEinoTools(NewMergedToolView(registry, nil), false)
			if err != nil {
				t.Fatal(err)
			}
			server := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, round int, req openai.ChatCompletionRequest) {
				if got := req.Messages[0].Content; got != `Keep {"literal":true}` {
					t.Errorf("system prompt = %q", got)
				}
				if !req.Stream || req.StreamOptions == nil || !req.StreamOptions.IncludeUsage {
					t.Errorf("round %d is missing streaming usage options", round+1)
				}
				wantTools := []string{"lookup"}
				if tt.capped && round == len(tt.calls) {
					wantTools = nil
				}
				if got := einoReplyToolNames(req); !reflect.DeepEqual(got, wantTools) {
					t.Errorf("round %d tools = %v, want %v", round+1, got, wantTools)
				}
				var previousIDs []string
				for _, batch := range tt.calls[:min(round, len(tt.calls))] {
					previousIDs = append(previousIDs, batch...)
				}
				assertEinoReplyTranscript(t, req.Messages, previousIDs, tt.capped && round == len(tt.calls))
				switch {
				case round < len(tt.calls):
					for index, id := range tt.calls[round] {
						einoModelSSE(w, einoReplyToolFrame(id, "lookup", index))
					}
					einoReplyFinish(w, "tool_calls")
				case round == len(tt.calls) && tt.rejected:
					einoModelSSE(w, einoReplyToolFrame("rejected", "lookup", 0))
					einoReplyFinish(w, "tool_calls")
				case round == len(tt.calls):
					einoModelSSE(w, einoReplyTextFrame(tt.final))
					einoReplyFinish(w, "stop")
				default:
					t.Errorf("unexpected model round %d", round+1)
					http.Error(w, "unexpected round", http.StatusBadRequest)
				}
			})
			b := &Bot{dashboardEvents: NewDashboardEventHub(0)}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			got, err := b.runEinoReply(ctx, server.snapshot(tt.max), newEinoReplyMessages(), set, einoReplyUsageContext(), nil, nil)
			if tt.rejected {
				if got != "" || !errors.Is(err, errEinoUnexpectedToolCalls) {
					t.Fatalf("reply = %q, %v, want tool-free request rejection", got, err)
				}
			} else {
				want := tt.final
				if want == "" {
					want = "⚠️ The model returned an empty response."
					if len(tt.calls) != 0 {
						want = "⚠️ The model did not generate a final reply. Here are the raw tool results for reference (information may be incomplete):\n\nresult 1"
					}
				}
				if got != want || err != nil {
					t.Fatalf("reply = %q, %v, want %q", got, err, want)
				}
			}
			wantRuns := 0
			for _, batch := range tt.calls {
				wantRuns += len(batch)
			}
			if got := int(toolRuns.Load()); got != wantRuns {
				t.Errorf("MCP calls = %d, want %d", got, wantRuns)
			}
			if got := int(server.calls.Load()); got != len(tt.calls)+1 {
				t.Errorf("model requests = %d, want %d", got, len(tt.calls)+1)
			}
			assertEinoReplyUsage(t, b, len(tt.calls), !tt.rejected, 10)
		})
	}
}

func TestEinoReplyClearsProvisionalText(t *testing.T) {
	var toolRuns atomic.Int32
	set, err := newEinoTools(NewMergedToolView(newEinoReplyRegistry(t, &toolRuns), nil), false)
	if err != nil {
		t.Fatal(err)
	}
	provisionalSeen := make(chan struct{})
	server := newEinoReplyProvider(t, func(w http.ResponseWriter, r *http.Request, round int, req openai.ChatCompletionRequest) {
		switch round {
		case 0:
			einoModelSSE(w, einoReplyTextFrame("draft"))
			select {
			case <-provisionalSeen:
			case <-r.Context().Done():
				return
			}
			einoModelSSE(w, einoReplyToolFrame("call-a", "lookup", 0), einoReplyTextFrame("discard this too"))
			einoReplyFinish(w, "tool_calls")
		case 1:
			assertEinoReplyTranscript(t, req.Messages, []string{"call-a"}, false)
			einoModelSSE(w, einoReplyTextFrame("final "), einoReplyTextFrame("answer"))
			einoReplyFinish(w, "stop")
		default:
			t.Errorf("unexpected model round %d", round+1)
			http.Error(w, "unexpected round", http.StatusBadRequest)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	b := &Bot{}
	var previews []string
	reply, err := b.runEinoReply(ctx, server.snapshot(3), newEinoReplyMessages(), set, einoReplyUsageContext(), func(text string) {
		previews = append(previews, text)
		if text == "draft" {
			close(provisionalSeen)
		}
	}, nil)
	if reply != "final answer" || err != nil {
		t.Fatalf("reply = %q, %v", reply, err)
	}
	if want := []string{"draft", "", "final ", "final answer"}; !reflect.DeepEqual(previews, want) {
		t.Errorf("previews = %q, want %q", previews, want)
	}
	if got := toolRuns.Load(); got != 1 {
		t.Errorf("MCP calls = %d, want 1", got)
	}
}

func TestEinoReplyErrorsStopExecution(t *testing.T) {
	for _, tt := range []struct {
		name   string
		cancel bool
		want   error
	}{
		{name: "incomplete stream with a tool call", want: io.ErrUnexpectedEOF},
		{name: "cancel while waiting on SSE", cancel: true, want: context.Canceled},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var toolRuns atomic.Int32
			set, err := newEinoTools(NewMergedToolView(newEinoReplyRegistry(t, &toolRuns), nil), false)
			if err != nil {
				t.Fatal(err)
			}
			providerClosed := make(chan struct{})
			server := newEinoReplyProvider(t, func(w http.ResponseWriter, r *http.Request, round int, _ openai.ChatCompletionRequest) {
				if round != 0 {
					t.Errorf("unexpected model round %d", round+1)
					http.Error(w, "unexpected round", http.StatusBadRequest)
					return
				}
				einoModelSSE(w, einoReplyTextFrame("incomplete"), einoReplyToolFrame("call-a", "lookup", 0))
				if tt.cancel {
					<-r.Context().Done()
					close(providerClosed)
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			b := &Bot{dashboardEvents: NewDashboardEventHub(0)}
			reply, err := b.runEinoReply(ctx, server.snapshot(3), newEinoReplyMessages(), set, einoReplyUsageContext(), func(string) {
				if tt.cancel {
					cancel()
				}
			}, nil)
			if reply != "" || !errors.Is(err, tt.want) {
				t.Fatalf("reply = %q, %v, want %v", reply, err, tt.want)
			}
			if tt.cancel {
				select {
				case <-providerClosed:
				case <-time.After(5 * time.Second):
					t.Fatal("cancellation did not reach provider")
				}
			}
			if server.calls.Load() != 1 || toolRuns.Load() != 0 {
				t.Errorf("model requests = %d, MCP calls = %d, want 1 and 0", server.calls.Load(), toolRuns.Load())
			}
			usage := einoReplyDashboardEvents(b, DashboardEventUsageRecorded)
			if len(usage) != 1 || usage[0].Success {
				t.Errorf("usage = %+v, want one failed call", usage)
			}
		})
	}
}

func TestEinoReplySummaryTools(t *testing.T) {
	var toolRuns atomic.Int32
	set, err := newEinoTools(NewMergedToolView(newEinoReplyRegistry(t, &toolRuns), nil), true)
	if err != nil {
		t.Fatal(err)
	}
	server := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, round int, req openai.ChatCompletionRequest) {
		var want []string
		switch round {
		case 0:
			want = []string{"get_tool_details"}
			einoModelSSE(w, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"discover","type":"function","function":{"name":"get_tool_details","arguments":"{\"groups\":[\"GLOBAL\"]}"}}]}}]}`)
			einoReplyFinish(w, "tool_calls")
		case 1:
			want = []string{"get_tool_details", "lookup"}
			einoModelSSE(w, einoReplyToolFrame("call-a", "lookup", 0))
			einoReplyFinish(w, "tool_calls")
		case 2:
			einoModelSSE(w, einoReplyTextFrame("answer"))
			einoReplyFinish(w, "stop")
		default:
			t.Errorf("unexpected model round %d", round+1)
			http.Error(w, "unexpected round", http.StatusBadRequest)
		}
		if got := einoReplyToolNames(req); !reflect.DeepEqual(got, want) {
			t.Errorf("round %d tools = %v, want %v", round+1, got, want)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	b := &Bot{}
	reply, err := b.runEinoReply(ctx, server.snapshot(2), newEinoReplyMessages(), set, einoReplyUsageContext(), nil, nil)
	if reply != "answer" || err != nil || server.calls.Load() != 3 || toolRuns.Load() != 1 {
		t.Fatalf("reply = %q, %v, model requests = %d, MCP calls = %d", reply, err, server.calls.Load(), toolRuns.Load())
	}
}

func TestEinoReplyFallbackAfterUnknownTool(t *testing.T) {
	var toolRuns atomic.Int32
	set, err := newEinoTools(NewMergedToolView(newEinoReplyRegistry(t, &toolRuns), nil), false)
	if err != nil {
		t.Fatal(err)
	}
	server := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, round int, req openai.ChatCompletionRequest) {
		switch round {
		case 0:
			einoModelSSE(w, einoReplyToolFrame("call-unknown", "missing", 0))
			einoReplyFinish(w, "tool_calls")
		case 1:
			last := req.Messages[len(req.Messages)-1]
			if last.Role != openai.ChatMessageRoleTool || last.ToolCallID != "call-unknown" || last.Content != `error: unknown tool "missing"` {
				t.Errorf("last model input = %+v, want the unknown tool result", last)
			}
			einoModelSSE(w, einoReplyUsageFrame, "[DONE]")
		default:
			t.Errorf("unexpected model round %d", round+1)
			http.Error(w, "unexpected round", http.StatusBadRequest)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	b := &Bot{}
	reply, err := b.runEinoReply(ctx, server.snapshot(3), newEinoReplyMessages(), set, einoReplyUsageContext(), nil, nil)
	if err != nil || !strings.HasSuffix(reply, "\n\n"+`error: unknown tool "missing"`) {
		t.Fatalf("reply = %q, %v, want fallback with unknown tool result", reply, err)
	}
	if toolRuns.Load() != 0 || server.calls.Load() != 2 {
		t.Errorf("MCP calls = %d, model requests = %d, want 0 and 2", toolRuns.Load(), server.calls.Load())
	}
}

func TestEinoReplyBotRouting(t *testing.T) {
	t.Setenv("CONFIG_FILE", filepath.Join(t.TempDir(), "config.yaml"))
	t.Setenv("EINO_CHAT_ENABLED", "")
	initiallyEnabled := loadConfig().EinoChatEnabled
	if initiallyEnabled {
		t.Fatal("Eino must be off by default")
	}
	t.Setenv("EINO_CHAT_ENABLED", "true")
	enabled := loadConfig().EinoChatEnabled
	if !enabled {
		t.Fatal("EINO_CHAT_ENABLED=true did not enable Eino")
	}

	var toolRuns atomic.Int32
	registry := newEinoReplyRegistry(t, &toolRuns)
	legacy := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, round int, req openai.ChatCompletionRequest) {
		if req.Model != "legacy-model" || req.Messages[0].Content != "old prompt" ||
			!reflect.DeepEqual(einoReplyToolNames(req), []string{"lookup", "personal"}) {
			t.Errorf("legacy request used the wrong snapshot: %+v", req)
		}
		switch {
		case round == 0 && !req.Stream:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"model":"legacy-model","choices":[{"index":0,"message":{"role":"assistant","content":"ignored"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)
		case round == 1 && req.Stream:
			einoModelSSE(w, einoReplyTextFrame("**legacy**"))
			einoReplyFinish(w, "stop")
		default:
			t.Errorf("legacy request %d stream = %t", round+1, req.Stream)
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	})
	var previewSeen, previewCleared <-chan struct{}
	eino := newEinoReplyProvider(t, func(w http.ResponseWriter, r *http.Request, round int, req openai.ChatCompletionRequest) {
		if !req.Stream || req.Model != "eino-model" || !strings.HasPrefix(req.Messages[0].Content, "fresh prompt {literal}") {
			t.Errorf("Eino request used the wrong snapshot: %+v", req)
		}
		switch round {
		case 0:
			if got := einoReplyToolNames(req); !reflect.DeepEqual(got, []string{"lookup", "personal"}) {
				t.Errorf("sender tool view = %v", got)
			}
			einoModelSSE(w, einoReplyTextFrame("**direct**"))
			einoReplyFinish(w, "stop")
		case 1:
			if got := einoReplyToolNames(req); !reflect.DeepEqual(got, []string{"lookup"}) {
				t.Errorf("nil sender tool view = %v", got)
			}
			einoModelSSE(w, einoReplyTextFrame("draft"))
			select {
			case <-previewSeen:
			case <-r.Context().Done():
				return
			case <-t.Context().Done():
				return
			}
			einoModelSSE(w, einoReplyToolFrame("call-a", "lookup", 0))
			einoReplyFinish(w, "tool_calls")
		case 2:
			assertEinoReplyTranscript(t, req.Messages, []string{"call-a"}, false)
			select {
			case <-previewCleared:
			case <-r.Context().Done():
				return
			case <-t.Context().Done():
				return
			}
			einoModelSSE(w, einoReplyTextFrame("**final**"))
			einoReplyFinish(w, "stop")
		default:
			t.Errorf("unexpected Eino request %d", round+1)
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	})

	personal := NewToolRegistry()
	addEinoTestMCPServer(t, personal, "personal", einoTestMCPTool("personal", "personal result", ""))
	userTools := NewUserToolManager(nil, nil, nil)
	userTools.registries[42] = personal

	snap := legacy.snapshot(3)
	snap.cfg.EinoChatEnabled = initiallyEnabled
	snap.cfg.OpenAIModel, snap.cfg.SystemPrompt, snap.cfg.ContextMaxMsgs = "legacy-model", "old prompt", 20
	snap.cfg.ChatDBPath = filepath.Join(t.TempDir(), "unused.db")
	snap.cfg.StickerRulesPath = filepath.Join(t.TempDir(), "sticker_rules.json")
	next := snap.cfg
	next.EinoChatEnabled = enabled
	next.OpenAIKey, next.OpenAIBase, next.OpenAIModel, next.SystemPrompt = "test-key", eino.url+"/v1", "eino-model", "fresh prompt {literal}"

	b := &Bot{
		cfg: snap.cfg, ai: snap.ai, tools: registry, userTools: userTools,
		store: NewHistoryStore(nil), dashboardEvents: NewDashboardEventHub(0),
	}
	telegram := newEinoReplyTelegram(t, func(id int) error {
		if id == 1 {
			return b.applyRuntimeConfig(next)
		}
		return nil
	})
	b.tg = telegram.bot
	previewSeen = telegram.previewSeen
	previewCleared = telegram.previewCleared

	input := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "question"}
	sender := &tele.User{ID: 42}
	chat := &tele.Chat{ID: 41, Type: tele.ChatPrivate}
	for i, tt := range []struct {
		sender *tele.User
		final  string
		calls  []UsageCallType
	}{
		{sender, "legacy", []UsageCallType{UsageCallToolRound, UsageCallMainChat}},
		{sender, "direct", []UsageCallType{UsageCallMainChat}},
		{nil, "final", []UsageCallType{UsageCallToolRound, UsageCallMainChat}},
	} {
		id := i + 1
		if err := b.startChatFlow(chat, tt.sender, input, false); err != nil {
			t.Fatal(err)
		}
		edit := telegram.nextFinal(t)
		if edit.MessageID != fmt.Sprint(id) || edit.Text != "<b>"+tt.final+"</b>" || edit.ParseMode != "HTML" {
			t.Fatalf("final edit = %+v", edit)
		}
		history := waitEinoReplyHistory(t, b, chat.ID, 2*id)
		if history[2*i].Content != "question" || history[2*i+1].Role != openai.ChatMessageRoleAssistant || history[2*i+1].Content != "**"+tt.final+"**" {
			t.Fatalf("unexpected history after request %d: %+v", id, history)
		}
		finished := einoReplyDashboardEvents(b, DashboardEventConversationFinished)
		if len(finished) != id {
			t.Fatalf("conversation finishes = %d, want %d", len(finished), id)
		}
		var usageSummaries []string
		for _, event := range einoReplyDashboardEvents(b, DashboardEventUsageRecorded) {
			if event.RequestID == finished[i].RequestID {
				usageSummaries = append(usageSummaries, event.Summary)
			}
		}
		var wantUsage []string
		for _, callType := range tt.calls {
			wantUsage = append(wantUsage, fmt.Sprintf("%s prompt=7 completion=3 total=10 success=true", callType))
		}
		if !reflect.DeepEqual(usageSummaries, wantUsage) {
			t.Errorf("request %d usage events = %q, want %q", id, usageSummaries, wantUsage)
		}
	}
	if legacy.calls.Load() != 2 || eino.calls.Load() != 3 || toolRuns.Load() != 1 {
		t.Errorf("legacy requests = %d, Eino requests = %d, MCP runs = %d", legacy.calls.Load(), eino.calls.Load(), toolRuns.Load())
	}
	telegram.mu.Lock()
	defer telegram.mu.Unlock()
	wantEdits := []einoReplyEdit{
		{MessageID: "1", Text: "<b>legacy</b>", ParseMode: "HTML"},
		{MessageID: "2", Text: "<b>direct</b>", ParseMode: "HTML"},
		{MessageID: "3", Text: "draft▌"},
		{MessageID: "3", Text: "⏳ Thinking…"},
		{MessageID: "3", Text: "<b>final</b>", ParseMode: "HTML"},
	}
	if !reflect.DeepEqual(telegram.edits, wantEdits) {
		t.Errorf("Telegram edits = %+v, want %+v", telegram.edits, wantEdits)
	}
}

func TestEinoReplyBotDoesNotReplayAfterError(t *testing.T) {
	var toolRuns atomic.Int32
	server := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, round int, req openai.ChatCompletionRequest) {
		if !req.Stream {
			t.Error("failed Eino request was replayed through legacy route")
		}
		switch round {
		case 0:
			einoModelSSE(w, einoReplyToolFrame("call-a", "lookup", 0))
			einoReplyFinish(w, "tool_calls")
		case 1:
			http.Error(w, `{"error":{"message":"provider-secret-fixture"}}`, http.StatusBadGateway)
		default:
			t.Errorf("unexpected model round %d", round+1)
			http.Error(w, "unexpected round", http.StatusBadRequest)
		}
	})
	telegram := newEinoReplyTelegram(t, nil)
	snap := server.snapshot(3)
	snap.cfg.EinoChatEnabled = true
	b := &Bot{
		cfg: snap.cfg, ai: snap.ai, tools: newEinoReplyRegistry(t, &toolRuns), tg: telegram.bot,
		store: NewHistoryStore(nil), dashboardEvents: NewDashboardEventHub(0),
	}
	if err := b.startChatFlow(&tele.Chat{ID: 41, Type: tele.ChatPrivate}, nil, newEinoReplyMessages()[1], false); err != nil {
		t.Fatal(err)
	}
	edit := telegram.nextFinal(t)
	if !strings.HasPrefix(edit.Text, "❌") || strings.Contains(edit.Text, "provider-secret-fixture") {
		t.Errorf("error edit = %+v", edit)
	}
	failed := einoReplyDashboardEvents(b, DashboardEventConversationError)
	if len(failed) != 1 || strings.Contains(fmt.Sprint(failed), "provider-secret-fixture") {
		t.Errorf("conversation errors = %+v", failed)
	}
	if len(einoReplyDashboardEvents(b, DashboardEventConversationFinished)) != 0 || len(b.store.Get(41)) != 0 {
		t.Error("failed reply was recorded as successful")
	}
	if got := len(einoReplyDashboardEvents(b, DashboardEventUsageRecorded)); got != 2 {
		t.Errorf("usage events = %d, want 2", got)
	}
	if server.calls.Load() != 2 || toolRuns.Load() != 1 {
		t.Errorf("model requests = %d, MCP calls = %d, want 2 and 1", server.calls.Load(), toolRuns.Load())
	}
}

func TestEinoReplyBotSpeechMode(t *testing.T) {
	server := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
		if !req.Stream || len(req.Tools) != 0 || !strings.Contains(req.Messages[0].Content, "Speech Mode Instruction") {
			t.Errorf("unexpected voice-only request: %+v", req)
		}
		einoModelSSE(w, einoReplyTextFrame("**spoken answer**"))
		einoReplyFinish(w, "stop")
	})
	var speechCalls atomic.Int32
	ttsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		speechCalls.Add(1)
		var req struct {
			Params struct {
				Text string `json:"text"`
			} `json:"req_params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Params.Text != "spoken answer" {
			t.Errorf("speech payload = %+v, %v", req, err)
		}
		// Exercise the existing text fallback without requiring an audio encoder.
		http.Error(w, "speech unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(ttsServer.Close)
	warningSent := make(chan struct{})
	telegram := newEinoReplyTelegram(t, func(id int) error {
		if id == 2 {
			close(warningSent)
		}
		return nil
	})
	snap := server.snapshot(3)
	snap.cfg.EinoChatEnabled = true
	b := &Bot{
		cfg: snap.cfg, ai: snap.ai, tg: telegram.bot, store: NewHistoryStore(nil),
		speechModes: NewSpeechModeStore(),
		tts:         &VolcengineTTSClient{url: ttsServer.URL, httpClient: ttsServer.Client()},
	}
	chat := &tele.Chat{ID: 41, Type: tele.ChatPrivate}
	b.speechModes.Set(chat.ID, true)
	if err := b.startChatFlow(chat, nil, newEinoReplyMessages()[1], false); err != nil {
		t.Fatal(err)
	}
	if edit := telegram.nextFinal(t); edit.Text != "<b>spoken answer</b>" || edit.ParseMode != "HTML" {
		t.Errorf("speech failure fallback = %+v", edit)
	}
	select {
	case <-warningSent:
	case <-time.After(5 * time.Second):
		t.Fatal("Telegram received no speech failure notice")
	}
	history := waitEinoReplyHistory(t, b, chat.ID, 2)
	if history[1].Content != "**spoken answer**" || speechCalls.Load() != 1 || server.calls.Load() != 1 {
		t.Errorf("history = %+v, speech calls = %d, model requests = %d", history, speechCalls.Load(), server.calls.Load())
	}
	telegram.mu.Lock()
	defer telegram.mu.Unlock()
	if len(telegram.edits) != 2 || telegram.edits[0].Text != "🔊 正在发送语音…" {
		t.Errorf("voice-only edits = %+v, want speech status then text fallback", telegram.edits)
	}
}

type einoReplyProvider struct {
	url    string
	client *openai.Client
	calls  atomic.Int32
}

func newEinoReplyProvider(t *testing.T, handle func(http.ResponseWriter, *http.Request, int, openai.ChatCompletionRequest)) *einoReplyProvider {
	t.Helper()
	p := &einoReplyProvider{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openai.ChatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		handle(w, r, int(p.calls.Add(1))-1, req)
	}))
	t.Cleanup(s.Close)
	cfg := openai.DefaultConfig("test-key")
	cfg.BaseURL, cfg.HTTPClient = s.URL+"/v1", s.Client()
	p.url, p.client = s.URL, openai.NewClientWithConfig(cfg)
	return p
}

func (p *einoReplyProvider) snapshot(maxRounds int) runtimeSnapshot {
	return runtimeSnapshot{
		ai:  p.client,
		cfg: Config{OpenAIModel: "test-model", ToolsMaxIterations: maxRounds, ContextMaxMsgs: 20},
	}
}

func newEinoReplyRegistry(t *testing.T, runs *atomic.Int32) *ToolRegistry {
	t.Helper()
	registry := NewToolRegistry()
	lookup := einoTestMCPTool("lookup", "look up a record", "")
	lookup.run = func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText(fmt.Sprintf("result %d", runs.Add(1))), nil
	}
	addEinoTestMCPServer(t, registry, "global", lookup)
	return registry
}

func newEinoReplyMessages() []openai.ChatCompletionMessage {
	return []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: `Keep {"literal":true}`},
		{Role: openai.ChatMessageRoleUser, Content: "a question"},
	}
}

func einoReplyTextFrame(text string) string {
	return fmt.Sprintf(`{"model":"provider-model","choices":[{"index":0,"delta":{"role":"assistant","content":%q}}]}`, text)
}

func einoReplyToolFrame(id, name string, index int) string {
	return fmt.Sprintf(`{"model":"provider-model","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":%d,"id":%q,"type":"function","function":{"name":%q,"arguments":"{}"}}]}}]}`, index, id, name)
}

func einoReplyFinish(w http.ResponseWriter, reason string) {
	einoModelSSE(w, fmt.Sprintf(`{"model":"provider-model","choices":[{"index":0,"delta":{},"finish_reason":%q}]}`, reason), einoReplyUsageFrame, "[DONE]")
}

func einoReplyToolNames(req openai.ChatCompletionRequest) []string {
	var names []string
	for _, tool := range req.Tools {
		names = append(names, tool.Function.Name)
	}
	return names
}

func assertEinoReplyTranscript(t *testing.T, messages []openai.ChatCompletionMessage, toolIDs []string, capped bool) {
	t.Helper()
	var calls, results []string
	var capPrompts int
	for _, msg := range messages {
		for _, call := range msg.ToolCalls {
			calls = append(calls, call.ID)
			if call.Index != nil || call.Function.Name != "lookup" || call.Function.Arguments != "{}" {
				t.Errorf("wrong assistant tool call: %+v", call)
			}
		}
		if msg.Role == openai.ChatMessageRoleTool {
			results = append(results, msg.ToolCallID)
			if want := fmt.Sprintf("result %d", len(results)); msg.Content != want {
				t.Errorf("tool result for %q = %q, want %q", msg.ToolCallID, msg.Content, want)
			}
		}
		if msg.Role == openai.ChatMessageRoleUser && msg.Content == einoReplyLimitPrompt {
			capPrompts++
		}
	}
	if !reflect.DeepEqual(calls, toolIDs) || !reflect.DeepEqual(results, toolIDs) {
		t.Errorf("assistant call IDs = %v, tool result IDs = %v, want %v", calls, results, toolIDs)
	}
	if capped && (capPrompts != 1 || messages[len(messages)-1].Content != einoReplyLimitPrompt) || !capped && capPrompts != 0 {
		t.Errorf("incorrect round limit prompt in messages: %+v", messages)
	}
}

func einoReplyUsageContext() UsageContext {
	return UsageContext{RequestID: "reply-test", ChatID: 41, UserID: 42, MessageID: 43}
}

func einoReplyDashboardEvents(b *Bot, kind DashboardEventType) []DashboardEvent {
	var events []DashboardEvent
	for _, event := range b.dashboardEvents.Tail(0, 100) {
		if event.Type == kind {
			events = append(events, event)
		}
	}
	return events
}

func assertEinoReplyUsage(t *testing.T, b *Bot, toolRounds int, finalSuccess bool, tokens int) {
	t.Helper()
	events := einoReplyDashboardEvents(b, DashboardEventUsageRecorded)
	if len(events) != toolRounds+1 {
		t.Fatalf("usage events = %+v, want %d calls", events, toolRounds+1)
	}
	for i, event := range events {
		callType, success := UsageCallToolRound, true
		if i == toolRounds {
			callType, success = UsageCallMainChat, finalSuccess
		}
		want := fmt.Sprintf("%s prompt=7 completion=3 total=%d success=%t", callType, tokens, success)
		if event.Summary != want || event.Success != success || event.Model != "provider-model" ||
			event.ChatID != 41 || event.UserID != 42 || event.RequestID != "reply-test" {
			t.Errorf("usage %d = %+v, want %q with request metadata", i+1, event, want)
		}
	}
}

type einoReplyEdit struct {
	MessageID string `json:"message_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode"`
}

type einoReplyTelegram struct {
	bot            *tele.Bot
	final          chan einoReplyEdit
	previewSeen    chan struct{}
	previewCleared chan struct{}
	mu             sync.Mutex
	edits          []einoReplyEdit
}

func newEinoReplyTelegram(t *testing.T, onSend func(int) error) *einoReplyTelegram {
	t.Helper()
	return newEinoReplyTelegramForChat(t, tele.Chat{ID: 41, Type: tele.ChatPrivate}, onSend, nil)
}

func newEinoReplyTelegramForChat(t *testing.T, chat tele.Chat, onSend func(int) error, extraRoutes http.Handler) *einoReplyTelegram {
	t.Helper()
	tg := &einoReplyTelegram{
		final:          make(chan einoReplyEdit, 8),
		previewSeen:    make(chan struct{}),
		previewCleared: make(chan struct{}),
	}
	var messages atomic.Int32
	var previewOnce, clearOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var id string
		switch r.URL.Path {
		case "/bottest-token/sendMessage":
			var send struct {
				ChatID string `json:"chat_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&send); err != nil || send.ChatID != chat.Recipient() {
				t.Errorf("Telegram send chat_id = %q, want %q, decode error: %v", send.ChatID, chat.Recipient(), err)
				http.Error(w, "invalid send", http.StatusBadRequest)
				return
			}
			messageID := int(messages.Add(1))
			id = fmt.Sprint(messageID)
			if onSend != nil {
				if err := onSend(messageID); err != nil {
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": err.Error()})
					return
				}
			}
		case "/bottest-token/editMessageText":
			var payload struct {
				einoReplyEdit
				ChatID string `json:"chat_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.ChatID != chat.Recipient() {
				t.Errorf("Telegram edit chat_id = %q, want %q, decode error: %v", payload.ChatID, chat.Recipient(), err)
				http.Error(w, "invalid edit", http.StatusBadRequest)
				return
			}
			edit := payload.einoReplyEdit
			id = edit.MessageID
			tg.mu.Lock()
			tg.edits = append(tg.edits, edit)
			tg.mu.Unlock()
			if edit.Text == "draft▌" {
				previewOnce.Do(func() { close(tg.previewSeen) })
			}
			if edit.Text == "⏳ Thinking…" {
				clearOnce.Do(func() { close(tg.previewCleared) })
			}
			if edit.ParseMode == "HTML" || strings.HasPrefix(edit.Text, "❌") {
				tg.final <- edit
			}
		default:
			if extraRoutes != nil {
				extraRoutes.ServeHTTP(w, r)
				return
			}
			t.Errorf("unexpected Telegram API request %s", r.URL.Path)
			http.Error(w, "unexpected Telegram request", http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%s,"chat":{"id":%d,"type":%q}}}`, id, chat.ID, chat.Type)
	}))
	t.Cleanup(server.Close)
	var err error
	tg.bot, err = tele.NewBot(tele.Settings{URL: server.URL, Token: "test-token", Client: server.Client(), Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	return tg
}

func (tg *einoReplyTelegram) nextFinal(t *testing.T) einoReplyEdit {
	t.Helper()
	select {
	case edit := <-tg.final:
		return edit
	case <-time.After(5 * time.Second):
		t.Fatal("Telegram received no final edit")
		return einoReplyEdit{}
	}
}

func waitEinoReplyHistory(t *testing.T, b *Bot, chatID int64, size int) []openai.ChatCompletionMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		history := b.store.Get(chatID)
		if len(history) == size {
			return history
		}
		if len(history) > size || time.Now().After(deadline) {
			t.Fatalf("history contains %d messages, want %d", len(history), size)
		}
		time.Sleep(time.Millisecond)
	}
}
