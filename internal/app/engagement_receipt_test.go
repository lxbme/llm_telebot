package app

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	tele "gopkg.in/telebot.v3"
)

func TestEngagementUnrecordedReceipt(t *testing.T) {
	const signup = "Could you sign me up for release notifications?"
	const claim = "Your request has been saved. The calendar will notify you when the stable release ships."
	const conditional = "Both calendars can notify you if your reminder is saved in both."
	const settings = "When you close Settings, your reminders are saved. The calendar will use them the next time it syncs."
	const howTo = "Open your device settings and select the app. Then turn off its alerts."
	for _, tc := range []struct {
		name, request, kind, answer string
		voice, readsContext         bool
		wantSafe                    bool
	}{
		{"ordinary conditional", "Why do reminders sometimes appear twice?", "", conditional, false, false, false},
		{"personal signup", signup, "answer", claim, false, false, true},
		{"personal signup voice", signup, "answer", claim, true, false, true},
		{"fronted when settings explanation", "When does the app save my reminders?", "", settings, false, false, false},
		{"zh watchlist promise", "可以把我加入稳定版的观察名单吗？", "answer", "我会把你加入稳定版的观察名单。一发布我就通知你。", true, false, true},
		{"mixed signup after context read", "How can I follow releases? " + signup, "engagement", claim, false, true, true},
		{"mixed signup in one sentence", "How can I follow releases, and could you sign me up for release notifications?", "answer", claim, false, false, true},
		{"ordinary how-to", "How do I stop calendar notifications?", "answer", howTo, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reviews, sttCalls, streams atomic.Int64
			draft := tc.answer
			if tc.kind != "" {
				draft = dialogueJSON(struct{ Kind, Text string }{tc.kind, tc.answer})
			}
			provider := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
				if !req.Stream {
					reviews.Add(1)
					http.Error(w, "Auxiliary unavailable", http.StatusServiceUnavailable)
					return
				}
				if tc.readsContext && streams.Add(1) == 1 {
					engagementToolFrame(w, "engagement_context", map[string]any{})
					return
				}
				engagementModelText(w, draft)
			})
			media := http.NewServeMux()
			media.HandleFunc("POST /botrole/getFile", func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, `{"ok":true,"result":{"file_id":"voice","file_path":"voice"}}`)
			})
			media.HandleFunc("GET /file/botrole/voice", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "local input audio") })
			voiceSends := make(chan struct{}, 1)
			b, sent := newRoleTestBotWithTelegram(t, provider, func(w http.ResponseWriter, r *http.Request, _ roleTestDelivery) bool {
				if r.URL.Path != "/botrole/sendVoice" {
					return false
				}
				voiceSends <- struct{}{}
				fmt.Fprintf(w, `{"ok":true,"result":{"message_id":15,"chat":{"id":%d,"type":"private"},"voice":{"file_id":"synthesized"}}}`, engagementOwner)
				return true
			}, media)
			b.cfg.ProfileEnabled = false
			b.engagements = NewEngagementStore(b.chatDB)
			b.engagements.now = func() time.Time { return engagementNow }
			msg := roleMessage(engagementOwner, engagementOwner, 980, tc.request)
			handler := b.handleText
			if tc.voice {
				stt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					sttCalls.Add(1)
					fmt.Fprintf(w, `{"text":%q}`, tc.request)
				}))
				t.Cleanup(stt.Close)
				b.stt = NewSTTClient(Config{STTEnabled: true, STTAPIBase: stt.URL + "/v1", STTAPIKey: "local"})
				msg.Text, msg.Voice = "", &tele.Voice{File: tele.File{FileID: "voice"}}
				handler = b.handleVoice
			}
			spoken := make(chan string, 1)
			if tc.voice {
				tts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var req struct {
						Params struct{ Text string } `json:"req_params"`
					}
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
					}
					spoken <- req.Params.Text
					// Valid short mono PCM WAV to exercise ffmpeg and Telegram's sendVoice.
					wav := make([]byte, 44+3200)
					copy(wav[0:], "RIFF")
					binary.LittleEndian.PutUint32(wav[4:], uint32(len(wav)-8))
					copy(wav[8:], "WAVEfmt ")
					binary.LittleEndian.PutUint32(wav[16:], 16)
					binary.LittleEndian.PutUint16(wav[20:], 1)
					binary.LittleEndian.PutUint16(wav[22:], 1)
					binary.LittleEndian.PutUint32(wav[24:], 16000)
					binary.LittleEndian.PutUint32(wav[28:], 32000)
					binary.LittleEndian.PutUint16(wav[32:], 2)
					binary.LittleEndian.PutUint16(wav[34:], 16)
					copy(wav[36:], "data")
					binary.LittleEndian.PutUint32(wav[40:], 3200)
					fmt.Fprintf(w, "event: 352\ndata: {\"code\":0,\"data\":%q}\n\nevent: 152\ndata: {\"code\":20000000}\n\n", base64.StdEncoding.EncodeToString(wav))
				}))
				t.Cleanup(tts.Close)
				b.tts = &VolcengineTTSClient{url: tts.URL, httpClient: tts.Client(), audioFormat: "wav"}
				b.speechModes = NewSpeechModeStore()
				b.speechModes.Set(engagementOwner, true)
			}
			roleHandle(t, b, handler, msg)
			got := roleReceive(t, sent)
			if tc.voice && got.text == "🔊 正在发送语音…" {
				speech := roleReceive(t, spoken)
				roleReceive(t, voiceSends)
				got = roleTestDelivery{path: "sendVoice", text: speech, chatID: engagementOwner}
			}
			waitEinoReplyHistory(t, b, engagementOwner, 2)
			rows, err := b.engagements.Visible(EngagementViewer{ChatID: engagementOwner, ActorID: engagementOwner}, "", 8)
			if err != nil || len(rows) != 0 {
				t.Fatalf("expected no agreement writes, got %d, %v", len(rows), err)
			}
			t.Logf("Telegram %s=%q; store=%d; auxiliary=%d; STT=%d", got.path, got.text, len(rows), reviews.Load(), sttCalls.Load())
			if tc.voice && sttCalls.Load() != 1 {
				t.Fatalf("STT did not run exactly once")
			}
			if tc.wantSafe && strings.Contains(html.UnescapeString(got.text), tc.answer) {
				t.Errorf("unrecorded durable promise delivered with no agreement")
			}
			if !tc.wantSafe && (got.text != tc.answer || reviews.Load() != 0) {
				t.Errorf("ordinary answer was replaced or needed an auxiliary model call")
			}
		})
	}
}
