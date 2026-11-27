package app

import (
	"context"
	"errors"
	"fmt"
	"html"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	openai "github.com/sashabaranov/go-openai"
	tele "gopkg.in/telebot.v3"
)

type roleReplyRunKey struct{}

var errEinoEmptyReply = errors.New("model returned no text or usable tool results")

type roleReplyResult struct {
	ModelError, DeliveryError error
	Delivered                 bool
}

func (r roleReplyResult) err() error {
	if errors.Is(r.ModelError, errEinoEmptyReply) {
		return errors.Join(errEinoEmptyReply, r.DeliveryError)
	}
	if r.ModelError != nil {
		return errors.Join(fmt.Errorf("model could not complete the reply"), r.DeliveryError)
	}
	if r.DeliveryError != nil {
		return r.DeliveryError
	}
	if !r.Delivered {
		return fmt.Errorf("Telegram did not confirm reply delivery")
	}
	return nil
}

type roleReplyRun struct {
	turn            RoleTurn
	deliveredText   string
	engagement      *engagementDialogue
	decisionReply   bool
	scheduled       bool
	speechAttempted bool
	toolFallback    *einoScheduledFallback
	result          roleReplyResult
}

func checkRoleReplyUse(ctx context.Context) error {
	if run, ok := ctx.Value(roleReplyRunKey{}).(*roleReplyRun); ok {
		return run.turn.checkUse(ctx)
	}
	return ctx.Err()
}

type einoReplyRounds struct {
	adk.BaseChatModelAgentMiddleware
	tools      *einoToolSet
	toolRounds int
	round      int
	modelInput []*schema.Message
}

func (r *einoReplyRounds) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, _ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	if err := checkRoleReplyUse(ctx); err != nil {
		return ctx, state, err
	}

	r.round++
	if r.round <= r.toolRounds {
		state.ToolInfos = r.tools.RoundToolInfos()
	} else {
		state.ToolInfos = nil
		// ADK only starts another model call after a tool-call round.
		if r.toolRounds > 0 {
			state.Messages = append(state.Messages, schema.UserMessage(toolRoundLimitPrompt))
		}
	}
	// The transcript includes results from UnknownToolsHandler, which ADK
	// forwards to the model without emitting tool events.
	r.modelInput = state.Messages
	return ctx, state, nil
}

