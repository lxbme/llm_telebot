package app

import (
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"

	openai "github.com/sashabaranov/go-openai"
	tele "gopkg.in/telebot.v3"
)

const (
	adminCancelButtonUnique = "admin_config_cancel"
	adminEmptyValueToken    = "<empty>"
)

type configOption struct {
	Number    int
	EnvKey    string
	Desc      string
	Sensitive bool
	GetValue  func(cfg Config) string
	Apply     func(input string, cfg *Config) error
}

type adminConfigSession struct {
	Step           string
	Selection      int
	PanelChatID    int64
	PanelMessageID int
}

type AdminConfigSessionStore struct {
	mu       sync.RWMutex
	sessions map[int64]adminConfigSession
}

func NewAdminConfigSessionStore() *AdminConfigSessionStore {
	return &AdminConfigSessionStore{
		sessions: make(map[int64]adminConfigSession),
	}
}

func (s *AdminConfigSessionStore) Get(userID int64) (adminConfigSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session, ok := s.sessions[userID]
	return session, ok
}

func (s *AdminConfigSessionStore) Set(userID int64, session adminConfigSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[userID] = session
}

func (s *AdminConfigSessionStore) Clear(userID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, userID)
}

type runtimeSnapshot struct {
	cfg            Config
	ai             *openai.Client
	detectorAI     *openai.Client
	detectorModel  string
	profileAI      *openai.Client
	profileModel   string
	summaryAI      *openai.Client
	summaryModel   string
	stickerAI      *openai.Client
	stickerModel   string
	chatDB         *ChatDB
	store          *HistoryStore
	stats          *StatsManager
	summaries      *SummaryStore
	profiles       *ProfileStore
	tools          *ToolRegistry
	mcpClients     *MCPClientManager
	userTools      *UserToolManager
	tasks          *TaskStore
	scheduleWizard *ScheduleWizardStore
	speechModes    *SpeechModeStore
	tts            *VolcengineTTSClient
	stickers       *StickerEngine
	tg             *tele.Bot
}

func (b *Bot) snapshot() runtimeSnapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return runtimeSnapshot{
		cfg:            b.cfg,
		ai:             b.ai,
		detectorAI:     b.detectorAI,
		detectorModel:  b.detectorModel,
		profileAI:      b.profileAI,
		profileModel:   b.profileModel,
		summaryAI:      b.summaryAI,
		summaryModel:   b.summaryModel,
		stickerAI:      b.stickerAI,
		stickerModel:   b.stickerModel,
		chatDB:         b.chatDB,
		store:          b.store,
		stats:          b.stats,
		summaries:      b.summaries,
		profiles:       b.profiles,
		tools:          b.tools,
		mcpClients:     b.mcpClients,
		userTools:      b.userTools,
		tasks:          b.tasks,
		scheduleWizard: b.scheduleWizard,
		speechModes:    b.speechModes,
		tts:            b.tts,
		stickers:       b.stickers,
		tg:             b.tg,
	}
}

func (b *Bot) currentConfig() Config {
	return b.snapshot().cfg
}

