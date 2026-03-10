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
	AutoDetect     bool   // true = use LLM to judge if an un-mentioned message is relevant and should trigger a reply

	// Separate (optional) model config for the AUTO_DETECT relevance classifier.
	// When empty, falls back to the main OpenAI settings above.
	AutoDetectBase  string
	AutoDetectKey   string
	AutoDetectModel string

	// Access control. When both are empty, the bot is open to everyone.
	AllowedUsers  map[int64]bool // user IDs allowed in private chats
	AllowedGroups map[int64]bool // group/supergroup IDs where all members are allowed

	// User profile extraction settings.
	ProfileEnabled      bool   // enable NoSQL user-profile extraction
	ProfileDBPath       string // bbolt database file path
	ProfileExtractEvery int    // trigger extraction every N bot-replies per user

	// Separate (optional) model config for profile extraction.
	// When empty, falls back to the main OpenAI settings above.
	ProfileBase  string
	ProfileKey   string
	ProfileModel string

	// Summary settings.
	SummaryMinOverflow int // minimum overflow messages before triggering summarisation (default 6)

	// Separate (optional) model config for conversation summarisation.
	// When empty, falls back to the main OpenAI settings above.
	SummaryBase  string
	SummaryKey   string
	SummaryModel string
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

	autoDetect := strings.ToLower(getEnv("AUTO_DETECT", "false")) == "true"

	profileExtractEvery, _ := strconv.Atoi(getEnv("PROFILE_EXTRACT_EVERY", "3"))
	if profileExtractEvery <= 0 {
		profileExtractEvery = 3
	}

	summaryMinOverflow, _ := strconv.Atoi(getEnv("SUMMARY_MIN_OVERFLOW", "6"))
	if summaryMinOverflow <= 0 {
		summaryMinOverflow = 6
	}

	allowedUsers := parseIDList(getEnv("ALLOWED_USERS", ""))
	allowedGroups := parseIDList(getEnv("ALLOWED_GROUPS", ""))

	if len(allowedUsers) > 0 {
		ids := make([]string, 0, len(allowedUsers))
		for id := range allowedUsers {
			ids = append(ids, strconv.FormatInt(id, 10))
		}
		log.Printf("Access control: ALLOWED_USERS = %v", ids)
	}
	if len(allowedGroups) > 0 {
		ids := make([]string, 0, len(allowedGroups))
		for id := range allowedGroups {
			ids = append(ids, strconv.FormatInt(id, 10))
		}
		log.Printf("Access control: ALLOWED_GROUPS = %v", ids)
	}

	return Config{
		OpenAIBase:          getEnv("OPENAI_API_BASE", ""),
		OpenAIKey:           getEnv("OPENAI_API_KEY", ""),
		OpenAIModel:         getEnv("OPENAI_MODEL", "gpt-4o"),
		TelegramToken:       getEnv("TELEGRAM_BOT_TOKEN", ""),
		SystemPrompt:        getEnv("SYSTEM_PROMPT", "You are a helpful assistant."),
		ContextMaxMsgs:      maxMsgs,
		MaxTokens:           maxTokens,
		BotUsername:         getEnv("BOT_USERNAME", ""),
		ContextMode:         contextMode,
		AutoDetect:          autoDetect,
		AutoDetectBase:      getEnv("AUTO_DETECT_API_BASE", ""),
		AutoDetectKey:       getEnv("AUTO_DETECT_API_KEY", ""),
		AutoDetectModel:     getEnv("AUTO_DETECT_MODEL", ""),
		AllowedUsers:        allowedUsers,
		AllowedGroups:       allowedGroups,
		ProfileEnabled:      strings.ToLower(getEnv("PROFILE_ENABLED", "true")) == "true",
		ProfileDBPath:       getEnv("PROFILE_DB_PATH", "./data/profiles.db"),
		ProfileExtractEvery: profileExtractEvery,
		ProfileBase:         getEnv("PROFILE_API_BASE", ""),
		ProfileKey:          getEnv("PROFILE_API_KEY", ""),
		ProfileModel:        getEnv("PROFILE_MODEL", ""),
		SummaryMinOverflow:  summaryMinOverflow,
		SummaryBase:         getEnv("SUMMARY_API_BASE", ""),
		SummaryKey:          getEnv("SUMMARY_API_KEY", ""),
		SummaryModel:        getEnv("SUMMARY_MODEL", ""),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseIDList parses a comma-separated string of int64 IDs into a set.
// Invalid entries are silently skipped.
func parseIDList(raw string) map[int64]bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	set := make(map[int64]bool)
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			log.Printf("Warning: ignoring invalid ID %q in whitelist", s)
			continue
		}
		set[id] = true
	}
	if len(set) == 0 {
		return nil
	}
	return set
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
	fmt.Fprintf(&sb, " time:%s", time.Now().UTC().Format(time.RFC3339))
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
	mu       sync.RWMutex
	history  map[int64][]openai.ChatCompletionMessage
	overflow map[int64][]openai.ChatCompletionMessage // messages trimmed from the sliding window
}

