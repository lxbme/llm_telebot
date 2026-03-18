package main

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
	Step      string
	Selection int
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
	cfg           Config
	ai            *openai.Client
	detectorAI    *openai.Client
	detectorModel string
	profileAI     *openai.Client
	profileModel  string
	summaryAI     *openai.Client
	summaryModel  string
	chatDB        *ChatDB
	store         *HistoryStore
	summaries     *SummaryStore
	profiles      *ProfileStore
	tools         *ToolRegistry
	mcpClients    *MCPClientManager
	userTools     *UserToolManager
	speechModes   *SpeechModeStore
	tts           *VolcengineTTSClient
	tg            *tele.Bot
}

func (b *Bot) snapshot() runtimeSnapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return runtimeSnapshot{
		cfg:           b.cfg,
		ai:            b.ai,
		detectorAI:    b.detectorAI,
		detectorModel: b.detectorModel,
		profileAI:     b.profileAI,
		profileModel:  b.profileModel,
		summaryAI:     b.summaryAI,
		summaryModel:  b.summaryModel,
		chatDB:        b.chatDB,
		store:         b.store,
		summaries:     b.summaries,
		profiles:      b.profiles,
		tools:         b.tools,
		mcpClients:    b.mcpClients,
		userTools:     b.userTools,
		speechModes:   b.speechModes,
		tts:           b.tts,
		tg:            b.tg,
	}
}

func (b *Bot) currentConfig() Config {
	return b.snapshot().cfg
}

