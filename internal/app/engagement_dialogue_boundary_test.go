package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	tele "gopkg.in/telebot.v3"
)

func TestEngagementDialogueResumeKeepsPausedAgreementOnScopeChanges(t *testing.T) {
	for _, tc := range []struct {
		name, request, message string
		intent                 map[string]any
		withCalendar           bool
	}{
		{"withdraw calendar", "Resume this tracking, but don't write to my calendar anymore.", "calendar",
			map[string]any{"task": true, "remove_calendar": true}, true},
		{"change only recipient", "Resume this tracking but send updates only to group -702.", "recipient",
			map[string]any{"task": true, "recipient": "other"}, false},
		{"change trigger", "Resume tracking, but only after the security patch ships.", "scope",
			map[string]any{"task": true, "resume_changes": true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newEngagementScript(t)
			enableTestEngagementMCP(t, s.b, newEngagementMCP(t, "release", true))
			args := regressionWatchArgs()
			chat, mention, initial := engagementGroup, "@testbot ", "Tell this group when the stable release arrives."
			if tc.withCalendar {
				chat, mention = engagementOwner, ""
				users, err := NewUserMCPStore(filepath.Join(t.TempDir(), "personal.db"))
				if err != nil {
					t.Fatal(err)
				}
				s.b.userTools = NewUserToolManager(users, nil, nil)
				t.Cleanup(func() { s.b.userTools.Close(); _ = users.Close() })
				url := newEngagementMCP(t, "insert_event", false)
				s.sendImport(chat, engagementOwner, 710, fmt.Sprintf(`{"mcpServers":{"calendar":{"url":%q,"background":{"insert_event":{"operation":"calendar.create","target":"work","arguments":{"calendarId":"work"}}}}}}`, url))
				condition, clause := "When the stable release arrives", "add a review to my work calendar"
				initial = condition + ", " + clause + "."
				s.intents.Store(initial, map[string]any{"calendar": map[string]any{"target": "work", "quote": clause, "trigger": condition, "once": true}})
				args["trigger"], args["authorization_quote"], args["actions"] = condition, clause, []string{"personal:calendar:insert_event"}
			}
			if got := sendRegressionAgreement(s, args, chat, engagementOwner, 711, mention+initial); !strings.Contains(got, "Agreement saved") ||
				(tc.withCalendar && !strings.Contains(got, "calendar.create")) {
				t.Fatal(got)
			}
			rows, err := s.b.engagements.Visible(EngagementViewer{ChatID: chat, ActorID: engagementOwner}, "", 8)
			if err != nil || len(rows) != 1 {
				t.Fatalf("agreements = %v, %v", rows, err)
			}
			ref := rows[0].Ref
			s.change(ref, map[string]any{"verdict": "accept", "change": "pause"})
			if got := s.send(chat, engagementOwner, 712, mention+"Pause this tracking."); !strings.Contains(got, "paused") {
				t.Fatal(got)
			}
			ref.Version++
			s.intents.Store(tc.request, tc.intent)
			s.change(ref, map[string]any{"verdict": "accept", "change": "resume"})
			got := s.send(chat, engagementOwner, 713, mention+tc.request)
			item, err := s.b.engagements.Get(ref.ID)
			if err != nil || item.Ref != ref || item.Status != EngagementPaused ||
				!strings.Contains(got, "paused") || !strings.Contains(got, tc.message) {
				t.Fatalf("scope-changing resume = %q, ref=%+v status=%s err=%v", got, item.Ref, item.Status, err)
			}

			s.change(ref, map[string]any{"verdict": "accept", "change": "resume"})
			request, confirmation := "Resume this tracking.", "resumed"
			if tc.withCalendar {
				request, confirmation = "請按原約恢復。", "已恢复"
			}
			got = s.send(chat, engagementOwner, 714, mention+request)
			item, err = s.b.engagements.Get(ref.ID)
			if err != nil || item.Ref.Version != ref.Version+1 || item.Status != EngagementActive ||
				!strings.Contains(got, confirmation) || !slices.Equal(item.Current.Recipients, []int64{chat}) ||
				(tc.withCalendar && len(item.Current.Actions) != 1) {
				t.Fatalf("plain resume = %q, ref=%+v status=%s err=%v", got, item.Ref, item.Status, err)
			}
		})
	}
}

