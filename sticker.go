package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	openai "github.com/sashabaranov/go-openai"
	tele "gopkg.in/telebot.v3"
)

const (
	stickerModeOff         = "off"
	stickerModeAppend      = "append"
	stickerModeReplace     = "replace"
	stickerModeCommandOnly = "command_only"
)

type StickerRules struct {
	LabelStickers  map[string][]string `json:"label_stickers"`
	KeywordToLabel map[string]string   `json:"keyword_to_label"`
	CommandToLabel map[string]string   `json:"command_to_label"`
}

type StickerEngine struct {
	mu    sync.RWMutex
	path  string
	rules StickerRules
}

func NewStickerEngine(path string) (*StickerEngine, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = "./data/sticker_rules.json"
	}
	engine := &StickerEngine{path: path}
	if err := engine.ensureRulesFile(); err != nil {
		return nil, err
	}
	if err := engine.Reload(path); err != nil {
		return nil, err
	}
	return engine, nil
}

func (e *StickerEngine) ensureRulesFile() error {
	if _, err := os.Stat(e.path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	dir := filepath.Dir(e.path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("create sticker rules directory: %w", err)
		}
	}

	defaultRules := StickerRules{
		LabelStickers: map[string][]string{
			"celebrate": {},
			"agree":     {},
			"comfort":   {},
			"confused":  {},
		},
		KeywordToLabel: map[string]string{
			"恭喜": "celebrate",
			"好耶": "celebrate",
			"收到": "agree",
			"赞同": "agree",
			"抱歉": "comfort",
			"无语": "confused",
		},
		CommandToLabel: map[string]string{
			"yay":      "celebrate",
			"ok":       "agree",
			"sorry":    "comfort",
			"confused": "confused",
		},
	}
	raw, err := json.MarshalIndent(defaultRules, "", "  ")
	if err != nil {
		return fmt.Errorf("encode default sticker rules: %w", err)
	}
	if err := os.WriteFile(e.path, raw, 0644); err != nil {
		return fmt.Errorf("write default sticker rules: %w", err)
	}
	return nil
}

func (e *StickerEngine) Reload(path string) error {
	path = strings.TrimSpace(path)
	if path != "" {
		e.path = path
	}
	raw, err := os.ReadFile(e.path)
	if err != nil {
		return fmt.Errorf("read rules file %s: %w", e.path, err)
	}

	var rules StickerRules
	if err := json.Unmarshal(raw, &rules); err != nil {
		return fmt.Errorf("parse rules file %s: %w", e.path, err)
	}
	e.mu.Lock()
	e.rules = normalizeStickerRules(rules)
	e.mu.Unlock()
	return nil
}

func normalizeStickerRules(rules StickerRules) StickerRules {
	normalized := StickerRules{
		LabelStickers:  make(map[string][]string),
		KeywordToLabel: make(map[string]string),
		CommandToLabel: make(map[string]string),
	}

	for label, stickers := range rules.LabelStickers {
		label = strings.ToLower(strings.TrimSpace(label))
		if label == "" {
			continue
		}
		out := make([]string, 0, len(stickers))
		for _, id := range stickers {
			id = strings.TrimSpace(id)
			if id != "" {
				out = append(out, id)
			}
		}
		normalized.LabelStickers[label] = out
	}

	for keyword, label := range rules.KeywordToLabel {
		keyword = strings.ToLower(strings.TrimSpace(keyword))
		label = strings.ToLower(strings.TrimSpace(label))
		if keyword == "" || label == "" {
			continue
		}
		normalized.KeywordToLabel[keyword] = label
	}

	for alias, label := range rules.CommandToLabel {
		alias = strings.ToLower(strings.TrimSpace(alias))
		label = strings.ToLower(strings.TrimSpace(label))
		if alias == "" || label == "" {
			continue
		}
		normalized.CommandToLabel[alias] = label
	}

	return normalized
}

