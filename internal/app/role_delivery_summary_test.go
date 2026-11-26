package app

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	tele "gopkg.in/telebot.v3"
)

func TestRoleDeliveryHistoryUsesConfirmedTruncatedText(t *testing.T) {
	const fact = "private conclusion only approved for this group"
	prefix := strings.Repeat("界", 4100)
	reply := prefix + " " + fact
	visible := strings.Repeat("界", 4093) + "…"
	requests := make(chan openai.ChatCompletionRequest, 2)
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, round int, req openai.ChatCompletionRequest) {
		requests <- req
		answer := reply
		if round != 0 {
			answer = "second answer"
		}
		einoModelSSE(w, einoReplyTextFrame(answer))
		einoReplyFinish(w, "stop")
	})
	var b *Bot
	bot, deliveries := newRoleTestBotWithTelegram(t, model, func(w http.ResponseWriter, _ *http.Request, d roleTestDelivery) bool {
		if d.path != "/botrole/editMessageText" || d.text != visible {
			return false
		}
		fmt.Fprintf(w, `{"ok":true,"result":{"message_id":10,"chat":{"id":%d,"type":"supergroup"},"text":%q}}`, regressionGroup, d.text)
		b.speechModes.Set(regressionGroup, true)
		dispatchRoleText(b, 42, 42, 3, "/memory_unshare -100123 "+fact)
		return true
	})
	b = bot
	b.cfg.ProfileEnabled = false
	b.speechModes = NewSpeechModeStore()
	var synthesized atomic.Int32
	speech := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		synthesized.Add(1)
		http.Error(w, "unexpected synthesis", http.StatusServiceUnavailable)
	}))
	t.Cleanup(speech.Close)
	b.tts = &VolcengineTTSClient{url: speech.URL, httpClient: speech.Client()}
	done := make(chan struct{}, 2)
	b.mcpClients = NewMCPClientManager()
	b.replyTools = &replyToolGeneration{cleanup: func() { done <- struct{}{} }}
	b.registerHandlers()
	seedRoleFact(t, b, fact)
	dispatchRoleText(b, 42, 42, 1, "/memory_share -100123 "+fact)
	roleReceive(t, deliveries)
	earlier := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "earlier confirmed group answer"}
	b.store.Append(regressionGroup, earlier, 20)

	dispatchRoleText(b, regressionGroup, 43, 2, "@testbot give the conclusion")
	if req := roleReceive(t, requests); !strings.Contains(fmt.Sprint(req.Messages), fact) {
		t.Fatal("first request did not contain the approved conclusion")
	}
	if d := roleReceive(t, deliveries); !strings.Contains(d.text, "withdrawn") {
		t.Fatalf("withdrawal = %+v", d)
	}
	roleReceive(t, done)
	if synthesized.Load() != 0 {
		t.Error("speech started after the group's authorization was withdrawn")
	}
	history := b.chatDB.LoadAllHistory()[regressionGroup]
	if len(history) != 3 || !reflect.DeepEqual(history[0], earlier) || history[2].Content != visible {
		t.Errorf("persisted history did not retain exactly the delivered replies (leaked conclusion: %t)", strings.Contains(fmt.Sprint(history), fact))
	}

	b.speechModes.Set(regressionGroup, false)
	dispatchRoleText(b, regressionGroup, 43, 4, "@testbot unrelated question")
	req := roleReceive(t, requests)
	var assistants []string
	for _, message := range req.Messages {
		if message.Role == openai.ChatMessageRoleAssistant {
			assistants = append(assistants, message.Content)
		}
	}
	if !reflect.DeepEqual(assistants, []string{earlier.Content, visible}) || strings.Contains(fmt.Sprint(req.Messages), fact) {
		t.Error("next group request did not use exactly the delivered assistant history")
	}
	roleReceive(t, done)
}

