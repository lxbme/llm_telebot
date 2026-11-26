package app

import (
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tele "gopkg.in/telebot.v3"
)

// ModelPrice describes per-1000-token pricing for a single model. Zero
// values mean "unpriced" — the computed cost will be zero for that side.
type ModelPrice struct {
	InputPer1K  float64 `json:"input_per_1k"`
	OutputPer1K float64 `json:"output_per_1k"`
}

// unknownModelLogOnce guards the "unknown model, cost=0" log line so a
// misconfigured MODEL_PRICE_TABLE does not flood the log on every request.
var unknownModelLogOnce sync.Map // map[string]struct{}

// computeCostUSD calculates the USD cost for a single usage event.
// Unknown models return 0 without error; a single debug line is logged the
// first time each unknown model is seen.
func computeCostUSD(event UsageEvent, table map[string]ModelPrice) float64 {
	if len(table) == 0 {
		return 0
	}
	price, ok := lookupModelPrice(table, event.Model)
	if !ok && event.requestedModel != "" {
		price, ok = lookupModelPrice(table, event.requestedModel)
	}
	if !ok {
		if event.Model != "" {
			if _, loaded := unknownModelLogOnce.LoadOrStore(strings.ToLower(event.Model), struct{}{}); !loaded {
				log.Printf("[quota] no price entry for model %q, cost will be recorded as 0", event.Model)
			}
		}
		return 0
	}
	cost := float64(event.PromptTokens) * price.InputPer1K / 1000
	cost += float64(event.CompletionTokens) * price.OutputPer1K / 1000
	if cost < 0 {
		return 0
	}
	return cost
}

func lookupModelPrice(table map[string]ModelPrice, model string) (ModelPrice, bool) {
	key := strings.ToLower(strings.TrimSpace(model))
	if key == "" {
		return ModelPrice{}, false
	}
	if p, ok := table[key]; ok {
		return p, true
	}
	// Tolerate provider-prefixed names like "openai/gpt-4o".
	if idx := strings.LastIndex(key, "/"); idx >= 0 && idx+1 < len(key) {
		if p, ok := table[key[idx+1:]]; ok {
			return p, true
		}
	}
	return ModelPrice{}, false
}

