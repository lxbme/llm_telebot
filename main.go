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
	SummaryEnabled     bool // enable conversation summary
	SummaryMinOverflow int  // minimum overflow messages before triggering summarisation (default 6)

	// Separate (optional) model config for conversation summarisation.
	// When empty, falls back to the main OpenAI settings above.
	SummaryBase  string
	SummaryKey   string
	SummaryModel string

	// Persistent chat storage.
	ChatDBPath string // bbolt database for persistent chat history & summaries

	// Tool calling (MCP) settings.
	ToolsEnabled       bool   // enable LLM tool/function calling
	ToolsMaxIterations int    // max tool-call round-trips per request (default 5)
	MCPConfigPath      string // path to MCP servers JSON config file
	UserMCPDBPath      string // bbolt database for per-user MCP configs

	// Admin settings.
	AdminIDs map[int64]bool // user IDs allowed to add command-based (stdio) MCP servers
	AdminAll bool           // when true, all users are treated as admin (ADMIN_ID=*)

	// Volcengine TTS settings.
	VolcengineTTSAppID       string
	VolcengineTTSAccessKey   string
	VolcengineTTSResourceID  string
	VolcengineTTSSpeaker     string
	VolcengineTTSAudioFormat string
	VolcengineTTSSampleRate  int
	VolcengineTTSSpeechRate  int
	VolcengineTTSSendText    bool
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

	toolsMaxIter, _ := strconv.Atoi(getEnv("TOOLS_MAX_ITERATIONS", "5"))
	if toolsMaxIter <= 0 {
		toolsMaxIter = 5
	}

	ttsSampleRate, _ := strconv.Atoi(getEnv("VOLCENGINE_TTS_SAMPLE_RATE", strconv.Itoa(defaultTTSSampleRate)))
	if ttsSampleRate <= 0 {
		ttsSampleRate = defaultTTSSampleRate
	}

	ttsSpeechRate, _ := strconv.Atoi(getEnv("VOLCENGINE_TTS_SPEECH_RATE", "0"))
	if ttsSpeechRate < -50 {
		ttsSpeechRate = -50
	}
	if ttsSpeechRate > 100 {
		ttsSpeechRate = 100
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
		OpenAIBase:               getEnv("OPENAI_API_BASE", ""),
		OpenAIKey:                getEnv("OPENAI_API_KEY", ""),
		OpenAIModel:              getEnv("OPENAI_MODEL", "gpt-4o"),
		TelegramToken:            getEnv("TELEGRAM_BOT_TOKEN", ""),
		SystemPrompt:             getEnv("SYSTEM_PROMPT", "You are a helpful assistant."),
		ContextMaxMsgs:           maxMsgs,
		MaxTokens:                maxTokens,
		BotUsername:              getEnv("BOT_USERNAME", ""),
		ContextMode:              contextMode,
		AutoDetect:               autoDetect,
		AutoDetectBase:           getEnv("AUTO_DETECT_API_BASE", ""),
		AutoDetectKey:            getEnv("AUTO_DETECT_API_KEY", ""),
		AutoDetectModel:          getEnv("AUTO_DETECT_MODEL", ""),
		AllowedUsers:             allowedUsers,
		AllowedGroups:            allowedGroups,
		ProfileEnabled:           strings.ToLower(getEnv("PROFILE_ENABLED", "true")) == "true",
		ProfileDBPath:            getEnv("PROFILE_DB_PATH", "./data/profiles.db"),
		ProfileExtractEvery:      profileExtractEvery,
		ProfileBase:              getEnv("PROFILE_API_BASE", ""),
		ProfileKey:               getEnv("PROFILE_API_KEY", ""),
		ProfileModel:             getEnv("PROFILE_MODEL", ""),
		SummaryEnabled:           strings.ToLower(getEnv("SUMMARY_ENABLED", "true")) == "true",
		SummaryMinOverflow:       summaryMinOverflow,
		SummaryBase:              getEnv("SUMMARY_API_BASE", ""),
		SummaryKey:               getEnv("SUMMARY_API_KEY", ""),
		SummaryModel:             getEnv("SUMMARY_MODEL", ""),
		ChatDBPath:               getEnv("CHAT_DB_PATH", "./data/chat.db"),
		ToolsEnabled:             strings.ToLower(getEnv("TOOLS_ENABLED", "false")) == "true",
		ToolsMaxIterations:       toolsMaxIter,
		MCPConfigPath:            getEnv("MCP_CONFIG_PATH", ""),
		UserMCPDBPath:            getEnv("USER_MCP_DB_PATH", "./data/user_mcp.db"),
		AdminIDs:                 parseIDList(getEnv("ADMIN_ID", "")),
		AdminAll:                 strings.TrimSpace(getEnv("ADMIN_ID", "")) == "*",
		VolcengineTTSAppID:       getEnv("VOLCENGINE_TTS_APP_ID", ""),
		VolcengineTTSAccessKey:   getEnv("VOLCENGINE_TTS_ACCESS_KEY", ""),
		VolcengineTTSResourceID:  getEnv("VOLCENGINE_TTS_RESOURCE_ID", defaultTTSResourceID),
		VolcengineTTSSpeaker:     getEnv("VOLCENGINE_TTS_SPEAKER", defaultTTSSpeaker),
		VolcengineTTSAudioFormat: getEnv("VOLCENGINE_TTS_AUDIO_FORMAT", defaultTTSAudioFormat),
		VolcengineTTSSampleRate:  ttsSampleRate,
		VolcengineTTSSpeechRate:  ttsSpeechRate,
		VolcengineTTSSendText:    strings.ToLower(getEnv("VOLCENGINE_TTS_SEND_TEXT", "true")) != "false",
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
	db       *ChatDB                                  // optional persistent backend
}