func TestRoleDeliveryHistoryUsesVisibleFormatting(t *testing.T) {
	const markdown = "**Cedar & oak** [label](https://example.invalid/hidden)"
	const htmlText = `<b>Cedar &amp; oak</b> <a href="https://example.invalid/hidden">label</a>`
	for _, tc := range []struct {
		name, accepted, result, history string
		rejectHTML, notModified         bool
	}{
		{name: "HTML", accepted: htmlText, result: "Cedar & oak label", history: "Cedar & oak label"},
		{name: "HTML not modified", accepted: htmlText, notModified: true, history: "Cedar & oak label"},
		{name: "plain fallback", accepted: markdown, result: markdown, history: markdown, rejectHTML: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ openai.ChatCompletionRequest) {
				einoModelSSE(w, einoReplyTextFrame(markdown))
				einoReplyFinish(w, "stop")
			})
			b, _ := newRoleTestBotWithTelegram(t, model, func(w http.ResponseWriter, _ *http.Request, d roleTestDelivery) bool {
				if d.path != "/botrole/editMessageText" {
					return false
				}
				if tc.rejectHTML && d.text == htmlText {
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`)
					return true
				}
				if d.text != tc.accepted {
					t.Errorf("Telegram edit = %q, want %q", d.text, tc.accepted)
				}
				if tc.notModified {
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: message is not modified"}`)
					return true
				}
				fmt.Fprintf(w, `{"ok":true,"result":{"message_id":10,"chat":{"id":%d,"type":"supergroup"},"text":%q}}`, regressionGroup, tc.result)
				return true
			})
			b.cfg.ProfileEnabled = false
			roleHandle(t, b, b.handleText, roleMessage(regressionGroup, 43, 1, "@testbot answer"))
			waitEinoReplyHistory(t, b, regressionGroup, 2)
			if h := b.chatDB.LoadAllHistory()[regressionGroup]; len(h) != 2 || h[1].Content != tc.history {
				t.Errorf("persisted assistant history = %+v, want %q", h, tc.history)
			}
		})
	}
}