func TestEngagementDialogueCompoundItemResults(t *testing.T) {
	for _, other := range []string{"omitted", "failed", "saved"} {
		t.Run(other, func(t *testing.T) {
			s := newEngagementScript(t)
			s.b.cfg.ToolsMaxIterations = 4
			a := engagementFixture(engagementGroup)
			a.Decision.Agreement.Trigger.Text = "release A"
			first := createEngagement(t, s.b.engagements, a)
			b := engagementFixture(engagementGroup)
			b.Decision.Request.Source.MessageID++
			b.Decision.Agreement.Trigger.Text = "release B"
			second := createEngagement(t, s.b.engagements, b)
			// A muted item remains active. Pause requires an active item, so an
			// already-paused B exercises a real unsuccessful store decision.
			if other == "failed" {
				s.change(second, map[string]any{"verdict": "accept", "change": "pause"})
				if got := s.send(engagementGroup, engagementOwner, 718, "@testbot Pause release B."); !strings.Contains(got, "paused") {
					t.Fatal(got)
				}
				second.Version++
			}
			request := "Mute release A and pause release B."
			s.intents.Store(request, map[string]any{"task": true, "requests": []map[string]string{
				{"item": "release A", "change": "mute", "quote": "Mute release A"},
				{"item": "release B", "change": "pause", "quote": "pause release B"},
			}})
			run := func() string {
				s.tool("engagement_context", map[string]string{"id": first.ID}, nil)
				s.tool("engagement_decide", map[string]any{"id": first.ID, "version": first.Version, "verdict": "accept", "change": "mute"}, nil)
				if other != "omitted" {
					s.tool("engagement_context", map[string]string{"id": second.ID}, nil)
					s.tool("engagement_decide", map[string]any{"id": second.ID, "version": second.Version, "verdict": "accept", "change": "pause"}, nil)
				}
				s.final("Both changes saved.", nil)
				return s.send(engagementGroup, engagementOwner, 719, "@testbot "+request)
			}
			reply := run()
			ai, aerr := s.b.engagements.Get(first.ID)
			bi, berr := s.b.engagements.Get(second.ID)
			if aerr != nil || berr != nil || !ai.Muted || ai.Ref.Version != first.Version+1 ||
				!strings.Contains(reply, "release A") || !strings.Contains(reply, "release B") || strings.Contains(reply, "Both changes saved") {
				t.Fatalf("compound receipt = %q, items = %+v, %+v; %v, %v", reply, ai.Ref, bi.Ref, aerr, berr)
			}
			switch other {
			case "omitted":
				if bi.Status != EngagementActive || !strings.Contains(reply, "No matching decision could be confirmed") {
					t.Fatalf("omission = %q, B = %s", reply, bi.Status)
				}
			case "failed":
				if bi.Ref != second || !strings.Contains(reply, "No change was saved") {
					t.Fatalf("failure = %q, B = %+v", reply, bi.Ref)
				}
			case "saved":
				if bi.Status != EngagementPaused || bi.Ref.Version != second.Version+1 || !strings.Contains(reply, "This item is paused") {
					t.Fatalf("save = %q, B = %+v", reply, bi)
				}
				first, second = ai.Ref, bi.Ref
				if replay := run(); strings.Count(replay, "already processed") != 2 ||
					!strings.Contains(replay, "release A") || !strings.Contains(replay, "release B") {
					t.Fatalf("replay = %q", replay)
				}
				ai, aerr = s.b.engagements.Get(first.ID)
				bi, berr = s.b.engagements.Get(second.ID)
				if aerr != nil || berr != nil || ai.Ref != first || bi.Ref != second {
					t.Fatalf("replay mutated records: %+v, %+v; %v, %v", ai.Ref, bi.Ref, aerr, berr)
				}
			}
		})
	}
}

