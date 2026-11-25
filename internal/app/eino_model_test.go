package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	openai "github.com/sashabaranov/go-openai"
)

func TestEinoModelRequestCompatibility(t *testing.T) {
	index := 4
	history := []openai.ChatCompletionMessage{
		{Role: "system", Content: `Keep {"literal": "{value}"} as written.`},
		{Role: "user", Name: "alice", MultiContent: []openai.ChatMessagePart{
			{Type: openai.ChatMessagePartTypeText, Text: "what is this?"},
			{Type: openai.ChatMessagePartTypeImageURL, ImageURL: &openai.ChatMessageImageURL{URL: "data:image/png;base64,dGVzdA==", Detail: "high"}},
		}},
		{Role: "assistant", ToolCalls: []openai.ToolCall{{
			Index: &index, ID: "call_4", Type: "function", Function: openai.FunctionCall{Name: "lookup", Arguments: `{"query":"x"}`},
		}}},
		{Role: "tool", Content: "found", ToolCallID: "call_4"},
		{Role: "developer", Name: "bot", Content: "Keep roles intact."},
	}
	input, err := toEinoMessages(history)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name      string
		modelName string
		tools     string
		options   []model.Option
		wanted    string
	}{
		{
			name: "reasoning max tokens and canonical schema", modelName: "gpt-5-mini",
			tools:   `,"tools":[{"type":"function","function":{"name":"lookup","description":"raw description","strict":true,"parameters":{"type":"object","oneOf":[{"required":["query"]}],"additionalProperties":false}}}]`,
			options: []model.Option{model.WithTools([]*schema.ToolInfo{{Name: "lookup", Desc: "Eino description"}}), model.WithTemperature(0.75), model.WithTopP(0.5)},
			wanted:  `"max_completion_tokens":321`,
		},
		{
			name: "legacy max tokens and explicitly cleared tools", modelName: "gpt-4o-mini",
			options: []model.Option{model.WithTools([]*schema.ToolInfo{{Name: "lookup"}}), model.WithTools(nil), model.WithTemperature(0.75), model.WithTopP(0.5)},
			wanted:  `"max_tokens":321,"temperature":0.75,"top_p":0.5`,
		},
		{
			name: "canonical map does not implicitly select tools", modelName: "gpt-4o-mini",
			wanted: `"max_tokens":321`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, events := newEinoModelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				einoModelAssertPayload(t, r, fmt.Sprintf(`{"model":%q,%s,"messages":[
					{"role":"system","content":"Keep {\"literal\": \"{value}\"} as written."},
					{"role":"user","name":"alice","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,dGVzdA==","detail":"high"}}]},
					{"role":"assistant","tool_calls":[{"index":4,"id":"call_4","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"x\"}"}}]},
					{"role":"tool","content":"found","tool_call_id":"call_4"},
					{"role":"developer","name":"bot","content":"Keep roles intact."}
				]%s}`, tc.modelName, tc.wanted, tc.tools))
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"chatcmpl_1","model":"provider-model","choices":[{"index":0,"message":{"role":"assistant","content":"answer","name":"bot"},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":2,"total_tokens":10}}`)
			})
			m.modelName = tc.modelName

			msg, err := m.Generate(t.Context(), input, tc.options...)
			if err != nil {
				t.Fatal(err)
			}
			if msg.Role != schema.Assistant || msg.Content != "answer" || msg.Name != "bot" || msg.ResponseMeta == nil ||
				msg.ResponseMeta.FinishReason != "stop" || msg.ResponseMeta.Usage == nil || msg.ResponseMeta.Usage.TotalTokens != 10 {
				t.Fatalf("unexpected response %#v", msg)
			}
			einoModelAssertUsage(t, events, "provider-model", UsageCallMainChat, 0, false, true, 10)
		})
	}
}

