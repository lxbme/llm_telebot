package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	openai "github.com/sashabaranov/go-openai"
)

func TestRoleAccessRelevanceRetryRechecksWithdrawal(t *testing.T) {
	const fact = "the group code is cedar"
	requests := make(chan openai.ChatCompletionRequest, 4)
	release := make(chan struct{})
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, r *http.Request, round int, req openai.ChatCompletionRequest) {
		requests <- req
		if round == 0 {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			// A truncated JSON reply returns EOF from the real model client.
			w.WriteHeader(http.StatusOK)
			return
		}
		roleCompletion(w, "NO")
	})
	b, deliveries := newRoleTestBot(t, model)
	b.cfg.AutoDetect = true
	b.detectorAI, b.detectorModel = model.client, "detector"
	b.registerHandlers()
	seedRoleFact(t, b, fact)
	dispatchRoleText(b, 42, 42, 1, "/memory_share -100123 "+fact)
	roleReceive(t, deliveries)
	done := make(chan struct{})
	go func() {
		defer close(done)
		dispatchRoleText(b, regressionGroup, 43, 2, "what is the group code?")
	}()
	if req := roleReceive(t, requests); req.Model != "detector" || !strings.Contains(fmt.Sprint(req.Messages), fact) {
		t.Errorf("initial relevance request = %+v", req)
	}
	dispatchRoleText(b, 42, 42, 3, "/memory_unshare -100123 "+fact)
	if d := roleReceive(t, deliveries); !strings.Contains(d.text, "withdrawn") {
		t.Errorf("withdrawal = %+v", d)
	}
	close(release)
	roleReceive(t, done)
	select {
	case retry := <-requests:
		if strings.Contains(fmt.Sprint(retry.Messages), fact) {
			t.Fatal("relevance retried with the withdrawn conclusion")
		}
	default:
	}
	events := b.dashboardEvents.Tail(0, 10)
	if len(events) != 1 || events[0].Type != DashboardEventUsageRecorded || events[0].Model != "detector" ||
		events[0].ChatID != regressionGroup || events[0].UserID != 43 || events[0].Success {
		t.Errorf("relevance usage should record only the failed provider attempt: %+v", events)
	}
}

func TestRoleAccessStickerBatchRechecksWithdrawal(t *testing.T) {
	for _, mode := range []string{stickerModeAppend, stickerModeReplace} {
		t.Run(mode, func(t *testing.T) {
			const fact = "group celebration reason"
			var b *Bot
			var stickerCalls atomic.Int32
			var deletionCalls atomic.Int32
			withdraw := func() { dispatchRoleText(b, 42, 42, 3, "/memory_unshare -100123 "+fact) }
			bot, deliveries := newRoleTestBotWithTelegram(t, newEinoReplyProvider(t, nil), func(w http.ResponseWriter, _ *http.Request, d roleTestDelivery) bool {
				switch d.path {
				case "/botrole/deleteMessage":
					if deletionCalls.Add(1) == 1 && mode == stickerModeReplace {
						withdraw()
					}
					fmt.Fprint(w, `{"ok":true,"result":true}`)
				case "/botrole/sendSticker":
					if stickerCalls.Add(1) == 1 && mode == stickerModeAppend {
						withdraw()
					}
					fmt.Fprint(w, `{"ok":true,"result":{"message_id":50,"sticker":{"file_id":"local-sticker-id","file_unique_id":"local-unique-id","width":64,"height":64,"is_animated":false,"is_video":false},"chat":{"id":-100123,"type":"supergroup"}}}`)
				default:
					return false
				}
				return true
			})
			b = bot
			b.cfg.StickerEnabled, b.cfg.StickerMode = true, mode
			b.cfg.StickerSendProbability, b.cfg.StickerMaxPerReply = 1, 3
			b.stickers = &StickerEngine{rules: StickerRules{
				LabelStickers:  map[string][]string{"celebrate": {"local-sticker-id"}},
				KeywordToLabel: map[string]string{"celebrate": "celebrate"},
			}}
			b.registerHandlers()
			seedRoleFact(t, b, fact)
			dispatchRoleText(b, 42, 42, 1, "/memory_share -100123 "+fact)
			roleReceive(t, deliveries)
			msg := roleMessage(regressionGroup, 43, 2, "celebrate")
			turn := b.beginTelegramTurn(msg)
			if prompt := b.snapshot().buildRoleContext(&turn, msg.Text, "", 0, true); !strings.Contains(fmt.Sprint(prompt), fact) {
				t.Fatal("the original sticker turn lacked its approved input")
			}
			b.finalizeStickerReply(turn, msg.Chat, openai.ChatCompletionMessage{Content: msg.Text}, "celebrate", msg)
			if d := roleReceive(t, deliveries); !strings.Contains(d.text, "withdrawn") {
				t.Errorf("withdrawal = %+v", d)
			}
			if mode == stickerModeReplace && (stickerCalls.Load() != 0 || deletionCalls.Load() != 1) {
				t.Fatalf("after replacement deletion, stickers = %d, deletions = %d", stickerCalls.Load(), deletionCalls.Load())
			}
			if mode == stickerModeAppend && stickerCalls.Load() != 1 {
				t.Fatalf("after the first sticker, the batch sent %d stickers", stickerCalls.Load())
			}
		})
	}
}