func NewHistoryStore(db *ChatDB) *HistoryStore {
	s := &HistoryStore{
		history:  make(map[int64][]openai.ChatCompletionMessage),
		overflow: make(map[int64][]openai.ChatCompletionMessage),
		db:       db,
	}
	// Restore persisted data on startup.
	if db != nil {
		for chatID, msgs := range db.LoadAllHistory() {
			s.history[chatID] = msgs
		}
		for chatID, msgs := range db.LoadAllOverflow() {
			s.overflow[chatID] = msgs
		}
		if len(s.history) > 0 {
			log.Printf("[chat-db] restored history for %d chat(s)", len(s.history))
		}
	}
	return s
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
	s.persistChat(chatID)
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
	s.persistChat(chatID)
}

// Clear deletes the history and overflow buffer for a chat.
func (s *HistoryStore) Clear(chatID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.history, chatID)
	delete(s.overflow, chatID)
	if s.db != nil {
		s.db.DeleteHistory(chatID)
		s.db.DeleteOverflow(chatID)
	}
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
	if s.db != nil {
		s.db.DeleteOverflow(chatID)
	}
	return overflow
}

// persistChat saves both history and overflow for a chat to the database.
// Must be called with s.mu held.
func (s *HistoryStore) persistChat(chatID int64) {
	if s.db == nil {
		return
	}
	s.db.SaveHistory(chatID, s.history[chatID])
	s.db.SaveOverflow(chatID, s.overflow[chatID])
}

// ─── Bot ──────────────────────────────────────────────────────────────────────

