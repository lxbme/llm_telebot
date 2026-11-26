package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	tele "gopkg.in/telebot.v3"
)

const (
	volcengineTTSSSEURL     = "https://openspeech.bytedance.com/api/v3/tts/unidirectional/sse"
	defaultTTSResourceID    = "seed-tts-1.0"
	defaultTTSSpeaker       = "zh_female_shuangkuaisisi_moon_bigtts"
	defaultTTSAudioFormat   = "mp3"
	defaultTTSSampleRate    = 24000
	speechModePrompt        = "\n\n=== Speech Mode Instruction ===\nAnswer the user briefly and directly in plain natural language that is easy to listen to. Avoid markdown tables, long lists, code blocks, and unnecessary preamble unless the user explicitly asks for detail."
	telegramTTSFailureReply = "⚠️ 语音合成失败，本次仅发送文本回复。"
	maxTTSTextRunes         = 1200
)

var (
	codeBlockRE    = regexp.MustCompile("(?s)```.*?```")
	markdownLinkRE = regexp.MustCompile(`\[(.*?)\]\((.*?)\)`)
	headingRE      = regexp.MustCompile(`(?m)^\s{0,3}#{1,6}\s*`)
	listMarkerRE   = regexp.MustCompile(`(?m)^\s*(?:[-*+]\s+|\d+\.\s+)`)
	quoteMarkerRE  = regexp.MustCompile(`(?m)^\s*>\s*`)
	inlineFormatRE = regexp.MustCompile(`[*_~` + "`" + `]`)
	multiNewlineRE = regexp.MustCompile(`\n{3,}`)
	multiSpaceRE   = regexp.MustCompile(`[ \t]{2,}`)
)

type SpeechModeStore struct {
	mu      sync.RWMutex
	enabled map[int64]bool
}

func NewSpeechModeStore() *SpeechModeStore {
	return &SpeechModeStore{
		enabled: make(map[int64]bool),
	}
}

func (s *SpeechModeStore) Enabled(chatID int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.enabled[chatID]
}

func (s *SpeechModeStore) Set(chatID int64, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if on {
		s.enabled[chatID] = true
		return
	}
	delete(s.enabled, chatID)
}

type VolcengineTTSClient struct {
	appID       string
	accessKey   string
	resourceID  string
	speaker     string
	audioFormat string
	sampleRate  int
	speechRate  int
	url         string
	httpClient  *http.Client
}

type ttsResponse struct {
	Audio  []byte
	Format string
	Text   string
}

type volcengineSSEPayload struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func NewVolcengineTTSClient(cfg Config) *VolcengineTTSClient {
	appID := strings.TrimSpace(cfg.VolcengineTTSAppID)
	accessKey := strings.TrimSpace(cfg.VolcengineTTSAccessKey)
	if appID == "" || accessKey == "" {
		return nil
	}

	resourceID := strings.TrimSpace(cfg.VolcengineTTSResourceID)
	if resourceID == "" {
		resourceID = defaultTTSResourceID
	}

	speaker := strings.TrimSpace(cfg.VolcengineTTSSpeaker)
	if speaker == "" {
		speaker = defaultTTSSpeaker
	}

	audioFormat := strings.ToLower(strings.TrimSpace(cfg.VolcengineTTSAudioFormat))
	switch audioFormat {
	case "", "mp3", "wav", "aac":
		if audioFormat == "" {
			audioFormat = defaultTTSAudioFormat
		}
	default:
		audioFormat = defaultTTSAudioFormat
	}

	sampleRate := cfg.VolcengineTTSSampleRate
	if sampleRate <= 0 {
		sampleRate = defaultTTSSampleRate
	}

	speechRate := cfg.VolcengineTTSSpeechRate
	if speechRate < -50 {
		speechRate = -50
	}
	if speechRate > 100 {
		speechRate = 100
	}

	return &VolcengineTTSClient{
		appID:       appID,
		accessKey:   accessKey,
		resourceID:  resourceID,
		speaker:     speaker,
		audioFormat: audioFormat,
		sampleRate:  sampleRate,
		speechRate:  speechRate,
		url:         volcengineTTSSSEURL,
		httpClient: &http.Client{
			Timeout: 2 * time.Minute,
		},
	}
}