func allConfigOptions() []configOption {
	return []configOption{
		stringOption(1, "OPENAI_API_BASE", "主 OpenAI Base URL，留空表示官方默认地址。", false,
			func(cfg Config) string { return cfg.OpenAIBase },
			func(cfg *Config, v string) { cfg.OpenAIBase = v }),
		stringOption(2, "OPENAI_API_KEY", "主 OpenAI API Key。", true,
			func(cfg Config) string { return cfg.OpenAIKey },
			func(cfg *Config, v string) { cfg.OpenAIKey = v }),
		stringOption(3, "OPENAI_MODEL", "主对话模型名称。", false,
			func(cfg Config) string { return cfg.OpenAIModel },
			func(cfg *Config, v string) { cfg.OpenAIModel = v }),
		stringOption(4, "TELEGRAM_BOT_TOKEN", "Telegram Bot Token。更新后会写入运行时配置，但轮询连接仍建议手动重启进程后完全生效。", true,
			func(cfg Config) string { return cfg.TelegramToken },
			func(cfg *Config, v string) { cfg.TelegramToken = v }),
		stringOption(5, "SYSTEM_PROMPT", "主系统提示词，可多行。", false,
			func(cfg Config) string { return cfg.SystemPrompt },
			func(cfg *Config, v string) { cfg.SystemPrompt = v }),
		intOption(6, "CONTEXT_MAX_MESSAGES", "上下文窗口保留的消息数，必须大于 0。", false,
			func(cfg Config) int { return cfg.ContextMaxMsgs },
			func(cfg *Config, v int) { cfg.ContextMaxMsgs = v },
			func(v int) error {
				if v <= 0 {
					return fmt.Errorf("必须大于 0")
				}
				return nil
			}),
		intOption(7, "MAX_TOKENS", "OpenAI 最大输出 token，0 表示不限制。", false,
			func(cfg Config) int { return cfg.MaxTokens },
			func(cfg *Config, v int) { cfg.MaxTokens = v },
			func(v int) error {
				if v < 0 {
					return fmt.Errorf("不能小于 0")
				}
				return nil
			}),
		stringOption(8, "BOT_USERNAME", "机器人用户名，通常带 @。留空时自动使用当前 Bot 用户名。", false,
			func(cfg Config) string { return cfg.BotUsername },
			func(cfg *Config, v string) { cfg.BotUsername = v }),
		enumOption(9, "CONTEXT_MODE", "群聊上下文模式：at 或 global。", false,
			func(cfg Config) string { return cfg.ContextMode },
			func(cfg *Config, v string) { cfg.ContextMode = v },
			map[string]string{"at": "at", "global": "global"}),
		boolOption(10, "AUTO_DETECT", "是否启用群聊自动相关性判断。可输入 true/false、on/off、1/0。", false,
			func(cfg Config) bool { return cfg.AutoDetect },
			func(cfg *Config, v bool) { cfg.AutoDetect = v }),
		stringOption(11, "AUTO_DETECT_API_BASE", "自动判定模型的 Base URL，留空则回退到主配置。", false,
			func(cfg Config) string { return cfg.AutoDetectBase },
			func(cfg *Config, v string) { cfg.AutoDetectBase = v }),
		stringOption(12, "AUTO_DETECT_API_KEY", "自动判定模型的 API Key，留空则回退到主配置。", true,
			func(cfg Config) string { return cfg.AutoDetectKey },
			func(cfg *Config, v string) { cfg.AutoDetectKey = v }),
		stringOption(13, "AUTO_DETECT_MODEL", "自动判定模型名称，留空则回退到主模型。", false,
			func(cfg Config) string { return cfg.AutoDetectModel },
			func(cfg *Config, v string) { cfg.AutoDetectModel = v }),
		customOption(14, "ALLOWED_USERS", "允许私聊访问机器人的用户 ID 列表，逗号分隔；可用 <empty> 清空。", false,
			func(cfg Config) string { return formatIDSet(cfg.AllowedUsers) },
			func(input string, cfg *Config) error {
				ids, err := parseIDListStrict(strings.TrimSpace(input))
				if err != nil {
					return err
				}
				cfg.AllowedUsers = ids
				return nil
			}),
		customOption(15, "ALLOWED_GROUPS", "允许群聊访问机器人的群组 ID 列表，逗号分隔；可用 <empty> 清空。", false,
			func(cfg Config) string { return formatIDSet(cfg.AllowedGroups) },
			func(input string, cfg *Config) error {
				ids, err := parseIDListStrict(strings.TrimSpace(input))
				if err != nil {
					return err
				}
				cfg.AllowedGroups = ids
				return nil
			}),
		boolOption(16, "PROFILE_ENABLED", "是否启用用户画像抽取。", false,
			func(cfg Config) bool { return cfg.ProfileEnabled },
			func(cfg *Config, v bool) { cfg.ProfileEnabled = v }),
		stringOption(17, "PROFILE_DB_PATH", "用户画像数据库路径。", false,
			func(cfg Config) string { return cfg.ProfileDBPath },
			func(cfg *Config, v string) { cfg.ProfileDBPath = v }),
		intOption(18, "PROFILE_EXTRACT_EVERY", "每 N 次回复触发一次画像抽取，必须大于 0。", false,
			func(cfg Config) int { return cfg.ProfileExtractEvery },
			func(cfg *Config, v int) { cfg.ProfileExtractEvery = v },
			func(v int) error {
				if v <= 0 {
					return fmt.Errorf("必须大于 0")
				}
				return nil
			}),
		stringOption(19, "PROFILE_API_BASE", "画像抽取模型的 Base URL，留空则回退到主配置。", false,
			func(cfg Config) string { return cfg.ProfileBase },
			func(cfg *Config, v string) { cfg.ProfileBase = v }),
		stringOption(20, "PROFILE_API_KEY", "画像抽取模型的 API Key，留空则回退到主配置。", true,
			func(cfg Config) string { return cfg.ProfileKey },
			func(cfg *Config, v string) { cfg.ProfileKey = v }),
		stringOption(21, "PROFILE_MODEL", "画像抽取模型名称，留空则回退到主模型。", false,
			func(cfg Config) string { return cfg.ProfileModel },
			func(cfg *Config, v string) { cfg.ProfileModel = v }),
		boolOption(22, "SUMMARY_ENABLED", "是否启用对话摘要。", false,
			func(cfg Config) bool { return cfg.SummaryEnabled },
			func(cfg *Config, v bool) { cfg.SummaryEnabled = v }),
		intOption(23, "SUMMARY_MIN_OVERFLOW", "累计多少条溢出消息后触发摘要，必须大于 0。", false,
			func(cfg Config) int { return cfg.SummaryMinOverflow },
			func(cfg *Config, v int) { cfg.SummaryMinOverflow = v },
			func(v int) error {
				if v <= 0 {
					return fmt.Errorf("必须大于 0")
				}
				return nil
			}),
		stringOption(24, "SUMMARY_API_BASE", "摘要模型的 Base URL，留空则回退到主配置。", false,
			func(cfg Config) string { return cfg.SummaryBase },
			func(cfg *Config, v string) { cfg.SummaryBase = v }),
		stringOption(25, "SUMMARY_API_KEY", "摘要模型的 API Key，留空则回退到主配置。", true,
			func(cfg Config) string { return cfg.SummaryKey },
			func(cfg *Config, v string) { cfg.SummaryKey = v }),
		stringOption(26, "SUMMARY_MODEL", "摘要模型名称，留空则回退到主模型。", false,
			func(cfg Config) string { return cfg.SummaryModel },
			func(cfg *Config, v string) { cfg.SummaryModel = v }),
		stringOption(27, "CHAT_DB_PATH", "聊天历史与摘要数据库路径。", false,
			func(cfg Config) string { return cfg.ChatDBPath },
			func(cfg *Config, v string) { cfg.ChatDBPath = v }),
		boolOption(28, "TOOLS_ENABLED", "是否启用 MCP/Tool Calling。", false,
			func(cfg Config) bool { return cfg.ToolsEnabled },
			func(cfg *Config, v bool) { cfg.ToolsEnabled = v }),
		intOption(29, "TOOLS_MAX_ITERATIONS", "单次请求最大工具调用轮数，必须大于 0。", false,
			func(cfg Config) int { return cfg.ToolsMaxIterations },
			func(cfg *Config, v int) { cfg.ToolsMaxIterations = v },
			func(v int) error {
				if v <= 0 {
					return fmt.Errorf("必须大于 0")
				}
				return nil
			}),
		stringOption(30, "MCP_CONFIG_PATH", "全局 MCP 配置文件路径，可用 <empty> 清空。", false,
			func(cfg Config) string { return cfg.MCPConfigPath },
			func(cfg *Config, v string) { cfg.MCPConfigPath = v }),
		stringOption(31, "USER_MCP_DB_PATH", "用户个人 MCP 配置数据库路径。", false,
			func(cfg Config) string { return cfg.UserMCPDBPath },
			func(cfg *Config, v string) { cfg.UserMCPDBPath = v }),
		customOption(32, "ADMIN_ID", "管理员用户 ID 列表，逗号分隔；输入 * 表示所有人；可用 <empty> 清空。", false,
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
		stringOption(33, "VOLCENGINE_TTS_APP_ID", "火山 TTS App ID。", false,
			func(cfg Config) string { return cfg.VolcengineTTSAppID },
			func(cfg *Config, v string) { cfg.VolcengineTTSAppID = v }),
		stringOption(34, "VOLCENGINE_TTS_ACCESS_KEY", "火山 TTS Access Key。", true,
			func(cfg Config) string { return cfg.VolcengineTTSAccessKey },
			func(cfg *Config, v string) { cfg.VolcengineTTSAccessKey = v }),
		stringOption(35, "VOLCENGINE_TTS_RESOURCE_ID", "火山 TTS Resource ID。", false,
			func(cfg Config) string { return cfg.VolcengineTTSResourceID },
			func(cfg *Config, v string) { cfg.VolcengineTTSResourceID = v }),
		stringOption(36, "VOLCENGINE_TTS_SPEAKER", "火山 TTS 音色。", false,
			func(cfg Config) string { return cfg.VolcengineTTSSpeaker },
			func(cfg *Config, v string) { cfg.VolcengineTTSSpeaker = v }),
		enumOption(37, "VOLCENGINE_TTS_AUDIO_FORMAT", "火山 TTS 音频格式：mp3、wav、aac。", false,
			func(cfg Config) string { return cfg.VolcengineTTSAudioFormat },
			func(cfg *Config, v string) { cfg.VolcengineTTSAudioFormat = v },
			map[string]string{"mp3": "mp3", "wav": "wav", "aac": "aac"}),
		intOption(38, "VOLCENGINE_TTS_SAMPLE_RATE", "火山 TTS 采样率，必须大于 0。", false,
			func(cfg Config) int { return cfg.VolcengineTTSSampleRate },
			func(cfg *Config, v int) { cfg.VolcengineTTSSampleRate = v },
			func(v int) error {
				if v <= 0 {
					return fmt.Errorf("必须大于 0")
				}
				return nil
			}),
		intOption(39, "VOLCENGINE_TTS_SPEECH_RATE", "火山 TTS 语速，范围 -50 到 100。", false,
			func(cfg Config) int { return cfg.VolcengineTTSSpeechRate },
			func(cfg *Config, v int) { cfg.VolcengineTTSSpeechRate = v },
			func(v int) error {
				if v < -50 || v > 100 {
					return fmt.Errorf("必须在 -50 到 100 之间")
				}
				return nil
			}),
		boolOption(40, "VOLCENGINE_TTS_SEND_TEXT", "语音模式下是否同时发送文本。", false,
			func(cfg Config) bool { return cfg.VolcengineTTSSendText },
			func(cfg *Config, v bool) { cfg.VolcengineTTSSendText = v }),
	}
}

func stringOption(number int, envKey, desc string, sensitive bool, getter func(Config) string, setter func(*Config, string)) configOption {
	return customOption(number, envKey, desc, sensitive, getter, func(input string, cfg *Config) error {
		value := normalizeConfigTextInput(input)
		if envKey == "OPENAI_API_KEY" || envKey == "OPENAI_MODEL" || envKey == "CHAT_DB_PATH" || envKey == "USER_MCP_DB_PATH" {
			if value == "" {
				return fmt.Errorf("不能为空")
			}
		}
		if envKey == "PROFILE_DB_PATH" && cfg.ProfileEnabled && value == "" {
			return fmt.Errorf("不能为空")
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
				return fmt.Errorf("请输入整数")
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
			return fmt.Errorf("仅支持：%s", strings.Join(keys, ", "))
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
			return nil, fmt.Errorf("ID 列表中不能出现空项")
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("无效 ID: %q", part)
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
		return false, fmt.Errorf("请输入 true/false、on/off 或 1/0")
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

func (b *Bot) buildAdminListText(cfg Config) string {
	var sb strings.Builder
	sb.WriteString("管理员配置界面\n\n")
	sb.WriteString("请回复配置项编号开始修改。\n")
	sb.WriteString("如需退出，点击下方 cancel。\n\n")
	for _, option := range allConfigOptions() {
		sb.WriteString(fmt.Sprintf("%02d. %s = %s\n", option.Number, option.EnvKey, previewConfigValue(option, cfg)))
	}
	return strings.TrimSpace(sb.String())
}

func (b *Bot) buildAdminEditPrompt(option configOption, cfg Config) string {
	return fmt.Sprintf(
		"正在修改 `%s`\n\n当前值：%s\n说明：%s\n\n请直接回复新的配置值。\n如需清空字符串或列表，请输入 `%s`。\n点击下方 cancel 返回配置列表。",
		option.EnvKey,
		previewConfigValue(option, cfg),
		option.Desc,
		adminEmptyValueToken,
	)
}

func (b *Bot) ensureAdminPrivateChat(c tele.Context) error {
	if c.Chat() == nil || c.Chat().Type != tele.ChatPrivate {
		return c.Reply("`/admin` 只能在管理员私聊中使用。")
	}
	if c.Sender() == nil || !b.isAdmin(c.Sender().ID) {
		return c.Reply("只有管理员可以使用该功能。")
	}
	return nil
}

func (b *Bot) handleAdminCommand(c tele.Context) error {
	if err := b.ensureAdminPrivateChat(c); err != nil {
		return err
	}
	cfg := b.currentConfig()
	b.adminSessions.Set(c.Sender().ID, adminConfigSession{Step: "select"})
	return c.Reply(b.buildAdminListText(cfg), b.adminCancelMarkup())
}

func (b *Bot) handleAdminCancel(c tele.Context) error {
	if cb := c.Callback(); cb != nil {
		_ = c.Respond()
	}
	if err := b.ensureAdminPrivateChat(c); err != nil {
		return err
	}
	session, ok := b.adminSessions.Get(c.Sender().ID)
	if !ok {
		return c.EditOrReply("当前没有活动的配置修改会话。")
	}
	if session.Step == "edit" {
		cfg := b.currentConfig()
		b.adminSessions.Set(c.Sender().ID, adminConfigSession{Step: "select"})
		return c.EditOrReply(b.buildAdminListText(cfg), b.adminCancelMarkup())
	}
	b.adminSessions.Clear(c.Sender().ID)
	return c.EditOrReply("已退出管理员配置界面。")
}

func (b *Bot) handleAdminTextIfNeeded(c tele.Context, text string) (bool, error) {
	if c.Chat() == nil || c.Chat().Type != tele.ChatPrivate || c.Sender() == nil || !b.isAdmin(c.Sender().ID) {
		return false, nil
	}
	session, ok := b.adminSessions.Get(c.Sender().ID)
	if !ok {
		return false, nil
	}

	switch session.Step {
	case "select":
		number, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil {
			return true, c.Reply("请输入配置项编号。", b.adminCancelMarkup())
		}
		option, found := findConfigOption(number)
		if !found {
			return true, c.Reply("配置项编号不存在，请重新输入。", b.adminCancelMarkup())
		}
		b.adminSessions.Set(c.Sender().ID, adminConfigSession{Step: "edit", Selection: number})
		return true, c.Reply(b.buildAdminEditPrompt(option, b.currentConfig()), b.adminCancelMarkup(), &tele.SendOptions{ParseMode: tele.ModeMarkdown})

	case "edit":
		option, found := findConfigOption(session.Selection)
		if !found {
			b.adminSessions.Set(c.Sender().ID, adminConfigSession{Step: "select"})
			return true, c.Reply("配置项不存在，已返回列表，请重新输入编号。", b.adminCancelMarkup())
		}

		next := b.currentConfig()
		if err := option.Apply(text, &next); err != nil {
			return true, c.Reply(fmt.Sprintf("输入无效：%v\n\n%s", err, b.buildAdminEditPrompt(option, b.currentConfig())), b.adminCancelMarkup(), &tele.SendOptions{ParseMode: tele.ModeMarkdown})
		}
		if err := b.applyRuntimeConfig(next); err != nil {
			return true, c.Reply(fmt.Sprintf("应用配置失败：%v\n\n%s", err, b.buildAdminEditPrompt(option, b.currentConfig())), b.adminCancelMarkup(), &tele.SendOptions{ParseMode: tele.ModeMarkdown})
		}

		updated := b.currentConfig()
		b.adminSessions.Set(c.Sender().ID, adminConfigSession{Step: "select"})
		return true, c.Reply(fmt.Sprintf("已更新 `%s`，当前值：%s\n\n%s", option.EnvKey, previewConfigValue(option, updated), b.buildAdminListText(updated)), b.adminCancelMarkup(), &tele.SendOptions{ParseMode: tele.ModeMarkdown})
	}

	return false, nil
}

func buildOpenAIClient(apiKey, baseURL string) (*openai.Client, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY 不能为空")
	}
	cfg := openai.DefaultConfig(apiKey)
	if strings.TrimSpace(baseURL) != "" {
		cfg.BaseURL = strings.TrimSpace(baseURL)
	}
	return openai.NewClientWithConfig(cfg), nil
}

func buildAIResources(cfg Config) (mainAI, detectorAI, profileAI, summaryAI *openai.Client, detectorModel, profileModel, summaryModel string, err error) {
	if strings.TrimSpace(cfg.OpenAIModel) == "" {
		err = fmt.Errorf("OPENAI_MODEL 不能为空")
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
			err = fmt.Errorf("AUTO_DETECT 配置无效: %w", err)
			return
		}
		detectorModel = firstNonEmpty(cfg.AutoDetectModel, cfg.OpenAIModel)
	}

	profileAI = mainAI
	profileModel = cfg.OpenAIModel
	if cfg.ProfileKey != "" || cfg.ProfileBase != "" || cfg.ProfileModel != "" {
		profileAI, err = buildOpenAIClient(firstNonEmpty(cfg.ProfileKey, cfg.OpenAIKey), firstNonEmpty(cfg.ProfileBase, cfg.OpenAIBase))
		if err != nil {
			err = fmt.Errorf("PROFILE 配置无效: %w", err)
			return
		}
		profileModel = firstNonEmpty(cfg.ProfileModel, cfg.OpenAIModel)
	}

	summaryAI = mainAI
	summaryModel = cfg.OpenAIModel
	if cfg.SummaryKey != "" || cfg.SummaryBase != "" || cfg.SummaryModel != "" {
		summaryAI, err = buildOpenAIClient(firstNonEmpty(cfg.SummaryKey, cfg.OpenAIKey), firstNonEmpty(cfg.SummaryBase, cfg.OpenAIBase))
		if err != nil {
			err = fmt.Errorf("SUMMARY 配置无效: %w", err)
			return
		}
		summaryModel = firstNonEmpty(cfg.SummaryModel, cfg.OpenAIModel)
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
		return nil, nil, nil, fmt.Errorf("USER_MCP_DB_PATH 不能为空")
	}

	registry := NewToolRegistry()
	var mcpManager *MCPClientManager
	if strings.TrimSpace(cfg.MCPConfigPath) != "" {
		mcpCfg, err := LoadMCPConfig(cfg.MCPConfigPath)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("加载 MCP_CONFIG_PATH 失败: %w", err)
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
		return nil, nil, nil, fmt.Errorf("初始化 USER_MCP_DB_PATH 失败: %w", err)
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
}

func (b *Bot) applyRuntimeConfig(next Config) error {
	current := b.snapshot()

	mainAI, detectorAI, profileAI, summaryAI, detectorModel, profileModel, summaryModel, err := buildAIResources(next)
	if err != nil {
		return err
	}

	var newChatDB *ChatDB
	chatDBChanged := strings.TrimSpace(next.ChatDBPath) != strings.TrimSpace(current.cfg.ChatDBPath)
	if strings.TrimSpace(next.ChatDBPath) == "" {
		return fmt.Errorf("CHAT_DB_PATH 不能为空")
	}
	if chatDBChanged {
		newChatDB, err = OpenChatDB(next.ChatDBPath)
		if err != nil {
			return fmt.Errorf("打开 CHAT_DB_PATH 失败: %w", err)
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
			return fmt.Errorf("PROFILE_DB_PATH 不能为空")
		}
		if profilesChanged {
			newProfiles, err = NewProfileStore(next.ProfileDBPath)
			if err != nil {
				if newChatDB != nil && chatDBChanged {
					_ = newChatDB.Close()
				}
				return fmt.Errorf("打开 PROFILE_DB_PATH 失败: %w", err)
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
	b.tts = newTTS

	if chatDBChanged {
		b.chatDB = newChatDB
		if b.store != nil {
			b.store.db = newChatDB
		}
		if b.summaries != nil {
			b.summaries.db = newChatDB
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
	return nil
}