func TestRoleAccessLegacyCronRechecksMembership(t *testing.T) {
	const approved, groupFact, privateFact = "approved cedar nickname", "group cedar project", "unshared cedar note"
	for _, name := range []string{"after_initial_check", "before_tool", "before_delivery", "group_only"} {
		t.Run(name, func(t *testing.T) {
			withCreator := name != "group_only"
			var memberCalls, toolCalls atomic.Int32
			var left atomic.Bool
			requests := make(chan openai.ChatCompletionRequest, 4)
			model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, round int, req openai.ChatCompletionRequest) {
				requests <- req
				if round == 0 && withCreator {
					left.Store(true)
				}
				if round == 0 && name == "before_tool" {
					einoModelSSE(w, einoReplyToolFrame("cron-lookup", "cron_lookup", 0))
					einoReplyFinish(w, "tool_calls")
					return
				}
				einoModelSSE(w, einoReplyTextFrame("scheduled cedar answer"))
				einoReplyFinish(w, "stop")
			})
			b, deliveries := newRoleTestBotWithTelegram(t, model, func(w http.ResponseWriter, _ *http.Request, d roleTestDelivery) bool {
				if d.path != "/botrole/getChatMember" {
					return false
				}
				status := "member"
				if left.Load() {
					status = "left"
				}
				if memberCalls.Add(1) == 1 && name == "after_initial_check" {
					left.Store(true)
				}
				fmt.Fprintf(w, `{"ok":true,"result":{"status":%q,"user":{"id":42,"first_name":"Owner"}}}`, status)
				return true
			})
			b.tools = NewToolRegistry()
			lookup := einoTestMCPTool("cron_lookup", "test global lookup", "")
			lookup.run = func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				toolCalls.Add(1)
				return mcp.NewToolResultText("global lookup"), nil
			}
			addEinoTestMCPServer(t, b.tools, "global", lookup)
			ref := seedRoleFact(t, b, approved)
			if _, err := b.memory.Share(ref, 42, regressionGroup, approved); err != nil {
				t.Fatal(err)
			}
			for _, item := range []struct {
				chat int64
				text string
				cue  MemoryCue
			}{{regressionGroup, groupFact, ""}, {42, privateFact, MemoryAskFirst}} {
				if _, err := b.memory.Learn(MemoryLearn{Subjects: []int64{42}, Text: item.text, Cue: item.cue,
					Evidence: MemoryEvidence{Kind: MemoryStatement, Source: MemorySource{ChatID: item.chat, SenderID: 42, MessageID: 100}}}); err != nil {
					t.Fatal(err)
				}
			}
			task := ScheduledTask{ID: "legacy", Prompt: "check cedar", Context: true, CronExpr: "0 8 * * *", Timezone: "UTC", Enabled: true}
			if withCreator {
				task.CreatedBy = 42
			} else {
				left.Store(true)
			}
			tasks := NewTaskStore(nil)
			tasks.Upsert(regressionGroup, task)
			NewTaskRunner(b, tasks).execute(regressionGroup, task)
			if name == "after_initial_check" {
				if got := tasks.List(regressionGroup); len(got) != 1 || got[0].LastError == "" {
					t.Errorf("cron did not report a failed admission: %+v", got)
				}
				if model.calls.Load() != 0 || memberCalls.Load() < 2 {
					t.Errorf("cron reached the model after loss of membership: model=%d membership=%d", model.calls.Load(), memberCalls.Load())
				}
				return
			}
			req := roleReceive(t, requests)
			material := fmt.Sprint(req.Messages)
			if !strings.Contains(material, approved) || !strings.Contains(material, groupFact) ||
				strings.Contains(material, privateFact) || strings.Contains(material, "ask_first") {
				t.Errorf("scheduled role context used the wrong audience: %s", material)
			}
			d := roleReceive(t, deliveries)
			if withCreator {
				if d.path != "/botrole/deleteMessage" || toolCalls.Load() != 0 || memberCalls.Load() < 2 {
					t.Errorf("membership-dependent cron after departure: delivery=%+v tools=%d membership checks=%d", d, toolCalls.Load(), memberCalls.Load())
				}
				select {
				case next := <-requests:
					t.Errorf("cron made another model request after departure: %+v", next.Messages)
				default:
				}
			} else if d.text != "scheduled cedar answer" || memberCalls.Load() != 0 {
				t.Errorf("group-only cron: delivery=%+v membership checks=%d", d, memberCalls.Load())
			} else {
				waitEinoReplyHistory(t, b, regressionGroup, 2)
			}
		})
	}
}

