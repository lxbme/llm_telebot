package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
	openai "github.com/sashabaranov/go-openai"
	tele "gopkg.in/telebot.v3"
)

// ─── Configuration ────────────────────────────────────────────────────────────

type Config struct {
	OpenAIBase     string
	OpenAIKey      string
	OpenAIModel    string
	TelegramToken  string
	SystemPrompt   string
	ContextMaxMsgs int
	MaxTokens      int    // 0 = no limit
	BotUsername    string // e.g. "@mybot"
	ContextMode    string // "at" = only @bot messages as context; "global" = all group messages as context
}

func loadConfig() Config {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, reading from environment")
	}

	maxMsgs, err := strconv.Atoi(getEnv("CONTEXT_MAX_MESSAGES", "20"))
	if err != nil || maxMsgs <= 0 {
		maxMsgs = 20
	}

	maxTokens, err := strconv.Atoi(getEnv("MAX_TOKENS", "0"))
	if err != nil || maxTokens < 0 {
		maxTokens = 0
	}

	contextMode := strings.ToLower(getEnv("CONTEXT_MODE", "at"))
	if contextMode != "at" && contextMode != "global" {
		contextMode = "at"
	}

	return Config{
		OpenAIBase:     getEnv("OPENAI_API_BASE", ""),
		OpenAIKey:      getEnv("OPENAI_API_KEY", ""),
		OpenAIModel:    getEnv("OPENAI_MODEL", "gpt-4o"),
		TelegramToken:  getEnv("TELEGRAM_BOT_TOKEN", ""),
		SystemPrompt:   getEnv("SYSTEM_PROMPT", "You are a helpful assistant."),
		ContextMaxMsgs: maxMsgs,
		MaxTokens:      maxTokens,
		BotUsername:    getEnv("BOT_USERNAME", ""),
		ContextMode:    contextMode,
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// buildUserContent wraps the user's text with a structured metadata header so
// the LLM always knows exactly who is speaking, even across multi-user group
// conversations, reducing the chance of identity hallucinations.
//
//	Format:
//	  [user_id:123456 username:@alice name:"Alice Smith"]
//	  <original text>
func buildUserContent(sender *tele.User, text string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[user_id:%d", sender.ID)
	if sender.Username != "" {
		fmt.Fprintf(&sb, " username:@%s", sender.Username)
	}
	if fullName := strings.TrimSpace(sender.FirstName + " " + sender.LastName); fullName != "" {
		fmt.Fprintf(&sb, " name:%q", fullName)
	}
	sb.WriteString("]\n")
	sb.WriteString(text)
	return sb.String()
}

// sanitizeName returns a Name value safe for the OpenAI API
// (only [a-zA-Z0-9_-], max 64 chars).
func sanitizeName(id int64) string {
	return fmt.Sprintf("user_%d", id)
}

// ─── Chat History ─────────────────────────────────────────────────────────────

type HistoryStore struct {
	mu      sync.RWMutex
	history map[int64][]openai.ChatCompletionMessage
}

func NewHistoryStore() *HistoryStore {
	return &HistoryStore{
		history: make(map[int64][]openai.ChatCompletionMessage),
	}
}

// Get returns a copy of the message history for a chat.
func (s *HistoryStore) Get(chatID int64) []openai.ChatCompletionMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.history[chatID]
	if len(src) == 0 {
		return nil
	}
	dst := make([]openai.ChatCompletionMessage, len(src))
	copy(dst, src)
	return dst
}

// Append adds a message and trims the history to maxMessages.
func (s *HistoryStore) Append(chatID int64, msg openai.ChatCompletionMessage, maxMessages int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history[chatID] = append(s.history[chatID], msg)
	if len(s.history[chatID]) > maxMessages {
		s.history[chatID] = s.history[chatID][len(s.history[chatID])-maxMessages:]
	}
}

// AppendBatch atomically appends multiple messages and trims the history.
// This is used to write the user message and assistant reply as a pair so
// concurrent requests cannot interleave between them.
func (s *HistoryStore) AppendBatch(chatID int64, msgs []openai.ChatCompletionMessage, maxMessages int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history[chatID] = append(s.history[chatID], msgs...)
	if len(s.history[chatID]) > maxMessages {
		s.history[chatID] = s.history[chatID][len(s.history[chatID])-maxMessages:]
	}
}

// Clear deletes the history for a chat.
func (s *HistoryStore) Clear(chatID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.history, chatID)
}