type Bot struct {
	mu            sync.RWMutex
	cfg           Config
	ai            *openai.Client // main LLM client
	detectorAI    *openai.Client // lighter model for relevance detection (may equal ai)
	detectorModel string         // model name for relevance detection
	profileAI     *openai.Client // LLM client for profile extraction (may equal ai)
	profileModel  string         // model name for profile extraction
	summaryAI     *openai.Client // LLM client for conversation summarisation (may equal ai)
	summaryModel  string         // model name for conversation summarisation
	chatDB        *ChatDB        // persistent chat storage (nil-safe)
	store         *HistoryStore
	summaries     *SummaryStore     // per-chat conversation summaries
	profiles      *ProfileStore     // NoSQL user-profile store (nil if disabled)
	tools         *ToolRegistry     // global registered MCP tools (nil if disabled)
	mcpClients    *MCPClientManager // global remote MCP server connections (nil if none)
	userTools     *UserToolManager  // per-user dynamic MCP tools (nil if disabled)
	speechModes   *SpeechModeStore
	adminSessions *AdminConfigSessionStore
	tts           *VolcengineTTSClient
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

	// Open persistent chat database.
	chatDB, err := OpenChatDB(cfg.ChatDBPath)
	if err != nil {
		return nil, fmt.Errorf("failed to init chat db: %w", err)
	}
	log.Printf("Persistent chat storage enabled (db: %s)", cfg.ChatDBPath)

	// Build summary LLM client – falls back to main client if not configured.
	summaryClient := aiClient
	summaryModel := cfg.OpenAIModel
	if cfg.SummaryEnabled {
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
	}

	// Optionally initialise tool registry.
	var toolRegistry *ToolRegistry
	var mcpManager *MCPClientManager
	if cfg.ToolsEnabled {
		toolRegistry = NewToolRegistry()

		// Connect to external MCP servers if configured.
		if cfg.MCPConfigPath != "" {
			mcpCfg, err := LoadMCPConfig(cfg.MCPConfigPath)
			if err != nil {
				log.Printf("Warning: failed to load MCP config from %s: %v", cfg.MCPConfigPath, err)
			} else {
				mcpManager = NewMCPClientManager()
				mcpManager.ConnectAll(mcpCfg, toolRegistry)
			}
		}

		log.Printf("Tool calling enabled (%d global tools, max %d iterations)", toolRegistry.Count(), cfg.ToolsMaxIterations)
	}

	// Initialise per-user MCP tool manager (always enabled when tools are enabled).
	var userToolMgr *UserToolManager
	if cfg.ToolsEnabled {
		umcpStore, err := NewUserMCPStore(cfg.UserMCPDBPath)
		if err != nil {
			return nil, fmt.Errorf("failed to init user MCP store: %w", err)
		}
		userToolMgr = NewUserToolManager(umcpStore)
		userToolMgr.RestoreAll()
		log.Printf("Per-user dynamic MCP enabled (db: %s)", cfg.UserMCPDBPath)
	}

	ttsClient := NewVolcengineTTSClient(cfg)
	if ttsClient != nil {
		log.Printf("Volcengine TTS enabled (resource: %s, speaker: %s, format: %s)",
			ttsClient.resourceID, ttsClient.speaker, ttsClient.audioFormat)
	}

	return &Bot{
		cfg:           cfg,
		ai:            aiClient,
		detectorAI:    detectorClient,
		detectorModel: detectorModel,
		profileAI:     profileClient,
		profileModel:  profileModel,
		summaryAI:     summaryClient,
		summaryModel:  summaryModel,
		chatDB:        chatDB,
		store:         NewHistoryStore(chatDB),
		summaries:     NewSummaryStore(chatDB),
		profiles:      profiles,
		tools:         toolRegistry,
		mcpClients:    mcpManager,
		userTools:     userToolMgr,
		speechModes:   NewSpeechModeStore(),
		adminSessions: NewAdminConfigSessionStore(),
		tts:           ttsClient,
		tg:            tgBot,
	}, nil
}

// isAllowed checks whether a message from the given chat/user is permitted.
// When no whitelist is configured (both maps nil), everything is allowed.
func (b *Bot) isAllowed(c tele.Context) bool {
	cfg := b.currentConfig()
	// No whitelist configured → open to everyone.
	if cfg.AllowedUsers == nil && cfg.AllowedGroups == nil {
		return true
	}

	chat := c.Chat()
	if chat == nil {
		return false
	}

	switch chat.Type {
	case tele.ChatPrivate:
		// Private chat: check user ID.
		if cfg.AllowedUsers != nil && cfg.AllowedUsers[c.Sender().ID] {
			return true
		}
	default:
		// Group / supergroup / channel: check group ID.
		if cfg.AllowedGroups != nil && cfg.AllowedGroups[chat.ID] {
			return true
		}
		// Also allow if the sender is individually whitelisted.
		if cfg.AllowedUsers != nil && c.Sender() != nil && cfg.AllowedUsers[c.Sender().ID] {
			return true
		}
	}

	return false
}

// isAdmin checks whether the given user ID is in the ADMIN_ID list.
// When ADMIN_ID is set to "*", all users are considered admins.
func (b *Bot) isAdmin(userID int64) bool {
	cfg := b.currentConfig()
	if cfg.AdminAll {
		return true
	}
	return len(cfg.AdminIDs) > 0 && cfg.AdminIDs[userID]
}