func (e *StickerEngine) Labels() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	labels := make([]string, 0, len(e.rules.LabelStickers))
	for label, ids := range e.rules.LabelStickers {
		if len(ids) > 0 {
			labels = append(labels, label)
		}
	}
	sort.Strings(labels)
	return labels
}

func (e *StickerEngine) PickByLabel(label string) (string, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	label = strings.ToLower(strings.TrimSpace(label))
	if label == "" {
		return "", false
	}
	ids := e.rules.LabelStickers[label]
	if len(ids) == 0 {
		return "", false
	}
	return ids[rand.Intn(len(ids))], true
}

func (e *StickerEngine) ResolveCommandLabel(input string) string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	label := strings.ToLower(strings.TrimSpace(input))
	if label == "" {
		return ""
	}
	if mapped := e.rules.CommandToLabel[label]; mapped != "" {
		return mapped
	}
	return label
}

func (e *StickerEngine) MatchLabelByText(text string) string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return ""
	}
	keywords := make([]string, 0, len(e.rules.KeywordToLabel))
	for keyword := range e.rules.KeywordToLabel {
		keywords = append(keywords, keyword)
	}
	sort.Slice(keywords, func(i, j int) bool {
		return len([]rune(keywords[i])) > len([]rune(keywords[j]))
	})
	for _, keyword := range keywords {
		if strings.Contains(text, keyword) {
			return e.rules.KeywordToLabel[keyword]
		}
	}
	return ""
}

func (b *Bot) isStickerChatAllowed(chatID int64) bool {
	cfg := b.currentConfig()
	if len(cfg.StickerAllowedChats) == 0 {
		return true
	}
	return cfg.StickerAllowedChats[chatID]
}

func (b *Bot) handleStickerCommand(c tele.Context) error {
	if !b.isAllowed(c) {
		return nil
	}
	if c.Chat() == nil {
		return nil
	}
	snap := b.snapshot()
	if !snap.cfg.StickerEnabled {
		return c.Reply("⚠️ Sticker strategy is disabled.")
	}
	if snap.stickers == nil {
		return c.Reply("⚠️ Sticker engine is not initialized.")
	}

	if !b.isStickerChatAllowed(c.Chat().ID) {
		return c.Reply("⚠️ Sticker is not enabled for this chat.")
	}

	payload := strings.TrimSpace(c.Message().Payload)
	if payload == "" || strings.EqualFold(payload, "status") {
		labels := snap.stickers.Labels()
		return c.Reply(fmt.Sprintf("🎯 Sticker status\nEnabled: %t\nMode: %s\nRules: %s\nModel assist: %t\nLabels with assets: %d",
			snap.cfg.StickerEnabled, snap.cfg.StickerMode, snap.cfg.StickerRulesPath, snap.cfg.StickerModelEnabled, len(labels)))
	}

	if strings.EqualFold(payload, "reload") {
		if c.Sender() == nil || !b.isAdmin(c.Sender().ID) {
			return c.Reply("🚫 Only admins can reload sticker rules.")
		}
		if err := snap.stickers.Reload(snap.cfg.StickerRulesPath); err != nil {
			return c.Reply(fmt.Sprintf("❌ Failed to reload sticker rules: %v", err))
		}
		return c.Reply("✅ Sticker rules reloaded.")
	}

	label := snap.stickers.ResolveCommandLabel(payload)
	fileID, ok := snap.stickers.PickByLabel(label)
	if !ok {
		return c.Reply("⚠️ No sticker found for that label. Check `sticker_rules.json`.")
	}
	if err := b.sendSticker(c.Chat(), fileID); err != nil {
		return c.Reply(fmt.Sprintf("❌ Failed to send sticker: %v", err))
	}
	return nil
}