func TestRoleDeliveryHistoryUsesConfirmedVoiceInput(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is needed by the production Telegram voice encoder")
	}
	body := strings.Repeat("界", 1300)
	const tail = "tail after the speech limit"
	reply := "**" + body + "** " + tail
	spoken := strings.Repeat("界", 1200) + "。"
	for _, tc := range []struct {
		name, history string
		text, fail    bool
	}{
		{name: "voice only", history: spoken},
		{name: "text and voice", text: true, history: body + " " + tail},
		{name: "text and failed voice", text: true, fail: true, history: body + " " + tail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A short PCM WAV exercises the real transcoder and upload.
			wav := append([]byte("RIFF\x00\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x80\x3e\x00\x00\x00\x7d\x00\x00\x02\x00\x10\x00data\x00\x00\x00\x00"), make([]byte, 3200)...)
			binary.LittleEndian.PutUint32(wav[4:8], uint32(len(wav)-8))
			binary.LittleEndian.PutUint32(wav[40:44], uint32(len(wav)-44))
			var synthesized, voiced atomic.Int32
			speech := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				synthesized.Add(1)
				var payload struct {
					Params struct{ Text string } `json:"req_params"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Params.Text != spoken {
					t.Error("TTS did not receive the expected normalized and truncated input")
				}
				if tc.fail {
					http.Error(w, "speech unavailable", http.StatusServiceUnavailable)
					return
				}
				fmt.Fprintf(w, "event: TTSResponse\ndata: {\"code\":0,\"data\":%q}\n\nevent: SessionFinish\ndata: {\"code\":0}\n\n", base64.StdEncoding.EncodeToString(wav))
			}))
			t.Cleanup(speech.Close)
			chat := tele.Chat{ID: regressionGroup, Type: tele.ChatSuperGroup}
			telegram := newEinoReplyTelegramForChat(t, chat, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/bottest-token/sendVoice":
					if err := r.ParseMultipartForm(1 << 20); err != nil || !strings.HasPrefix(r.FormValue("voice"), "OggS") {
						t.Errorf("read Ogg voice upload: %v", err)
						http.Error(w, "missing voice file", http.StatusBadRequest)
						return
					}
					voiced.Add(1)
					fmt.Fprintf(w, `{"ok":true,"result":{"message_id":10,"chat":{"id":%d,"type":"supergroup"},"voice":{"file_id":"voice","file_unique_id":"unique","duration":1}}}`, regressionGroup)
				case "/bottest-token/deleteMessage":
					fmt.Fprint(w, `{"ok":true,"result":true}`)
				default:
					t.Errorf("unexpected Telegram voice operation: %s", r.URL.Path)
					http.Error(w, "unexpected operation", http.StatusBadRequest)
				}
			}))
			model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ openai.ChatCompletionRequest) {
				einoModelSSE(w, einoReplyTextFrame(reply))
				einoReplyFinish(w, "stop")
			})
			b, _ := newRoleTestBot(t, model)
			b.tg = telegram.bot
			b.cfg.ProfileEnabled, b.cfg.VolcengineTTSSendText = false, tc.text
			b.speechModes = NewSpeechModeStore()
			b.speechModes.Set(regressionGroup, true)
			b.tts = &VolcengineTTSClient{url: speech.URL, httpClient: speech.Client(), audioFormat: "wav"}
			done := make(chan struct{})
			b.mcpClients = NewMCPClientManager()
			b.replyTools = &replyToolGeneration{cleanup: func() { close(done) }}
			roleHandle(t, b, b.handleText, roleMessage(regressionGroup, 43, 1, "@testbot answer"))
			roleReceive(t, done)
			wantVoice := int32(1)
			if tc.fail {
				wantVoice = 0
			}
			if synthesized.Load() != 1 || voiced.Load() != wantVoice {
				t.Errorf("synthesis calls = %d, confirmed voice uploads = %d, want 1 and %d", synthesized.Load(), voiced.Load(), wantVoice)
			}
			if h := b.chatDB.LoadAllHistory()[regressionGroup]; len(h) != 2 || h[1].Content != tc.history {
				t.Error("persisted content differed from the confirmed text or voice input")
			}
		})
	}
}

func TestRoleSummaryPendingSurvivesCronMembershipLoss(t *testing.T) {
	const oldSummary = "prior group summary"
	old := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "earlier group topic"}
	oldReply := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "earlier group answer"}
	want := []openai.ChatCompletionMessage{old, oldReply}
	const remotePrivate = "unshared private context"
	var left atomic.Bool
	memberChecked := make(chan struct{}, 1)
	requests := make(chan openai.ChatCompletionRequest, 3)
	b, _ := newRoleTestBotWithTelegram(t, newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, round int, req openai.ChatCompletionRequest) {
		requests <- req
		if req.Stream {
			einoModelSSE(w, einoReplyTextFrame("group cron reply"))
			einoReplyFinish(w, "stop")
			return
		}
		if round == 1 {
			http.Error(w, "summary model unavailable", http.StatusServiceUnavailable)
			return
		}
		if round == 2 {
			fmt.Fprint(w, `{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":"incomplete summary"}}]}`)
			return
		}
		roleCompletion(w, "merged group summary")
	}), func(w http.ResponseWriter, _ *http.Request, d roleTestDelivery) bool {
		switch d.path {
		case "/botrole/editMessageText":
			if d.text == "group cron reply" {
				fmt.Fprintf(w, `{"ok":true,"result":{"message_id":10,"chat":{"id":%d,"type":"supergroup"},"text":%q}}`, regressionGroup, d.text)
				left.Store(true)
				return true
			}
		case "/botrole/getChatMember":
			status := "member"
			if left.Load() {
				status = "left"
				memberChecked <- struct{}{}
			}
			fmt.Fprintf(w, `{"ok":true,"result":{"status":%q,"user":{"id":42,"first_name":"Owner"}}}`, status)
			return true
		}
		return false
	})
	b.cfg.ProfileEnabled = false
	b.cfg.SummaryEnabled, b.cfg.SummaryMinOverflow, b.cfg.ContextMaxMsgs = true, 1, 2
	b.summaries.Set(regressionGroup, oldSummary)
	b.store.AppendBatch(regressionGroup, want, 2)
	seedRoleFact(t, b, remotePrivate)
	task := ScheduledTask{ID: "legacy", Prompt: "group update", Context: true, CreatedBy: 42}
	tasks := NewTaskStore(nil)
	tasks.Upsert(regressionGroup, task)
	generation := b.beginRoleTurn(RoleTurn{ChatID: regressionGroup}).generation
	NewTaskRunner(b, tasks).execute(regressionGroup, task)
	if req := roleReceive(t, requests); !req.Stream {
		t.Fatal("expected the real cron reply before summarization")
	}
	roleReceive(t, memberChecked)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, busy := b.summaries.summarizing.Load(generation); !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("summary did not finish after membership loss")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-requests:
		t.Error("departed initiator's cron made a summary model request")
	default:
	}
	checkRolePendingSummary(t, b, want, oldSummary)
	if t.Failed() {
		return
	}

	// Reopen the same database, then retry with a newly authorized group-only turn.
	if err := b.chatDB.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := OpenChatDB(b.cfg.ChatDBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	b.mu.Lock()
	b.chatDB, b.store, b.summaries, b.memory = db, NewHistoryStore(db), NewSummaryStore(db), NewMemoryBook(db)
	b.mu.Unlock()
	checkRolePendingSummary(t, b, want, oldSummary)
	turn := b.beginRoleTurn(RoleTurn{ChatID: regressionGroup, Purpose: RoleBackground})
	if _, err := turn.verifyBackgroundAudience(b.snapshot()); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		b.summarizeOverflow(turn)
		req := roleReceive(t, requests)
		prompt := req.Messages[len(req.Messages)-1].Content
		if req.Stream || !strings.Contains(prompt, oldSummary) || strings.Count(prompt, old.Content) != 1 ||
			strings.Count(prompt, oldReply.Content) != 1 || strings.Contains(fmt.Sprint(req.Messages), remotePrivate) {
			t.Errorf("group-only summary of queued data = %+v", req.Messages)
		}
		if i < 2 {
			checkRolePendingSummary(t, b, want, oldSummary)
		}
	}
	checkRolePendingSummary(t, b, nil, "merged group summary")
	b.summarizeOverflow(turn)
	select {
	case <-requests:
		t.Error("committed overflow was submitted again")
	default:
	}
}

