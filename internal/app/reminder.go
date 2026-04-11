package app

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	openai "github.com/sashabaranov/go-openai"
	tele "gopkg.in/telebot.v3"
)

// Reminder is a user-owned, natural-language-created time trigger.
// Unlike ScheduledTask (cron, admin-only, chat-scoped), a Reminder is
// private to its owning user, one-shot by default, and fires only in the
// chat where it was created. When CronExpr is non-empty the reminder is
// treated as recurring and survives each firing.
type Reminder struct {
	ID        string    `json:"id"`
	UserID    int64     `json:"user_id"`
	ChatID    int64     `json:"chat_id"`
	Prompt    string    `json:"prompt"`
	FireAt    time.Time `json:"fire_at"`
	CronExpr  string    `json:"cron,omitempty"`
	Timezone  string    `json:"timezone"`
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Original  string    `json:"original,omitempty"`
}

// IsRecurring reports whether the reminder should re-arm after firing.
func (r Reminder) IsRecurring() bool {
	return strings.TrimSpace(r.CronExpr) != ""
}

// ReminderStore is the in-memory + bbolt-persisted store of user reminders.
// Mirrors TaskStore's shape but is keyed by user ID (not chat ID) because
// /reminders in a private chat has to list every reminder the caller owns
// across every chat they've used the bot in.
type ReminderStore struct {
	mu   sync.RWMutex
	data map[int64]map[string]Reminder // userID → reminderID → Reminder
	db   *ChatDB
}

func NewReminderStore(db *ChatDB) *ReminderStore {
	s := &ReminderStore{
		data: make(map[int64]map[string]Reminder),
		db:   db,
	}
	if db != nil {
		for userID, items := range db.LoadAllReminders() {
			if len(items) == 0 {
				continue
			}
			s.data[userID] = make(map[string]Reminder, len(items))
			for _, rem := range items {
				s.data[userID][rem.ID] = rem
			}
		}
		if len(s.data) > 0 {
			log.Printf("[reminder] restored reminders for %d user(s)", len(s.data))
		}
	}
	return s
}

func (s *ReminderStore) RebindDB(db *ChatDB) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db = db
}

func (s *ReminderStore) persistLocked(userID int64) {
	if s.db == nil {
		return
	}
	items := s.data[userID]
	if len(items) == 0 {
		s.db.DeleteRemindersForUser(userID)
		return
	}
	list := make([]Reminder, 0, len(items))
	for _, rem := range items {
		list = append(list, rem)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	s.db.SaveReminders(userID, list)
}

// ListForUser returns every reminder owned by userID, sorted by next fire time.
func (s *ReminderStore) ListForUser(userID int64) []Reminder {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := s.data[userID]
	if len(items) == 0 {
		return nil
	}
	out := make([]Reminder, 0, len(items))
	for _, rem := range items {
		out = append(out, rem)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FireAt.IsZero() && out[j].FireAt.IsZero() {
			return out[i].ID < out[j].ID
		}
		if out[i].FireAt.IsZero() {
			return false
		}
		if out[j].FireAt.IsZero() {
			return true
		}
		return out[i].FireAt.Before(out[j].FireAt)
	})
	return out
}

// Get fetches a reminder by ID and owner. Returns zero value and false when
// not found. Ownership is implicit in the userID key.
func (s *ReminderStore) Get(userID int64, id string) (Reminder, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := s.data[userID]
	rem, ok := items[id]
	return rem, ok
}

// Insert stores a new reminder. Caller is responsible for ID uniqueness.
func (s *ReminderStore) Insert(rem Reminder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := s.data[rem.UserID]
	if items == nil {
		items = make(map[string]Reminder)
		s.data[rem.UserID] = items
	}
	items[rem.ID] = rem
	s.persistLocked(rem.UserID)
}

// Delete removes a reminder owned by userID. Returns true if it existed.
func (s *ReminderStore) Delete(userID int64, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := s.data[userID]
	if _, ok := items[id]; !ok {
		return false
	}
	delete(items, id)
	if len(items) == 0 {
		delete(s.data, userID)
	}
	s.persistLocked(userID)
	return true
}

// HasID reports whether id is already used by any of this user's reminders.
func (s *ReminderStore) HasID(userID int64, id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.data[userID][id]
	return ok
}

// Due returns reminders whose FireAt <= now.
type reminderRef struct {
	UserID int64
	Item   Reminder
}