func allConfigOptions() []configOption {
	return []configOption{
		stringOption(1, "OPENAI_API_BASE", "Primary OpenAI-compatible base URL. Leave empty to use the official default.", false,
			func(cfg Config) string { return cfg.OpenAIBase },
			func(cfg *Config, v string) { cfg.OpenAIBase = v }),
		stringOption(2, "OPENAI_API_KEY", "Primary OpenAI API key.", true,
			func(cfg Config) string { return cfg.OpenAIKey },
			func(cfg *Config, v string) { cfg.OpenAIKey = v }),
		stringOption(3, "OPENAI_MODEL", "Primary chat model name.", false,
			func(cfg Config) string { return cfg.OpenAIModel },
			func(cfg *Config, v string) { cfg.OpenAIModel = v }),
		stringOption(4, "TELEGRAM_BOT_TOKEN", "Telegram bot token. Runtime config is updated immediately, but a process restart is still recommended for polling to fully rebind.", true,
			func(cfg Config) string { return cfg.TelegramToken },
			func(cfg *Config, v string) { cfg.TelegramToken = v }),
		stringOption(5, "SYSTEM_PROMPT", "Main system prompt. Multiline input is supported.", false,
			func(cfg Config) string { return cfg.SystemPrompt },
			func(cfg *Config, v string) { cfg.SystemPrompt = v }),
		intOption(6, "CONTEXT_MAX_MESSAGES", "How many messages the sliding context window keeps. Must be greater than 0.", false,
			func(cfg Config) int { return cfg.ContextMaxMsgs },
			func(cfg *Config, v int) { cfg.ContextMaxMsgs = v },
			func(v int) error {
				if v <= 0 {
					return fmt.Errorf("must be greater than 0")
				}
				return nil
			}),
		intOption(7, "MAX_TOKENS", "Maximum output tokens for OpenAI requests. Use 0 for no limit.", false,
			func(cfg Config) int { return cfg.MaxTokens },
			func(cfg *Config, v int) { cfg.MaxTokens = v },
			func(v int) error {
				if v < 0 {
					return fmt.Errorf("cannot be less than 0")
				}
				return nil
			}),
		stringOption(8, "BOT_USERNAME", "Bot username, usually with @. Leave empty to auto-use the current Telegram bot username.", false,
			func(cfg Config) string { return cfg.BotUsername },
			func(cfg *Config, v string) { cfg.BotUsername = v }),
		enumOption(9, "CONTEXT_MODE", "Group context mode: at or global.", false,
			func(cfg Config) string { return cfg.ContextMode },
			func(cfg *Config, v string) { cfg.ContextMode = v },
			map[string]string{"at": "at", "global": "global"}),
		boolOption(10, "AUTO_DETECT", "Enable automatic relevance detection in groups. Accepts true/false, on/off, or 1/0.", false,
			func(cfg Config) bool { return cfg.AutoDetect },
			func(cfg *Config, v bool) { cfg.AutoDetect = v }),
		stringOption(11, "AUTO_DETECT_API_BASE", "Base URL for the auto-detect model. Leave empty to fall back to the primary config.", false,
			func(cfg Config) string { return cfg.AutoDetectBase },
			func(cfg *Config, v string) { cfg.AutoDetectBase = v }),
		stringOption(12, "AUTO_DETECT_API_KEY", "API key for the auto-detect model. Leave empty to fall back to the primary config.", true,
			func(cfg Config) string { return cfg.AutoDetectKey },
			func(cfg *Config, v string) { cfg.AutoDetectKey = v }),
		stringOption(13, "AUTO_DETECT_MODEL", "Model name for auto-detect. Leave empty to fall back to the primary model.", false,
			func(cfg Config) string { return cfg.AutoDetectModel },
			func(cfg *Config, v string) { cfg.AutoDetectModel = v }),
		customOption(14, "ALLOWED_USERS", "Comma-separated Telegram user IDs allowed to access the bot in private chats. Use <empty> to clear.", false,
			func(cfg Config) string { return formatIDSet(cfg.AllowedUsers) },
			func(input string, cfg *Config) error {
				ids, err := parseIDListStrict(strings.TrimSpace(input))
				if err != nil {
					return err
				}
				cfg.AllowedUsers = ids
				return nil
			}),
		customOption(15, "ALLOWED_GROUPS", "Comma-separated Telegram group IDs allowed to access the bot. Use <empty> to clear.", false,
			func(cfg Config) string { return formatIDSet(cfg.AllowedGroups) },
			func(input string, cfg *Config) error {
				ids, err := parseIDListStrict(strings.TrimSpace(input))
				if err != nil {
					return err
				}
				cfg.AllowedGroups = ids
				return nil
			}),
		boolOption(16, "PROFILE_ENABLED", "Enable user profile extraction.", false,
			func(cfg Config) bool { return cfg.ProfileEnabled },
			func(cfg *Config, v bool) { cfg.ProfileEnabled = v }),
		stringOption(17, "PROFILE_DB_PATH", "Path to the user profile database.", false,
			func(cfg Config) string { return cfg.ProfileDBPath },
			func(cfg *Config, v string) { cfg.ProfileDBPath = v }),
		intOption(18, "PROFILE_EXTRACT_EVERY", "Run profile extraction every N bot replies. Must be greater than 0.", false,
			func(cfg Config) int { return cfg.ProfileExtractEvery },
			func(cfg *Config, v int) { cfg.ProfileExtractEvery = v },
			func(v int) error {
				if v <= 0 {
					return fmt.Errorf("must be greater than 0")
				}
				return nil
			}),
		stringOption(19, "PROFILE_API_BASE", "Base URL for the profile model. Leave empty to fall back to the primary config.", false,
			func(cfg Config) string { return cfg.ProfileBase },
			func(cfg *Config, v string) { cfg.ProfileBase = v }),
		stringOption(20, "PROFILE_API_KEY", "API key for the profile model. Leave empty to fall back to the primary config.", true,
			func(cfg Config) string { return cfg.ProfileKey },
			func(cfg *Config, v string) { cfg.ProfileKey = v }),
		stringOption(21, "PROFILE_MODEL", "Model name for profile extraction. Leave empty to fall back to the primary model.", false,
			func(cfg Config) string { return cfg.ProfileModel },
			func(cfg *Config, v string) { cfg.ProfileModel = v }),
		boolOption(22, "SUMMARY_ENABLED", "Enable conversation summaries.", false,
			func(cfg Config) bool { return cfg.SummaryEnabled },
			func(cfg *Config, v bool) { cfg.SummaryEnabled = v }),
		intOption(23, "SUMMARY_MIN_OVERFLOW", "How many overflow messages must accumulate before summarization runs. Must be greater than 0.", false,
			func(cfg Config) int { return cfg.SummaryMinOverflow },
			func(cfg *Config, v int) { cfg.SummaryMinOverflow = v },
			func(v int) error {
				if v <= 0 {
					return fmt.Errorf("must be greater than 0")
				}
				return nil
			}),
		stringOption(24, "SUMMARY_API_BASE", "Base URL for the summary model. Leave empty to fall back to the primary config.", false,
			func(cfg Config) string { return cfg.SummaryBase },
			func(cfg *Config, v string) { cfg.SummaryBase = v }),
		stringOption(25, "SUMMARY_API_KEY", "API key for the summary model. Leave empty to fall back to the primary config.", true,
			func(cfg Config) string { return cfg.SummaryKey },
			func(cfg *Config, v string) { cfg.SummaryKey = v }),
		stringOption(26, "SUMMARY_MODEL", "Model name for summarization. Leave empty to fall back to the primary model.", false,
			func(cfg Config) string { return cfg.SummaryModel },
			func(cfg *Config, v string) { cfg.SummaryModel = v }),
		stringOption(27, "CHAT_DB_PATH", "Path to the persistent chat database.", false,
			func(cfg Config) string { return cfg.ChatDBPath },
			func(cfg *Config, v string) { cfg.ChatDBPath = v }),
		boolOption(28, "TOOLS_ENABLED", "Enable MCP / tool calling.", false,
			func(cfg Config) bool { return cfg.ToolsEnabled },
			func(cfg *Config, v bool) { cfg.ToolsEnabled = v }),
		intOption(29, "TOOLS_MAX_ITERATIONS", "Maximum tool-call rounds per request. Must be greater than 0.", false,
			func(cfg Config) int { return cfg.ToolsMaxIterations },
			func(cfg *Config, v int) { cfg.ToolsMaxIterations = v },
			func(v int) error {
				if v <= 0 {
					return fmt.Errorf("must be greater than 0")
				}
				return nil
			}),
		stringOption(30, "MCP_CONFIG_PATH", "Path to the global MCP config file. Use <empty> to clear it.", false,
			func(cfg Config) string { return cfg.MCPConfigPath },
			func(cfg *Config, v string) { cfg.MCPConfigPath = v }),
		stringOption(31, "USER_MCP_DB_PATH", "Path to the per-user MCP database.", false,
			func(cfg Config) string { return cfg.UserMCPDBPath },
			func(cfg *Config, v string) { cfg.UserMCPDBPath = v }),
		customOption(32, "ADMIN_ID", "Comma-separated admin Telegram user IDs. Use * to allow everyone, or <empty> to clear.", false,
			func(cfg Config) string {
				if cfg.AdminAll {
					return "*"
				}
				return formatIDSet(cfg.AdminIDs)
			},
			func(input string, cfg *Config) error {
				raw := strings.TrimSpace(input)
				cfg.AdminAll = raw == "*"
				if cfg.AdminAll {
					cfg.AdminIDs = nil
					return nil
				}
				ids, err := parseIDListStrict(raw)
				if err != nil {
					return err
				}
				cfg.AdminIDs = ids
				return nil
			}),
		stringOption(33, "VOLCENGINE_TTS_APP_ID", "Volcengine TTS App ID.", false,
			func(cfg Config) string { return cfg.VolcengineTTSAppID },
			func(cfg *Config, v string) { cfg.VolcengineTTSAppID = v }),
		stringOption(34, "VOLCENGINE_TTS_ACCESS_KEY", "Volcengine TTS access key.", true,
			func(cfg Config) string { return cfg.VolcengineTTSAccessKey },
			func(cfg *Config, v string) { cfg.VolcengineTTSAccessKey = v }),
		stringOption(35, "VOLCENGINE_TTS_RESOURCE_ID", "Volcengine TTS resource ID.", false,
			func(cfg Config) string { return cfg.VolcengineTTSResourceID },
			func(cfg *Config, v string) { cfg.VolcengineTTSResourceID = v }),
		stringOption(36, "VOLCENGINE_TTS_SPEAKER", "Volcengine TTS speaker/voice ID.", false,
			func(cfg Config) string { return cfg.VolcengineTTSSpeaker },
			func(cfg *Config, v string) { cfg.VolcengineTTSSpeaker = v }),
		enumOption(37, "VOLCENGINE_TTS_AUDIO_FORMAT", "Volcengine TTS audio format: mp3, wav, or aac.", false,
			func(cfg Config) string { return cfg.VolcengineTTSAudioFormat },
			func(cfg *Config, v string) { cfg.VolcengineTTSAudioFormat = v },
			map[string]string{"mp3": "mp3", "wav": "wav", "aac": "aac"}),
		intOption(38, "VOLCENGINE_TTS_SAMPLE_RATE", "Volcengine TTS sample rate. Must be greater than 0.", false,
			func(cfg Config) int { return cfg.VolcengineTTSSampleRate },
			func(cfg *Config, v int) { cfg.VolcengineTTSSampleRate = v },
			func(v int) error {
				if v <= 0 {
					return fmt.Errorf("must be greater than 0")
				}
				return nil
			}),
		intOption(39, "VOLCENGINE_TTS_SPEECH_RATE", "Volcengine TTS speech rate. Must be between -50 and 100.", false,
			func(cfg Config) int { return cfg.VolcengineTTSSpeechRate },
			func(cfg *Config, v int) { cfg.VolcengineTTSSpeechRate = v },
			func(v int) error {
				if v < -50 || v > 100 {
					return fmt.Errorf("must be between -50 and 100")
				}
				return nil
			}),
		boolOption(40, "VOLCENGINE_TTS_SEND_TEXT", "Whether speech mode should also send the text reply.", false,
			func(cfg Config) bool { return cfg.VolcengineTTSSendText },
			func(cfg *Config, v bool) { cfg.VolcengineTTSSendText = v }),
		boolOption(41, "STICKER_ENABLED", "Enable Telegram sticker strategy after replies.", false,
			func(cfg Config) bool { return cfg.StickerEnabled },
			func(cfg *Config, v bool) { cfg.StickerEnabled = v }),
		enumOption(42, "STICKER_MODE", "Sticker mode: off, append, replace, command_only.", false,
			func(cfg Config) string { return cfg.StickerMode },
			func(cfg *Config, v string) { cfg.StickerMode = v },
			map[string]string{
				"off":          "off",
				"append":       "append",
				"replace":      "replace",
				"command_only": "command_only",
			}),
		stringOption(43, "STICKER_PACK_NAME", "Optional sticker pack alias for business semantics.", false,
			func(cfg Config) string { return cfg.StickerPackName },
			func(cfg *Config, v string) { cfg.StickerPackName = v }),
		stringOption(44, "STICKER_RULES_PATH", "Path to sticker rules JSON file.", false,
			func(cfg Config) string { return cfg.StickerRulesPath },
			func(cfg *Config, v string) { cfg.StickerRulesPath = v }),
		customOption(45, "STICKER_SEND_PROBABILITY", "Probability (0~1) for auto sticker send in append/replace mode.", false,
			func(cfg Config) string { return strconv.FormatFloat(cfg.StickerSendProbability, 'f', -1, 64) },
			func(input string, cfg *Config) error {
				value := normalizeConfigTextInput(input)
				p, err := strconv.ParseFloat(value, 64)
				if err != nil {
					return fmt.Errorf("please enter a decimal number between 0 and 1")
				}
				if p < 0 || p > 1 {
					return fmt.Errorf("must be between 0 and 1")
				}
				cfg.StickerSendProbability = p
				return nil
			}),
		intOption(46, "STICKER_MAX_PER_REPLY", "Maximum stickers sent per reply. Must be greater than 0.", false,
			func(cfg Config) int { return cfg.StickerMaxPerReply },
			func(cfg *Config, v int) { cfg.StickerMaxPerReply = v },
			func(v int) error {
				if v <= 0 {
					return fmt.Errorf("must be greater than 0")
				}
				return nil
			}),
		boolOption(47, "STICKER_WITH_SPEECH", "Allow stickers when speech mode is enabled for this chat.", false,
			func(cfg Config) bool { return cfg.StickerWithSpeech },
			func(cfg *Config, v bool) { cfg.StickerWithSpeech = v }),
		customOption(48, "STICKER_ALLOWED_CHATS", "Comma-separated chat IDs allowed to use sticker strategy. Use <empty> to allow all chats.", false,
			func(cfg Config) string { return formatIDSet(cfg.StickerAllowedChats) },
			func(input string, cfg *Config) error {
				ids, err := parseIDListStrict(strings.TrimSpace(input))
				if err != nil {
					return err
				}
				cfg.StickerAllowedChats = ids
				return nil
			}),
		boolOption(49, "STICKER_MODEL_ENABLED", "Enable model-assisted sticker label selection when rule matching misses.", false,
			func(cfg Config) bool { return cfg.StickerModelEnabled },
			func(cfg *Config, v bool) { cfg.StickerModelEnabled = v }),
		stringOption(50, "STICKER_MODEL_BASE", "Base URL for sticker strategy model. Leave empty to fall back to primary config.", false,
			func(cfg Config) string { return cfg.StickerModelBase },
			func(cfg *Config, v string) { cfg.StickerModelBase = v }),
		stringOption(51, "STICKER_MODEL_KEY", "API key for sticker strategy model. Leave empty to fall back to primary config.", true,
			func(cfg Config) string { return cfg.StickerModelKey },
			func(cfg *Config, v string) { cfg.StickerModelKey = v }),
		stringOption(52, "STICKER_MODEL", "Model name for sticker strategy. Leave empty to fall back to primary model.", false,
			func(cfg Config) string { return cfg.StickerModel },
			func(cfg *Config, v string) { cfg.StickerModel = v }),
	}
}

