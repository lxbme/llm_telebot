package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	openai "github.com/sashabaranov/go-openai"
)

var (
	errEinoNoChoices           = errors.New("chat completion returned no choices")
	errEinoUnexpectedToolCalls = errors.New("chat completion returned tool calls for a tool-free request")
)

var _ model.BaseChatModel = (*einoChatModel)(nil)

// einoChatModel serves one bot request, including all of its model calls.
type einoChatModel struct {
	client      *openai.Client
	modelName   string
	maxTokens   int
	tools       map[string]openai.Tool
	usageCtx    UsageContext
	recordUsage func(UsageEvent)

	callIndex atomic.Int64
}

func (m *einoChatModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (message *schema.Message, err error) {
	req, err := m.request(input, opts)
	if err != nil {
		return nil, err
	}

	started := time.Now()
	callIndex := int(m.callIndex.Add(1))
	resp, err := m.client.CreateChatCompletion(ctx, req)
	hasToolCalls := false
	defer func() {
		m.recordCall(req, callIndex, started, resp.Model, &resp.Usage, hasToolCalls, err)
	}()
	if err != nil {
		return nil, err
	}
	if len(resp.Choices) == 0 {
		return nil, errEinoNoChoices
	}
	choice := resp.Choices[0]
	hasToolCalls = len(choice.Message.ToolCalls) != 0
	if len(req.Tools) == 0 && (hasToolCalls || choice.Message.FunctionCall != nil) {
		return nil, errEinoUnexpectedToolCalls
	}
	message, err = toEinoMessage(choice.Message)
	if err != nil {
		return nil, err
	}
	message.ResponseMeta = einoResponseMeta(choice.FinishReason, &resp.Usage)
	return message, nil
}

func (m *einoChatModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	req, err := m.request(input, opts)
	if err != nil {
		return nil, err
	}
	req.Stream = true
	req.StreamOptions = &openai.StreamOptions{IncludeUsage: true}

	started := time.Now()
	callIndex := int(m.callIndex.Add(1))
	streamCtx, cancel := context.WithCancel(ctx)
	upstream, err := m.client.CreateChatCompletionStream(streamCtx, req)
	if err != nil {
		cancel()
		m.recordCall(req, callIndex, started, "", nil, false, err)
		return nil, err
	}

	// Keep a Send pending while the HTTP reader is in Recv or waiting to deliver
	// a chunk. Eino signals early reader closure through Send's return value.
	permits, writer := schema.Pipe[struct{}](0)
	permits.SetAutomaticClose() // Allow both producer shutdown and reader Close.
	go func() {
		defer writer.Close()
		defer cancel()
		for !writer.Send(struct{}{}, nil) {
		}
	}()

	chunks := make(chan *schema.Message)
	var terminalErr error // Published to the reader by closing chunks.
	go func() {
		defer permits.Close()
		defer close(chunks)
		terminalErr = m.receiveStream(streamCtx, upstream, req, callIndex, started, chunks)
	}()

	next := func(struct{}) (*schema.Message, error) {
		if chunk, ok := <-chunks; ok {
			return chunk, nil
		}
		if terminalErr != nil {
			err := terminalErr
			terminalErr = nil
			return nil, err
		}
		return nil, io.EOF
	}
	return schema.StreamReaderWithConvert(permits, next, schema.WithOnEOF(func() (any, error) {
		// The producer can finish just before the reader acquires its next permit.
		return next(struct{}{})
	})), nil
}

func (m *einoChatModel) receiveStream(
	ctx context.Context,
	upstream *openai.ChatCompletionStream,
	req openai.ChatCompletionRequest,
	callIndex int,
	started time.Time,
	chunks chan<- *schema.Message,
) (err error) {
	var (
		usage        *openai.Usage
		actualModel  string
		sawChoice    bool
		completed    bool
		hasToolCalls bool
		rejected     bool
	)
	defer func() {
		_ = upstream.Close()
		m.recordCall(req, callIndex, started, actualModel, usage, hasToolCalls, err)
	}()

	for {
		resp, err := upstream.Recv()
		actualModel = firstNonEmpty(resp.Model, actualModel)
		if resp.Usage != nil {
			usage = resp.Usage
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == io.EOF {
			switch {
			case rejected:
				return errEinoUnexpectedToolCalls
			case !sawChoice:
				return errEinoNoChoices
			case !completed:
				return io.ErrUnexpectedEOF
			default:
				return nil
			}
		}
		if err != nil {
			return err
		}

		var chunk *schema.Message
		if len(resp.Choices) != 0 {
			sawChoice = true
			choice := resp.Choices[0]
			completed = completed || choice.FinishReason != ""
			delta := choice.Delta
			hasToolCalls = hasToolCalls || len(delta.ToolCalls) != 0
			if len(req.Tools) == 0 && (hasToolCalls || delta.FunctionCall != nil) {
				rejected = true
			}
			chunk = &schema.Message{
				Role:             schema.RoleType(firstNonEmpty(delta.Role, string(schema.Assistant))),
				Content:          delta.Content,
				ReasoningContent: delta.ReasoningContent,
				ToolCalls:        toEinoToolCalls(delta.ToolCalls),
				ResponseMeta:     einoResponseMeta(choice.FinishReason, resp.Usage),
			}
		} else if resp.Usage != nil && sawChoice {
			completed = true
			chunk = &schema.Message{ResponseMeta: einoResponseMeta("", resp.Usage)}
		}

		// Drain the rest of a rejected response so the provider's usage tail is
		// still accounted for, without handing its tool calls to the consumer.
		if rejected || chunk == nil {
			continue
		}
		select {
		case chunks <- chunk:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (m *einoChatModel) request(input []*schema.Message, opts []model.Option) (openai.ChatCompletionRequest, error) {
	messages, err := fromEinoMessages(input)
	if err != nil {
		return openai.ChatCompletionRequest{}, err
	}

	options := model.GetCommonOptions(&model.Options{Model: &m.modelName, MaxTokens: &m.maxTokens}, opts...)
	req := openai.ChatCompletionRequest{Model: *options.Model, Messages: messages, Stop: options.Stop}
	if options.Temperature != nil {
		req.Temperature = *options.Temperature
	}
	if options.TopP != nil {
		req.TopP = *options.TopP
	}
	for _, info := range options.Tools {
		if info == nil {
			return openai.ChatCompletionRequest{}, errors.New("nil Eino tool info")
		}
		canonical, ok := m.tools[info.Name]
		if !ok {
			return openai.ChatCompletionRequest{}, fmt.Errorf("no OpenAI schema for Eino tool %q", info.Name)
		}
		req.Tools = append(req.Tools, canonical)
	}

	applyMaxTokens(&req, *options.MaxTokens)
	sanitizeBetaRequest(&req)
	return req, nil
}

func (m *einoChatModel) recordCall(req openai.ChatCompletionRequest, callIndex int, started time.Time, actualModel string, usage *openai.Usage, hasToolCalls bool, err error) {
	if m.recordUsage == nil {
		return
	}
	callType, round := UsageCallMainChat, 0
	if (err == nil && hasToolCalls) || (err != nil && len(req.Tools) != 0) {
		callType, round = UsageCallToolRound, callIndex
	}
	m.recordUsage(usageEvent(m.usageCtx, callType, firstNonEmpty(actualModel, req.Model), req.Stream, round, started, usage, err == nil))
}

func einoResponseMeta(finishReason openai.FinishReason, usage *openai.Usage) *schema.ResponseMeta {
	if finishReason == "" && usage == nil {
		return nil
	}
	meta := &schema.ResponseMeta{FinishReason: string(finishReason)}
	if usage != nil {
		meta.Usage = &schema.TokenUsage{
			PromptTokens:     usage.PromptTokens,
			CompletionTokens: usage.CompletionTokens,
			TotalTokens:      usage.TotalTokens,
		}
		if usage.PromptTokensDetails != nil {
			meta.Usage.PromptTokenDetails.CachedTokens = usage.PromptTokensDetails.CachedTokens
		}
		if usage.CompletionTokensDetails != nil {
			meta.Usage.CompletionTokensDetails.ReasoningTokens = usage.CompletionTokensDetails.ReasoningTokens
		}
	}
	return meta
}

func toEinoMessages(messages []openai.ChatCompletionMessage) ([]*schema.Message, error) {
	result := make([]*schema.Message, len(messages))
	for i, message := range messages {
		converted, err := toEinoMessage(message)
		if err != nil {
			return nil, fmt.Errorf("OpenAI message %d: %w", i, err)
		}
		result[i] = converted
	}
	return result, nil
}

func toEinoMessage(message openai.ChatCompletionMessage) (*schema.Message, error) {
	if message.Content != "" && message.MultiContent != nil {
		return nil, openai.ErrContentFieldsMisused
	}
	result := &schema.Message{
		Role:             schema.RoleType(message.Role),
		Name:             message.Name,
		Content:          message.Content,
		ReasoningContent: message.ReasoningContent,
		ToolCallID:       message.ToolCallID,
		ToolCalls:        toEinoToolCalls(message.ToolCalls),
	}
	for _, part := range message.MultiContent {
		converted := schema.MessageInputPart{Type: schema.ChatMessagePartType(part.Type)}
		switch part.Type {
		case openai.ChatMessagePartTypeText:
			converted.Text = part.Text
		case openai.ChatMessagePartTypeImageURL:
			if part.ImageURL == nil {
				return nil, errors.New("OpenAI image part is missing its image URL")
			}
			url := part.ImageURL.URL
			converted.Image = &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{URL: &url},
				Detail:            schema.ImageURLDetail(part.ImageURL.Detail),
			}
		default:
			return nil, fmt.Errorf("unsupported OpenAI message part type %q", part.Type)
		}
		result.UserInputMultiContent = append(result.UserInputMultiContent, converted)
	}
	return result, nil
}

func fromEinoMessages(messages []*schema.Message) ([]openai.ChatCompletionMessage, error) {
	result := make([]openai.ChatCompletionMessage, len(messages))
	for i, message := range messages {
		converted, err := fromEinoMessage(message)
		if err != nil {
			return nil, fmt.Errorf("Eino message %d: %w", i, err)
		}
		result[i] = converted
	}
	return result, nil
}

func fromEinoMessage(message *schema.Message) (openai.ChatCompletionMessage, error) {
	if message == nil {
		return openai.ChatCompletionMessage{}, errors.New("nil Eino message")
	}
	if len(message.AssistantGenMultiContent) != 0 {
		return openai.ChatCompletionMessage{}, errors.New("Eino assistant output parts are unsupported in OpenAI chat history")
	}
	if message.Content != "" && (len(message.UserInputMultiContent) != 0 || len(message.MultiContent) != 0) {
		return openai.ChatCompletionMessage{}, openai.ErrContentFieldsMisused
	}
	parts, err := einoInputParts(message)
	if err != nil {
		return openai.ChatCompletionMessage{}, err
	}
	result := openai.ChatCompletionMessage{
		Role:             string(message.Role),
		Name:             message.Name,
		Content:          message.Content,
		ReasoningContent: message.ReasoningContent,
		ToolCallID:       message.ToolCallID,
	}
	for _, call := range message.ToolCalls {
		result.ToolCalls = append(result.ToolCalls, openai.ToolCall{
			Type:     openai.ToolType(call.Type),
			ID:       call.ID,
			Function: openai.FunctionCall(call.Function),
		})
	}
	for _, part := range parts {
		converted := openai.ChatMessagePart{Type: openai.ChatMessagePartType(part.Type)}
		switch part.Type {
		case schema.ChatMessagePartTypeText:
			converted.Text = part.Text
		case schema.ChatMessagePartTypeImageURL:
			url, err := einoImageURL(part.Image)
			if err != nil {
				return openai.ChatCompletionMessage{}, err
			}
			converted.ImageURL = &openai.ChatMessageImageURL{
				URL:    url,
				Detail: openai.ImageURLDetail(part.Image.Detail),
			}
		default:
			return openai.ChatCompletionMessage{}, fmt.Errorf("unsupported Eino message part type %q", part.Type)
		}
		result.MultiContent = append(result.MultiContent, converted)
	}
	return result, nil
}

func einoInputParts(message *schema.Message) ([]schema.MessageInputPart, error) {
	if len(message.MultiContent) == 0 {
		return message.UserInputMultiContent, nil
	}
	if len(message.UserInputMultiContent) != 0 {
		return nil, errors.New("Eino message has two sets of input parts")
	}

	parts := make([]schema.MessageInputPart, len(message.MultiContent))
	for i, part := range message.MultiContent {
		parts[i] = schema.MessageInputPart{Type: part.Type, Text: part.Text}
		if part.ImageURL != nil {
			parts[i].Image = &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{URL: &part.ImageURL.URL},
				Detail:            part.ImageURL.Detail,
			}
		}
	}
	return parts, nil
}

func einoImageURL(image *schema.MessageInputImage) (string, error) {
	if image != nil {
		switch {
		case image.URL != nil:
			return *image.URL, nil
		case image.Base64Data != nil && image.MIMEType != "":
			return "data:" + image.MIMEType + ";base64," + *image.Base64Data, nil
		}
	}
	return "", errors.New("Eino image part requires a URL or base64 data with a MIME type")
}

func toEinoToolCalls(calls []openai.ToolCall) []schema.ToolCall {
	var result []schema.ToolCall
	for _, call := range calls {
		result = append(result, schema.ToolCall{
			Index:    call.Index,
			Type:     string(call.Type),
			ID:       call.ID,
			Function: schema.FunctionCall(call.Function),
		})
	}
	return result
}