// runEinoReply consumes this request's ADK events and reports cumulative text
// for the current round. An empty preview clears text from a tool-call round.
func (b *Bot) runEinoReply(
	ctx context.Context,
	snap runtimeSnapshot,
	messages []openai.ChatCompletionMessage,
	tools *einoToolSet,
	usageCtx UsageContext,
	onPreview func(string),
	waitForPreviewReset func(context.Context),
) (string, error) {
	input, err := toEinoMessages(messages)
	if err != nil {
		return "", err
	}
	model := &einoChatModel{
		client:              snap.ai,
		modelName:           snap.cfg.OpenAIModel,
		maxTokens:           snap.cfg.MaxTokens,
		retry:               snap.cfg.EinoModelRetryEnabled,
		backupModel:         snap.cfg.EinoFallbackModel,
		usageCtx:            usageCtx,
		onPreview:           onPreview,
		waitForPreviewReset: waitForPreviewReset,
		recordUsage: func(event UsageEvent) {
			b.recordUsageEventFor(snap, event)
		},
	}
	rounds := &einoReplyRounds{tools: tools}
	if tools != nil && len(tools.rawTools) > 0 {
		model.tools = tools.rawTools
		rounds.toolRounds = min(max(snap.cfg.ToolsMaxIterations, 0), math.MaxInt-1)
	}
	ctx = withEinoToolEvents(ctx, usageCtx, b.recordDashboardEvent)
	run, _ := ctx.Value(roleReplyRunKey{}).(*roleReplyRun)
	toolPayloads := einoToolPayloads{scheduled: run != nil && run.scheduled}
	ctx = context.WithValue(ctx, einoToolPayloadsKey{}, &toolPayloads)
	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:          "telegram_reply",
		Model:         model,
		ToolsConfig:   adk.ToolsConfig{ToolsNodeConfig: tools.ToolsNodeConfig()},
		MaxIterations: rounds.toolRounds + 1,
		Handlers:      []adk.ChatModelAgentMiddleware{rounds},
		GenModelInput: func(_ context.Context, _ string, input *adk.AgentInput) ([]*schema.Message, error) {
			return input.Messages, nil
		},
	})
	if err != nil {
		return "", err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	runner := adk.NewRunner(runCtx, adk.RunnerConfig{Agent: agent, EnableStreaming: true})
	events := runner.Run(runCtx, input)
	var finalText string
	for event, ok := events.Next(); ok; event, ok = events.Next() {
		if event.Err != nil && err == nil {
			err = event.Err
			cancel()
		}
		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}
		message := event.Output.MessageOutput
		if err != nil {
			// The iterator has no Close. Let the canceled run finish while
			// releasing any event streams that were already queued.
			if message.IsStreaming {
				message.MessageStream.Close()
			}
			continue
		}
		switch message.Role {
		case schema.Assistant:
			var reply *schema.Message
			reply, err = message.GetMessage()
			finalText = ""
			if err == nil && len(reply.ToolCalls) == 0 {
				finalText = reply.Content
			}
		case schema.Tool:
			_, err = message.GetMessage()
		default:
			if message.IsStreaming {
				message.MessageStream.Close()
			}
		}
		if err != nil {
			cancel()
		}
	}

	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil && !errors.Is(err, errEinoNoChoices) {
		return "", err
	}
	if strings.TrimSpace(finalText) == "" {
		// ADK appends tool rounds to the supplied input. Only this reply's
		// results belong in its fallback and can establish a usable outcome.
		current := rounds.modelInput[min(len(input), len(rounds.modelInput)):]
		hasPayload, results := toolPayloads.snapshot()
		var text string
		if toolPayloads.scheduled {
			if len(results) != 0 {
				run.toolFallback = newEinoScheduledFallback(results)
				text = run.toolFallback.plain
			} else {
				text = einoToolFallback(nil)
			}
		} else {
			text = einoToolFallback(current)
		}
		if !hasPayload {
			return text, errEinoEmptyReply
		}
		finalText = text
	}
	return finalText, nil
}

const einoFallbackHeader = "⚠️ The model did not generate a final reply. Here are the raw tool results for reference (information may be incomplete):\n\n"
const einoFallbackSeparator = "\n\n---\n\n"

func einoToolFallback(messages []*schema.Message) string {
	var parts []string
	for _, m := range messages {
		if m.Role == schema.Tool && strings.TrimSpace(m.Content) != "" {
			parts = append(parts, m.Content)
		}
	}
	if len(parts) == 0 {
		return "⚠️ The model returned an empty response."
	}
	return einoFallbackHeader + strings.Join(parts, einoFallbackSeparator)
}

type einoScheduledFallback struct {
	html, plain              string
	htmlProof                string
	payloadStart, payloadEnd int
}