func (s *ReminderStore) Due(now time.Time) []reminderRef {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var due []reminderRef
	for userID, items := range s.data {
		for _, rem := range items {
			if rem.FireAt.IsZero() || rem.FireAt.After(now) {
				continue
			}
			due = append(due, reminderRef{UserID: userID, Item: rem})
		}
	}
	return due
}

// CompleteFire handles post-fire bookkeeping. One-shot reminders are deleted;
// recurring reminders get their FireAt advanced to the next cron occurrence.
// If cron re-parse fails the reminder is removed so it doesn't get stuck.
func (s *ReminderStore) CompleteFire(userID int64, id string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := s.data[userID]
	rem, ok := items[id]
	if !ok {
		return
	}
	if !rem.IsRecurring() {
		delete(items, id)
		if len(items) == 0 {
			delete(s.data, userID)
		}
		s.persistLocked(userID)
		return
	}
	loc, err := time.LoadLocation(rem.Timezone)
	if err != nil {
		log.Printf("[reminder] drop recurring %s: bad timezone %q: %v", id, rem.Timezone, err)
		delete(items, id)
		if len(items) == 0 {
			delete(s.data, userID)
		}
		s.persistLocked(userID)
		return
	}
	schedule, err := cron.ParseStandard(rem.CronExpr)
	if err != nil {
		log.Printf("[reminder] drop recurring %s: bad cron %q: %v", id, rem.CronExpr, err)
		delete(items, id)
		if len(items) == 0 {
			delete(s.data, userID)
		}
		s.persistLocked(userID)
		return
	}
	rem.FireAt = schedule.Next(now.In(loc)).UTC()
	items[id] = rem
	s.persistLocked(userID)
}

// ─── Runner hook (shared with scheduler) ─────────────────────────────────────

// runDueReminders is called from TaskRunner.loop alongside runDue (the schedule
// dispatcher) so reminders share the existing 15s ticker instead of running a
// second goroutine. Keys on running sync.Map are prefixed with "rem:" to avoid
// collisions with schedule keys of the form "<chatID>:<id>".
func (r *TaskRunner) runDueReminders(now time.Time) {
	if r == nil || r.bot == nil || r.bot.reminders == nil {
		return
	}
	for _, ref := range r.bot.reminders.Due(now) {
		runKey := fmt.Sprintf("rem:%d:%s", ref.UserID, ref.Item.ID)
		if _, loaded := r.running.LoadOrStore(runKey, true); loaded {
			continue
		}
		go func(ref reminderRef, runKey string) {
			defer r.running.Delete(runKey)
			r.executeReminder(ref.UserID, ref.Item)
		}(ref, runKey)
	}
}

func (r *TaskRunner) executeReminder(userID int64, rem Reminder) {
	log.Printf("[reminder] triggering user=%d chat=%d id=%s", userID, rem.ChatID, rem.ID)
	r.bot.recordDashboardEvent(DashboardEvent{
		Type:    DashboardEventScheduleTriggered,
		ChatID:  rem.ChatID,
		UserID:  userID,
		Summary: fmt.Sprintf("trigger reminder %s", rem.ID),
		Detail:  truncateDashboardText("[reminder] "+rem.Prompt, 160),
		Success: true,
	})

	msg := buildReminderMessage(rem)
	chat := &tele.Chat{ID: rem.ChatID}
	if err := r.bot.startChatFlow(chat, nil, msg, false); err != nil {
		log.Printf("[reminder] trigger failed user=%d id=%s: %v", userID, rem.ID, err)
		r.bot.recordDashboardEvent(DashboardEvent{
			Type:    DashboardEventScheduleFailed,
			ChatID:  rem.ChatID,
			UserID:  userID,
			Summary: fmt.Sprintf("reminder %s failed", rem.ID),
			Detail:  err.Error(),
		})
		// Leave recurring reminders in place so we retry next tick; advance
		// one-shot reminders to avoid wedging the runner on a dead chat.
		if !rem.IsRecurring() {
			r.bot.reminders.Delete(userID, rem.ID)
		}
		return
	}
	r.bot.reminders.CompleteFire(userID, rem.ID, time.Now().UTC())
}