func stringOption(number int, envKey, desc string, sensitive bool, getter func(Config) string, setter func(*Config, string)) configOption {
	return customOption(number, envKey, desc, sensitive, getter, func(input string, cfg *Config) error {
		value := normalizeConfigTextInput(input)
		if envKey == "OPENAI_API_KEY" || envKey == "OPENAI_MODEL" || envKey == "CHAT_DB_PATH" || envKey == "USER_MCP_DB_PATH" || envKey == "STICKER_RULES_PATH" {
			if value == "" {
				return fmt.Errorf("cannot be empty")
			}
		}
		if envKey == "PROFILE_DB_PATH" && cfg.ProfileEnabled && value == "" {
			return fmt.Errorf("cannot be empty")
		}
		setter(cfg, value)
		return nil
	})
}

func boolOption(number int, envKey, desc string, sensitive bool, getter func(Config) bool, setter func(*Config, bool)) configOption {
	return customOption(number, envKey, desc, sensitive,
		func(cfg Config) string { return strconv.FormatBool(getter(cfg)) },
		func(input string, cfg *Config) error {
			v, err := parseFlexibleBool(normalizeConfigTextInput(input))
			if err != nil {
				return err
			}
			setter(cfg, v)
			return nil
		})
}

func intOption(number int, envKey, desc string, sensitive bool, getter func(Config) int, setter func(*Config, int), validate func(int) error) configOption {
	return customOption(number, envKey, desc, sensitive,
		func(cfg Config) string { return strconv.Itoa(getter(cfg)) },
		func(input string, cfg *Config) error {
			value := normalizeConfigTextInput(input)
			n, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("please enter an integer")
			}
			if err := validate(n); err != nil {
				return err
			}
			setter(cfg, n)
			return nil
		})
}