func TestEinoModelEinoImageContent(t *testing.T) {
	data := "dGVzdA=="
	input := []*schema.Message{
		{Role: schema.User, UserInputMultiContent: []schema.MessageInputPart{
			{Type: schema.ChatMessagePartTypeText, Text: "new image"},
			{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{Base64Data: &data, MIMEType: "image/png"},
				Detail:            schema.ImageURLDetailLow,
			}},
		}},
		{Role: schema.User, MultiContent: []schema.ChatMessagePart{
			{Type: schema.ChatMessagePartTypeText, Text: "earlier image"},
			{Type: schema.ChatMessagePartTypeImageURL, ImageURL: &schema.ChatMessageImageURL{URL: "data:image/jpeg;base64,dGVzdA==", Detail: schema.ImageURLDetailHigh}},
		}},
	}
	got, err := fromEinoMessages(input)
	if err != nil {
		t.Fatal(err)
	}
	want := []openai.ChatCompletionMessage{
		{Role: "user", MultiContent: []openai.ChatMessagePart{
			{Type: openai.ChatMessagePartTypeText, Text: "new image"},
			{Type: openai.ChatMessagePartTypeImageURL, ImageURL: &openai.ChatMessageImageURL{URL: "data:image/png;base64,dGVzdA==", Detail: "low"}},
		}},
		{Role: "user", MultiContent: []openai.ChatMessagePart{
			{Type: openai.ChatMessagePartTypeText, Text: "earlier image"},
			{Type: openai.ChatMessagePartTypeImageURL, ImageURL: &openai.ChatMessageImageURL{URL: "data:image/jpeg;base64,dGVzdA==", Detail: "high"}},
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("messages = %#v, want %#v", got, want)
	}
}

func TestEinoModelStreamUsage(t *testing.T) {
	requestNumber := 0
	m, events := newEinoModelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestNumber++
		w.Header().Set("Content-Type", "text/event-stream")
		switch requestNumber {
		case 1:
			einoModelAssertPayload(t, r, `{"model":"gpt-4o-mini","max_tokens":321,"messages":[],"stream":true,"stream_options":{"include_usage":true}}`)
			einoModelSSE(w, `{"id":"first","choices":[{"index":0,"delta":{"role":"assistant","content":"first answer"},"finish_reason":"stop"}]}`, "[DONE]")
		case 2:
			einoModelSSE(w,
				`{"id":"second","model":"initial-model","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"lookup","arguments":"{\"b\":"}},{"index":0,"id":"call_a","type":"function","function":{"name":"lookup","arguments":"{\"a\":"}}]}}]}`,
				`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}},{"index":1,"function":{"arguments":"2}"}}]},"finish_reason":"tool_calls"}]}`,
				`{"id":"second","model":"actual-tool-model","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":2},"completion_tokens_details":{"reasoning_tokens":1}}}`)
		default:
			t.Errorf("unexpected request %d", requestNumber)
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	})

	chunks, err := einoModelReadStream(t, m)
	if err != nil || len(chunks) != 1 || chunks[0].Content != "first answer" {
		t.Fatalf("first stream = %#v, %v", chunks, err)
	}
	einoModelAssertUsage(t, events, "gpt-4o-mini", UsageCallMainChat, 0, true, true, 0)

	chunks, err = einoModelReadStream(t, m, model.WithTools([]*schema.ToolInfo{{Name: "lookup"}}))
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 3 || len(chunks[0].ToolCalls) != 2 || len(chunks[1].ToolCalls) != 2 {
		t.Fatalf("missing streamed tool-call deltas or usage tail: %#v", chunks)
	}
	second := chunks[1].ToolCalls[0]
	if second.Index == nil || *second.Index != 0 || second.ID != "" || second.Type != "" || second.Function.Name != "" || second.Function.Arguments != "1}" {
		t.Fatalf("tool-call fragment = %#v", second)
	}
	msg, err := schema.ConcatMessages(chunks)
	if err != nil {
		t.Fatal(err)
	}
	a, b := 0, 1
	want := []schema.ToolCall{
		{Index: &a, ID: "call_a", Type: "function", Function: schema.FunctionCall{Name: "lookup", Arguments: `{"a":1}`}},
		{Index: &b, ID: "call_b", Type: "function", Function: schema.FunctionCall{Name: "lookup", Arguments: `{"b":2}`}},
	}
	if !reflect.DeepEqual(msg.ToolCalls, want) || msg.Role != schema.Assistant {
		t.Fatalf("assembled message = %#v", msg)
	}
	if msg.ResponseMeta == nil || msg.ResponseMeta.FinishReason != "tool_calls" || msg.ResponseMeta.Usage == nil ||
		msg.ResponseMeta.Usage.TotalTokens != 15 || msg.ResponseMeta.Usage.PromptTokenDetails.CachedTokens != 2 ||
		msg.ResponseMeta.Usage.CompletionTokensDetails.ReasoningTokens != 1 {
		t.Fatalf("assembled response metadata = %#v", msg.ResponseMeta)
	}
	einoModelAssertUsage(t, events, "actual-tool-model", UsageCallToolRound, 2, true, true, 15)
}

func TestEinoModelStreamFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		frames  []string
		tools   bool
		wantErr string
		total   int64
	}{
		{name: "startup", status: http.StatusServiceUnavailable, wantErr: "upstream unavailable", tools: true},
		{
			name: "malformed frame after usage", tools: true, total: 9, wantErr: "invalid character",
			frames: []string{`{"choices":[{"delta":{"content":"partial"}}]}`, `{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`, "{broken"},
		},
		{
			name: "tool call without tools", total: 6, wantErr: "tool calls",
			frames: []string{`{"choices":[{"delta":{"role":"assistant","content":"partial"}}]}`, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"unexpected","type":"function","function":{"name":"lookup","arguments":"{}"}}]}}]}`, `{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`, "[DONE]"},
		},
		{
			name: "no choices", total: 4, wantErr: "no choices",
			frames: []string{`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`, "[DONE]"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, events := newEinoModelTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
				if tc.status != 0 {
					w.WriteHeader(tc.status)
					io.WriteString(w, `{"error":{"message":"upstream unavailable"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				einoModelSSE(w, tc.frames...)
			})

			var opts []model.Option
			callType, round := UsageCallMainChat, 0
			if tc.tools {
				opts = append(opts, model.WithTools([]*schema.ToolInfo{{Name: "lookup"}}))
				callType, round = UsageCallToolRound, 1
			}
			chunks, err := einoModelReadStream(t, m, opts...)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("stream error = %v, want %q", err, tc.wantErr)
			}
			for _, chunk := range chunks {
				if len(chunk.ToolCalls) != 0 {
					t.Fatalf("tool call exposed to consumer: %#v", chunk)
				}
			}
			einoModelAssertUsage(t, events, "gpt-4o-mini", callType, round, true, false, tc.total)
		})
	}
}

func TestEinoModelStreamTransportFailure(t *testing.T) {
	m, events := newEinoModelTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		// Declare a longer body to simulate an interrupted provider connection.
		w.Header().Set("Content-Length", "4096")
		w.Header().Set("Content-Type", "text/event-stream")
		einoModelSSE(w, `{"model":"actual-model","choices":[{"delta":{"content":"partial"},"finish_reason":null}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	})
	chunks, err := einoModelReadStream(t, m)
	if !errors.Is(err, io.ErrUnexpectedEOF) || len(chunks) != 1 || chunks[0].Content != "partial" {
		t.Fatalf("interrupted stream = %#v, %v", chunks, err)
	}
	einoModelAssertUsage(t, events, "actual-model", UsageCallMainChat, 0, true, false, 5)
}

func TestEinoModelGenerateFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		choices string
		wantErr string
	}{
		{name: "no choices", choices: "[]", wantErr: "no choices"},
		{name: "tool call without tools", choices: `[{"message":{"role":"assistant","tool_calls":[{"id":"bad","type":"function","function":{"name":"lookup","arguments":"{}"}}]}}]`, wantErr: "tool calls"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, events := newEinoModelTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"model":"actual-model","choices":%s,"usage":{"prompt_tokens":6,"completion_tokens":2,"total_tokens":8}}`, tc.choices)
			})
			msg, err := m.Generate(t.Context(), nil)
			if msg != nil || err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Generate = %#v, %v, want nil message and %q", msg, err, tc.wantErr)
			}
			einoModelAssertUsage(t, events, "actual-model", UsageCallMainChat, 0, false, false, 8)
		})
	}
}