// buildReminderMessage wraps the reminder prompt in a tagged user message so
// the LLM knows it was triggered by the reminder subsystem rather than a real
// user turn. Mirrors buildScheduledTaskMessage (scheduler.go).
func buildReminderMessage(rem Reminder) openai.ChatCompletionMessage {
	var sb strings.Builder
	sb.WriteString("[reminder")
	sb.WriteString(fmt.Sprintf(" id:%q", rem.ID))
	if rem.CreatedBy != "" {
		sb.WriteString(fmt.Sprintf(" user:%q", rem.CreatedBy))
	}
	sb.WriteString(fmt.Sprintf(" triggered_at:%s", time.Now().UTC().Format(time.RFC3339)))
	sb.WriteString("]\n")
	sb.WriteString(rem.Prompt)
	return openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Name:    "reminder",
		Content: sb.String(),
	}
}

// ─── Natural-language parser ─────────────────────────────────────────────────

type parsedReminder struct {
	FireAt   time.Time
	CronExpr string
	Timezone string
	Prompt   string
	Error    string
}

// reminderParseRaw matches the JSON shape we ask the LLM to emit.
type reminderParseRaw struct {
	FireAt   string `json:"fire_at"`
	Cron     string `json:"cron"`
	Timezone string `json:"timezone"`
	Prompt   string `json:"prompt"`
	Error    string `json:"error"`
}

const reminderParseSystemPromptTemplate = `You convert a user's reminder request into strict JSON. Current time is %s (timezone: %s). Output ONLY a JSON object, no prose, no markdown fences.

Schema:
{
  "fire_at": "RFC3339 timestamp with offset, e.g. 2026-04-12T09:00:00+08:00",
  "cron":    "",
  "timezone":"%s",
  "prompt":  "what the assistant should say when triggered, in the user's language"
}

Rules:
- If the user describes a single future moment, fill "fire_at" and leave "cron" empty.
- If the user describes a recurring cadence ("每周一", "every morning"), fill "cron" with a 5-field cron expression and "timezone" to the matching IANA zone, and set "fire_at" to the next occurrence.
- Relative times ("明天", "tomorrow", "in 2 hours") are resolved against the current time above.
- "prompt" should rewrite the reminder content as a short, natural message the bot will say at trigger time — NOT echo the user's raw command. Example input "提醒我明天早上9点交报销" → "prompt": "该交报销了".
- If the request is ambiguous or has no resolvable time, output {"error": "short reason"} instead.`

// parseReminderNL sends payload to the LLM with a strict JSON-only schema and
// returns a parsedReminder. It validates timezone, cron, and fire_at before
// returning. Modeled after extractProfile's one-shot pattern (profile.go).
func (b *Bot) parseReminderNL(ctx context.Context, payload string) (parsedReminder, error) {
	snap := b.snapshot()
	tz := strings.TrimSpace(snap.cfg.ReminderDefaultTimezone)
	if tz == "" {
		tz = "Asia/Shanghai"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return parsedReminder{}, fmt.Errorf("invalid REMINDER_DEFAULT_TIMEZONE %q: %w", tz, err)
	}
	nowInTZ := time.Now().In(loc).Format(time.RFC3339)
	sysPrompt := fmt.Sprintf(reminderParseSystemPromptTemplate, nowInTZ, tz, tz)

	client := snap.reminderAI
	model := snap.reminderModel
	if client == nil {
		client = snap.ai
		model = snap.cfg.OpenAIModel
	}
	if client == nil {
		return parsedReminder{}, fmt.Errorf("no LLM client available for reminder parsing")
	}

	req := openai.ChatCompletionRequest{
		Model: model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: sysPrompt},
			{Role: openai.ChatMessageRoleUser, Content: payload},
		},
		Temperature: 0.1,
	}
	applyMaxTokens(&req, 300)
	sanitizeBetaRequest(&req)

	usageCtx := newUsageContext(0, 0, 0)
	started := time.Now()
	resp, err := client.CreateChatCompletion(ctx, req)
	b.recordUsageEvent(usageEvent(usageCtx, UsageCallReminderParse, firstNonEmpty(resp.Model, model), false, 0, started, &resp.Usage, err == nil))
	if err != nil {
		return parsedReminder{}, fmt.Errorf("LLM call failed: %w", err)
	}
	if len(resp.Choices) == 0 {
		return parsedReminder{}, fmt.Errorf("empty LLM response")
	}

	raw := strings.TrimSpace(resp.Choices[0].Message.Content)
	raw = stripJSONFence(raw)

	var parsed reminderParseRaw
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		// Defensive fallback: locate first {...}.
		start := strings.Index(raw, "{")
		end := strings.LastIndex(raw, "}")
		if start < 0 || end <= start {
			return parsedReminder{}, fmt.Errorf("parser returned non-JSON: %s", truncateForError(raw))
		}
		if err2 := json.Unmarshal([]byte(raw[start:end+1]), &parsed); err2 != nil {
			return parsedReminder{}, fmt.Errorf("parser returned invalid JSON: %s", truncateForError(raw))
		}
	}

	if strings.TrimSpace(parsed.Error) != "" {
		return parsedReminder{Error: parsed.Error}, nil
	}

	resolvedTZ := strings.TrimSpace(parsed.Timezone)
	if resolvedTZ == "" {
		resolvedTZ = tz
	}
	if _, err := time.LoadLocation(resolvedTZ); err != nil {
		return parsedReminder{}, fmt.Errorf("parser returned invalid timezone %q", resolvedTZ)
	}

	cronExpr := strings.TrimSpace(parsed.Cron)
	if cronExpr != "" {
		if _, err := cron.ParseStandard(cronExpr); err != nil {
			return parsedReminder{}, fmt.Errorf("parser returned invalid cron %q: %w", cronExpr, err)
		}
	}

	fireAtStr := strings.TrimSpace(parsed.FireAt)
	var fireAt time.Time
	if fireAtStr != "" {
		parsedTime, err := time.Parse(time.RFC3339, fireAtStr)
		if err != nil {
			return parsedReminder{}, fmt.Errorf("parser returned invalid fire_at %q: %w", fireAtStr, err)
		}
		fireAt = parsedTime.UTC()
	}

	promptText := strings.TrimSpace(parsed.Prompt)
	if promptText == "" {
		promptText = strings.TrimSpace(payload)
	}

	return parsedReminder{
		FireAt:   fireAt,
		CronExpr: cronExpr,
		Timezone: resolvedTZ,
		Prompt:   promptText,
	}, nil
}

func stripJSONFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	return s
}

func truncateForError(s string) string {
	const limit = 200
	if len([]rune(s)) <= limit {
		return s
	}
	return string([]rune(s)[:limit]) + "…"
}

// ─── ID generation ───────────────────────────────────────────────────────────

// generateReminderID returns a short, URL-safe, non-ambiguous ID like "r_a7f3k2".
// 6 base32 characters = 30 bits of entropy, collision-checked against the
// caller's existing reminders.
func (s *ReminderStore) generateReminderID(userID int64) (string, error) {
	enc := base32.StdEncoding.WithPadding(base32.NoPadding)
	var buf [4]byte
	for attempt := 0; attempt < 8; attempt++ {
		if _, err := rand.Read(buf[:]); err != nil {
			return "", err
		}
		id := "r_" + strings.ToLower(enc.EncodeToString(buf[:])[:6])
		if !s.HasID(userID, id) {
			return id, nil
		}
	}
	return "", fmt.Errorf("failed to generate unique reminder ID after 8 attempts")
}

// ─── Command handlers ────────────────────────────────────────────────────────

const reminderCommandHelp = `🔔 Reminder 命令

/remind <自然语言>   用自然语言创建一次性或重复提醒
/reminders           列出你的提醒（群聊仅限本群聊；私聊列出全部）
/reminder_del <id>   删除指定 ID 的提醒

示例：
  /remind 明天早上9点提醒我交报销
  /remind 10秒后说 hello
  /remind 每周一早上9点提醒开会`