func enumOption(number int, envKey, desc string, sensitive bool, getter func(Config) string, setter func(*Config, string), allowed map[string]string) configOption {
	return customOption(number, envKey, desc, sensitive, getter, func(input string, cfg *Config) error {
		value := strings.ToLower(normalizeConfigTextInput(input))
		normalized, ok := allowed[value]
		if !ok {
			keys := make([]string, 0, len(allowed))
			for k := range allowed {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			return fmt.Errorf("allowed values: %s", strings.Join(keys, ", "))
		}
		setter(cfg, normalized)
		return nil
	})
}

func customOption(number int, envKey, desc string, sensitive bool, getter func(Config) string, apply func(string, *Config) error) configOption {
	return configOption{
		Number:    number,
		EnvKey:    envKey,
		Desc:      desc,
		Sensitive: sensitive,
		GetValue:  getter,
		Apply:     apply,
	}
}

func normalizeConfigTextInput(input string) string {
	raw := strings.TrimSpace(input)
	if strings.EqualFold(raw, adminEmptyValueToken) {
		return ""
	}
	return raw
}

func parseIDListStrict(raw string) (map[int64]bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	set := make(map[int64]bool)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("the ID list cannot contain empty items")
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid ID: %q", part)
		}
		set[id] = true
	}
	return set, nil
}