func NewHistoryStore() *HistoryStore {
	return &HistoryStore{
		history:  make(map[int64][]openai.ChatCompletionMessage),
		overflow: make(map[int64][]openai.ChatCompletionMessage),
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
// Trimmed messages are accumulated in the overflow buffer for later summarisation.
func (s *HistoryStore) Append(chatID int64, msg openai.ChatCompletionMessage, maxMessages int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history[chatID] = append(s.history[chatID], msg)
	if len(s.history[chatID]) > maxMessages {
		trimCount := len(s.history[chatID]) - maxMessages
		s.overflow[chatID] = append(s.overflow[chatID], s.history[chatID][:trimCount]...)
		s.history[chatID] = s.history[chatID][trimCount:]
	}
}

// AppendBatch atomically appends multiple messages and trims the history.
// This is used to write the user message and assistant reply as a pair so
// concurrent requests cannot interleave between them.
// Trimmed messages are accumulated in the overflow buffer for later summarisation.
func (s *HistoryStore) AppendBatch(chatID int64, msgs []openai.ChatCompletionMessage, maxMessages int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history[chatID] = append(s.history[chatID], msgs...)
	if len(s.history[chatID]) > maxMessages {
		trimCount := len(s.history[chatID]) - maxMessages
		s.overflow[chatID] = append(s.overflow[chatID], s.history[chatID][:trimCount]...)
		s.history[chatID] = s.history[chatID][trimCount:]
	}
}

// Clear deletes the history and overflow buffer for a chat.
func (s *HistoryStore) Clear(chatID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.history, chatID)
	delete(s.overflow, chatID)
}

// OverflowCount returns the number of messages in the overflow buffer.
func (s *HistoryStore) OverflowCount(chatID int64) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.overflow[chatID])
}

// DrainOverflow returns accumulated overflow messages and clears the buffer.
func (s *HistoryStore) DrainOverflow(chatID int64) []openai.ChatCompletionMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	overflow := s.overflow[chatID]
	delete(s.overflow, chatID)
	return overflow
}

// ─── Bot ──────────────────────────────────────────────────────────────────────