// ─── Bot ──────────────────────────────────────────────────────────────────────

type Bot struct {
	cfg   Config
	ai    *openai.Client
	store *HistoryStore
	tg    *tele.Bot
}

func NewBot(cfg Config) (*Bot, error) {
	if cfg.TelegramToken == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is not set")
	}
	if cfg.OpenAIKey == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY is not set")
	}

	// Build OpenAI client with optional custom base URL.
	aiCfg := openai.DefaultConfig(cfg.OpenAIKey)
	if cfg.OpenAIBase != "" {
		aiCfg.BaseURL = cfg.OpenAIBase
	}
	aiClient := openai.NewClientWithConfig(aiCfg)

	// Build Telegram bot.
	pref := tele.Settings{
		Token:  cfg.TelegramToken,
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
	}
	tgBot, err := tele.NewBot(pref)
	if err != nil {
		return nil, fmt.Errorf("failed to create Telegram bot: %w", err)
	}

	return &Bot{
		cfg:   cfg,
		ai:    aiClient,
		store: NewHistoryStore(),
		tg:    tgBot,
	}, nil
}

func (b *Bot) Run() {
	b.tg.Handle("/start", b.handleStart)
	b.tg.Handle("/clear", b.handleClear)
	b.tg.Handle(tele.OnText, b.handleText)

	log.Printf("Bot @%s is running…", b.tg.Me.Username)
	b.tg.Start()
}

// ─── Handlers ─────────────────────────────────────────────────────────────────

func (b *Bot) handleStart(c tele.Context) error {
	return c.Reply("👋 Hello! I'm your AI assistant. Ask me anything.\nUse /clear to reset conversation history.")
}

func (b *Bot) handleClear(c tele.Context) error {
	chatID := c.Chat().ID
	b.store.Clear(chatID)
	return c.Reply("✅ Conversation history cleared.")
}

func (b *Bot) handleText(c tele.Context) error {
	msg := c.Message()
	if msg == nil {
		return nil
	}

	text := strings.TrimSpace(msg.Text)

	// ── Group logic: only respond when mentioned ──────────────────────────────
	mentioned := false
	if msg.Chat.Type != tele.ChatPrivate {
		mention := b.cfg.BotUsername
		if mention == "" {
			mention = "@" + b.tg.Me.Username
		}
		// normalise casing for comparison
		mentioned = strings.Contains(strings.ToLower(text), strings.ToLower(mention))

		if !mentioned {
			// In global mode, store every group message as context even if bot is not mentioned.
			if b.cfg.ContextMode == "global" && text != "" {
				b.store.Append(c.Chat().ID, openai.ChatCompletionMessage{
					Role:    openai.ChatMessageRoleUser,
					Name:    sanitizeName(msg.Sender.ID),
					Content: buildUserContent(msg.Sender, text),
				}, b.cfg.ContextMaxMsgs)
			}
			return nil // not mentioned – do not reply
		}

		// strip the mention from the text so LLM sees clean input
		// restore original casing by removing mention from the original text
		lowerOrig := strings.ToLower(msg.Text)
		lowerMention := strings.ToLower(mention)
		idx := strings.Index(lowerOrig, lowerMention)
		if idx >= 0 {
			text = strings.TrimSpace(msg.Text[:idx] + msg.Text[idx+len(mention):])
		}
	}

	if text == "" {
		return c.Reply("Please send me some text.")
	}

	chatID := c.Chat().ID
	sender := msg.Sender

	// Build the user message but do NOT append it to history yet.
	// We will atomically append (user msg + assistant reply) after the
	// LLM finishes, so concurrent requests never interleave.
	userMsg := openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Name:    sanitizeName(sender.ID),
		Content: buildUserContent(sender, text),
	}

	// Snapshot current history and build the prompt.
	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: b.cfg.SystemPrompt},
	}
	messages = append(messages, b.store.Get(chatID)...)
	messages = append(messages, userMsg)

	// Send the initial placeholder message.
	placeholder, err := b.tg.Send(c.Chat(), "⏳ Thinking…")
	if err != nil {
		return fmt.Errorf("failed to send placeholder: %w", err)
	}

	// Run streaming in a goroutine so we can tick-update Telegram.
	go b.streamReply(chatID, userMsg, messages, placeholder)

	return nil
}