func (b *Bot) handleRemindCommand(c tele.Context) error {
	if !b.isAllowed(c) {
		return nil
	}
	if c.Sender() == nil || c.Chat() == nil {
		return nil
	}
	if b.reminders == nil {
		return c.Reply("⚠️ Reminder storage is unavailable.")
	}

	payload := strings.TrimSpace(c.Message().Payload)
	if payload == "" {
		return c.Reply(reminderCommandHelp)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	parsed, err := b.parseReminderNL(ctx, payload)
	if err != nil {
		return c.Reply(fmt.Sprintf("❌ Failed to parse reminder: %v", err))
	}
	if parsed.Error != "" {
		return c.Reply(fmt.Sprintf("⚠️ Could not understand: %s", parsed.Error))
	}
	if parsed.FireAt.IsZero() && parsed.CronExpr == "" {
		return c.Reply("⚠️ Could not extract a time from your request. Try something like \"明天早上9点提醒我交报销\".")
	}

	now := time.Now().UTC()
	if parsed.FireAt.IsZero() && parsed.CronExpr != "" {
		loc, _ := time.LoadLocation(parsed.Timezone)
		schedule, _ := cron.ParseStandard(parsed.CronExpr)
		parsed.FireAt = schedule.Next(now.In(loc)).UTC()
	}
	if parsed.FireAt.Before(now.Add(-2 * time.Second)) {
		return c.Reply("⚠️ That time is already in the past.")
	}

	sender := c.Sender()
	id, err := b.reminders.generateReminderID(sender.ID)
	if err != nil {
		return c.Reply(fmt.Sprintf("❌ Failed to allocate reminder ID: %v", err))
	}

	rem := Reminder{
		ID:        id,
		UserID:    sender.ID,
		ChatID:    c.Chat().ID,
		Prompt:    parsed.Prompt,
		FireAt:    parsed.FireAt,
		CronExpr:  parsed.CronExpr,
		Timezone:  parsed.Timezone,
		CreatedBy: formatReminderUser(sender),
		CreatedAt: now,
		Original:  payload,
	}
	b.reminders.Insert(rem)

	loc, _ := time.LoadLocation(parsed.Timezone)
	fireLocal := parsed.FireAt.In(loc).Format("2006-01-02 15:04")
	tag := "one-shot"
	if rem.IsRecurring() {
		tag = "recurring: " + rem.CronExpr
	}
	return c.Reply(fmt.Sprintf("✅ Reminder `%s` set for %s %s (%s)\n→ %s",
		rem.ID, fireLocal, parsed.Timezone, tag, rem.Prompt))
}

func (b *Bot) handleRemindersCommand(c tele.Context) error {
	if !b.isAllowed(c) {
		return nil
	}
	if c.Sender() == nil || c.Chat() == nil {
		return nil
	}
	if b.reminders == nil {
		return c.Reply("⚠️ Reminder storage is unavailable.")
	}

	list := b.reminders.ListForUser(c.Sender().ID)
	isPrivate := c.Chat().Type == tele.ChatPrivate
	if !isPrivate {
		filtered := list[:0]
		for _, rem := range list {
			if rem.ChatID == c.Chat().ID {
				filtered = append(filtered, rem)
			}
		}
		list = filtered
	}

	if len(list) == 0 {
		return c.Reply("📋 No reminders. Use /remind <natural language> to create one.")
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "📋 Your reminders (%d):\n", len(list))
	for _, rem := range list {
		loc, err := time.LoadLocation(rem.Timezone)
		if err != nil {
			loc = time.UTC
		}
		fireLocal := rem.FireAt.In(loc).Format("2006-01-02 15:04")
		tag := ""
		if rem.IsRecurring() {
			tag = "  (recurring: " + rem.CronExpr + ")"
		}
		chatHint := ""
		if isPrivate && rem.ChatID != c.Chat().ID {
			chatHint = fmt.Sprintf("  [chat=%d]", rem.ChatID)
		}
		fmt.Fprintf(&sb, "• `%s`  %s %s%s%s\n   → %s\n",
			rem.ID, fireLocal, rem.Timezone, tag, chatHint, truncateForError(rem.Prompt))
	}
	return c.Reply(sb.String())
}

func (b *Bot) handleReminderDeleteCommand(c tele.Context) error {
	if !b.isAllowed(c) {
		return nil
	}
	if c.Sender() == nil {
		return nil
	}
	if b.reminders == nil {
		return c.Reply("⚠️ Reminder storage is unavailable.")
	}

	payload := strings.TrimSpace(c.Message().Payload)
	if payload == "" {
		return c.Reply("Usage: /reminder_del <id>")
	}
	id := strings.Fields(payload)[0]

	userID := c.Sender().ID
	rem, ok := b.reminders.Get(userID, id)
	if !ok || rem.UserID != userID {
		return c.Reply(fmt.Sprintf("⚠️ Reminder `%s` not found or not yours.", id))
	}
	if !b.reminders.Delete(userID, id) {
		return c.Reply(fmt.Sprintf("⚠️ Reminder `%s` could not be deleted.", id))
	}
	return c.Reply(fmt.Sprintf("✅ Deleted reminder `%s`.", id))
}

// formatReminderUser picks the friendliest display label available for a sender.
func formatReminderUser(u *tele.User) string {
	if u == nil {
		return ""
	}
	if u.Username != "" {
		return "@" + u.Username
	}
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if name != "" {
		return name
	}
	return fmt.Sprintf("user:%d", u.ID)
}
