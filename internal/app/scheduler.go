package app

import (
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	openai "github.com/sashabaranov/go-openai"
	tele "gopkg.in/telebot.v3"
)

const (
	scheduleActionUpsert = "upsert"
	scheduleActionDelete = "delete"
	scheduleActionList   = "list"
	scheduleActionPause  = "pause"
	scheduleActionResume = "resume"
)

var scheduleIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type ScheduleEnvelope struct {
	Schedule ScheduleRequest `json:"schedule"`
}

type ScheduleRequest struct {
	Action  string           `json:"action"`
	ID      string           `json:"id"`
	Name    string           `json:"name,omitempty"`
	Prompt  string           `json:"prompt"`
	Time    ScheduleTimeSpec `json:"time"`
	Context *bool            `json:"context,omitempty"`
	Enabled *bool            `json:"enabled,omitempty"`
}

type ScheduleTimeSpec struct {
	CronExpr string `json:"cron"`
	Timezone string `json:"timezone"`
}

type ScheduledTask struct {
	ID        string    `json:"id"`
	Name      string    `json:"name,omitempty"`
	Prompt    string    `json:"prompt"`
	CronExpr  string    `json:"cron"`
	Timezone  string    `json:"timezone"`
	Context   bool      `json:"context"`
	Enabled   bool      `json:"enabled"`
	NextRunAt time.Time `json:"next_run_at,omitempty"`
	LastRunAt time.Time `json:"last_run_at,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	CreatedBy int64     `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type TaskStore struct {
	mu    sync.RWMutex
	tasks map[int64]map[string]ScheduledTask
	db    *ChatDB
}

type TaskRunner struct {
	bot     *Bot
	store   *TaskStore
	running sync.Map
	started sync.Once
}

type ScheduleWizardSession struct {
	ChatID           int64
	Step             string
	ID               string
	CronExpr         string
	Timezone         string
	Prompt           string
	Name             string
	CreatedAt        time.Time
	PanelChatID      int64
	PanelMessageID   int
	TriggerMessageID int
}

type ScheduleWizardStore struct {
	mu       sync.RWMutex
	sessions map[int64]ScheduleWizardSession
}

func NewScheduleWizardStore() *ScheduleWizardStore {
	return &ScheduleWizardStore{
		sessions: make(map[int64]ScheduleWizardSession),
	}
}

func (s *ScheduleWizardStore) Get(userID int64) (ScheduleWizardSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session, ok := s.sessions[userID]
	return session, ok
}

func (s *ScheduleWizardStore) Set(userID int64, session ScheduleWizardSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[userID] = session
}

func (s *ScheduleWizardStore) Clear(userID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, userID)
}

func TryParseScheduleRequest(text string) (*ScheduleRequest, bool, error) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "{") {
		return nil, false, nil
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil, false, nil
	}

	if _, ok := raw["schedule"]; !ok {
		return nil, false, nil
	}

	var envelope ScheduleEnvelope
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		return nil, true, fmt.Errorf("invalid schedule JSON: %w", err)
	}
	if err := envelope.Schedule.NormalizeAndValidate(); err != nil {
		return nil, true, err
	}
	return &envelope.Schedule, true, nil
}

func (r *ScheduleRequest) NormalizeAndValidate() error {
	r.Action = strings.ToLower(strings.TrimSpace(r.Action))
	if r.Action == "" {
		r.Action = scheduleActionUpsert
	}
	r.ID = strings.TrimSpace(r.ID)
	r.Name = strings.TrimSpace(r.Name)
	r.Prompt = strings.TrimSpace(r.Prompt)
	r.Time.CronExpr = strings.TrimSpace(r.Time.CronExpr)
	r.Time.Timezone = strings.TrimSpace(r.Time.Timezone)

	switch r.Action {
	case scheduleActionUpsert:
		if !scheduleIDPattern.MatchString(r.ID) {
			return fmt.Errorf("schedule.id is required and must match %q", scheduleIDPattern.String())
		}
		if r.Prompt == "" {
			return fmt.Errorf("schedule.prompt is required")
		}
		if r.Time.CronExpr == "" {
			return fmt.Errorf("schedule.time.cron is required")
		}
		if _, err := cron.ParseStandard(r.Time.CronExpr); err != nil {
			return fmt.Errorf("invalid schedule.time.cron: %w", err)
		}
		if r.Time.Timezone == "" {
			return fmt.Errorf("schedule.time.timezone is required")
		}
		if _, err := time.LoadLocation(r.Time.Timezone); err != nil {
			return fmt.Errorf("invalid schedule.time.timezone: %w", err)
		}
	case scheduleActionDelete, scheduleActionPause, scheduleActionResume:
		if !scheduleIDPattern.MatchString(r.ID) {
			return fmt.Errorf("schedule.id is required and must match %q", scheduleIDPattern.String())
		}
	case scheduleActionList:
	default:
		return fmt.Errorf("unsupported schedule.action %q", r.Action)
	}
	return nil
}