type Bot struct {
	cfg           Config
	ai            *openai.Client // main LLM client
	detectorAI    *openai.Client // lighter model for relevance detection (may equal ai)
	detectorModel string         // model name for relevance detection
	profileAI     *openai.Client // LLM client for profile extraction (may equal ai)
	profileModel  string         // model name for profile extraction
	summaryAI     *openai.Client // LLM client for conversation summarisation (may equal ai)
	summaryModel  string         // model name for conversation summarisation
	store         *HistoryStore
	summaries     *SummaryStore // per-chat conversation summaries
	profiles      *ProfileStore // NoSQL user-profile store (nil if disabled)
	tg            *tele.Bot
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

	// Build detector client – falls back to main client if not configured.
	detectorClient := aiClient
	detectorModel := cfg.OpenAIModel
	if cfg.AutoDetectKey != "" || cfg.AutoDetectBase != "" || cfg.AutoDetectModel != "" {
		detKey := cfg.AutoDetectKey
		if detKey == "" {
			detKey = cfg.OpenAIKey
		}
		detCfg := openai.DefaultConfig(detKey)
		detBase := cfg.AutoDetectBase
		if detBase == "" {
			detBase = cfg.OpenAIBase
		}
		if detBase != "" {
			detCfg.BaseURL = detBase
		}
		detectorClient = openai.NewClientWithConfig(detCfg)
		if cfg.AutoDetectModel != "" {
			detectorModel = cfg.AutoDetectModel
		}
	}

	// Build Telegram bot.
	pref := tele.Settings{
		Token:  cfg.TelegramToken,
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
	}
	tgBot, err := tele.NewBot(pref)
	if err != nil {
		return nil, fmt.Errorf("failed to create Telegram bot: %w", err)
	}

	// Optionally initialise user-profile store and its LLM client.
	var profiles *ProfileStore
	profileClient := aiClient
	profileModel := cfg.OpenAIModel
	if cfg.ProfileEnabled {
		profiles, err = NewProfileStore(cfg.ProfileDBPath)
		if err != nil {
			return nil, fmt.Errorf("failed to init profile store: %w", err)
		}
		// Build profile-extraction LLM client – falls back to main client.
		if cfg.ProfileKey != "" || cfg.ProfileBase != "" || cfg.ProfileModel != "" {
			pKey := cfg.ProfileKey
			if pKey == "" {
				pKey = cfg.OpenAIKey
			}
			pCfg := openai.DefaultConfig(pKey)
			pBase := cfg.ProfileBase
			if pBase == "" {
				pBase = cfg.OpenAIBase
			}
			if pBase != "" {
				pCfg.BaseURL = pBase
			}
			profileClient = openai.NewClientWithConfig(pCfg)
			if cfg.ProfileModel != "" {
				profileModel = cfg.ProfileModel
			}
		}
		log.Printf("User profile extraction enabled (db: %s, every %d msgs, model: %s)",
			cfg.ProfileDBPath, cfg.ProfileExtractEvery, profileModel)
	}

	// Build summary LLM client – falls back to main client if not configured.
	summaryClient := aiClient
	summaryModel := cfg.OpenAIModel
	if cfg.SummaryKey != "" || cfg.SummaryBase != "" || cfg.SummaryModel != "" {
		sKey := cfg.SummaryKey
		if sKey == "" {
			sKey = cfg.OpenAIKey
		}
		sCfg := openai.DefaultConfig(sKey)
		sBase := cfg.SummaryBase
		if sBase == "" {
			sBase = cfg.OpenAIBase
		}
		if sBase != "" {
			sCfg.BaseURL = sBase
		}
		summaryClient = openai.NewClientWithConfig(sCfg)
		if cfg.SummaryModel != "" {
			summaryModel = cfg.SummaryModel
		}
	}
	log.Printf("Summary enabled (min_overflow: %d, model: %s)", cfg.SummaryMinOverflow, summaryModel)

	return &Bot{
		cfg:           cfg,
		ai:            aiClient,
		detectorAI:    detectorClient,
		detectorModel: detectorModel,
		profileAI:     profileClient,
		profileModel:  profileModel,
		summaryAI:     summaryClient,
		summaryModel:  summaryModel,
		store:         NewHistoryStore(),
		summaries:     NewSummaryStore(),
		profiles:      profiles,
		tg:            tgBot,
	}, nil
}

// isAllowed checks whether a message from the given chat/user is permitted.
// When no whitelist is configured (both maps nil), everything is allowed.
func (b *Bot) isAllowed(c tele.Context) bool {
	// No whitelist configured → open to everyone.
	if b.cfg.AllowedUsers == nil && b.cfg.AllowedGroups == nil {
		return true
	}

	chat := c.Chat()
	if chat == nil {
		return false
	}

	switch chat.Type {
	case tele.ChatPrivate:
		// Private chat: check user ID.
		if b.cfg.AllowedUsers != nil && b.cfg.AllowedUsers[c.Sender().ID] {
			return true
		}
	default:
		// Group / supergroup / channel: check group ID.
		if b.cfg.AllowedGroups != nil && b.cfg.AllowedGroups[chat.ID] {
			return true
		}
		// Also allow if the sender is individually whitelisted.
		if b.cfg.AllowedUsers != nil && c.Sender() != nil && b.cfg.AllowedUsers[c.Sender().ID] {
			return true
		}
	}

	return false
}

func (b *Bot) Run() {
	b.tg.Handle("/start", b.handleStart)
	b.tg.Handle("/clear", b.handleClear)
	b.tg.Handle("/summary", b.handleSummary)
	b.tg.Handle("/clearp", b.handleClearProfile)
	b.tg.Handle("/displayp", b.handleDisplayProfile)
	b.tg.Handle(tele.OnText, b.handleText)

	log.Printf("Bot @%s is running…", b.tg.Me.Username)
	b.tg.Start()
}

// ─── Handlers ─────────────────────────────────────────────────────────────────