// ─── Streaming ────────────────────────────────────────────────────────────────

func (b *Bot) streamReply(
	chatID int64,
	userMsg openai.ChatCompletionMessage,
	messages []openai.ChatCompletionMessage,
	placeholder *tele.Message,
) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	req := openai.ChatCompletionRequest{
		Model:     b.cfg.OpenAIModel,
		Messages:  messages,
		Stream:    true,
		MaxTokens: b.cfg.MaxTokens,
	}

	stream, err := b.ai.CreateChatCompletionStream(ctx, req)
	if err != nil {
		b.editOrLog(placeholder, fmt.Sprintf("❌ Error starting stream: %v", err))
		return
	}
	defer stream.Close()

	var (
		fullResponse strings.Builder
		mu           sync.Mutex // guards fullResponse

		ticker   = time.NewTicker(1500 * time.Millisecond)
		lastSent string
	)
	defer ticker.Stop()

	// Goroutine: periodically push intermediate updates to Telegram.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ticker.C:
				mu.Lock()
				current := fullResponse.String()
				mu.Unlock()
				if current != lastSent && current != "" {
					b.editOrLog(placeholder, current+"▌")
					lastSent = current
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Main loop: consume stream tokens.
	streamErr := func() error {
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			if len(resp.Choices) == 0 {
				continue
			}
			delta := resp.Choices[0].Delta.Content
			mu.Lock()
			fullResponse.WriteString(delta)
			mu.Unlock()
		}
	}()

	// Stop the ticker goroutine.
	cancel()
	<-done

	finalText := fullResponse.String()

	if streamErr != nil {
		errMsg := fmt.Sprintf("❌ Stream error: %v", streamErr)
		if finalText != "" {
			errMsg = finalText + "\n\n" + errMsg
		}
		b.editOrLog(placeholder, errMsg)
		return
	}

	if finalText == "" {
		finalText = "⚠️ The model returned an empty response."
	}

	// One final edit with the complete text.
	b.editOrLog(placeholder, finalText)

	// Atomically persist the user message and assistant reply as a pair
	// so concurrent requests never interleave between them.
	b.store.AppendBatch(chatID, []openai.ChatCompletionMessage{
		userMsg,
		{Role: openai.ChatMessageRoleAssistant, Content: finalText},
	}, b.cfg.ContextMaxMsgs)
}

// editOrLog edits a Telegram message, falling back to a log on error.
func (b *Bot) editOrLog(msg *tele.Message, text string) {
	// Telegram has a 4096-character limit per message.
	if len([]rune(text)) > 4096 {
		runes := []rune(text)
		text = string(runes[:4093]) + "…"
	}
	if _, err := b.tg.Edit(msg, text); err != nil {
		// Ignore "message not modified" – it's benign.
		if !strings.Contains(err.Error(), "message is not modified") {
			log.Printf("editOrLog error: %v", err)
		}
	}
}

// ─── Entry Point ──────────────────────────────────────────────────────────────

func main() {
	cfg := loadConfig()

	bot, err := NewBot(cfg)
	if err != nil {
		log.Fatalf("Failed to initialise bot: %v", err)
	}

	bot.Run()
}