func newEinoScheduledFallback(results []mcpToolOutput) *einoScheduledFallback {
	var rendered, plain strings.Builder
	var payloads []string
	var htmlEnd, payloadStart, payloadEnd int
	rendered.WriteString(einoFallbackHeader)
	plain.WriteString(einoFallbackHeader)
	for i, result := range results {
		if i > 0 {
			rendered.WriteString("\n\n———\n\n")
			plain.WriteString(einoFallbackSeparator)
		}
		start := plain.Len()
		plain.WriteString(result.text)
		if !result.hasPayload {
			rendered.WriteString(mdToTgHTML(result.text))
			continue
		}
		// MCP text is data. A Markdown reference, indentation or log
		// punctuation must survive rendering as literal text.
		rendered.WriteString("<pre>" + html.EscapeString(result.text) + "</pre>")
		if len(payloads) == 0 {
			htmlEnd, payloadStart, payloadEnd = rendered.Len(), start, plain.Len()
		}
		payloads = append(payloads, result.text)
	}
	fallback := &einoScheduledFallback{html: rendered.String(), plain: plain.String()}
	if len(payloads) == 0 {
		return fallback
	}
	fallback.htmlProof = visibleTelegramHTML(fallback.html[:htmlEnd])
	// Try the combined rendered edit first. When plain text needs
	// truncation, give its budget to the business results.
	if !fitsTelegramMessage(fallback.plain) {
		fallback.plain = einoFallbackHeader + strings.Join(payloads, einoFallbackSeparator)
		payloadStart = len(einoFallbackHeader)
		payloadEnd = payloadStart + len(payloads[0])
	}
	fallback.payloadStart, fallback.payloadEnd = payloadStart, payloadEnd
	return fallback
}

func (f *einoScheduledFallback) confirms(text string, spoken bool, mode tele.ParseMode) bool {
	// Include the result's position to distinguish a diagnostic that
	// happens to quote the same text from a delivered business result.
	if f.payloadEnd == 0 {
		return false
	}
	plainProof := f.plain[:f.payloadEnd]
	if spoken {
		// Require the entire literal result at its position in the actual
		// TTS input, allowing only its usual whitespace normalization.
		before := formatTextForTTS(f.plain[:f.payloadStart])
		proof := formatTextForTTS(plainProof)
		payload := normalizeSpeechWhitespace(telegramLineEndings(f.plain[f.payloadStart:f.payloadEnd]))
		after, ok := strings.CutPrefix(proof, before)
		return ok && payload != "" && strings.TrimSpace(after) == payload &&
			utf8.RuneCountInString(proof) <= maxTTSTextRunes && strings.HasPrefix(text, proof)
	}
	text = telegramLineEndings(text)
	if mode == tele.ModeHTML {
		return strings.HasPrefix(text, telegramLineEndings(f.htmlProof))
	}
	return strings.HasPrefix(text, telegramLineEndings(plainProof))
}