func (r ScheduleRequest) ContextEnabled() bool {
	return r.Context != nil && *r.Context
}

func (r ScheduleRequest) EnabledValue() bool {
	if r.Enabled == nil {
		return true
	}
	return *r.Enabled
}

func NewTaskStore(db *ChatDB) *TaskStore {
	s := &TaskStore{
		tasks: make(map[int64]map[string]ScheduledTask),
		db:    db,
	}
	if db != nil {
		for chatID, tasks := range db.LoadAllSchedules() {
			if len(tasks) == 0 {
				continue
			}
			s.tasks[chatID] = make(map[string]ScheduledTask, len(tasks))
			for _, task := range tasks {
				s.tasks[chatID][task.ID] = normalizeLoadedTask(task)
			}
		}
		if len(s.tasks) > 0 {
			log.Printf("[schedule] restored tasks for %d chat(s)", len(s.tasks))
		}
	}
	return s
}

func normalizeLoadedTask(task ScheduledTask) ScheduledTask {
	task.ID = strings.TrimSpace(task.ID)
	task.Name = strings.TrimSpace(task.Name)
	task.Prompt = strings.TrimSpace(task.Prompt)
	task.CronExpr = strings.TrimSpace(task.CronExpr)
	task.Timezone = strings.TrimSpace(task.Timezone)
	if task.Timezone == "" {
		task.Timezone = "UTC"
	}
	if task.Enabled {
		if next, err := computeNextRun(task, time.Now().UTC()); err == nil {
			task.NextRunAt = next
		} else {
			task.Enabled = false
			task.LastError = err.Error()
			task.NextRunAt = time.Time{}
		}
	} else {
		task.NextRunAt = time.Time{}
	}
	return task
}

func (s *TaskStore) RebindDB(db *ChatDB) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db = db
}

func (s *TaskStore) List(chatID int64) []ScheduledTask {
	s.mu.RLock()
	defer s.mu.RUnlock()

	chatTasks := s.tasks[chatID]
	if len(chatTasks) == 0 {
		return nil
	}

	result := make([]ScheduledTask, 0, len(chatTasks))
	for _, task := range chatTasks {
		result = append(result, task)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Enabled != result[j].Enabled {
			return result[i].Enabled
		}
		if result[i].NextRunAt.Equal(result[j].NextRunAt) {
			return result[i].ID < result[j].ID
		}
		if result[i].NextRunAt.IsZero() {
			return false
		}
		if result[j].NextRunAt.IsZero() {
			return true
		}
		return result[i].NextRunAt.Before(result[j].NextRunAt)
	})
	return result
}

func (s *TaskStore) Upsert(chatID int64, task ScheduledTask) (ScheduledTask, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	chatTasks := s.tasks[chatID]
	if chatTasks == nil {
		chatTasks = make(map[string]ScheduledTask)
		s.tasks[chatID] = chatTasks
	}

	existing, exists := chatTasks[task.ID]
	if exists {
		task.CreatedAt = existing.CreatedAt
		task.CreatedBy = existing.CreatedBy
		task.LastRunAt = existing.LastRunAt
	}
	chatTasks[task.ID] = task
	s.persistLocked(chatID)
	return task, !exists
}

func (s *TaskStore) Delete(chatID int64, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	chatTasks := s.tasks[chatID]
	if len(chatTasks) == 0 {
		return false
	}
	if _, ok := chatTasks[id]; !ok {
		return false
	}
	delete(chatTasks, id)
	if len(chatTasks) == 0 {
		delete(s.tasks, chatID)
	}
	s.persistLocked(chatID)
	return true
}

func (s *TaskStore) SetEnabled(chatID int64, id string, enabled bool, now time.Time) (ScheduledTask, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	chatTasks := s.tasks[chatID]
	task, ok := chatTasks[id]
	if !ok {
		return ScheduledTask{}, false, nil
	}

	task.Enabled = enabled
	task.UpdatedAt = now.UTC()
	task.LastError = ""
	if enabled {
		next, err := computeNextRun(task, now.UTC())
		if err != nil {
			return ScheduledTask{}, true, err
		}
		task.NextRunAt = next
	} else {
		task.NextRunAt = time.Time{}
	}

	chatTasks[id] = task
	s.persistLocked(chatID)
	return task, true, nil
}