func TestRoleAccessGroupGrantRequiresExplicitRequest(t *testing.T) {
	requests := make(chan openai.ChatCompletionRequest, 8)
	var calls atomic.Int32
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
		requests <- req
		if !strings.Contains(fmt.Sprint(req.Messages), "ledger-call") {
			frame := einoReplyToolFrame("ledger-call", "ledger", 0)
			einoModelSSE(w, strings.Replace(frame, `"arguments":"{}"`, `"arguments":"{\"id\":7}"`, 1))
			einoReplyFinish(w, "tool_calls")
			return
		}
		einoModelSSE(w, einoReplyTextFrame("done"))
		einoReplyFinish(w, "stop")
	})
	b, deliveries := newRoleTestBot(t, model)
	b.cfg.ProfileEnabled = false
	b.tools = NewToolRegistry()
	addEinoTestMCPServer(t, b.tools, "global", einoTestMCPTool("public_lookup", "public", ""))
	b.userTools = NewUserToolManager(nil, nil, nil)
	personal := NewToolRegistry()
	ledger := einoTestMCPTool("ledger", "private ledger", "")
	ledger.run = func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if req.GetInt("id", 0) != 7 {
			t.Errorf("unapproved arguments: %+v", req.Params.Arguments)
		}
		calls.Add(1)
		return mcp.NewToolResultText("authorized ledger value"), nil
	}
	addEinoTestMCPServer(t, personal, "private", ledger)
	b.userTools.registries[42] = personal
	b.registerHandlers()
	dispatchRoleText(b, 42, 42, 1, `/mcp_share -100123 ledger {"id":7}`)
	roleReceive(t, deliveries)

	dispatchRoleText(b, regressionGroup, 42, 2, "@testbot tell me a joke")
	assertReplyMCPNames(t, roleReceive(t, requests), "public_lookup")
	roleReceive(t, requests)
	roleReceive(t, deliveries)
	waitEinoReplyHistory(t, b, regressionGroup, 2)
	if got := calls.Load(); got != 0 {
		t.Errorf("unrelated owner message executed the personal lookup %d time(s)", got)
	}

	for _, denied := range []struct {
		chat, sender int64
		payload      string
	}{
		{regressionGroup, 43, `{"id":7}`},
		{-100124, 42, `{"id":7}`},
		{regressionGroup, 42, `{"id":8}`},
	} {
		dispatchRoleText(b, denied.chat, denied.sender, 3, "/mcp_use ledger "+denied.payload)
		if d := roleReceive(t, deliveries); !strings.Contains(d.text, "no matching authorization") {
			t.Errorf("unauthorized lookup admission: %+v", d)
		}
	}

	dispatchRoleText(b, regressionGroup, 42, 4, `/mcp_use ledger {"id":7}`)
	assertReplyMCPNames(t, roleReceive(t, requests), "public_lookup", "ledger")
	next := roleReceive(t, requests)
	if got := calls.Load(); got != 1 || !strings.Contains(fmt.Sprint(next.Messages), "authorized ledger value") {
		t.Errorf("explicit group lookup: calls = %d, next prompt = %+v", got, next.Messages)
	}
	roleReceive(t, deliveries)
	waitEinoReplyHistory(t, b, regressionGroup, 4)
}