func TestEngagementDialogueCompoundPreferenceChannels(t *testing.T) {
	s := newEngagementScript(t)
	request := "In this group, stop starting conversations and mute tracking notifications."
	s.intents.Store(request, map[string]any{"preference": "group", "requests": []map[string]string{
		{"scope": "group", "channel": "chatter", "quote": "stop starting conversations"},
		{"scope": "group", "channel": "notifications", "quote": "mute tracking notifications"},
	}})
	s.tool("conversation_preference", map[string]any{"scope": "group", "verdict": "accept", "chatter": "off"}, nil)
	s.final("Both preferences saved.", nil)
	got := s.send(engagementGroup, engagementOwner, 721, "@testbot "+request)
	pref, err := s.b.engagements.ConversationPreferenceAt(engagementGroup, 0, engagementNow)
	if err != nil || pref.Chatter != ConversationOff || pref.Notifications != ConversationDefault ||
		!strings.Contains(got, "Conversation preference saved") ||
		!strings.Contains(got, "No matching decision could be confirmed for this part of the request — mute tracking notifications") {
		t.Fatalf("preference receipt = %q, settings = %+v, %v", got, pref, err)
	}
}

func TestEngagementDialoguePreferenceReplayAfterLaterUpdate(t *testing.T) {
	s := newEngagementScript(t)
	request := "In this group, stop starting conversations and mute tracking notifications."
	s.intents.Store(request, map[string]any{"preference": "group", "requests": []map[string]string{
		{"scope": "group", "channel": "chatter", "quote": "stop starting conversations"},
		{"scope": "group", "channel": "notifications", "quote": "mute tracking notifications"},
	}})
	send := func(id int, actor int64, text string, version int, chatter, notifications string) string {
		s.tool("conversation_preference", map[string]any{"scope": "group", "verdict": "accept",
			"version": version, "chatter": chatter, "notifications": notifications}, nil)
		s.final("Both preferences saved.", nil)
		return s.send(engagementGroup, actor, id, "@testbot "+text)
	}
	send(723, engagementOwner, request, 0, "off", "")
	later := "For this entire group, turn both chatter and tracking notifications on."
	s.intents.Store(later, map[string]string{"preference": "group"})
	send(724, engagementPeer, later, 1, "on", "on")
	replay := send(723, engagementOwner, request, 2, "on", "off")
	if !strings.Contains(replay, "Proactive chatter — off") || !strings.Contains(replay, "mute tracking notifications") ||
		strings.Contains(replay, "Tracking notifications —") || strings.Contains(replay, "Both preferences saved") {
		t.Fatalf("replay did not report the original channel and omitted request: %q", replay)
	}
	current, err := s.b.engagements.ConversationPreferenceAt(engagementGroup, 0, engagementNow)
	if err != nil || current.Chatter != ConversationOn || current.Notifications != ConversationOn {
		t.Fatalf("replay overwrote a later save: %+v, %v", current, err)
	}
	for _, verdict := range []string{"clarify", "reject"} {
		s.tool("conversation_preference", map[string]any{"scope": "self", "verdict": verdict}, nil)
		s.final("I couldn't decide.", nil)
		got := s.send(engagementGroup, engagementOwner, 723, "@testbot "+request)
		if !strings.Contains(got, "Proactive chatter — off") || strings.Contains(got, "Tracking notifications —") {
			t.Errorf("%s replay did not report the original save: %q", verdict, got)
		}
	}
	// A model outage before any tool call still has the durable event receipt.
	s.calls <- func(w http.ResponseWriter, _ openai.ChatCompletionRequest) {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	}
	historySize := len(s.b.store.Get(engagementGroup))
	roleHandle(t, s.b, s.b.handleText, roleMessage(engagementGroup, engagementOwner, 723, "@testbot "+request))
	got := roleReceive(t, s.replies).text
	if !strings.Contains(got, "Proactive chatter — off") || strings.Contains(got, "Tracking notifications —") {
		t.Errorf("model outage hid the original save: %q", got)
	} else {
		waitEinoReplyHistory(t, s.b, engagementGroup, historySize+2)
	}
	for _, tc := range []struct {
		id    int
		actor int64
		want  string
	}{
		{723, engagementPeer, "No change was saved"},
		{727, engagementOwner, "Conversation preferences were unchanged"},
	} {
		s.tool("conversation_preference", map[string]any{"scope": "group", "verdict": "reject"}, nil)
		s.final("No additional changes.", nil)
		got := s.send(engagementGroup, tc.actor, tc.id, "@testbot "+request)
		if !strings.Contains(got, tc.want) || strings.Contains(got, "Proactive chatter — off") {
			t.Errorf("a different speaker or event got the historical receipt: %q", got)
		}
	}
	// A replay cannot add a new scope either, even with a valid self version.
	s.tool("conversation_preference", map[string]any{"scope": "self", "verdict": "accept", "notifications": "off"}, nil)
	s.final("Personal settings saved too.", nil)
	got = s.send(engagementGroup, engagementOwner, 723, "@testbot "+request)
	current, err = s.b.engagements.ConversationPreferenceAt(engagementGroup, engagementOwner, engagementNow)
	if err != nil || current.Notifications != ConversationOn || strings.Contains(got, "Tracking notifications —") {
		t.Fatalf("replay added a scope: %+v, %v, receipt=%q", current, err, got)
	}
}

