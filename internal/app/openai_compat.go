package app

import (
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

// requiresMaxCompletionTokens returns true for OpenAI model families that
// use max_completion_tokens instead of the deprecated max_tokens field.
// Matches o1/o3/o4 reasoning models and gpt-5+ generation models.
func requiresMaxCompletionTokens(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"), strings.HasPrefix(m, "o4"):
		return true
	case strings.HasPrefix(m, "gpt-5"), strings.HasPrefix(m, "gpt-6"), strings.HasPrefix(m, "gpt-7"):
		return true
	}
	return false
}

// sanitizeBetaRequest zeros out sampling parameters that newer model families
// (o1/o3/o4, gpt-5+) do not accept. With go-openai's omitempty json tags,
// zero values are omitted from the request, satisfying the API constraint.
func sanitizeBetaRequest(req *openai.ChatCompletionRequest) {
	if !requiresMaxCompletionTokens(req.Model) {
		return
	}
	req.Temperature = 0
	req.TopP = 0
	req.N = 0
	req.PresencePenalty = 0
	req.FrequencyPenalty = 0
}

// applyMaxTokens sets either MaxTokens or MaxCompletionTokens on req,
// depending on which field the model supports. limit=0 is a no-op (no limit).
func applyMaxTokens(req *openai.ChatCompletionRequest, limit int) {
	if limit == 0 {
		return
	}
	if requiresMaxCompletionTokens(req.Model) {
		req.MaxCompletionTokens = limit
	} else {
		req.MaxTokens = limit
	}
}