// WithTimeout consults the parent deadline immediately before admission to
// the completion client. Hold that boundary without changing runtime code.
type pausedRoleDeadline struct {
	context.Context
	once             sync.Once
	reached, release chan struct{}
}

func (ctx *pausedRoleDeadline) Deadline() (time.Time, bool) {
	ctx.once.Do(func() {
		close(ctx.reached)
		<-ctx.release
	})
	return ctx.Context.Deadline()
}

func TestRoleAccessCorrectionRefreshesBeforeFirstClassification(t *testing.T) {
	const original, revised, correction = "I choose black tea", "I choose white tea", "Correction: I choose green tea"
	requests := make(chan openai.ChatCompletionRequest, 3)
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
		requests <- req
		roleCompletion(w, `{"kind":"correction","conclusion":"`+correction+`","replace":1}`)
	})
	b, _ := newRoleTestBot(t, model)
	ref := seedRoleFact(t, b, original)
	turn := b.beginTelegramTurn(roleMessage(42, 42, 102, correction))
	barrier := &pausedRoleDeadline{Context: turn.generation, reached: make(chan struct{}), release: make(chan struct{})}
	turn.generation = barrier
	review := b.prepareMemoryReview(turn, b.snapshot(), correction)
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.learnFromRoleTurn(turn, review)
	}()
	roleReceive(t, barrier.reached)
	_, err := b.memory.Learn(MemoryLearn{Expect: ref, Subjects: []int64{42}, Text: revised,
		Evidence: MemoryEvidence{Kind: MemoryStatement, Source: MemorySource{ChatID: 42, SenderID: 42, MessageID: 101}}})
	close(barrier.release)
	if err != nil {
		t.Fatal(err)
	}
	roleReceive(t, done)
	entry, err := b.memory.Get(ref.ID)
	if err != nil || entry.Text != correction || len(entry.Evidence) != 3 {
		t.Fatalf("correction was lost before classification: %+v (%v)", entry, err)
	}
	req := roleReceive(t, requests)
	if material := fmt.Sprint(req.Messages); strings.Contains(material, original) || !strings.Contains(material, revised) || model.calls.Load() != 1 {
		t.Errorf("reflection sent stale candidates or issued extra requests: %s", material)
	}
}