// relevancePromptTpl is the system prompt template for the lightweight relevance check.
// The bot's name and persona are injected at runtime so the classifier knows the bot's identity.
const relevancePromptTpl = `You are a binary classifier that decides whether a new group-chat message should be answered by a chat bot.

Bot info:
- Telegram username: %s
- Persona / system prompt: %s

Rules — reply YES if ANY of the following is true:
1. The message explicitly mentions the bot by name, username, nickname, or any common alias (e.g. "助手", "机器人", "bot", "AI").
2. The message is a direct reply to one of the bot's previous messages.
3. The message is clearly continuing or following up on a conversation the bot was just involved in (look at the recent context).
4. The message asks a question or makes a request that only an AI assistant would be expected to answer, and there is no other obvious human addressee.

Reply NO if the message is clearly directed at another human, is casual chatter unrelated to the bot, or is a greeting with no indication it is aimed at the bot.

!!! Reply with ONLY one word: YES or NO. Do not explain. !!!`

// isRelevant uses a cheap, low-token LLM call to decide whether an
// un-mentioned group message is relevant to this bot and should trigger a reply.
func (b *Bot) isRelevant(chatID int64, sender *tele.User, text string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	botName := b.cfg.BotUsername
	if botName == "" {
		botName = "@" + b.tg.Me.Username
	}

	systemPrompt := fmt.Sprintf(relevancePromptTpl, botName, b.cfg.SystemPrompt)

	// Build a minimal prompt: classifier system + recent context + new message.
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
	}

	// Include up to 5 recent history messages for context (keep it cheap).
	history := b.store.Get(chatID)
	if len(history) > 5 {
		history = history[len(history)-5:]
	}
	for _, h := range history {
		msgs = append(msgs, openai.ChatCompletionMessage{
			Role:    h.Role,
			Content: h.Content,
		})
	}

	msgs = append(msgs, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: fmt.Sprintf("New message from %s:\n%s\n\nShould the bot reply? YES or NO.", sanitizeName(sender.ID), text),
	})

	req := openai.ChatCompletionRequest{
		Model:     b.detectorModel,
		Messages:  msgs,
		MaxTokens: 100,
	}

	// Retry up to 3 times on transient errors (EOF, timeout, etc.).
	var resp openai.ChatCompletionResponse
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		resp, err = b.detectorAI.CreateChatCompletion(ctx, req)
		if err == nil {
			break
		}
		// Only retry on transient network errors (EOF, connection reset, etc.).
		errMsg := err.Error()
		if strings.Contains(errMsg, "EOF") ||
			strings.Contains(errMsg, "connection reset") ||
			strings.Contains(errMsg, "timeout") {
			log.Printf("isRelevant attempt %d/%d transient error: %v", attempt+1, 3, err)
			time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
			continue
		}
		break // non-transient error, don't retry
	}
	if err != nil {
		log.Printf("isRelevant error after retries: %v", err)
		return false
	}
	if len(resp.Choices) == 0 {
		log.Println("isRelevant: no choices returned")
		return false
	}
	// Debug: log full response details
	choice := resp.Choices[0]
	log.Printf("isRelevant debug: finish_reason=%s, content=%q, role=%s",
		choice.FinishReason, choice.Message.Content, choice.Message.Role)
	answer := strings.TrimSpace(strings.ToUpper(choice.Message.Content))
	return strings.HasPrefix(answer, "YES")
}

func (b *Bot) handleStart(c tele.Context) error {
	if !b.isAllowed(c) {
		log.Printf("Access denied: user=%d chat=%d", c.Sender().ID, c.Chat().ID)
		return nil
	}
	return c.Reply("👋 Hello! I'm your AI assistant. Ask me anything.\nUse /clear to reset conversation history.")
}

func (b *Bot) handleClear(c tele.Context) error {
	if !b.isAllowed(c) {
		log.Printf("Access denied: user=%d chat=%d", c.Sender().ID, c.Chat().ID)
		return nil
	}
	chatID := c.Chat().ID
	b.store.Clear(chatID)
	b.summaries.Clear(chatID)
	return c.Reply("✅ Conversation history and summary cleared.")
}

func (b *Bot) handleSummary(c tele.Context) error {
	if !b.isAllowed(c) {
		log.Printf("Access denied: user=%d chat=%d", c.Sender().ID, c.Chat().ID)
		return nil
	}
	chatID := c.Chat().ID
	summary := b.summaries.Get(chatID)
	if summary == "" {
		return c.Reply("📭 No conversation summary yet. A summary will be generated automatically when the context window overflows.")
	}
	reply := fmt.Sprintf("📝 Current conversation summary:\n\n%s", summary)
	if len([]rune(reply)) > 4096 {
		runes := []rune(reply)
		reply = string(runes[:4093]) + "…"
	}
	return c.Reply(reply)
}