func (s *TaskStore) Due(now time.Time) []ScheduledTaskRef {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var due []ScheduledTaskRef
	for chatID, chatTasks := range s.tasks {
		for id, task := range chatTasks {
			if !task.Enabled || task.NextRunAt.IsZero() || task.NextRunAt.After(now) {
				continue
			}
			due = append(due, ScheduledTaskRef{
				ChatID: chatID,
				ID:     id,
				Task:   task,
			})
		}
	}
	return due
}

type ScheduledTaskRef struct {
	ChatID int64
	ID     string
	Task   ScheduledTask
}

func (s *TaskStore) MarkRunResult(chatID int64, id string, ranAt time.Time, errText string) (ScheduledTask, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	chatTasks := s.tasks[chatID]
	task, ok := chatTasks[id]
	if !ok {
		return ScheduledTask{}, false
	}

	task.LastRunAt = ranAt.UTC()
	task.LastError = strings.TrimSpace(errText)
	if task.Enabled {
		next, err := computeNextRun(task, ranAt.UTC())
		if err != nil {
			task.Enabled = false
			task.NextRunAt = time.Time{}
			if task.LastError == "" {
				task.LastError = err.Error()
			}
		} else {
			task.NextRunAt = next
		}
	} else {
		task.NextRunAt = time.Time{}
	}
	task.UpdatedAt = ranAt.UTC()

	chatTasks[id] = task
	s.persistLocked(chatID)
	return task, true
}

func (s *TaskStore) persistLocked(chatID int64) {
	if s.db == nil {
		return
	}
	chatTasks := s.tasks[chatID]
	if len(chatTasks) == 0 {
		s.db.DeleteSchedules(chatID)
		return
	}

	items := make([]ScheduledTask, 0, len(chatTasks))
	for _, task := range chatTasks {
		items = append(items, task)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	s.db.SaveSchedules(chatID, items)
}

func computeNextRun(task ScheduledTask, from time.Time) (time.Time, error) {
	loc, err := time.LoadLocation(task.Timezone)
	if err != nil {
		return time.Time{}, err
	}
	schedule, err := cron.ParseStandard(task.CronExpr)
	if err != nil {
		return time.Time{}, err
	}
	return schedule.Next(from.In(loc)).UTC(), nil
}

func NewTaskRunner(bot *Bot, store *TaskStore) *TaskRunner {
	return &TaskRunner{bot: bot, store: store}
}

func (r *TaskRunner) Start() {
	if r == nil || r.store == nil {
		return
	}
	r.started.Do(func() {
		go r.loop()
	})
}

func (r *TaskRunner) loop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	r.runDue(time.Now().UTC())
	for now := range ticker.C {
		r.runDue(now.UTC())
	}
}

func (r *TaskRunner) runDue(now time.Time) {
	for _, ref := range r.store.Due(now) {
		runKey := fmt.Sprintf("%d:%s", ref.ChatID, ref.ID)
		if _, loaded := r.running.LoadOrStore(runKey, true); loaded {
			continue
		}

		go func(ref ScheduledTaskRef, runKey string) {
			defer r.running.Delete(runKey)
			r.execute(ref.ChatID, ref.Task)
		}(ref, runKey)
	}
}

func (r *TaskRunner) execute(chatID int64, task ScheduledTask) {
	log.Printf("[schedule] triggering chat=%d id=%s", chatID, task.ID)
	r.bot.recordDashboardEvent(DashboardEvent{
		Type:    DashboardEventScheduleTriggered,
		ChatID:  chatID,
		Summary: fmt.Sprintf("trigger schedule %s", task.ID),
		Detail:  truncateDashboardText(task.Prompt, 160),
		Success: true,
	})

	taskMsg := buildScheduledTaskMessage(task)
	chat := &tele.Chat{ID: chatID}
	err := r.bot.startChatFlow(chat, nil, taskMsg, task.Context)
	ranAt := time.Now().UTC()
	errText := ""
	if err != nil {
		errText = err.Error()
		log.Printf("[schedule] trigger failed chat=%d id=%s: %v", chatID, task.ID, err)
		r.bot.recordDashboardEvent(DashboardEvent{
			Type:    DashboardEventScheduleFailed,
			ChatID:  chatID,
			Summary: fmt.Sprintf("schedule %s failed", task.ID),
			Detail:  err.Error(),
		})
	}
	r.store.MarkRunResult(chatID, task.ID, ranAt, errText)
}