func TestEinoModelStreamClosesConnection(t *testing.T) {
	for _, tc := range []struct {
		name      string
		readFirst bool
		cancel    bool
		total     int64
	}{
		{name: "early close while provider is idle"},
		{name: "cancel after text", readFirst: true, cancel: true, total: 7},
		{name: "cancel without consuming text", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disconnected := make(chan struct{})
			stop := make(chan struct{})
			m, events := newEinoModelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if tc.cancel {
					einoModelSSE(w, `{"model":"gpt-4o-mini","choices":[{"delta":{"role":"assistant","content":"partial"}}],"usage":{"prompt_tokens":6,"completion_tokens":1,"total_tokens":7}}`)
				} else {
					w.(http.Flusher).Flush()
				}
				select {
				case <-r.Context().Done():
					close(disconnected)
				case <-stop:
				}
			})
			defer close(stop)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			reader, err := m.Stream(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.readFirst {
				msg, err := reader.Recv()
				if err != nil || msg.Content != "partial" {
					t.Fatalf("first chunk = %#v, %v", msg, err)
				}
			}
			if tc.cancel {
				cancel()
			} else {
				reader.Close()
			}

			select {
			case <-disconnected:
			case <-time.After(3 * time.Second):
				t.Fatal("provider did not see the request close")
			}
			if tc.cancel && !tc.readFirst {
				// Cancellation can win before the first frame is read.
				event := einoModelNextUsage(t, events)
				if event.Success || event.CallType != UsageCallMainChat || !event.Streamed {
					t.Fatalf("unexpected usage event %+v", event)
				}
				reader.Close()
				return
			}
			if tc.cancel {
				defer reader.Close()
				_, err := reader.Recv()
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Recv after cancellation = %v, want context.Canceled", err)
				}
			}
			einoModelAssertUsage(t, events, "gpt-4o-mini", UsageCallMainChat, 0, true, false, tc.total)
		})
	}
}

func newEinoModelTestServer(t *testing.T, handle http.HandlerFunc) (*einoChatModel, <-chan UsageEvent) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected request %s %s, Authorization %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		handle(w, r)
	}))
	t.Cleanup(server.Close)
	cfg := openai.DefaultConfig("test-key")
	cfg.BaseURL, cfg.HTTPClient = server.URL+"/v1", server.Client()
	events := make(chan UsageEvent, 8)
	return &einoChatModel{
		client: openai.NewClientWithConfig(cfg), modelName: "gpt-4o-mini", maxTokens: 321,
		tools: map[string]openai.Tool{
			"lookup": {Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{
				Name: "lookup", Description: "raw description", Strict: true,
				Parameters: json.RawMessage(`{"type":"object","oneOf":[{"required":["query"]}],"additionalProperties":false}`),
			}},
			"other": {Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{Name: "other"}},
		},
		usageCtx: UsageContext{ChatID: 41, UserID: 42, MessageID: 43, RequestID: "bot-request"},
		recordUsage: func(event UsageEvent) {
			events <- event
		},
	}, events
}

func einoModelAssertPayload(t *testing.T, r *http.Request, wantJSON string) {
	t.Helper()
	var got, want map[string]any
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Errorf("decode request: %v", err)
		return
	}
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Errorf("decode expected payload: %v", err)
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("payload = %#v\nwant    = %#v", got, want)
	}
}

func einoModelSSE(w http.ResponseWriter, frames ...string) {
	for _, frame := range frames {
		fmt.Fprintf(w, "data: %s\n\n", frame)
	}
	w.(http.Flusher).Flush()
}

func einoModelReadStream(t *testing.T, m *einoChatModel, opts ...model.Option) ([]*schema.Message, error) {
	t.Helper()
	reader, err := m.Stream(t.Context(), []*schema.Message{}, opts...)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	var chunks []*schema.Message
	for {
		chunk, err := reader.Recv()
		if err == io.EOF {
			return chunks, nil
		}
		if err != nil {
			return chunks, err
		}
		chunks = append(chunks, chunk)
	}
}

func einoModelNextUsage(t *testing.T, events <-chan UsageEvent) UsageEvent {
	t.Helper()
	select {
	case event := <-events:
		select {
		case extra := <-events:
			t.Fatalf("extra usage event %+v", extra)
		default:
		}
		return event
	case <-time.After(3 * time.Second):
		t.Fatal("no usage event")
		return UsageEvent{}
	}
}

func einoModelAssertUsage(t *testing.T, events <-chan UsageEvent, modelName string, callType UsageCallType, round int, stream, success bool, total int64) {
	t.Helper()
	event := einoModelNextUsage(t, events)
	if event.Model != modelName || event.CallType != callType || event.ToolIterations != round ||
		event.Streamed != stream || event.Success != success || event.TotalTokens != total ||
		event.ChatID != 41 || event.UserID != 42 || event.MessageID != 43 || event.RequestID != "bot-request" {
		t.Fatalf("unexpected usage event %+v, want model %s, type %s, round %d, streamed %v, success %v, total %d", event, modelName, callType, round, stream, success, total)
	}
}