// parseModelPriceTable accepts a flat string of the form
//
//	"gpt-4o:0.0025/0.01,gpt-4o-mini:0.00015/0.0006"
//
// and returns a map keyed by lower-case model name. An empty input is
// treated as "no table" (returns nil). Invalid rows produce a descriptive
// error so the admin panel can surface it.
func parseModelPriceTable(raw string) (map[string]ModelPrice, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	out := make(map[string]ModelPrice)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		colon := strings.LastIndex(entry, ":")
		if colon <= 0 || colon == len(entry)-1 {
			return nil, fmt.Errorf("invalid price entry %q, expected model:input/output", entry)
		}
		name := strings.ToLower(strings.TrimSpace(entry[:colon]))
		priceStr := strings.TrimSpace(entry[colon+1:])
		slash := strings.Index(priceStr, "/")
		if slash < 0 {
			return nil, fmt.Errorf("invalid price entry %q, missing input/output separator", entry)
		}
		inputStr := strings.TrimSpace(priceStr[:slash])
		outputStr := strings.TrimSpace(priceStr[slash+1:])
		inputPrice, err := strconv.ParseFloat(inputStr, 64)
		if err != nil || inputPrice < 0 {
			return nil, fmt.Errorf("invalid input price %q in entry %q", inputStr, entry)
		}
		outputPrice, err := strconv.ParseFloat(outputStr, 64)
		if err != nil || outputPrice < 0 {
			return nil, fmt.Errorf("invalid output price %q in entry %q", outputStr, entry)
		}
		if name == "" {
			return nil, fmt.Errorf("empty model name in entry %q", entry)
		}
		out[name] = ModelPrice{InputPer1K: inputPrice, OutputPer1K: outputPrice}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// formatModelPriceTable is the inverse of parseModelPriceTable; it produces
// a deterministic, comma-separated representation suitable for both YAML
// serialization and admin-panel display.
func formatModelPriceTable(table map[string]ModelPrice) string {
	if len(table) == 0 {
		return ""
	}
	keys := make([]string, 0, len(table))
	for k := range table {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		p := table[k]
		parts = append(parts, fmt.Sprintf("%s:%s/%s", k,
			strconv.FormatFloat(p.InputPer1K, 'f', -1, 64),
			strconv.FormatFloat(p.OutputPer1K, 'f', -1, 64)))
	}
	return strings.Join(parts, ",")
}

// currentLocalDate returns the "YYYYMMDD" stamp used to key quota buckets
// for the configured quota reset timezone. Caller is responsible for
// passing a non-nil location; falls back to UTC if loc is nil.
func currentLocalDate(loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return time.Now().In(loc).Format("20060102")
}

// checkDailyQuota returns a non-empty denial message when the user or chat
// has exceeded any of their configured daily limits. Admins always pass.
// Callers should short-circuit the request when ok is false.
func (b *Bot) checkDailyQuota(userID, chatID int64) (string, bool) {
	return b.snapshot().dailyQuotaAt(userID, chatID, time.Now())
}

func (b *Bot) backgroundQuota(userID, chatID int64, now time.Time) bool {
	_, allowed := b.snapshot().dailyQuotaAt(userID, chatID, now)
	return allowed
}

func (snap runtimeSnapshot) dailyQuotaAt(userID, chatID int64, now time.Time) (string, bool) {
	userTokenLimit := snap.cfg.UserDailyTokenLimit
	userUSDLimit := snap.cfg.UserDailyUSDLimit
	chatTokenLimit := snap.cfg.ChatDailyTokenLimit
	if userTokenLimit <= 0 && userUSDLimit <= 0 && chatTokenLimit <= 0 {
		return "", true
	}
	if snap.cfg.isAdmin(userID) {
		return "", true
	}
	if snap.stats == nil {
		return "", true
	}
	loc := snap.cfg.quotaResetLocation
	if loc == nil {
		loc = time.UTC
	}
	localDate := now.In(loc).Format("20060102")

	if userID != 0 && (userTokenLimit > 0 || userUSDLimit > 0) {
		bucket := snap.stats.GetQuotaBucket(UsageScopeUser, 0, userID, localDate)
		if userTokenLimit > 0 && bucket.QuotaTokens >= userTokenLimit {
			log.Printf("[quota] user %d denied tokens=%d limit=%d chat=%d", userID, bucket.QuotaTokens, userTokenLimit, chatID)
			return fmt.Sprintf("💳 本日个人 token 配额已用完（已用 %d / 上限 %d）。请明天再试，或联系管理员使用 /quota_reset。",
				bucket.QuotaTokens, userTokenLimit), false
		}
		if userUSDLimit > 0 && bucket.QuotaUSD >= userUSDLimit {
			log.Printf("[quota] user %d denied usd=%.4f limit=%.4f chat=%d", userID, bucket.QuotaUSD, userUSDLimit, chatID)
			return fmt.Sprintf("💳 本日个人 USD 配额已用完（已用 $%.4f / 上限 $%.4f）。请明天再试，或联系管理员。",
				bucket.QuotaUSD, userUSDLimit), false
		}
	}

	if chatID != 0 && chatTokenLimit > 0 {
		bucket := snap.stats.GetQuotaBucket(UsageScopeChat, chatID, 0, localDate)
		if bucket.QuotaTokens >= chatTokenLimit {
			log.Printf("[quota] chat %d denied tokens=%d limit=%d user=%d", chatID, bucket.QuotaTokens, chatTokenLimit, userID)
			return fmt.Sprintf("💳 本聊天今日 token 配额已达上限（已用 %d / 上限 %d）。请明日再试或联系管理员。",
				bucket.QuotaTokens, chatTokenLimit), false
		}
	}

	return "", true
}

// handleQuotaReset implements the /quota_reset admin command. Supported
// forms:
//
//	/quota_reset                 (prints usage)
//	/quota_reset me              (reset sender's user counter)
//	/quota_reset here            (reset current chat counter)
//	/quota_reset user <id>
//	/quota_reset chat <id>
func (b *Bot) handleQuotaReset(c tele.Context) error {
	sender := c.Sender()
	if sender == nil {
		return nil
	}
	return b.replyAsAdmin(c, "🚫 Only admins can reset quota.", func(snap runtimeSnapshot) string {
		return quotaResetReply(snap, sender.ID, c.Chat().ID, c.Message().Payload)
	})
}

func quotaResetReply(snap runtimeSnapshot, senderID, currentChatID int64, payload string) string {
	args := strings.Fields(payload)
	if len(args) == 0 {
		return "用法：\n" +
			"/quota_reset me              重置你自己的今日配额\n" +
			"/quota_reset here            重置当前聊天的今日配额\n" +
			"/quota_reset user <user_id>  重置指定用户\n" +
			"/quota_reset chat <chat_id>  重置指定聊天"
	}

	var (
		scope  UsageScope
		chatID int64
		userID int64
		label  string
	)
	switch strings.ToLower(args[0]) {
	case "me":
		scope = UsageScopeUser
		userID = senderID
		label = fmt.Sprintf("user %d (me)", userID)
	case "here":
		scope = UsageScopeChat
		chatID = currentChatID
		label = fmt.Sprintf("chat %d (here)", chatID)
	case "user":
		if len(args) < 2 {
			return "Usage: /quota_reset user <user_id>"
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return fmt.Sprintf("❌ Invalid user ID: %q", args[1])
		}
		scope = UsageScopeUser
		userID = id
		label = fmt.Sprintf("user %d", userID)
	case "chat":
		if len(args) < 2 {
			return "Usage: /quota_reset chat <chat_id>"
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return fmt.Sprintf("❌ Invalid chat ID: %q", args[1])
		}
		scope = UsageScopeChat
		chatID = id
		label = fmt.Sprintf("chat %d", chatID)
	default:
		return fmt.Sprintf("❌ Unknown target %q. Valid: me, here, user, chat", args[0])
	}

	if snap.stats == nil {
		return "❌ Stats subsystem not available."
	}
	evicted := snap.stats.ResetQuota(scope, chatID, userID)

	// Also delete today's persisted daily bucket so a restart doesn't
	// re-hydrate the pre-reset counter.
	if snap.chatDB != nil {
		loc := snap.cfg.quotaResetLocation
		if loc == nil {
			loc = time.UTC
		}
		// Delete today's daily row for every calendar day that could still be
		// "today" in the configured TZ — usually one; at a TZ boundary maybe
		// two UTC stamps overlap local "today".
		stamps := localDateToUTCStamps(time.Now(), loc)
		for _, stamp := range stamps {
			if err := snap.chatDB.DeleteUsageAggregate(UsageGranularityDaily, scope, chatID, userID, stamp); err != nil {
				log.Printf("[quota] delete daily %s %s error: %v", scope, stamp, err)
			}
		}
	}

	log.Printf("[quota] reset by admin %d: %s evicted=%v", senderID, label, evicted)
	if evicted {
		return fmt.Sprintf("✅ Quota reset for %s. The counter is back to zero for today.", label)
	}
	return fmt.Sprintf("ℹ️ No active counter for %s today — nothing to reset.", label)
}

// localDateToUTCStamps returns the set of UTC "YYYYMMDD" stamps that could
// correspond to "today" in loc at now. For most TZs this is a single stamp;
// when now is near a boundary (e.g. 00:30 Asia/Shanghai == 16:30 previous
// UTC day) two stamps are returned so delete covers both rows.
func localDateToUTCStamps(now time.Time, loc *time.Location) []string {
	if loc == nil {
		loc = time.UTC
	}
	localNow := now.In(loc)
	localMidnight := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, loc)
	stamps := map[string]struct{}{
		localMidnight.UTC().Format("20060102"):                     {},
		localMidnight.Add(23 * time.Hour).UTC().Format("20060102"): {},
		now.UTC().Format("20060102"):                               {},
	}
	out := make([]string, 0, len(stamps))
	for s := range stamps {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