func parseFlexibleBool(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "on", "yes", "y", "enable", "enabled", "开启", "开":
		return true, nil
	case "0", "false", "off", "no", "n", "disable", "disabled", "关闭", "关":
		return false, nil
	default:
		return false, fmt.Errorf("please enter true/false, on/off, or 1/0")
	}
}

func findConfigOption(number int) (configOption, bool) {
	for _, option := range allConfigOptions() {
		if option.Number == number {
			return option, true
		}
	}
	return configOption{}, false
}

func formatIDSet(set map[int64]bool) string {
	if len(set) == 0 {
		return ""
	}
	ids := make([]int64, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, ",")
}

func previewConfigValue(option configOption, cfg Config) string {
	raw := option.GetValue(cfg)
	if option.Sensitive {
		return maskSecret(raw)
	}
	if raw == "" {
		return "(empty)"
	}
	raw = strings.ReplaceAll(raw, "\n", "\\n")
	runes := []rune(raw)
	if len(runes) > 80 {
		return string(runes[:80]) + "…"
	}
	return raw
}

func maskSecret(value string) string {
	if strings.TrimSpace(value) == "" {
		return "(empty)"
	}
	runes := []rune(value)
	if len(runes) <= 8 {
		return strings.Repeat("*", len(runes))
	}
	return string(runes[:3]) + strings.Repeat("*", len(runes)-6) + string(runes[len(runes)-3:])
}

func (b *Bot) adminCancelMarkup() *tele.ReplyMarkup {
	menu := &tele.ReplyMarkup{}
	cancel := menu.Data("cancel", adminCancelButtonUnique)
	menu.Inline(menu.Row(cancel))
	return menu
}

func (b *Bot) adminSendOptions(parseMode tele.ParseMode) *tele.SendOptions {
	return &tele.SendOptions{
		ReplyMarkup: b.adminCancelMarkup(),
		ParseMode:   parseMode,
	}
}

func (b *Bot) adminMessageRef(session adminConfigSession) *tele.Message {
	if session.PanelChatID == 0 || session.PanelMessageID == 0 {
		return nil
	}
	return &tele.Message{
		ID:   session.PanelMessageID,
		Chat: &tele.Chat{ID: session.PanelChatID, Type: tele.ChatPrivate},
	}
}

func (b *Bot) renderAdminPanel(c tele.Context, session adminConfigSession, text string, opts *tele.SendOptions) (adminConfigSession, error) {
	if opts == nil {
		opts = &tele.SendOptions{}
	}

	snap := b.snapshot()
	if msgRef := b.adminMessageRef(session); msgRef != nil {
		msg, err := snap.tg.Edit(msgRef, text, opts)
		if err == nil {
			if msg != nil {
				session.PanelMessageID = msg.ID
				if msg.Chat != nil {
					session.PanelChatID = msg.Chat.ID
				}
			}
			return session, nil
		}
		log.Printf("[admin] edit panel failed, sending a new one: %v", err)
	}

	msg, err := snap.tg.Send(c.Chat(), text, opts)
	if err != nil {
		return session, err
	}
	session.PanelMessageID = msg.ID
	if msg.Chat != nil {
		session.PanelChatID = msg.Chat.ID
	} else if c.Chat() != nil {
		session.PanelChatID = c.Chat().ID
	}
	return session, nil
}

func (b *Bot) deleteAdminInputMessage(msg *tele.Message) {
	if msg == nil {
		return
	}
	snap := b.snapshot()
	if err := snap.tg.Delete(msg); err != nil {
		log.Printf("[admin] failed to delete input message %d: %v", msg.ID, err)
	}
}

func (b *Bot) buildAdminListText(cfg Config) string {
	var sb strings.Builder
	sb.WriteString("🔧 Admin Config Panel\n\n")
	sb.WriteString(fmt.Sprintf("💾 Config file: %s\n", effectiveConfigFilePath(cfg)))
	sb.WriteString("🧭 Load order: config.yaml -> environment overrides\n\n")
	sb.WriteString("🔢 Reply with an item number to edit it.\n")
	sb.WriteString("↩️ Tap cancel to exit.\n\n")
	for _, option := range allConfigOptions() {
		sb.WriteString(fmt.Sprintf("%02d. %s = %s\n", option.Number, option.EnvKey, previewConfigValue(option, cfg)))
	}
	return strings.TrimSpace(sb.String())
}

func (b *Bot) buildAdminEditPrompt(option configOption, cfg Config) string {
	return fmt.Sprintf(
		"✏️ Editing: %s\n\n📌 Current value: %s\nℹ️ Description: %s\n\n📝 Send the new value directly.\n🧹 To clear a string or list, send %s.\n↩️ Tap cancel to go back to the list.",
		option.EnvKey,
		previewConfigValue(option, cfg),
		option.Desc,
		adminEmptyValueToken,
	)
}

func (b *Bot) ensureAdminPrivateChat(c tele.Context) (bool, error) {
	if !b.isAllowed(c) {
		if c.Sender() != nil && c.Chat() != nil {
			log.Printf("Access denied for /admin: user=%d chat=%d", c.Sender().ID, c.Chat().ID)
		}
		return false, nil
	}
	if c.Chat() == nil || c.Chat().Type != tele.ChatPrivate {
		return false, c.Reply("🔒 `/admin` is only available in a private admin chat.")
	}
	if c.Sender() == nil || !b.isAdmin(c.Sender().ID) {
		return false, c.Reply("⛔ Only admins can use this feature.")
	}
	return true, nil
}