func TestRoleSummaryBacklogFitsModelInput(t *testing.T) {
	const modelBudget = 8192
	requests := make(chan string, 1)
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
		requests <- req.Messages[len(req.Messages)-1].Content
		total := 0
		for _, msg := range req.Messages {
			total += len(msg.Content)
		}
		if total > modelBudget {
			http.Error(w, "context length exceeded", http.StatusRequestEntityTooLarge)
			return
		}
		roleCompletion(w, "merged summary")
	})
	b, _ := newRoleTestBot(t, model)
	b.cfg.SummaryEnabled = true
	queued := make([]openai.ChatCompletionMessage, 21)
	for i := range queued {
		queued[i] = openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser,
			Content: fmt.Sprintf("message-%02d %s", i, strings.Repeat("界", 145))}
	}
	b.store.AppendBatch(regressionGroup, queued, 0)
	turn := b.beginTelegramTurn(roleMessage(regressionGroup, 43, 1, "turn"))
	for len(queued) > 0 {
		b.summarizeOverflow(turn)
		prompt := roleReceive(t, requests)
		remaining := b.store.OverflowCount(regressionGroup)
		if remaining >= len(queued) {
			t.Fatalf("no progress summarizing %d queued messages", len(queued))
		}
		consumed := len(queued) - remaining
		for i, msg := range queued {
			if strings.Contains(prompt, msg.Content) != (i < consumed) {
				t.Fatalf("summary input and committed prefix differ at index %d", i)
			}
		}
		queued = queued[consumed:]
		checkRolePendingSummary(t, b, append([]openai.ChatCompletionMessage(nil), queued...), "merged summary")
	}
}