func (c *VolcengineTTSClient) Synthesize(ctx context.Context, uid, text string) (*ttsResponse, error) {
	if c == nil {
		return nil, fmt.Errorf("tts client is not configured")
	}

	text = normalizeTextForTTS(text)
	if text == "" {
		return nil, fmt.Errorf("tts input is empty")
	}

	body := map[string]any{
		"user": map[string]string{
			"uid": uid,
		},
		"req_params": map[string]any{
			"text":    text,
			"speaker": c.speaker,
			"audio_params": map[string]any{
				"format":      c.audioFormat,
				"sample_rate": c.sampleRate,
				"speech_rate": c.speechRate,
			},
			"additions": `{"disable_markdown_filter":true}`,
		},
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal tts payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build tts request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Api-App-Id", c.appID)
	req.Header.Set("X-Api-Access-Key", c.accessKey)
	req.Header.Set("X-Api-Resource-Id", c.resourceID)
	req.Header.Set("X-Api-Request-Id", uuid.NewString())
	req.Header.Set("X-Control-Require-Usage-Tokens-Return", "text_words")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call volcengine tts: %w", err)
	}
	defer resp.Body.Close()

	if logID := strings.TrimSpace(resp.Header.Get("X-Tt-Logid")); logID != "" {
		log.Printf("[tts] upstream logid=%s", logID)
	}

	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("tts http %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var (
		audioBuf  bytes.Buffer
		eventName string
		dataLines []string
		finished  bool
	)

	flushEvent := func() error {
		if len(dataLines) == 0 {
			eventName = ""
			return nil
		}

		payloadText := strings.Join(dataLines, "\n")
		eventName = strings.TrimSpace(eventName)
		dataLines = nil

		var payload volcengineSSEPayload
		if err := json.Unmarshal([]byte(payloadText), &payload); err != nil {
			return fmt.Errorf("decode sse payload: %w", err)
		}

		if payload.Code != 0 && payload.Code != 20000000 {
			msg := strings.TrimSpace(payload.Message)
			if msg == "" {
				msg = "unknown tts error"
			}
			return fmt.Errorf("%s (code=%d, event=%s)", msg, payload.Code, eventName)
		}

		switch eventName {
		case "352", "TTSResponse", "":
			if isNullJSON(payload.Data) {
				return nil
			}
			var encoded string
			if err := json.Unmarshal(payload.Data, &encoded); err != nil {
				return fmt.Errorf("decode audio chunk: %w", err)
			}
			if encoded == "" {
				return nil
			}
			chunk, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return fmt.Errorf("base64 decode audio: %w", err)
			}
			audioBuf.Write(chunk)
		case "152", "SessionFinish":
			finished = true
		case "151", "SessionCancel", "153", "SessionFailed":
			msg := strings.TrimSpace(payload.Message)
			if msg == "" {
				msg = "tts stream terminated unexpectedly"
			}
			return fmt.Errorf("%s (code=%d, event=%s)", msg, payload.Code, eventName)
		}

		return nil
	}

	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		switch {
		case line == "":
			if err := flushEvent(); err != nil {
				return nil, err
			}
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(line[len("data:"):]))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read sse stream: %w", err)
	}
	if err := flushEvent(); err != nil {
		return nil, err
	}

	if audioBuf.Len() == 0 {
		return nil, fmt.Errorf("tts returned no audio")
	}
	if !finished {
		return nil, fmt.Errorf("tts stream ended before finish event")
	}

	return &ttsResponse{
		Audio:  audioBuf.Bytes(),
		Format: c.audioFormat,
		Text:   text,
	}, nil
}

func (c *VolcengineTTSClient) telegramVoice(ctx context.Context, uid, text string) ([]byte, string, error) {
	resp, err := c.Synthesize(ctx, uid, text)
	if err != nil {
		return nil, "", err
	}

	voiceData, err := transcodeToTelegramVoice(ctx, resp.Audio, resp.Format)
	if err != nil {
		return nil, "", err
	}
	return voiceData, resp.Text, nil
}

func normalizeTextForTTS(text string) string {
	text = formatTextForTTS(text)
	if runes := []rune(text); len(runes) > maxTTSTextRunes {
		text = string(runes[:maxTTSTextRunes]) + "。"
	}
	return text
}

func formatTextForTTS(text string) string {
	text = telegramLineEndings(strings.TrimSpace(text))
	text = codeBlockRE.ReplaceAllString(text, "")
	text = markdownLinkRE.ReplaceAllString(text, "$1")
	text = headingRE.ReplaceAllString(text, "")
	text = listMarkerRE.ReplaceAllString(text, "")
	text = quoteMarkerRE.ReplaceAllString(text, "")
	text = inlineFormatRE.ReplaceAllString(text, "")
	text = strings.ReplaceAll(text, "|", " ")
	return normalizeSpeechWhitespace(text)
}

func normalizeSpeechWhitespace(text string) string {
	text = multiSpaceRE.ReplaceAllString(text, " ")
	text = multiNewlineRE.ReplaceAllString(text, "\n\n")
	return strings.TrimSpace(text)
}