func buildScheduledTaskMessage(task ScheduledTask) openai.ChatCompletionMessage {
	var sb strings.Builder
	sb.WriteString("[scheduled_task")
	sb.WriteString(fmt.Sprintf(" id:%q", task.ID))
	if task.Name != "" {
		sb.WriteString(fmt.Sprintf(" name:%q", task.Name))
	}
	sb.WriteString(fmt.Sprintf(" cron:%q timezone:%q", task.CronExpr, task.Timezone))
	sb.WriteString(fmt.Sprintf(" triggered_at:%s", time.Now().UTC().Format(time.RFC3339)))
	sb.WriteString("]\n")
	sb.WriteString(task.Prompt)
	return openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Name:    "scheduler",
		Content: sb.String(),
	}
}

func (b *Bot) handleScheduleMessage(c tele.Context, text string) (bool, error) {
	req, lookedLike, err := TryParseScheduleRequest(text)
	if !lookedLike {
		return false, nil
	}
	if err != nil {
		return true, c.Reply(fmt.Sprintf("❌ Invalid schedule message: %v", err))
	}

	snap := b.snapshot()
	if snap.tasks == nil {
		return true, c.Reply("⚠️ Schedule storage is unavailable.")
	}

	chatID := c.Chat().ID
	userID := c.Sender().ID
	switch req.Action {
	case scheduleActionList:
		return true, c.Reply(formatScheduledTaskList(snap.tasks.List(chatID)))
	case scheduleActionUpsert:
		if !b.isAdmin(userID) {
			return true, c.Reply("🚫 Only admins can create or update scheduled tasks.")
		}
		now := time.Now().UTC()
		task := ScheduledTask{
			ID:        req.ID,
			Name:      req.Name,
			Prompt:    req.Prompt,
			CronExpr:  req.Time.CronExpr,
			Timezone:  req.Time.Timezone,
			Context:   req.ContextEnabled(),
			Enabled:   req.EnabledValue(),
			CreatedBy: userID,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if task.Enabled {
			nextRun, err := computeNextRun(task, now)
			if err != nil {
				return true, c.Reply(fmt.Sprintf("❌ Failed to compute next run: %v", err))
			}
			task.NextRunAt = nextRun
		}
		saved, created := snap.tasks.Upsert(chatID, task)
		actionLabel := "updated"
		if created {
			actionLabel = "created"
		}
		return true, c.Reply(formatScheduledTaskUpsert(saved, actionLabel))
	case scheduleActionDelete:
		if !b.isAdmin(userID) {
			return true, c.Reply("🚫 Only admins can delete scheduled tasks.")
		}
		if !snap.tasks.Delete(chatID, req.ID) {
			return true, c.Reply(fmt.Sprintf("⚠️ Schedule %q was not found in this chat.", req.ID))
		}
		return true, c.Reply(fmt.Sprintf("✅ Deleted schedule %q.", req.ID))
	case scheduleActionPause:
		if !b.isAdmin(userID) {
			return true, c.Reply("🚫 Only admins can pause scheduled tasks.")
		}
		task, found, err := snap.tasks.SetEnabled(chatID, req.ID, false, time.Now().UTC())
		if err != nil {
			return true, c.Reply(fmt.Sprintf("❌ Failed to pause schedule: %v", err))
		}
		if !found {
			return true, c.Reply(fmt.Sprintf("⚠️ Schedule %q was not found in this chat.", req.ID))
		}
		return true, c.Reply(formatScheduledTaskState(task, "paused"))
	case scheduleActionResume:
		if !b.isAdmin(userID) {
			return true, c.Reply("🚫 Only admins can resume scheduled tasks.")
		}
		task, found, err := snap.tasks.SetEnabled(chatID, req.ID, true, time.Now().UTC())
		if err != nil {
			return true, c.Reply(fmt.Sprintf("❌ Failed to resume schedule: %v", err))
		}
		if !found {
			return true, c.Reply(fmt.Sprintf("⚠️ Schedule %q was not found in this chat.", req.ID))
		}
		return true, c.Reply(formatScheduledTaskState(task, "resumed"))
	default:
		return true, c.Reply(fmt.Sprintf("❌ Unsupported schedule action %q.", req.Action))
	}
}

func (b *Bot) handleScheduleCommand(c tele.Context) error {
	if !b.isAllowed(c) {
		return nil
	}

	payload := strings.TrimSpace(c.Message().Payload)
	if payload == "" {
		return c.Reply(scheduleCommandHelp())
	}

	fields := strings.Fields(payload)
	if len(fields) == 0 {
		return c.Reply(scheduleCommandHelp())
	}

	switch strings.ToLower(fields[0]) {
	case "help":
		return c.Reply(scheduleCommandHelp())
	case "example", "json", "create":
		return c.Reply(scheduleJSONExample())
	case "new":
		return b.handleScheduleNewCommand(c)
	case "list", "ls":
		return b.handleScheduleListCommand(c)
	case "pause":
		return b.handleScheduleToggleCommand(c, false, firstArg(fields))
	case "resume":
		return b.handleScheduleToggleCommand(c, true, firstArg(fields))
	case "delete", "del", "rm":
		return b.handleScheduleDeleteCommand(c, firstArg(fields))
	default:
		return c.Reply("⚠️ Unknown schedule subcommand.\n\n" + scheduleCommandHelp())
	}
}

func (b *Bot) handleScheduleNewCommand(c tele.Context) error {
	if !b.isAllowed(c) {
		return nil
	}
	if c.Sender() == nil || !b.isAdmin(c.Sender().ID) {
		return c.Reply("🚫 Only admins can create scheduled tasks.")
	}

	session := ScheduleWizardSession{
		ChatID:           c.Chat().ID,
		Step:             "id",
		Timezone:         "Asia/Shanghai",
		CreatedAt:        time.Now().UTC(),
		TriggerMessageID: c.Message().ID,
	}
	rendered, err := b.renderScheduleWizardPanel(c, session, scheduleWizardPrompt(session))
	if err != nil {
		return err
	}
	b.scheduleWizard.Set(c.Sender().ID, rendered)
	b.deleteScheduleWizardMessage(c.Message())
	return nil
}

func (b *Bot) handleScheduleTextIfNeeded(c tele.Context, text string) (bool, error) {
	if !b.isAllowed(c) || c.Sender() == nil || c.Chat() == nil {
		return false, nil
	}

	session, ok := b.scheduleWizard.Get(c.Sender().ID)
	if !ok || session.ChatID != c.Chat().ID {
		return false, nil
	}
	if !b.isAdmin(c.Sender().ID) {
		b.scheduleWizard.Clear(c.Sender().ID)
		b.cleanupScheduleWizardPanel(session)
		return true, c.Reply("🚫 Only admins can create scheduled tasks.")
	}

	trimmed := strings.TrimSpace(text)
	if strings.EqualFold(trimmed, "cancel") {
		b.scheduleWizard.Clear(c.Sender().ID)
		b.deleteScheduleWizardMessage(c.Message())
		b.cleanupScheduleWizardPanel(session)
		return true, c.Reply("🛑 Schedule creation cancelled.")
	}

	shouldDeleteInput := false
	defer func() {
		if shouldDeleteInput {
			b.deleteScheduleWizardMessage(c.Message())
		}
	}()

	switch session.Step {
	case "id":
		if !scheduleIDPattern.MatchString(trimmed) {
			rendered, err := b.renderScheduleWizardPanel(c, session, "❌ 任务 ID 无效。\n\n"+scheduleWizardPrompt(session))
			if err == nil {
				b.scheduleWizard.Set(c.Sender().ID, rendered)
				shouldDeleteInput = true
			}
			return true, err
		}
		session.ID = trimmed
		session.Step = "cron"
		rendered, err := b.renderScheduleWizardPanel(c, session, scheduleWizardPrompt(session))
		if err == nil {
			b.scheduleWizard.Set(c.Sender().ID, rendered)
			shouldDeleteInput = true
		}
		return true, err
	case "cron":
		if _, err := cron.ParseStandard(trimmed); err != nil {
			rendered, renderErr := b.renderScheduleWizardPanel(c, session, "❌ Cron 表达式无效。\n\n"+scheduleWizardPrompt(session))
			if renderErr == nil {
				b.scheduleWizard.Set(c.Sender().ID, rendered)
				shouldDeleteInput = true
			}
			return true, renderErr
		}
		session.CronExpr = trimmed
		session.Step = "timezone"
		rendered, err := b.renderScheduleWizardPanel(c, session, scheduleWizardPrompt(session))
		if err == nil {
			b.scheduleWizard.Set(c.Sender().ID, rendered)
			shouldDeleteInput = true
		}
		return true, err
	case "timezone":
		if _, err := time.LoadLocation(trimmed); err != nil {
			rendered, renderErr := b.renderScheduleWizardPanel(c, session, "❌ 时区无效。\n\n"+scheduleWizardPrompt(session))
			if renderErr == nil {
				b.scheduleWizard.Set(c.Sender().ID, rendered)
				shouldDeleteInput = true
			}
			return true, renderErr
		}
		session.Timezone = trimmed
		session.Step = "prompt"
		rendered, err := b.renderScheduleWizardPanel(c, session, scheduleWizardPrompt(session))
		if err == nil {
			b.scheduleWizard.Set(c.Sender().ID, rendered)
			shouldDeleteInput = true
		}
		return true, err
	case "prompt":
		if trimmed == "" {
			rendered, renderErr := b.renderScheduleWizardPanel(c, session, "❌ Prompt 不能为空。\n\n"+scheduleWizardPrompt(session))
			if renderErr == nil {
				b.scheduleWizard.Set(c.Sender().ID, rendered)
				shouldDeleteInput = true
			}
			return true, renderErr
		}
		session.Prompt = text
		session.Step = "context"
		rendered, err := b.renderScheduleWizardPanel(c, session, scheduleWizardPrompt(session))
		if err == nil {
			b.scheduleWizard.Set(c.Sender().ID, rendered)
			shouldDeleteInput = true
		}
		return true, err
	case "context":
		contextValue, ok := parseScheduleWizardContext(trimmed)
		if !ok {
			rendered, renderErr := b.renderScheduleWizardPanel(c, session, "❌ context 输入无效。\n\n"+scheduleWizardPrompt(session))
			if renderErr == nil {
				b.scheduleWizard.Set(c.Sender().ID, rendered)
				shouldDeleteInput = true
			}
			return true, renderErr
		}

		snap := b.snapshot()
		if snap.tasks == nil {
			b.scheduleWizard.Clear(c.Sender().ID)
			b.cleanupScheduleWizardPanel(session)
			return true, c.Reply("⚠️ Schedule storage is unavailable.")
		}

		now := time.Now().UTC()
		task := ScheduledTask{
			ID:        session.ID,
			Prompt:    strings.TrimSpace(session.Prompt),
			CronExpr:  session.CronExpr,
			Timezone:  session.Timezone,
			Context:   contextValue,
			Enabled:   true,
			CreatedBy: c.Sender().ID,
			CreatedAt: now,
			UpdatedAt: now,
		}
		nextRun, err := computeNextRun(task, now)
		if err != nil {
			return true, c.Reply(fmt.Sprintf("❌ Failed to compute next run: %v", err))
		}
		task.NextRunAt = nextRun

		saved, created := snap.tasks.Upsert(c.Chat().ID, task)
		b.scheduleWizard.Clear(c.Sender().ID)
		shouldDeleteInput = true
		b.cleanupScheduleWizardPanel(session)

		actionLabel := "updated"
		if created {
			actionLabel = "created"
		}
		_, err = snap.tg.Send(c.Chat(), formatScheduledTaskUpsert(saved, actionLabel))
		return true, err
	}

	b.scheduleWizard.Clear(c.Sender().ID)
	b.cleanupScheduleWizardPanel(session)
	return true, c.Reply("⚠️ Schedule wizard state was invalid and has been reset. Please run /schedule_new again.")
}

func (b *Bot) handleScheduleListCommand(c tele.Context) error {
	if !b.isAllowed(c) {
		return nil
	}
	snap := b.snapshot()
	if snap.tasks == nil {
		return c.Reply("⚠️ Schedule storage is unavailable.")
	}
	return c.Reply(formatScheduledTaskList(snap.tasks.List(c.Chat().ID)))
}

func (b *Bot) handleSchedulePauseCommand(c tele.Context) error {
	return b.handleScheduleToggleCommand(c, false, strings.TrimSpace(c.Message().Payload))
}

func (b *Bot) handleScheduleResumeCommand(c tele.Context) error {
	return b.handleScheduleToggleCommand(c, true, strings.TrimSpace(c.Message().Payload))
}

func (b *Bot) handleScheduleDeleteCommandAlias(c tele.Context) error {
	return b.handleScheduleDeleteCommand(c, strings.TrimSpace(c.Message().Payload))
}

func (b *Bot) handleScheduleExampleCommand(c tele.Context) error {
	if !b.isAllowed(c) {
		return nil
	}
	return c.Reply(scheduleJSONExample())
}

func (b *Bot) handleScheduleToggleCommand(c tele.Context, enabled bool, id string) error {
	if !b.isAllowed(c) {
		return nil
	}
	if c.Sender() == nil || !b.isAdmin(c.Sender().ID) {
		if enabled {
			return c.Reply("🚫 Only admins can resume scheduled tasks.")
		}
		return c.Reply("🚫 Only admins can pause scheduled tasks.")
	}

	id = strings.TrimSpace(id)
	if !scheduleIDPattern.MatchString(id) {
		usage := "/schedule_pause <id>"
		if enabled {
			usage = "/schedule_resume <id>"
		}
		return c.Reply("⚠️ Please provide a valid schedule id.\nUsage: " + usage)
	}

	snap := b.snapshot()
	if snap.tasks == nil {
		return c.Reply("⚠️ Schedule storage is unavailable.")
	}
	task, found, err := snap.tasks.SetEnabled(c.Chat().ID, id, enabled, time.Now().UTC())
	if err != nil {
		return c.Reply(fmt.Sprintf("❌ Failed to update schedule: %v", err))
	}
	if !found {
		return c.Reply(fmt.Sprintf("⚠️ Schedule %q was not found in this chat.", id))
	}
	state := "paused"
	if enabled {
		state = "resumed"
	}
	return c.Reply(formatScheduledTaskState(task, state))
}

func (b *Bot) handleScheduleDeleteCommand(c tele.Context, id string) error {
	if !b.isAllowed(c) {
		return nil
	}
	if c.Sender() == nil || !b.isAdmin(c.Sender().ID) {
		return c.Reply("🚫 Only admins can delete scheduled tasks.")
	}

	id = strings.TrimSpace(id)
	if !scheduleIDPattern.MatchString(id) {
		return c.Reply("⚠️ Please provide a valid schedule id.\nUsage: /schedule_del <id>")
	}

	snap := b.snapshot()
	if snap.tasks == nil {
		return c.Reply("⚠️ Schedule storage is unavailable.")
	}
	if !snap.tasks.Delete(c.Chat().ID, id) {
		return c.Reply(fmt.Sprintf("⚠️ Schedule %q was not found in this chat.", id))
	}
	return c.Reply(fmt.Sprintf("✅ Deleted schedule %q.", id))
}

func firstArg(fields []string) string {
	if len(fields) < 2 {
		return ""
	}
	return fields[1]
}

func scheduleCommandHelp() string {
	return strings.Join([]string{
		"🗓 Schedule commands:",
		"/schedule help - show this help",
		"/schedule new - interactive schedule creation",
		"/schedule example - show JSON template",
		"/schedule list - list schedules in this chat",
		"/schedule pause <id> - pause a schedule (admin)",
		"/schedule resume <id> - resume a schedule (admin)",
		"/schedule delete <id> - delete a schedule (admin)",
		"",
		"Short aliases:",
		"/schedule_new",
		"/schedule_list",
		"/schedule_example",
		"/schedule_pause <id>",
		"/schedule_resume <id>",
		"/schedule_del <id>",
		"",
		"Creation and updates still use the JSON `schedule` message format.",
	}, "\n")
}

func scheduleJSONExample() string {
	return strings.Join([]string{
		"Send this JSON to create or update a schedule:",
		"",
		"{",
		`  "schedule": {`,
		`    "action": "upsert",`,
		`    "id": "morning-brief",`,
		`    "name": "每日早报",`,
		`    "prompt": "总结今天需要关注的技术新闻",`,
		`    "time": {`,
		`      "cron": "0 9 * * *",`,
		`      "timezone": "Asia/Shanghai"`,
		`    },`,
		`    "context": false,`,
		`    "enabled": true`,
		`  }`,
		"}",
		"",
		"`context=true` 时会把当前聊天的滑窗上下文和摘要一起带给 LLM。",
	}, "\n")
}

func scheduleWizardPrompt(session ScheduleWizardSession) string {
	switch session.Step {
	case "id":
		return strings.Join([]string{
			"🧭 创建定时任务 1/5：任务 ID",
			"说明：任务 ID 在当前聊天内必须唯一，只能使用字母、数字、`.`、`_`、`-`。",
			"示例：`morning-brief`",
			"请发送任务 ID，或发送 `cancel` 取消。",
		}, "\n")
	case "cron":
		return strings.Join([]string{
			"🧭 创建定时任务 2/5：Cron 表达式",
			"说明：使用标准 5 段格式：`分 时 日 月 星期`。",
			"示例：`0 9 * * *` 表示每天 9 点，`*/5 * * * *` 表示每 5 分钟一次。",
			"请发送 cron 表达式，或发送 `cancel` 取消。",
		}, "\n")
	case "timezone":
		return strings.Join([]string{
			"🧭 创建定时任务 3/5：时区",
			"说明：请使用 IANA 时区名。",
			"示例：`Asia/Shanghai`、`UTC`、`America/New_York`。",
			fmt.Sprintf("默认建议：`%s`", session.Timezone),
			"请发送 timezone，或发送 `cancel` 取消。",
		}, "\n")
	case "prompt":
		return strings.Join([]string{
			"🧭 创建定时任务 4/5：Prompt",
			"说明：这里填写定时发送给 LLM 的内容，可以是多行文本。",
			"示例：`总结今天需要关注的技术新闻`",
			"请直接发送 prompt，或发送 `cancel` 取消。",
		}, "\n")
	case "context":
		return strings.Join([]string{
			"🧭 创建定时任务 5/5：是否带上下文",
			"说明：`true` 会把当前聊天的滑窗上下文和摘要一起带给 LLM；`false` 只发送本任务的 prompt。",
			"可用输入：`true/false`、`yes/no`、`y/n`、`1/0`。",
			"请发送 context 值，或发送 `cancel` 取消。",
		}, "\n")
	default:
		return "⚠️ Unknown schedule wizard step."
	}
}

func parseScheduleWizardContext(input string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "true", "yes", "y", "1", "on":
		return true, true
	case "false", "no", "n", "0", "off", "":
		return false, true
	default:
		return false, false
	}
}