func checkRolePendingSummary(t *testing.T, b *Bot, want []openai.ChatCompletionMessage, summary string) {
	t.Helper()
	if got := b.store.OverflowCount(regressionGroup); got != len(want) {
		t.Errorf("pending count = %d, want %d", got, len(want))
	}
	if got := b.chatDB.LoadAllOverflow()[regressionGroup]; !reflect.DeepEqual(got, want) {
		t.Errorf("persisted overflow = %+v, want %+v", got, want)
	}
	if memory, persisted := b.summaries.Get(regressionGroup), b.chatDB.LoadAllSummaries()[regressionGroup]; memory != summary || persisted != summary {
		t.Errorf("summary in memory = %q, persisted = %q, want %q", memory, persisted, summary)
	}
}

func TestRoleSummaryCommitPreservesConcurrentOverflow(t *testing.T) {
	old := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "old pending"}
	next := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "new pending"}
	for _, clear := range []bool{false, true} {
		t.Run(fmt.Sprintf("clear=%t", clear), func(t *testing.T) {
			started := make(chan string, 2)
			release := make(chan struct{})
			model := newEinoReplyProvider(t, func(w http.ResponseWriter, r *http.Request, round int, req openai.ChatCompletionRequest) {
				started <- req.Messages[len(req.Messages)-1].Content
				if round == 0 {
					select {
					case <-release:
					case <-r.Context().Done():
					case <-t.Context().Done():
					}
					roleCompletion(w, "first summary")
					return
				}
				roleCompletion(w, "second summary")
			})
			b, deliveries := newRoleTestBot(t, model)
			b.cfg.SummaryEnabled = true
			b.store.Append(regressionGroup, old, 0)
			turn := b.beginTelegramTurn(roleMessage(regressionGroup, 43, 1, "turn"))
			done := make(chan struct{})
			go func() {
				defer close(done)
				b.summarizeOverflow(turn)
			}()
			if prompt := roleReceive(t, started); !strings.Contains(prompt, old.Content) {
				t.Errorf("first summary prompt = %q", prompt)
			}
			want := []openai.ChatCompletionMessage{old, next}
			if clear {
				roleHandle(t, b, b.handleClear, roleMessage(regressionGroup, 43, 2, "/clear"))
				roleReceive(t, deliveries)
				want = []openai.ChatCompletionMessage{next}
			}
			b.store.Append(regressionGroup, next, 0)
			checkRolePendingSummary(t, b, want, "")
			if !clear {
				b.summarizeOverflow(turn) // A concurrent worker cannot claim the same prefix.
				if calls := model.calls.Load(); calls != 1 {
					t.Errorf("concurrent summary started a second model request: %d calls", calls)
				}
			}
			close(release)
			roleReceive(t, done)
			existing := ""
			if !clear {
				existing = "first summary"
			}
			checkRolePendingSummary(t, b, []openai.ChatCompletionMessage{next}, existing)
			b.summarizeOverflow(b.beginTelegramTurn(roleMessage(regressionGroup, 43, 3, "new turn")))
			if prompt := roleReceive(t, started); !strings.Contains(prompt, next.Content) || strings.Contains(prompt, old.Content) ||
				(clear == strings.Contains(prompt, "first summary")) {
				t.Errorf("retry summary prompt = %q", prompt)
			}
			checkRolePendingSummary(t, b, nil, "second summary")
		})
	}
}
