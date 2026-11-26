package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

const einoModelRetryDelay = 100 * time.Millisecond

type einoRequestError struct {
	status    int
	hardQuota bool
}

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
	// RequestError can wrap a partly decoded APIError without an HTTP status.
	var reqErr *openai.RequestError
	if errors.As(err, &reqErr) {
		safe := einoRequestError{status: reqErr.HTTPStatusCode}
		if safe.status == http.StatusTooManyRequests {
			var body struct {
				Error struct{ Code, Type any }
			}
			if json.Unmarshal(reqErr.Body, &body) == nil {
				safe.hardQuota = einoHardQuota(body.Error.Code) || einoHardQuota(body.Error.Type)
			}
		}
		return safe
	}
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		return einoRequestError{
			status: apiErr.HTTPStatusCode,
			hardQuota: apiErr.HTTPStatusCode == http.StatusTooManyRequests &&
				(einoHardQuota(apiErr.Code) || einoHardQuota(apiErr.Type)),
		}
	}
	return einoRequestError{}
}

func einoHardQuota(value any) bool {
	category, _ := value.(string)
	switch category {
	case "insufficient_quota", "billing_hard_limit_reached":
		return true
	default:
		return false
	}
}

func einoModelCanRecover(err error) bool {
	if err == errEinoStreamInterrupted {
		return true
	}
	if reqErr, ok := err.(einoRequestError); ok && !reqErr.hardQuota {
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
			m.waitForPreviewReset(ctx)
		}
		if err := checkRoleReplyUse(ctx); err != nil {
			return result, err
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