func (b *Bot) handleAdminCommand(c tele.Context) error {
	if ok, err := b.ensureAdminPrivateChat(c); !ok || err != nil {
		return err
	}
	cfg := b.currentConfig()
	session, _ := b.adminSessions.Get(c.Sender().ID)
	session.Step = "select"
	session.Selection = 0
	rendered, err := b.renderAdminPanel(c, session, b.buildAdminListText(cfg), b.adminSendOptions(tele.ModeDefault))
	if err != nil {
		return err
	}
	b.adminSessions.Set(c.Sender().ID, rendered)
	return nil
}

func (b *Bot) handleAdminCancel(c tele.Context) error {
	if cb := c.Callback(); cb != nil {
		_ = c.Respond()
	}
	if ok, err := b.ensureAdminPrivateChat(c); !ok || err != nil {
		return err
	}
	session, ok := b.adminSessions.Get(c.Sender().ID)
	if !ok {
		return c.EditOrReply("ℹ️ No active config session.")
	}
	if session.Step == "edit" {
		cfg := b.currentConfig()
		session.Step = "select"
		session.Selection = 0
		rendered, err := b.renderAdminPanel(c, session, b.buildAdminListText(cfg), b.adminSendOptions(tele.ModeDefault))
		if err != nil {
			return err
		}
		b.adminSessions.Set(c.Sender().ID, rendered)
		return nil
	}
	b.adminSessions.Clear(c.Sender().ID)
	if _, err := b.renderAdminPanel(c, session, "👋 Exited the admin config panel.", &tele.SendOptions{}); err != nil {
		return err
	}
	return nil
}

func (b *Bot) handleAdminTextIfNeeded(c tele.Context, text string) (bool, error) {
	if !b.isAllowed(c) || c.Chat() == nil || c.Chat().Type != tele.ChatPrivate || c.Sender() == nil || !b.isAdmin(c.Sender().ID) {
		return false, nil
	}
	session, ok := b.adminSessions.Get(c.Sender().ID)
	if !ok {
		return false, nil
	}
	shouldDeleteInput := false
	defer func() {
		if shouldDeleteInput {
			b.deleteAdminInputMessage(c.Message())
		}
	}()

	switch session.Step {
	case "select":
		number, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil {
			rendered, renderErr := b.renderAdminPanel(c, session, "🔢 Please send a config item number.\n\n"+b.buildAdminListText(b.currentConfig()), b.adminSendOptions(tele.ModeDefault))
			if renderErr == nil {
				b.adminSessions.Set(c.Sender().ID, rendered)
				shouldDeleteInput = true
			}
			return true, renderErr
		}
		option, found := findConfigOption(number)
		if !found {
			rendered, renderErr := b.renderAdminPanel(c, session, "❓ Unknown config item number. Please try again.\n\n"+b.buildAdminListText(b.currentConfig()), b.adminSendOptions(tele.ModeDefault))
			if renderErr == nil {
				b.adminSessions.Set(c.Sender().ID, rendered)
				shouldDeleteInput = true
			}
			return true, renderErr
		}
		session.Step = "edit"
		session.Selection = number
		rendered, renderErr := b.renderAdminPanel(c, session, b.buildAdminEditPrompt(option, b.currentConfig()), b.adminSendOptions(tele.ModeDefault))
		if renderErr == nil {
			b.adminSessions.Set(c.Sender().ID, rendered)
			shouldDeleteInput = true
		}
		return true, renderErr

	case "edit":
		option, found := findConfigOption(session.Selection)
		if !found {
			session.Step = "select"
			session.Selection = 0
			rendered, renderErr := b.renderAdminPanel(c, session, "⚠️ That config item no longer exists. Back to the list.\n\n"+b.buildAdminListText(b.currentConfig()), b.adminSendOptions(tele.ModeDefault))
			if renderErr == nil {
				b.adminSessions.Set(c.Sender().ID, rendered)
				shouldDeleteInput = true
			}
			return true, renderErr
		}

		next := b.currentConfig()
		if err := option.Apply(text, &next); err != nil {
			rendered, renderErr := b.renderAdminPanel(c, session, fmt.Sprintf("❌ Invalid input: %v\n\n%s", err, b.buildAdminEditPrompt(option, b.currentConfig())), b.adminSendOptions(tele.ModeDefault))
			if renderErr == nil {
				b.adminSessions.Set(c.Sender().ID, rendered)
				shouldDeleteInput = true
			}
			return true, renderErr
		}
		if err := b.applyAndPersistConfig(next); err != nil {
			rendered, renderErr := b.renderAdminPanel(c, session, fmt.Sprintf("❌ Failed to apply config: %v\n\n%s", err, b.buildAdminEditPrompt(option, b.currentConfig())), b.adminSendOptions(tele.ModeDefault))
			if renderErr == nil {
				b.adminSessions.Set(c.Sender().ID, rendered)
				shouldDeleteInput = true
			}
			return true, renderErr
		}

		updated := b.currentConfig()
		session.Step = "select"
		session.Selection = 0
		rendered, renderErr := b.renderAdminPanel(c, session, fmt.Sprintf("✅ Updated: %s\n📌 Current value: %s%s\n\n%s", option.EnvKey, previewConfigValue(option, updated), configOverrideNotice(option.EnvKey), b.buildAdminListText(updated)), b.adminSendOptions(tele.ModeDefault))
		if renderErr == nil {
			b.adminSessions.Set(c.Sender().ID, rendered)
			shouldDeleteInput = true
		}
		return true, renderErr
	}

	return false, nil
}

func buildOpenAIClient(apiKey, baseURL string) (*openai.Client, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY cannot be empty")
	}
	cfg := openai.DefaultConfig(apiKey)
	if strings.TrimSpace(baseURL) != "" {
		cfg.BaseURL = strings.TrimSpace(baseURL)
	}
	return openai.NewClientWithConfig(cfg), nil
}