func TestRoleAccessWithdrawDuplicateApprovedWording(t *testing.T) {
	const text = "Call me Cedar"
	b, deliveries := newRoleTestBot(t, newEinoReplyProvider(t, nil))
	b.registerHandlers()
	first := seedRoleFact(t, b, text+" during the meeting")
	dispatchRoleText(b, 42, 42, 1, "/memory_share -100123 "+text)
	roleReceive(t, deliveries)
	add := func(owner, id int64) MemoryRef {
		ref, err := b.memory.Learn(MemoryLearn{Subjects: []int64{owner}, Text: text,
			Evidence: MemoryEvidence{Kind: MemoryStatement, Source: MemorySource{ChatID: owner, SenderID: owner, MessageID: id}}})
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	second, otherOwner := add(42, 100), add(43, 100)
	for _, grant := range []struct{ owner, group int64 }{{42, regressionGroup}, {42, -100124}, {43, regressionGroup}} {
		dispatchRoleText(b, grant.owner, grant.owner, 2, fmt.Sprintf("/memory_share %d %s", grant.group, text))
		if d := roleReceive(t, deliveries); !strings.Contains(d.text, "can now be used") {
			t.Fatalf("setup share = %+v", d)
		}
	}
	dispatchRoleText(b, 42, 42, 3, "/memory_unshare -100123 "+text)
	if d := roleReceive(t, deliveries); !strings.Contains(d.text, "withdrawn") || strings.Contains(d.text, text) {
		t.Errorf("duplicate withdrawal = %+v", d)
	}
	for _, check := range []struct {
		ref   MemoryRef
		group int64
		want  string
	}{{first, regressionGroup, ""}, {second, regressionGroup, ""}, {second, -100124, text}, {otherOwner, regressionGroup, text}} {
		entry, err := b.memory.Get(check.ref.ID)
		if err != nil || entry.Shares[check.group] != check.want {
			t.Errorf("scope %s/%d: grant=%q want=%q err=%v", check.ref.ID, check.group, entry.Shares[check.group], check.want, err)
		}
	}
}

func TestRoleAccessAlreadyDeliveredTextSurvivesSpeechModeChange(t *testing.T) {
	const fact = "the shared cedar password"
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
		if !strings.Contains(fmt.Sprint(req.Messages), fact) {
			t.Error("model did not receive the authorized conclusion")
		}
		einoModelSSE(w, einoReplyTextFrame(fact))
		einoReplyFinish(w, "stop")
	})
	var voiced, synthesized atomic.Int32
	tts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		synthesized.Add(1)
		http.Error(w, "unexpected synthesis", http.StatusServiceUnavailable)
	}))
	t.Cleanup(tts.Close)
	var b *Bot
	bot, deliveries := newRoleTestBotWithTelegram(t, model, func(w http.ResponseWriter, _ *http.Request, d roleTestDelivery) bool {
		if d.path == "/botrole/editMessageText" && d.text == fact {
			// Telegram has accepted the text. Make the subsequent voice
			// attempt see a new speech setting and a withdrawn conclusion.
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":10,"chat":{"id":-100123,"type":"supergroup"}}}`)
			b.speechModes.Set(regressionGroup, true)
			dispatchRoleText(b, 42, 42, 3, "/memory_unshare -100123 "+fact)
			return true
		}
		if d.path == "/botrole/sendVoice" {
			voiced.Add(1)
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":11,"chat":{"id":-100123,"type":"supergroup"}}}`)
			return true
		}
		return false
	})
	b = bot
	b.cfg.ProfileEnabled = false
	b.speechModes = NewSpeechModeStore()
	b.tts = &VolcengineTTSClient{url: tts.URL, httpClient: tts.Client()}
	// The real reply releases its tool generation after all follow-up
	// delivery work, including attempts made after saving history.
	done := make(chan struct{})
	b.mcpClients = NewMCPClientManager()
	b.replyTools = &replyToolGeneration{cleanup: func() { close(done) }}
	b.registerHandlers()
	seedRoleFact(t, b, fact)
	dispatchRoleText(b, 42, 42, 1, "/memory_share -100123 "+fact)
	roleReceive(t, deliveries)
	dispatchRoleText(b, regressionGroup, 43, 2, "@testbot what is the password?")
	if d := roleReceive(t, deliveries); !strings.Contains(d.text, "withdrawn") {
		t.Fatalf("withdrawal = %+v", d)
	}
	roleReceive(t, done)
	select {
	case d := <-deliveries:
		t.Errorf("Telegram received a new operation after text delivery and withdrawal: %+v", d)
	default:
	}
	if h := b.store.Get(regressionGroup); len(h) != 2 || h[1].Content != fact {
		t.Errorf("the text confirmed by Telegram was lost from group history: %+v", h)
	}
	if synthesized.Load() != 0 || voiced.Load() != 0 {
		t.Error("withdrawal did not prevent new speech")
	}
}