func TestEngagementDialogueAnswerLabelCannotConfirmPreference(t *testing.T) {
	s := newEngagementScript(t)
	request := "请把主动闲聊关掉。"
	s.reviews.Store(request, map[string]bool{"allow": true})
	s.final(`{"kind":"answer","text":"我已关闭主动闲聊。"}`, nil)
	got := s.send(engagementOwner, engagementOwner, 725, request)
	pref, err := s.b.engagements.ConversationPreferenceAt(engagementOwner, engagementOwner, engagementNow)
	if err != nil || pref.Chatter != ConversationDefault || strings.Contains(got, "我已关闭") {
		t.Fatalf("receipt = %q, settings = %+v, %v", got, pref, err)
	}
}

func TestEngagementDialogueReplyBoundaryWithReviewUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name, msg, draft, want string
		voice, forwarded       bool
		commit, existing       bool
		contextRead            bool
		wantReviews            int64
	}{
		{name: "typed ordinary answer", msg: "請把兩句近況總結成一句。", draft: `{"kind":"answer","text":"專案進展順利。"}`, want: "專案進展順利。"},
		{name: "legacy factual answer", msg: "What is the capital of Taiwan?", draft: "Taipei is the capital of Taiwan.", want: "Taipei is the capital of Taiwan."},
		{name: "bare informational answer after reading context", msg: "Why does ice float?", draft: "Ice is less dense than liquid water.", want: "Ice is less dense than liquid water.", contextRead: true},
		{name: "conditional explanation", msg: "Why can a reminder notify me twice?", draft: "If a reminder is saved in two calendars, both can notify you.", want: "If a reminder is saved in two calendars"},
		{name: "trailing conditional explanation", msg: "Why do reminders sometimes appear twice?", draft: "Both calendars can notify you if your reminder is saved in both.", want: "Both calendars can notify you if your reminder is saved in both."},
		{name: "settings then sync explanation", msg: "When does the app save my settings?", draft: "Your changes are saved when you close Settings. The calendar will use them the next time it syncs.", want: "Your changes are saved when you close Settings. The calendar will use them the next time it syncs."},
		{name: "general behavior in mistaken engagement draft", msg: "When does the app save my reminders?", draft: `{"kind":"engagement","text":"Your reminders are saved when you close Settings."}`, want: "Your reminders are saved when you close Settings."},
		{name: "general saving behavior", msg: "What happens to offline reminders?", draft: "A reminder is saved in your local calendar while sync is off.", want: "A reminder is saved in your local calendar"},
		{name: "calendar instructions", msg: "How can I disable calendar notifications?", draft: `{"kind":"answer","text":"Open your settings and turn off calendar notifications."}`, want: "Open your settings", contextRead: true},
		{name: "quoted UI status", msg: "What does the reminder confirmation look like?", draft: `The app displays “Your reminder was saved.”`, want: "The app displays"},
		{name: "condition followed by an actual promise", msg: "Why can reminders appear twice?", draft: "If a reminder is saved twice, both calendars may notify you. I'll notify you when it happens.", want: "No tracking agreement"},
		{name: "condition followed by a completed action", msg: "Why can reminders appear twice?", draft: "If a reminder is saved twice, both calendars may notify you. I've saved yours.", want: "No tracking agreement"},
		{name: "model volunteered an unconfirmed change", msg: "I'm swamped this week.", draft: "I'll set up a reminder for you.", want: "No tracking agreement", wantReviews: 1},
		{name: "split passive save and follow-up", msg: "Could you let me know as soon as the stable release ships?", draft: `{"kind":"answer","text":"Your request is saved. I'll let you know as soon as it ships."}`, want: "No tracking agreement", voice: true},
		{name: "text without a decision", msg: "Let me know when the stable release arrives.", draft: "You're on the watch list.", want: "No tracking agreement"},
		{name: "typed proposal without a decision", msg: "Please keep tracking the stable release.", draft: `{"kind":"engagement","text":"You're on the watch list."}`, want: "No tracking agreement"},
		{name: "typed watchlist promise", msg: "Could you put me on the release watchlist?", draft: `{"kind":"engagement","text":"I'll add you to the release watchlist. You'll hear from me as soon as it ships."}`, want: "No tracking agreement"},
		{name: "ordinary label on transcribed watchlist request", msg: "Could you put me on the release watchlist?", draft: `{"kind":"answer","text":"I'll add you to the release watchlist. You'll hear from me as soon as it ships."}`, want: "No tracking agreement", voice: true},
		{name: "typed engagement without familiar action words", msg: "Could you take care of it?", draft: `{"kind":"engagement","text":"Leave it with me. You'll hear from me."}`, want: "No tracking agreement", wantReviews: 1},
		{name: "engagement draft on an informational question", msg: "Why can reminders appear twice?", draft: `{"kind":"engagement","text":"I'll take care of yours after the next sync."}`, want: "No tracking agreement", wantReviews: 1},
		{name: "context without familiar action words", msg: "Could you take care of it?", draft: `{"kind":"answer","text":"Leave it with me. You'll hear from me."}`, want: "No tracking agreement", wantReviews: 1, contextRead: true},
		{name: "ordinary label after reading agreements", msg: "Please keep tracking the stable release.", draft: `{"kind":"answer","text":"You're on the watch list."}`, want: "No tracking agreement", contextRead: true},
		{name: "transcribed voice without a decision", msg: "Let me know when the stable release arrives.", draft: "I'll keep an ear out and ping you.", want: "No tracking agreement", voice: true, wantReviews: 1},
		{name: "transcribed voice with a decision", msg: "Let me know when the stable release arrives.", draft: "I have no idea.", want: "Agreement saved", voice: true, commit: true},
		{name: "committed pause despite review outage", msg: "Pause the release tracking.", want: "This item is paused", voice: true, commit: true, existing: true, wantReviews: 1},
		{name: "forwarded voice is not authority", msg: "Let me know when the stable release arrives.", draft: "You're on the watch list.", want: "No tracking agreement", voice: true, forwarded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stream, reviews, transcriptions, syntheses atomic.Int64
			var ref EngagementRef
			provider := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
				if !req.Stream {
					if tc.commit && !tc.existing && strings.HasSuffix(req.Messages[0].Content, engagementIntentInstruction) {
						roleCompletion(w, `{"task":true}`)
						return
					}
					reviews.Add(1)
					http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
					return
				}
				names := einoReplyToolNames(req)
				if tc.voice && (slices.Contains(names, "engagement_decide") == tc.forwarded) {
					t.Errorf("voice tool policy: %v (forwarded %v)", names, tc.forwarded)
				}
				round := stream.Add(1)
				switch {
				case (tc.existing || tc.contextRead) && round == 1:
					engagementToolFrame(w, "engagement_context", map[string]string{"id": ref.ID})
				case tc.existing && round == 2:
					engagementToolFrame(w, "engagement_decide", map[string]any{
						"id": ref.ID, "version": ref.Version, "change": "pause", "verdict": "accept",
					})
				case tc.existing && round == 3:
					http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
				case tc.commit && round == 1:
					engagementToolFrame(w, "engagement_decide", regressionWatchArgs())
				default:
					engagementModelText(w, tc.draft)
				}
			})
			media := http.NewServeMux()
			media.HandleFunc("POST /botrole/getFile", func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, `{"ok":true,"result":{"file_id":"voice","file_path":"voice"}}`)
			})
			media.HandleFunc("GET /file/botrole/voice", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "local audio") })
			b, sent := newRoleTestBot(t, provider, media)
			b.cfg.ProfileEnabled = false
			b.engagements = NewEngagementStore(b.chatDB)
			b.engagements.now = func() time.Time { return engagementNow }
			if tc.existing {
				ref = createEngagement(t, b.engagements, engagementFixture(engagementOwner))
			} else if tc.commit {
				enableTestEngagementMCP(t, b, newEngagementMCP(t, "release", true))
			}
			msg := roleMessage(engagementOwner, engagementOwner, 720, tc.msg)
			if tc.voice {
				stt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					transcriptions.Add(1)
					fmt.Fprintf(w, `{"text":%q}`, tc.msg)
				}))
				t.Cleanup(stt.Close)
				b.stt = NewSTTClient(Config{STTEnabled: true, STTAPIBase: stt.URL + "/v1", STTAPIKey: "local"})
				msg.Text, msg.Voice = "", &tele.Voice{File: tele.File{FileID: "voice"}}
				if tc.forwarded {
					msg.OriginalUnixtime = 1
				}
			}
			if strings.Contains(tc.want, "agreement") || tc.commit {
				tts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					syntheses.Add(1)
					http.Error(w, "unexpected synthesis", http.StatusBadRequest)
				}))
				t.Cleanup(tts.Close)
				b.tts = &VolcengineTTSClient{url: tts.URL, httpClient: tts.Client()}
				b.speechModes = NewSpeechModeStore()
				b.speechModes.Set(engagementOwner, true)
			}
			handler := b.handleText
			if tc.voice {
				handler = b.handleVoice
			}
			roleHandle(t, b, handler, msg)
			reply := roleReceive(t, sent)
			if reply.text == "🔊 正在发送语音…" {
				reply = roleReceive(t, sent)
			}
			waitEinoReplyHistory(t, b, engagementOwner, 2)
			if !strings.Contains(reply.text, tc.want) || strings.HasPrefix(reply.text, "{") {
				t.Errorf("delivered %q, want %q", reply.text, tc.want)
			}
			rows, err := b.engagements.Visible(EngagementViewer{ChatID: engagementOwner, ActorID: engagementOwner}, "", 8)
			if err != nil || (len(rows) == 1) != tc.commit ||
				reviews.Load() != tc.wantReviews || syntheses.Load() != 0 {
				t.Fatalf("agreements=%d reviews=%d (want %d) syntheses=%d err=%v", len(rows), reviews.Load(), tc.wantReviews, syntheses.Load(), err)
			}
			if tc.existing && (rows[0].Status != EngagementPaused || rows[0].Ref.Version != ref.Version+1) {
				t.Fatalf("the pause receipt did not reflect the persisted item: %+v", rows[0])
			}
			if tc.voice && transcriptions.Load() != 1 {
				t.Fatalf("STT requests=%d, want 1", transcriptions.Load())
			}
			t.Logf("Telegram %s=%q; agreements=%d; auxiliary reviews=%d; STT=%d; TTS=%d", reply.path, reply.text,
				len(rows), reviews.Load(), transcriptions.Load(), syntheses.Load())
		})
	}
}