func buildAIResources(cfg Config) (mainAI, detectorAI, profileAI, summaryAI, stickerAI *openai.Client, detectorModel, profileModel, summaryModel, stickerModel string, err error) {
	if strings.TrimSpace(cfg.OpenAIModel) == "" {
		err = fmt.Errorf("OPENAI_MODEL cannot be empty")
		return
	}

	mainAI, err = buildOpenAIClient(cfg.OpenAIKey, cfg.OpenAIBase)
	if err != nil {
		return
	}

	detectorAI = mainAI
	detectorModel = cfg.OpenAIModel
	if cfg.AutoDetectKey != "" || cfg.AutoDetectBase != "" || cfg.AutoDetectModel != "" {
		detectorAI, err = buildOpenAIClient(firstNonEmpty(cfg.AutoDetectKey, cfg.OpenAIKey), firstNonEmpty(cfg.AutoDetectBase, cfg.OpenAIBase))
		if err != nil {
			err = fmt.Errorf("AUTO_DETECT configuration is invalid: %w", err)
			return
		}
		detectorModel = firstNonEmpty(cfg.AutoDetectModel, cfg.OpenAIModel)
	}

	profileAI = mainAI
	profileModel = cfg.OpenAIModel
	if cfg.ProfileKey != "" || cfg.ProfileBase != "" || cfg.ProfileModel != "" {
		profileAI, err = buildOpenAIClient(firstNonEmpty(cfg.ProfileKey, cfg.OpenAIKey), firstNonEmpty(cfg.ProfileBase, cfg.OpenAIBase))
		if err != nil {
			err = fmt.Errorf("PROFILE configuration is invalid: %w", err)
			return
		}
		profileModel = firstNonEmpty(cfg.ProfileModel, cfg.OpenAIModel)
	}

	summaryAI = mainAI
	summaryModel = cfg.OpenAIModel
	if cfg.SummaryKey != "" || cfg.SummaryBase != "" || cfg.SummaryModel != "" {
		summaryAI, err = buildOpenAIClient(firstNonEmpty(cfg.SummaryKey, cfg.OpenAIKey), firstNonEmpty(cfg.SummaryBase, cfg.OpenAIBase))
		if err != nil {
			err = fmt.Errorf("SUMMARY configuration is invalid: %w", err)
			return
		}
		summaryModel = firstNonEmpty(cfg.SummaryModel, cfg.OpenAIModel)
	}

	stickerAI = mainAI
	stickerModel = cfg.OpenAIModel
	if cfg.StickerModelKey != "" || cfg.StickerModelBase != "" || cfg.StickerModel != "" {
		stickerAI, err = buildOpenAIClient(firstNonEmpty(cfg.StickerModelKey, cfg.OpenAIKey), firstNonEmpty(cfg.StickerModelBase, cfg.OpenAIBase))
		if err != nil {
			err = fmt.Errorf("STICKER_MODEL configuration is invalid: %w", err)
			return
		}
		stickerModel = firstNonEmpty(cfg.StickerModel, cfg.OpenAIModel)
	}

	return
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func buildToolResources(cfg Config) (*ToolRegistry, *MCPClientManager, *UserToolManager, error) {
	if !cfg.ToolsEnabled {
		return nil, nil, nil, nil
	}
	if strings.TrimSpace(cfg.UserMCPDBPath) == "" {
		return nil, nil, nil, fmt.Errorf("USER_MCP_DB_PATH cannot be empty")
	}

	registry := NewToolRegistry()
	var mcpManager *MCPClientManager
	if strings.TrimSpace(cfg.MCPConfigPath) != "" {
		mcpCfg, err := LoadMCPConfig(cfg.MCPConfigPath)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to load MCP_CONFIG_PATH: %w", err)
		}
		mcpManager = NewMCPClientManager()
		failures := mcpManager.ConnectAll(mcpCfg, registry)
		for name, ferr := range failures {
			log.Printf("[config] global MCP %q reconnect failed: %v", name, ferr)
		}
	}

	store, err := NewUserMCPStore(cfg.UserMCPDBPath)
	if err != nil {
		if mcpManager != nil {
			mcpManager.Close()
		}
		return nil, nil, nil, fmt.Errorf("failed to initialize USER_MCP_DB_PATH: %w", err)
	}
	userTools := NewUserToolManager(store)
	userTools.RestoreAll()
	return registry, mcpManager, userTools, nil
}

func (b *Bot) persistRuntimeChatState(db *ChatDB) {
	if db == nil {
		return
	}

	snap := b.snapshot()
	if snap.store != nil {
		snap.store.mu.RLock()
		historyCopy := make(map[int64][]openai.ChatCompletionMessage, len(snap.store.history))
		overflowCopy := make(map[int64][]openai.ChatCompletionMessage, len(snap.store.overflow))
		for chatID, messages := range snap.store.history {
			cloned := make([]openai.ChatCompletionMessage, len(messages))
			copy(cloned, messages)
			historyCopy[chatID] = cloned
		}
		for chatID, messages := range snap.store.overflow {
			cloned := make([]openai.ChatCompletionMessage, len(messages))
			copy(cloned, messages)
			overflowCopy[chatID] = cloned
		}
		snap.store.mu.RUnlock()

		for chatID, messages := range historyCopy {
			db.SaveHistory(chatID, messages)
		}
		for chatID, messages := range overflowCopy {
			db.SaveOverflow(chatID, messages)
		}
	}

	if snap.summaries != nil {
		snap.summaries.mu.RLock()
		summaryCopy := make(map[int64]string, len(snap.summaries.summaries))
		for chatID, summary := range snap.summaries.summaries {
			summaryCopy[chatID] = summary
		}
		snap.summaries.mu.RUnlock()

		for chatID, summary := range summaryCopy {
			db.SaveSummary(chatID, summary)
		}
	}

	if snap.tasks != nil {
		snap.tasks.mu.RLock()
		taskCopy := make(map[int64][]ScheduledTask, len(snap.tasks.tasks))
		for chatID, chatTasks := range snap.tasks.tasks {
			items := make([]ScheduledTask, 0, len(chatTasks))
			for _, task := range chatTasks {
				items = append(items, task)
			}
			taskCopy[chatID] = items
		}
		snap.tasks.mu.RUnlock()

		for chatID, tasks := range taskCopy {
			db.SaveSchedules(chatID, tasks)
		}
	}
}