func isNullJSON(raw json.RawMessage) bool {
	return len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func audioMIMEType(format string) string {
	switch format {
	case "aac":
		return "audio/aac"
	case "wav":
		return "audio/wav"
	default:
		return "audio/mpeg"
	}
}

func transcodeToTelegramVoice(ctx context.Context, audio []byte, format string) ([]byte, error) {
	inputFormat, err := ffmpegInputFormat(format)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(
		ctx,
		"ffmpeg",
		"-hide_banner",
		"-loglevel", "error",
		"-f", inputFormat,
		"-i", "pipe:0",
		"-vn",
		"-ac", "1",
		"-ar", "48000",
		"-c:a", "libopus",
		"-b:a", "48k",
		"-vbr", "on",
		"-application", "voip",
		"-f", "ogg",
		"pipe:1",
	)
	cmd.Stdin = bytes.NewReader(audio)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("transcode voice with ffmpeg: %s", msg)
	}
	if stdout.Len() == 0 {
		return nil, fmt.Errorf("ffmpeg returned empty voice output")
	}
	return stdout.Bytes(), nil
}

func ffmpegInputFormat(format string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "mp3":
		return "mp3", nil
	case "wav":
		return "wav", nil
	case "aac":
		return "aac", nil
	default:
		return "", fmt.Errorf("unsupported tts audio format %q for voice transcoding", format)
	}
}

func (snap runtimeSnapshot) speechInstruction(chatID int64) string {
	if snap.speechModes == nil || !snap.speechModes.Enabled(chatID) {
		return ""
	}
	return speechModePrompt
}

func (snap runtimeSnapshot) shouldSendSpeechText(chatID int64) bool {
	if snap.speechModes == nil || !snap.speechModes.Enabled(chatID) {
		return true
	}
	return snap.cfg.VolcengineTTSSendText
}

type speechReplyResult struct {
	text   string
	spoken bool
	err    error
}

func (snap runtimeSnapshot) finalizeSpeechReply(
	turn RoleTurn,
	chat *tele.Chat,
	sender *tele.User,
	text string,
	placeholder *tele.Message,
	textDelivered bool,
	editFallback func(allowed func() bool) (string, error),
) (result speechReplyResult) {
	if chat == nil || snap.speechModes == nil || !snap.speechModes.Enabled(turn.ChatID) {
		return result
	}

	uid := strconv.FormatInt(chat.ID, 10)
	if sender != nil {
		uid = strconv.FormatInt(sender.ID, 10)
	}
	ctx, cancel := context.WithTimeout(turn.generation, 2*time.Minute)
	defer cancel()
	if err := turn.checkUse(ctx); err != nil {
		result.err = err
		return result
	}
	allowed := func() bool { return turn.checkUse(ctx) == nil }
	replacePlaceholder := !textDelivered && placeholder != nil
	var voice []byte
	var spokenText string
	var err error
	if snap.tts != nil {
		voice, spokenText, err = snap.tts.telegramVoice(ctx, uid, text)
	}
	if !turn.withCurrent(func() {
		if result.err = turn.checkUse(ctx); result.err != nil {
			return
		}
		if err == nil && voice != nil {
			err = snap.sendSpeechVoice(ctx, chat.ID, voice)
			if err == nil {
				result.text, result.spoken = spokenText, true
			} else if errors.Is(err, errTelegramDeliveryUnknown) {
				result.err = err
				return
			}
		}
		if err != nil {
			log.Printf("[tts] telegram send error: %v", err)
			if replacePlaceholder {
				if editFallback != nil {
					result.text, result.err = editFallback(allowed)
				} else {
					result.text = snap.editFinalResponse(placeholder, text, allowed)
				}
			}
			if allowed() {
				_, _ = snap.tg.Send(chat, telegramTTSFailureReply)
			}
			return
		}

		if replacePlaceholder {
			if err := snap.tg.Delete(placeholder); err != nil {
				log.Printf("[tts] delete placeholder error: %v", err)
			}
		}
	}) {
		result.err = ctx.Err()
	}
	return result
}

func (snap runtimeSnapshot) sendSpeechVoice(ctx context.Context, chatID int64, voice []byte) error {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
		return err
	}
	// Match the Bot API upload used by the previous Telegram adapter.
	part, err := form.CreateFormFile("voice", "")
	if err != nil {
		return err
	}
	if _, err := part.Write(voice); err != nil {
		return err
	}
	if err := form.Close(); err != nil {
		return err
	}
	response, err := snap.telegramRequest(ctx, "sendVoice", form.FormDataContentType(), &body)
	if err != nil {
		return errTelegramDeliveryUnknown
	}
	if !response.OK {
		if response.Code >= 400 && response.Code < 500 {
			return fmt.Errorf("Telegram rejected voice delivery (%d)", response.Code)
		}
		return errTelegramDeliveryUnknown
	}
	var sent tele.Message
	if json.Unmarshal(response.Result, &sent) != nil || sent.ID <= 0 || sent.Chat == nil ||
		sent.Chat.ID != chatID || sent.Voice == nil || sent.Voice.FileID == "" {
		return errTelegramDeliveryUnknown
	}
	return nil
}
