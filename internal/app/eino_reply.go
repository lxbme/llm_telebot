package app

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	openai "github.com/sashabaranov/go-openai"
	tele "gopkg.in/telebot.v3"
)

type einoReplyRounds struct {
	adk.BaseChatModelAgentMiddleware
	tools      *einoToolSet
	toolRounds int
	round      int
	modelInput []*schema.Message
}

func (r *einoReplyRounds) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, _ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	if err := ctx.Err(); err != nil {
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
	if finalText == "" {
		transcript, err := fromEinoMessages(rounds.modelInput)
		if err != nil {
			return "", err
		}
		finalText = buildFallbackFromMessages(transcript)
	}
	return finalText, nil
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
	showText := snap.shouldSendSpeechText(usageCtx.ChatID)

	var finalText string
	if err == nil {
		preview, waitForPreviewReset, stopPreview := snap.startReplyPreview(ctx, placeholder, showText)
		finalText, err = b.runEinoReply(ctx, snap, messages, tools, usageCtx, preview, waitForPreviewReset)
		stopPreview()
	}

	event := DashboardEvent{
		ChatID:    usageCtx.ChatID,
		UserID:    usageCtx.UserID,
		Model:     snap.cfg.OpenAIModel,
		RequestID: usageCtx.RequestID,
		LatencyMs: time.Since(started).Milliseconds(),
	}
	if err != nil {
		event.Type = DashboardEventConversationError
		event.Summary = "stream failed"
		b.recordDashboardEvent(event)
		snap.editOrLog(placeholder, "❌ Stream error: the model could not complete the reply.")
		return ""
	}
	event.Type = DashboardEventConversationFinished
	event.Success = true
	event.Summary = truncateDashboardText(finalText, 200)
	b.recordDashboardEvent(event)
	snap.editReplyResult(placeholder, finalText, showText)
	return finalText
}