func (b *Bot) streamEinoReply(
	ctx context.Context,
	snap runtimeSnapshot,
	messages []openai.ChatCompletionMessage,
	placeholder *tele.Message,
	view *MergedToolView,
	summaryMode bool,
	usageCtx UsageContext,
) string {
	started := time.Now()
	tools, err := newEinoTools(view, summaryMode)
	run, _ := ctx.Value(roleReplyRunKey{}).(*roleReplyRun)
	if err == nil && run != nil && run.engagement != nil && run.engagement.message.trusted {
		err = tools.addEngagementDialogue(run.engagement)
	}
	showText := snap.shouldSendSpeechText(usageCtx.ChatID)

	var finalText string
	if err == nil {
		dialogue := run != nil && run.engagement != nil
		// A partial draft cannot be checked for an implied promise. Keep the
		// neutral placeholder while the dialogue's text is still provisional.
		preview, waitForReset, stopPreview := snap.startReplyPreview(ctx, placeholder, showText && !dialogue && (run == nil || !run.scheduled))
		finalText, err = b.runEinoReply(ctx, snap, messages, tools, usageCtx, preview, waitForReset)
		stopPreview()
	}

	deliveryCtx := ctx
	if run != nil && run.engagement != nil {
		d := run.engagement
		if receipt := d.receipt(ctx, snap); receipt != "" {
			// A model failure after a local commit does not erase its result.
			finalText, err = receipt, nil
			run.decisionReply = true
			showText, deliveryCtx = true, run.turn.generation
		} else if err == nil {
			finalText, run.decisionReply = d.resolveDraft(ctx, snap, finalText)
			showText = showText || run.decisionReply
		}
	}
	if run != nil {
		run.result.ModelError = err
		if useErr := run.turn.checkUse(deliveryCtx); useErr != nil {
			run.result.DeliveryError = useErr
			return ""
		}
	}
	deliver := func(edit func()) bool {
		if run == nil {
			edit()
			return true
		}
		var permitted bool
		run.turn.withCurrent(func() {
			if run.turn.checkUse(deliveryCtx) != nil {
				return
			}
			permitted = true
			edit()
		})
		return permitted
	}

	event := DashboardEvent{
		ChatID:    usageCtx.ChatID,
		UserID:    usageCtx.UserID,
		Model:     snap.cfg.OpenAIModel,
		RequestID: usageCtx.RequestID,
		LatencyMs: time.Since(started).Milliseconds(),
	}
	if err != nil && !errors.Is(err, errEinoEmptyReply) {
		event.Type = DashboardEventConversationError
		event.Summary = "stream failed"
		deliver(func() {
			b.recordDashboardEvent(event)
			text := snap.editOrLog(placeholder, "❌ Stream error: the model could not complete the reply.")
			if run != nil {
				run.deliveredText = text
				if text == "" {
					run.result.DeliveryError = fmt.Errorf("Telegram did not confirm error message delivery")
				}
			}
		})
		return ""
	}
	event.Type = DashboardEventConversationFinished
	event.Success = err == nil
	if !event.Success {
		event.Type = DashboardEventConversationError
	}
	event.Summary = truncateDashboardText(finalText, 200)
	if run != nil && run.toolFallback != nil {
		fallback := run.toolFallback
		var mode tele.ParseMode
		edit := func(allowed func() bool) (string, error) {
			text, confirmedMode, err := snap.editRenderedReply(placeholder, fallback.html, fallback.plain, allowed, true)
			mode = confirmedMode
			return text, err
		}
		if placeholder.Chat == nil || placeholder.Chat.ID != run.turn.ChatID {
			run.result.DeliveryError = errTelegramDeliveryUnknown
			return ""
		}
		if !deliver(func() {
			allowed := func() bool { return run.turn.checkUse(deliveryCtx) == nil }
			if showText {
				run.deliveredText, run.result.DeliveryError = edit(allowed)
			} else {
				snap.editReplyResult(placeholder, finalText, false, allowed)
			}
		}) {
			run.result.DeliveryError = errRoleAudience
			return ""
		}
		var spoken bool
		if !showText {
			run.speechAttempted = true
			speech := snap.finalizeSpeechReply(run.turn, placeholder.Chat, nil, finalText, placeholder, false, edit)
			run.deliveredText, spoken, run.result.DeliveryError = speech.text, speech.spoken, speech.err
		}
		if run.result.DeliveryError == nil {
			if run.deliveredText == "" {
				run.result.DeliveryError = errTelegramDeliveryUnknown
			} else if !fallback.confirms(run.deliveredText, spoken, mode) {
				run.result.ModelError = errEinoEmptyReply
			}
		}
		event.Success = run.result.ModelError == nil && run.result.DeliveryError == nil
		if !event.Success {
			event.Type = DashboardEventConversationError
		}
		event.Summary = truncateDashboardText(run.deliveredText, 200)
		b.recordDashboardEvent(event)
		if showText || run.deliveredText == "" {
			return run.deliveredText
		}
		return finalText
	}
	if !deliver(func() {
		allowed := func() bool { return run == nil || run.turn.checkUse(deliveryCtx) == nil }
		b.recordDashboardEvent(event)
		if run != nil && run.decisionReply {
			current := b.snapshot()
			run.deliveredText = current.deliverEngagementReceipt(placeholder, finalText, allowed)
			return
		}
		text, deliveryErr := snap.editReplyResult(placeholder, finalText, showText, allowed)
		if run != nil {
			run.deliveredText, run.result.DeliveryError = text, deliveryErr
		}
	}) {
		return ""
	}
	return finalText
}