func (b *Bot) Run() {
	b.tg.Handle("/start", b.handleStart)
	b.tg.Handle("/clear", b.handleClear)
	b.tg.Handle("/summary", b.handleSummary)
	b.tg.Handle("/speach", b.handleSpeechMode)
	b.tg.Handle("/speech", b.handleSpeechMode)
	b.tg.Handle("/clearp", b.handleClearProfile)
	b.tg.Handle("/displayp", b.handleDisplayProfile)
	b.tg.Handle("/mcp_list", b.handleMCPList)
	b.tg.Handle("/mcp_del", b.handleMCPDel)
	b.tg.Handle("/mcp_clear", b.handleMCPClear)
	b.tg.Handle("/admin", b.handleAdminCommand)
	adminCancelBtn := &tele.Btn{Unique: adminCancelButtonUnique}
	b.tg.Handle(adminCancelBtn, b.handleAdminCancel)
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
	snap := b.snapshot()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	botName := snap.cfg.BotUsername
	if botName == "" {
		botName = "@" + snap.tg.Me.Username
	}

	systemPrompt := fmt.Sprintf(relevancePromptTpl, botName, snap.cfg.SystemPrompt)

	// Build a minimal prompt: classifier system + recent context + new message.
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
	}

	// Include up to 5 recent history messages for context (keep it cheap).
	history := snap.store.Get(chatID)
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
		Model:     snap.detectorModel,
		Messages:  msgs,
		MaxTokens: 100,
	}

	// Retry up to 3 times on transient errors (EOF, timeout, etc.).
	var resp openai.ChatCompletionResponse
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		resp, err = snap.detectorAI.CreateChatCompletion(ctx, req)
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
	// log.Printf("isRelevant debug: finish_reason=%s, content=%q, role=%s",
	// 	choice.FinishReason, choice.Message.Content, choice.Message.Role)
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
	snap := b.snapshot()
	snap.store.Clear(chatID)
	if snap.cfg.SummaryEnabled {
		snap.summaries.Clear(chatID)
	}
	return c.Reply("✅ Conversation history and summary cleared.")
}