func (b *Bot) sendSticker(chat *tele.Chat, fileID string) error {
	if chat == nil {
		return fmt.Errorf("chat is nil")
	}
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return fmt.Errorf("sticker file_id is empty")
	}
	snap := b.snapshot()
	sticker := &tele.Sticker{
		File: tele.File{FileID: fileID},
	}
	if _, err := snap.tg.Send(chat, sticker); err != nil {
		return err
	}
	return nil
}

func (b *Bot) selectStickerLabel(userMsg, finalText string) string {
	snap := b.snapshot()
	if snap.stickers == nil {
		return ""
	}

	if label := snap.stickers.MatchLabelByText(finalText); label != "" {
		return label
	}

	userText := userMsg
	if idx := strings.Index(userMsg, "\n"); idx >= 0 && idx+1 < len(userMsg) {
		userText = userMsg[idx+1:]
	}
	if label := snap.stickers.MatchLabelByText(userText); label != "" {
		return label
	}

	if !snap.cfg.StickerModelEnabled || snap.stickerAI == nil {
		return ""
	}
	return b.selectStickerLabelWithModel(userText, finalText, snap.stickers.Labels())
}

func (b *Bot) selectStickerLabelWithModel(userText, finalText string, labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	snap := b.snapshot()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	systemPrompt := fmt.Sprintf("You select one sticker label for a Telegram bot reply. Allowed labels: %s. Return ONE label only, or NONE if no sticker should be sent.", strings.Join(labels, ", "))
	userPrompt := fmt.Sprintf("User message:\n%s\n\nAssistant final reply:\n%s\n\nPick one label.", strings.TrimSpace(userText), strings.TrimSpace(finalText))
	req := openai.ChatCompletionRequest{
		Model: snap.stickerModel,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: userPrompt},
		},
		MaxTokens:   16,
		Temperature: 0,
	}
	resp, err := snap.stickerAI.CreateChatCompletion(ctx, req)
	if err != nil {
		log.Printf("[sticker] model selection error: %v", err)
		return ""
	}
	if len(resp.Choices) == 0 {
		return ""
	}
	label := strings.ToLower(strings.TrimSpace(resp.Choices[0].Message.Content))
	label = strings.Trim(label, "`\"'")
	if label == "" || label == "none" {
		return ""
	}
	for _, allowed := range labels {
		if label == allowed {
			return label
		}
	}
	return ""
}

func (b *Bot) finalizeStickerReply(
	chatID int64,
	chat *tele.Chat,
	userMsg openai.ChatCompletionMessage,
	finalText string,
	placeholder *tele.Message,
) {
	snap := b.snapshot()
	if chat == nil || snap.stickers == nil || !snap.cfg.StickerEnabled {
		return
	}
	if !b.isStickerChatAllowed(chatID) {
		return
	}
	if snap.cfg.StickerMode == stickerModeOff || snap.cfg.StickerMode == stickerModeCommandOnly {
		return
	}
	if snap.speechModes != nil && snap.speechModes.Enabled(chatID) && !snap.cfg.StickerWithSpeech {
		return
	}
	if snap.cfg.StickerSendProbability <= 0 || rand.Float64() > snap.cfg.StickerSendProbability {
		return
	}

	label := b.selectStickerLabel(userMsg.Content, finalText)
	if label == "" {
		return
	}

	maxCount := snap.cfg.StickerMaxPerReply
	if maxCount <= 0 {
		maxCount = 1
	}
	if snap.cfg.StickerMode == stickerModeReplace && placeholder != nil {
		if err := snap.tg.Delete(placeholder); err != nil && !strings.Contains(strings.ToLower(err.Error()), "message to delete not found") {
			log.Printf("[sticker] delete placeholder error: %v", err)
		}
	}
	for i := 0; i < maxCount; i++ {
		fileID, ok := snap.stickers.PickByLabel(label)
		if !ok {
			return
		}
		if err := b.sendSticker(chat, fileID); err != nil {
			log.Printf("[sticker] send error: %v", err)
			return
		}
	}
}