func (b *Bot) applyRuntimeConfig(next Config) error {
	current := b.snapshot()

	mainAI, detectorAI, profileAI, summaryAI, stickerAI, detectorModel, profileModel, summaryModel, stickerModel, err := buildAIResources(next)
	if err != nil {
		return err
	}

	var newChatDB *ChatDB
	chatDBChanged := strings.TrimSpace(next.ChatDBPath) != strings.TrimSpace(current.cfg.ChatDBPath)
	if strings.TrimSpace(next.ChatDBPath) == "" {
		return fmt.Errorf("CHAT_DB_PATH cannot be empty")
	}
	if chatDBChanged {
		newChatDB, err = OpenChatDB(next.ChatDBPath)
		if err != nil {
			return fmt.Errorf("failed to open CHAT_DB_PATH: %w", err)
		}
	} else {
		newChatDB = current.chatDB
	}

	var newProfiles *ProfileStore
	profilesChanged := next.ProfileEnabled != current.cfg.ProfileEnabled || strings.TrimSpace(next.ProfileDBPath) != strings.TrimSpace(current.cfg.ProfileDBPath)
	if next.ProfileEnabled {
		if strings.TrimSpace(next.ProfileDBPath) == "" {
			if newChatDB != nil && chatDBChanged {
				_ = newChatDB.Close()
			}
			return fmt.Errorf("PROFILE_DB_PATH cannot be empty")
		}
		if profilesChanged {
			newProfiles, err = NewProfileStore(next.ProfileDBPath)
			if err != nil {
				if newChatDB != nil && chatDBChanged {
					_ = newChatDB.Close()
				}
				return fmt.Errorf("failed to open PROFILE_DB_PATH: %w", err)
			}
		} else {
			newProfiles = current.profiles
		}
	}

	toolsChanged := next.ToolsEnabled != current.cfg.ToolsEnabled ||
		strings.TrimSpace(next.MCPConfigPath) != strings.TrimSpace(current.cfg.MCPConfigPath) ||
		strings.TrimSpace(next.UserMCPDBPath) != strings.TrimSpace(current.cfg.UserMCPDBPath)
	newTools := current.tools
	newMCPClients := current.mcpClients
	newUserTools := current.userTools
	if toolsChanged {
		newTools, newMCPClients, newUserTools, err = buildToolResources(next)
		if err != nil {
			if newProfiles != nil && profilesChanged {
				_ = newProfiles.Close()
			}
			if newChatDB != nil && chatDBChanged {
				_ = newChatDB.Close()
			}
			return err
		}
	}

	newTTS := NewVolcengineTTSClient(next)
	newStickerEngine, err := NewStickerEngine(next.StickerRulesPath)
	if err != nil {
		if newProfiles != nil && profilesChanged {
			_ = newProfiles.Close()
		}
		if newChatDB != nil && chatDBChanged {
			_ = newChatDB.Close()
		}
		return fmt.Errorf("failed to load STICKER_RULES_PATH: %w", err)
	}

	b.mu.Lock()
	oldChatDB := b.chatDB
	oldProfiles := b.profiles
	oldMCPClients := b.mcpClients
	oldUserTools := b.userTools

	b.cfg = next
	b.ai = mainAI
	b.detectorAI = detectorAI
	b.detectorModel = detectorModel
	b.profileAI = profileAI
	b.profileModel = profileModel
	b.summaryAI = summaryAI
	b.summaryModel = summaryModel
	b.stickerAI = stickerAI
	b.stickerModel = stickerModel
	b.tts = newTTS
	b.stickers = newStickerEngine

	if chatDBChanged {
		b.chatDB = newChatDB
		if b.store != nil {
			b.store.db = newChatDB
		}
		if b.stats != nil {
			b.stats.RebindDB(newChatDB)
		}
		if b.summaries != nil {
			b.summaries.db = newChatDB
		}
		if b.tasks != nil {
			b.tasks.RebindDB(newChatDB)
		}
	}

	if profilesChanged {
		b.profiles = newProfiles
	}

	if toolsChanged {
		b.tools = newTools
		b.mcpClients = newMCPClients
		b.userTools = newUserTools
	}
	b.mu.Unlock()

	if chatDBChanged {
		b.persistRuntimeChatState(newChatDB)
		if b.stats != nil {
			_ = b.stats.Flush()
		}
		if oldChatDB != nil {
			_ = oldChatDB.Close()
		}
	}
	if profilesChanged && oldProfiles != nil && oldProfiles != newProfiles {
		_ = oldProfiles.Close()
	}
	if toolsChanged {
		if oldMCPClients != nil && oldMCPClients != newMCPClients {
			oldMCPClients.Close()
		}
		if oldUserTools != nil && oldUserTools != newUserTools {
			oldUserTools.Close()
		}
	}

	log.Printf("[config] runtime config updated")
	b.recordDashboardEvent(DashboardEvent{
		Type:    DashboardEventConfigReloaded,
		Summary: "runtime config updated",
		Success: true,
	})
	return nil
}
