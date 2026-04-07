package app

import (
	"context"
	"fmt"
	"io"
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

// STTClient wraps an OpenAI-compatible client scoped to audio transcription.
// It is nil when STT is disabled. All public methods are nil-safe.
type STTClient struct {
	client *openai.Client
	model  string
}

// NewSTTClient returns a configured STTClient, or nil when STT is disabled or
// the API key is missing. Follows the same guard pattern as NewVolcengineTTSClient.
func NewSTTClient(cfg Config) *STTClient {
	if !cfg.STTEnabled {
		return nil
	}
	apiKey := strings.TrimSpace(cfg.STTAPIKey)
	if apiKey == "" {
		return nil
	}
	oaiCfg := openai.DefaultConfig(apiKey)
	if base := strings.TrimSpace(cfg.STTAPIBase); base != "" {
		oaiCfg.BaseURL = base
	}
	model := strings.TrimSpace(cfg.STTModel)
	if model == "" {
		model = "whisper-1"
	}
	return &STTClient{
		client: openai.NewClientWithConfig(oaiCfg),
		model:  model,
	}
}

// Transcribe sends audio bytes (OGG/OPUS from Telegram) to the STT API and
// returns the transcribed text. The FilePath "voice.ogg" is a codec hint used
// by the library to set the multipart Content-Type; no disk I/O occurs.
func (s *STTClient) Transcribe(ctx context.Context, r io.Reader) (string, error) {
	if s == nil {
		return "", fmt.Errorf("STT is not configured")
	}
	resp, err := s.client.CreateTranscription(ctx, openai.AudioRequest{
		Model:    s.model,
		FilePath: "voice.ogg",
		Reader:   r,
		Format:   openai.AudioResponseFormatJSON,
	})
	if err != nil {
		return "", fmt.Errorf("STT transcription failed: %w", err)
	}
	return strings.TrimSpace(resp.Text), nil
}