func (b *Bot) handleClearProfile(c tele.Context) error {
	if !b.isAllowed(c) {
		log.Printf("Access denied: user=%d chat=%d", c.Sender().ID, c.Chat().ID)
		return nil
	}
	username := c.Sender().Username
	if username == "" {
		return c.Reply("⚠️ You don't have a Telegram username set, so no profile is stored.")
	}
	if b.profiles == nil {
		return c.Reply("⚠️ Profile extraction is disabled.")
	}
	if err := b.profiles.Delete(username); err != nil {
		log.Printf("[profile] delete error for @%s: %v", username, err)
		return c.Reply("❌ Failed to clear profile.")
	}
	return c.Reply("✅ Your user profile has been cleared.")
}

func (b *Bot) handleDisplayProfile(c tele.Context) error {
	if !b.isAllowed(c) {
		log.Printf("Access denied: user=%d chat=%d", c.Sender().ID, c.Chat().ID)
		return nil
	}
	username := c.Sender().Username
	if username == "" {
		return c.Reply("⚠️ You don't have a Telegram username set, so no profile is stored.")
	}
	if b.profiles == nil {
		return c.Reply("⚠️ Profile extraction is disabled.")
	}
	profile, err := b.profiles.Get(username)
	if err != nil {
		log.Printf("[profile] read error for @%s: %v", username, err)
		return c.Reply("❌ Failed to read profile.")
	}
	if profile == nil || len(profile.Facts) == 0 {
		return c.Reply("📭 No profile data yet. Keep chatting and I'll learn about you!")
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("👤 Profile for @%s:\n", username))
	for i, fact := range profile.Facts {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, fact))
	}
	sb.WriteString(fmt.Sprintf("\n🕐 Last updated: %s", profile.UpdatedAt.Format("2006-01-02 15:04 UTC")))
	return c.Reply(sb.String())
}

func (b *Bot) handleText(c tele.Context) error {
	if !b.isAllowed(c) {
		log.Printf("Access denied: user=%d chat=%d", c.Sender().ID, c.Chat().ID)
		return nil
	}

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

			// AUTO_DETECT: ask LLM whether this message is relevant to the bot.
			if b.cfg.AutoDetect && text != "" {
				if b.isRelevant(c.Chat().ID, msg.Sender, text) {
					// Treat as if the bot was mentioned – fall through to reply.
					mentioned = true
				}
			}

			if !mentioned {
				return nil // not mentioned and not relevant – do not reply
			}
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
	history := b.store.Get(chatID)

	// Inject user profiles of conversation participants into the system prompt.
	profileSection := b.buildProfileSection(append(history, userMsg))
	systemPrompt := b.cfg.SystemPrompt + profileSection

	// Inject conversation summary (compressed history beyond the sliding window).
	if summary := b.summaries.Get(chatID); summary != "" {
		systemPrompt += "\n\n=== Previous Conversation Summary ===\n" + summary
	}

	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
	}
	messages = append(messages, history...)
	messages = append(messages, userMsg)

	// Send the initial placeholder message.
	placeholder, err := b.tg.Send(c.Chat(), "⏳ Thinking…")
	if err != nil {
		return fmt.Errorf("failed to send placeholder: %w", err)
	}

	// Run streaming in a goroutine so we can tick-update Telegram.
	go b.streamReply(chatID, userMsg, messages, placeholder, sender)

	return nil
}

// ─── Streaming ────────────────────────────────────────────────────────────────

func (b *Bot) streamReply(
	chatID int64,
	userMsg openai.ChatCompletionMessage,
	messages []openai.ChatCompletionMessage,
	placeholder *tele.Message,
	sender *tele.User,
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

	// Trigger background summarisation when enough overflow has accumulated.
	if b.store.OverflowCount(chatID) >= b.cfg.SummaryMinOverflow {
		go b.summarizeOverflow(chatID)
	}

	// Trigger background user-profile extraction if due.
	if b.profiles != nil && sender != nil && sender.Username != "" {
		if b.profiles.ShouldExtract(sender.Username, b.cfg.ProfileExtractEvery, 2*time.Minute) {
			allMsgs := b.store.Get(chatID)
			displayName := strings.TrimSpace(sender.FirstName + " " + sender.LastName)
			go b.extractProfile(sender.Username, displayName, allMsgs)
		}
	}
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
	if bot.profiles != nil {
		defer bot.profiles.Close()
	}

	bot.Run()
}
