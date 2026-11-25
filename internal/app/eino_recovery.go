package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

const einoModelRetryDelay = 100 * time.Millisecond

type einoRequestError struct{ status int }

func (e einoRequestError) Error() string {
	if e.status != 0 {
		return fmt.Sprintf("chat completion failed with HTTP status %d", e.status)
	}
	return "chat completion request failed"
}

// Preserve only the facts needed for recovery. SDK errors may contain a URL,
// response body or provider-controlled message.
func safeEinoRequestError(ctx context.Context, err error) error {
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	}
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		return einoRequestError{status: apiErr.HTTPStatusCode}
	}
	var reqErr *openai.RequestError
	if errors.As(err, &reqErr) {
		return einoRequestError{status: reqErr.HTTPStatusCode}
	}
	return einoRequestError{}
}

func einoModelCanRecover(err error) bool {
	if err == errEinoStreamInterrupted {
		return true
	}
	if reqErr, ok := err.(einoRequestError); ok {
		switch reqErr.status {
		case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
			http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
	}
	return false
}

// Each logical model call starts from the primary using the same inputs and
// tool schemas. Recovery never re-enters the agent or advances its tool rounds.
func runEinoModelAttempts[T any](
	ctx context.Context,
	m *einoChatModel,
	req openai.ChatCompletionRequest,
	attempt func(openai.ChatCompletionRequest) (T, error),
) (result T, err error) {
	names := []string{req.Model}
	if m.retry {
		names = append(names, req.Model)
	}
	if m.backupModel != "" && m.backupModel != req.Model {
		names = append(names, m.backupModel)
	}
	for i, name := range names {
		if i != 0 && name == req.Model {
			timer := time.NewTimer(einoModelRetryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return result, ctx.Err()
			case <-timer.C:
			}
		}
		if i != 0 && req.Stream && m.waitForPreviewReset != nil {
			m.waitForPreviewReset()
		}
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		wire := req
		wire.Model, wire.MaxTokens = name, 0
		applyMaxTokens(&wire, req.MaxTokens)
		sanitizeBetaRequest(&wire)
		result, err = attempt(wire)
		if !einoModelCanRecover(err) {
			break
		}
	}
	return result, err
}