func (b *Bot) handleSummary(c tele.Context) error {
	if !b.isAllowed(c) {
		log.Printf("Access denied: user=%d chat=%d", c.Sender().ID, c.Chat().ID)
		return nil
	}
	snap := b.snapshot()
	if !snap.cfg.SummaryEnabled {
		return c.Reply("⚠️ Conversation summary is disabled.")
	}
	chatID := c.Chat().ID
	summary := snap.summaries.Get(chatID)
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

func (b *Bot) handleSpeechMode(c tele.Context) error {
	if !b.isAllowed(c) {
		return nil
	}
	if c.Sender() == nil || !b.isAdmin(c.Sender().ID) {
		return c.Reply("🚫 Only admins can use /speach。")
	}
	snap := b.snapshot()
	if snap.tts == nil {
		return c.Reply("⚠️ TTS is not configured, please set the `VOLCENGINE_TTS_*` environment variables。")
	}

	chatID := c.Chat().ID
	arg := strings.ToLower(strings.TrimSpace(c.Message().Payload))
	current := snap.speechModes.Enabled(chatID)
	next := current

	switch arg {
	case "", "toggle", "status", "状态":
		if arg == "status" || arg == "状态" {
			if current {
				return c.Reply("🔊 The /speach mode is currently enabled for this chat。")
			}
			return c.Reply("🔇 The /speach mode is currently disabled for this chat。")
		}
		next = !current
	case "on", "enable", "start", "true", "1", "开启", "开":
		next = true
	case "off", "disable", "stop", "false", "0", "关闭", "关":
		next = false
	default:
		return c.Reply("Usage: `/speach [on|off|toggle|status]`\nWithout arguments, the mode will be toggled automatically。")
	}

	snap.speechModes.Set(chatID, next)
	if next {
		return c.Reply("✅ The /speach mode is currently enabled for this chat。")
	}
	return c.Reply("✅ The /speach mode is currently disabled for this chat。")
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
	snap := b.snapshot()
	if snap.profiles == nil {
		return c.Reply("⚠️ Profile extraction is disabled.")
	}
	if err := snap.profiles.Delete(username); err != nil {
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
	snap := b.snapshot()
	if snap.profiles == nil {
		return c.Reply("⚠️ Profile extraction is disabled.")
	}
	profile, err := snap.profiles.Get(username)
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

// ─── MCP Management Commands ─────────────────────────────────────────────────

func (b *Bot) handleMCPList(c tele.Context) error {
	if !b.isAllowed(c) {
		return nil
	}
	snap := b.snapshot()
	if snap.userTools == nil {
		return c.Reply("⚠️ Tool calling is disabled.")
	}
	userID := c.Sender().ID
	servers, err := snap.userTools.ListServers(userID)
	if err != nil {
		log.Printf("[user-mcp] list error for user %d: %v", userID, err)
		return c.Reply("❌ Failed to list MCP servers.")
	}
	return c.Reply(FormatServerList(servers))
}

func (b *Bot) handleMCPDel(c tele.Context) error {
	if !b.isAllowed(c) {
		return nil
	}
	snap := b.snapshot()
	if snap.userTools == nil {
		return c.Reply("⚠️ Tool calling is disabled.")
	}
	name := strings.TrimSpace(c.Message().Payload)
	if name == "" {
		return c.Reply("Usage: /mcp_del <server_name>\n\nUse /mcp_list to see your servers.")
	}
	userID := c.Sender().ID
	found, err := snap.userTools.RemoveServer(userID, name)
	if err != nil {
		log.Printf("[user-mcp] remove error for user %d: %v", userID, err)
		return c.Reply("❌ Failed to remove MCP server.")
	}
	if !found {
		return c.Reply(fmt.Sprintf("⚠️ Server %q not found in your config.", name))
	}
	return c.Reply(fmt.Sprintf("✅ Removed MCP server %q.", name))
}

func (b *Bot) handleMCPClear(c tele.Context) error {
	if !b.isAllowed(c) {
		return nil
	}
	snap := b.snapshot()
	if snap.userTools == nil {
		return c.Reply("⚠️ Tool calling is disabled.")
	}
	userID := c.Sender().ID
	if err := snap.userTools.ClearAll(userID); err != nil {
		log.Printf("[user-mcp] clear error for user %d: %v", userID, err)
		return c.Reply("❌ Failed to clear MCP servers.")
	}
	return c.Reply("✅ All your personal MCP servers have been removed.")
}

func (b *Bot) handleText(c tele.Context) error {
	msg := c.Message()
	if msg == nil {
		return nil
	}

	text := strings.TrimSpace(msg.Text)

	if handled, err := b.handleAdminTextIfNeeded(c, text); handled {
		return err
	}

	if !b.isAllowed(c) {
		log.Printf("Access denied: user=%d chat=%d", c.Sender().ID, c.Chat().ID)
		return nil
	}

	// ── Skip bot commands ────────────────────────────────────────────────────
	// Commands like /mcp_list should be handled by their own handlers, not by
	// the LLM. Guard against any edge cases where they might reach handleText.
	if strings.HasPrefix(text, "/") {
		return nil
	}

	// ── MCP JSON auto-detection ──────────────────────────────────────────────
	// If the user sends a JSON block with "mcpServers", treat it as an MCP
	// import command rather than a normal chat message.
	if strings.HasPrefix(text, "{") {
		if mcpCfg, ok := TryParseMCPConfig(text); ok {
			snap := b.snapshot()
			if snap.userTools == nil {
				return c.Reply("⚠️ Tool calling is disabled. Cannot import MCP servers.")
			}
			userID := c.Sender().ID
			// Non-admin users may only add network-based (HTTP/SSE) MCP servers.
			if !b.isAdmin(userID) {
				if rejected := filterCommandMCPs(mcpCfg); len(rejected) > 0 {
					return c.Reply(fmt.Sprintf("🚫 Only admins can add command-based (stdio) MCP servers.\nRejected: %s", strings.Join(rejected, ", ")))
				}
			}
			result, err := snap.userTools.AddServers(userID, mcpCfg.MCPServers)
			if err != nil {
				log.Printf("[user-mcp] add error for user %d: %v", userID, err)
				return c.Reply(fmt.Sprintf("❌ Failed to add MCP servers: %v", err))
			}
			return c.Reply(formatMCPAddResult(result))
		}
	}

	// ── Group logic: only respond when mentioned ──────────────────────────────
	mentioned := false
	if msg.Chat.Type != tele.ChatPrivate {
		snap := b.snapshot()
		mention := snap.cfg.BotUsername
		if mention == "" {
			mention = "@" + snap.tg.Me.Username
		}
		// normalise casing for comparison
		mentioned = strings.Contains(strings.ToLower(text), strings.ToLower(mention))

		if !mentioned {
			// In global mode, store every group message as context even if bot is not mentioned.
			if snap.cfg.ContextMode == "global" && text != "" {
				snap.store.Append(c.Chat().ID, openai.ChatCompletionMessage{
					Role:    openai.ChatMessageRoleUser,
					Name:    sanitizeName(msg.Sender.ID),
					Content: buildUserContent(msg.Sender, text),
				}, snap.cfg.ContextMaxMsgs)
			}

			// AUTO_DETECT: ask LLM whether this message is relevant to the bot.
			if snap.cfg.AutoDetect && text != "" {
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
	snap := b.snapshot()
	history := snap.store.Get(chatID)

	// Inject user profiles of conversation participants into the system prompt.
	profileSection := b.buildProfileSection(append(history, userMsg))
	systemPrompt := snap.cfg.SystemPrompt + b.speechInstruction(chatID) + profileSection

	// Inject conversation summary (compressed history beyond the sliding window).
	if snap.cfg.SummaryEnabled {
		if summary := snap.summaries.Get(chatID); summary != "" {
			systemPrompt += "\n\n=== Previous Conversation Summary ===\n" + summary
		}
	}

	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
	}
	messages = append(messages, history...)
	messages = append(messages, userMsg)

	// Send the initial placeholder message.
	placeholder, err := snap.tg.Send(c.Chat(), "⏳ Thinking…")
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
	snap := b.snapshot()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Build a merged tool view: global tools + this user's personal tools.
	var toolView *MergedToolView
	if snap.tools != nil {
		var userReg *ToolRegistry
		if snap.userTools != nil && sender != nil {
			userReg = snap.userTools.GetRegistry(sender.ID)
		}
		toolView = NewMergedToolView(snap.tools, userReg)
	}

	// ── Tool-call loop ──────────────────────────────────────────────────
	// If tools are enabled, we do non-streaming requests in a loop so the
	// LLM can call tools repeatedly. Once all tool calls are resolved, the
	// conversation (including tool results) is sent to the streaming path
	// so the LLM can summarise/present the results fluently.
	if toolView != nil && toolView.Count() > 0 {
		maxIter := snap.cfg.ToolsMaxIterations
		for i := 0; i < maxIter; i++ {
			req := openai.ChatCompletionRequest{
				Model:     snap.cfg.OpenAIModel,
				Messages:  messages,
				Tools:     toolView.OpenAITools(),
				MaxTokens: snap.cfg.MaxTokens,
			}

			resp, err := snap.ai.CreateChatCompletion(ctx, req)
			if err != nil {
				b.editOrLog(placeholder, fmt.Sprintf("\u274c Error: %v", err))
				return
			}
			if len(resp.Choices) == 0 {
				b.editOrLog(placeholder, "\u26a0\ufe0f The model returned an empty response.")
				return
			}

			choice := resp.Choices[0]

			// If the model chose to call tool(s):
			if len(choice.Message.ToolCalls) > 0 {
				// Append the assistant's tool-call message to the conversation.
				messages = append(messages, choice.Message)

				// Show the user which tools are being called.
				var toolNames []string
				for _, tc := range choice.Message.ToolCalls {
					toolNames = append(toolNames, tc.Function.Name)
				}
				b.editOrLog(placeholder, fmt.Sprintf("\U0001f527 Calling tools: %s…", strings.Join(toolNames, ", ")))

				// Execute each tool call and append the results.
				for _, tc := range choice.Message.ToolCalls {
					log.Printf("[tools] executing %s", tc.Function.Name)
					result := toolView.ExecuteToolCall(tc)
					log.Printf("[tools] %s done (%d bytes)", tc.Function.Name, len(result))
					messages = append(messages, openai.ChatCompletionMessage{
						Role:       openai.ChatMessageRoleTool,
						Content:    result,
						ToolCallID: tc.ID,
					})
				}
				continue // loop back to let the LLM process tool results
			}

			// No more tool calls — break out to the streaming path below
			// so the LLM's final answer is streamed to the user.
			break
		}
	}

	// ── Streaming path ──────────────────────────────────────────────────
	// Streams the LLM's final response to the user. When tools were used,
	// the messages slice now contains the full tool-call conversation, so
	// the LLM will summarise the tool results into a natural reply.
	finalText := b.doStream(ctx, messages, placeholder, toolView)
	if finalText == "" {
		return // error already reported by doStream
	}
	b.postReply(chatID, userMsg, finalText, sender, placeholder.Chat, placeholder)
}

// doStream performs the streaming LLM call and returns the final text.
// Returns empty string if an error was already reported to the user.
// toolView may be nil if tools are disabled.
func (b *Bot) doStream(
	ctx context.Context,
	messages []openai.ChatCompletionMessage,
	placeholder *tele.Message,
	toolView *MergedToolView,
) string {
	snap := b.snapshot()
	showText := true
	if placeholder != nil && placeholder.Chat != nil {
		showText = b.shouldSendSpeechText(placeholder.Chat.ID)
	}

	req := openai.ChatCompletionRequest{
		Model:     snap.cfg.OpenAIModel,
		Messages:  messages,
		Stream:    true,
		MaxTokens: snap.cfg.MaxTokens,
	}

	// If tools are available, include them so the model knows about them
	// even in streaming mode.
	if toolView != nil && toolView.Count() > 0 {
		req.Tools = toolView.OpenAITools()
	}

	stream, err := snap.ai.CreateChatCompletionStream(ctx, req)
	if err != nil {
		b.editOrLog(placeholder, fmt.Sprintf("❌ Error starting stream: %v", err))
		return ""
	}
	defer stream.Close()

	var (
		fullResponse strings.Builder
		mu           sync.Mutex // guards fullResponse

		ticker   = time.NewTicker(1500 * time.Millisecond)
		lastSent string
	)
	defer ticker.Stop()

	// Cancel context for ticker goroutine.
	streamCtx, streamCancel := context.WithCancel(ctx)

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
				if showText && current != lastSent && current != "" {
					b.editOrLog(placeholder, current+"▌")
					lastSent = current
				}
			case <-streamCtx.Done():
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
	streamCancel()
	<-done

	finalText := fullResponse.String()

	if streamErr != nil {
		errMsg := fmt.Sprintf("❌ Stream error: %v", streamErr)
		if finalText != "" {
			errMsg = finalText + "\n\n" + errMsg
		}
		b.editOrLog(placeholder, errMsg)
		return ""
	}

	if finalText == "" {
		finalText = "⚠️ The model returned an empty response."
	}

	if showText {
		// One final edit with the complete text, rendered as Telegram HTML.
		b.editFinalResponse(placeholder, finalText)
	} else {
		b.editOrLog(placeholder, "🔊 正在发送语音…")
	}
	return finalText
}

// postReply handles post-response bookkeeping: history persistence, summary
// triggering, and profile extraction.
func (b *Bot) postReply(
	chatID int64,
	userMsg openai.ChatCompletionMessage,
	finalText string,
	sender *tele.User,
	chat *tele.Chat,
	placeholder *tele.Message,
) {
	snap := b.snapshot()
	// Atomically persist the user message and assistant reply as a pair
	// so concurrent requests never interleave between them.
	snap.store.AppendBatch(chatID, []openai.ChatCompletionMessage{
		userMsg,
		{Role: openai.ChatMessageRoleAssistant, Content: finalText},
	}, snap.cfg.ContextMaxMsgs)

	// Trigger background summarisation when enough overflow has accumulated.
	if snap.cfg.SummaryEnabled && snap.store.OverflowCount(chatID) >= snap.cfg.SummaryMinOverflow {
		go b.summarizeOverflow(chatID)
	}

	// Trigger background user-profile extraction if due.
	if snap.profiles != nil && sender != nil && sender.Username != "" {
		if snap.profiles.ShouldExtract(sender.Username, snap.cfg.ProfileExtractEvery, 2*time.Minute) {
			allMsgs := snap.store.Get(chatID)
			displayName := strings.TrimSpace(sender.FirstName + " " + sender.LastName)
			go b.extractProfile(sender.Username, displayName, allMsgs)
		}
	}

	b.finalizeSpeechReply(chatID, chat, sender, finalText, placeholder)
}

// truncate shortens a string for logging purposes.
func truncate(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "\u2026"
}

// editOrLog edits a Telegram message, falling back to a log on error.
func (b *Bot) editOrLog(msg *tele.Message, text string) {
	// Telegram has a 4096-character limit per message.
	if len([]rune(text)) > 4096 {
		runes := []rune(text)
		text = string(runes[:4093]) + "…"
	}
	tg := b.snapshot().tg
	if _, err := tg.Edit(msg, text); err != nil {
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
	if bot.chatDB != nil {
		defer bot.chatDB.Close()
	}
	if bot.profiles != nil {
		defer bot.profiles.Close()
	}
	if bot.mcpClients != nil {
		defer bot.mcpClients.Close()
	}
	if bot.userTools != nil {
		defer bot.userTools.Close()
	}

	bot.Run()
}