func (b *Bot) renderScheduleWizardPanel(c tele.Context, session ScheduleWizardSession, text string) (ScheduleWizardSession, error) {
	snap := b.snapshot()
	if session.PanelMessageID != 0 && session.PanelChatID != 0 {
		panel := &tele.Message{
			ID:   session.PanelMessageID,
			Chat: &tele.Chat{ID: session.PanelChatID},
		}
		if _, err := snap.tg.Edit(panel, text); err == nil {
			return session, nil
		}
	}

	msg, err := snap.tg.Send(c.Chat(), text)
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

func (b *Bot) cleanupScheduleWizardPanel(session ScheduleWizardSession) {
	if session.PanelMessageID != 0 && session.PanelChatID != 0 {
		b.deleteScheduleWizardMessage(&tele.Message{
			ID:   session.PanelMessageID,
			Chat: &tele.Chat{ID: session.PanelChatID},
		})
	}
	if session.TriggerMessageID != 0 && session.ChatID != 0 {
		b.deleteScheduleWizardMessage(&tele.Message{
			ID:   session.TriggerMessageID,
			Chat: &tele.Chat{ID: session.ChatID},
		})
	}
}

func (b *Bot) deleteScheduleWizardMessage(msg *tele.Message) {
	if msg == nil {
		return
	}
	snap := b.snapshot()
	if err := snap.tg.Delete(msg); err != nil {
		errText := strings.ToLower(err.Error())
		if !strings.Contains(errText, "message to delete not found") {
			log.Printf("[schedule] failed to delete wizard message %d: %v", msg.ID, err)
		}
	}
}

func formatScheduledTaskUpsert(task ScheduledTask, actionLabel string) string {
	lines := []string{
		fmt.Sprintf("✅ Schedule %s: %q", actionLabel, task.ID),
		fmt.Sprintf("Cron: %s", task.CronExpr),
		fmt.Sprintf("Timezone: %s", task.Timezone),
		fmt.Sprintf("Context: %t", task.Context),
		fmt.Sprintf("Enabled: %t", task.Enabled),
	}
	if task.Name != "" {
		lines = append(lines, fmt.Sprintf("Name: %s", task.Name))
	}
	if !task.NextRunAt.IsZero() {
		lines = append(lines, "Next run: "+formatTaskTime(task.NextRunAt, task.Timezone))
	}
	return strings.Join(lines, "\n")
}

func formatScheduledTaskState(task ScheduledTask, state string) string {
	lines := []string{
		fmt.Sprintf("✅ Schedule %q %s.", task.ID, state),
		fmt.Sprintf("Enabled: %t", task.Enabled),
	}
	if !task.NextRunAt.IsZero() {
		lines = append(lines, "Next run: "+formatTaskTime(task.NextRunAt, task.Timezone))
	}
	return strings.Join(lines, "\n")
}

func formatScheduledTaskList(tasks []ScheduledTask) string {
	if len(tasks) == 0 {
		return "📭 No scheduled tasks in this chat."
	}

	var lines []string
	lines = append(lines, "🗓 Scheduled tasks for this chat:")
	for _, task := range tasks {
		status := "paused"
		if task.Enabled {
			status = "enabled"
		}
		line := fmt.Sprintf("- %s (%s, %s)", task.ID, status, task.Timezone)
		if task.Name != "" {
			line = fmt.Sprintf("- %s [%s] (%s, %s)", task.ID, task.Name, status, task.Timezone)
		}
		lines = append(lines, line)
		lines = append(lines, fmt.Sprintf("  cron: %s", task.CronExpr))
		lines = append(lines, fmt.Sprintf("  context: %t", task.Context))
		if !task.NextRunAt.IsZero() {
			lines = append(lines, "  next: "+formatTaskTime(task.NextRunAt, task.Timezone))
		}
		if task.LastError != "" {
			lines = append(lines, "  last_error: "+task.LastError)
		}
	}
	return strings.Join(lines, "\n")
}

func formatTaskTime(ts time.Time, timezone string) string {
	if ts.IsZero() {
		return "n/a"
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.UTC
	}
	return ts.In(loc).Format("2006-01-02 15:04:05 MST")
}
