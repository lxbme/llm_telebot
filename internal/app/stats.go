package app

import (
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

const (
	defaultStatsQueueSize     = 2048
	defaultStatsShardCount    = 32
	defaultStatsFlushInterval = time.Second
	usageProviderOpenAICompat = "openai_compatible"
)

var requestSequence atomic.Uint64

type UsageGranularity string

const (
	UsageGranularityMinute UsageGranularity = "minute"
	UsageGranularityDaily  UsageGranularity = "daily"
)

type UsageScope string

const (
	UsageScopeChat     UsageScope = "chat"
	UsageScopeUser     UsageScope = "user"
	UsageScopeChatUser UsageScope = "chat_user"
)

type UsageCallType string

const (
	UsageCallMainChat       UsageCallType = "main_chat"
	UsageCallToolRound      UsageCallType = "tool_round"
	UsageCallRelevanceCheck UsageCallType = "relevance_check"
	UsageCallSummary        UsageCallType = "summary"
	UsageCallProfileExtract UsageCallType = "profile_extract"
	UsageCallStickerModel   UsageCallType = "sticker_strategy"
)

type UsageContext struct {
	ChatID    int64
	UserID    int64
	MessageID int
	RequestID string
}

func newUsageContext(chatID, userID int64, messageID int) UsageContext {
	return UsageContext{
		ChatID:    chatID,
		UserID:    userID,
		MessageID: messageID,
		RequestID: nextRequestID(),
	}
}

type UsageEvent struct {
	Timestamp        time.Time     `json:"timestamp"`
	ChatID           int64         `json:"chat_id,omitempty"`
	UserID           int64         `json:"user_id,omitempty"`
	Model            string        `json:"model,omitempty"`
	Provider         string        `json:"provider,omitempty"`
	CallType         UsageCallType `json:"call_type,omitempty"`
	Success          bool          `json:"success"`
	LatencyMs        int64         `json:"latency_ms"`
	PromptTokens     int64         `json:"prompt_tokens"`
	CompletionTokens int64         `json:"completion_tokens"`
	TotalTokens      int64         `json:"total_tokens"`
	RequestID        string        `json:"request_id,omitempty"`
	MessageID        int           `json:"message_id,omitempty"`
	ToolIterations   int           `json:"tool_iterations,omitempty"`
	Streamed         bool          `json:"streamed,omitempty"`
}

func (e *UsageEvent) normalize() {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	} else {
		e.Timestamp = e.Timestamp.UTC()
	}
	if e.Provider == "" {
		e.Provider = usageProviderOpenAICompat
	}
	if e.TotalTokens == 0 {
		e.TotalTokens = e.PromptTokens + e.CompletionTokens
	}
	if e.PromptTokens < 0 {
		e.PromptTokens = 0
	}
	if e.CompletionTokens < 0 {
		e.CompletionTokens = 0
	}
	if e.TotalTokens < 0 {
		e.TotalTokens = 0
	}
	if e.LatencyMs < 0 {
		e.LatencyMs = 0
	}
}

type UsageAggregate struct {
	RequestCount     int64     `json:"request_count"`
	SuccessCount     int64     `json:"success_count"`
	ErrorCount       int64     `json:"error_count"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	TotalTokens      int64     `json:"total_tokens"`
	TotalLatencyMs   int64     `json:"total_latency_ms"`
	LastSeenAt       time.Time `json:"last_seen_at"`
}

func (a *UsageAggregate) AddEvent(event UsageEvent) {
	a.RequestCount++
	if event.Success {
		a.SuccessCount++
	} else {
		a.ErrorCount++
	}
	a.PromptTokens += event.PromptTokens
	a.CompletionTokens += event.CompletionTokens
	a.TotalTokens += event.TotalTokens
	a.TotalLatencyMs += event.LatencyMs
	if a.LastSeenAt.IsZero() || event.Timestamp.After(a.LastSeenAt) {
		a.LastSeenAt = event.Timestamp
	}
}

func (a *UsageAggregate) Merge(other UsageAggregate) {
	a.RequestCount += other.RequestCount
	a.SuccessCount += other.SuccessCount
	a.ErrorCount += other.ErrorCount
	a.PromptTokens += other.PromptTokens
	a.CompletionTokens += other.CompletionTokens
	a.TotalTokens += other.TotalTokens
	a.TotalLatencyMs += other.TotalLatencyMs
	if a.LastSeenAt.IsZero() || other.LastSeenAt.After(a.LastSeenAt) {
		a.LastSeenAt = other.LastSeenAt
	}
}

func (a UsageAggregate) clone() UsageAggregate {
	return a
}

type UsagePoint struct {
	Stamp     string         `json:"stamp"`
	Time      time.Time      `json:"time"`
	Aggregate UsageAggregate `json:"aggregate"`
}

type UsageOverview struct {
	Points []UsagePoint   `json:"points"`
	Total  UsageAggregate `json:"total"`
}

type UsageQuery struct {
	Granularity UsageGranularity
	Scope       UsageScope
	ChatID      int64
	UserID      int64
	From        time.Time
	To          time.Time
}

type UsageRecentMeta struct {
	Scope         UsageScope    `json:"scope"`
	ChatID        int64         `json:"chat_id,omitempty"`
	UserID        int64         `json:"user_id,omitempty"`
	LastSeenAt    time.Time     `json:"last_seen_at"`
	LastModel     string        `json:"last_model,omitempty"`
	LastCallType  UsageCallType `json:"last_call_type,omitempty"`
	LastRequestID string        `json:"last_request_id,omitempty"`
}

type statsShard struct {
	mu      sync.Mutex
	pending map[string]*UsageAggregate
}

type statsKeyLock struct {
	locks sync.Map
}

func (l *statsKeyLock) TryLock(key string) bool {
	_, loaded := l.locks.LoadOrStore(key, struct{}{})
	return !loaded
}

func (l *statsKeyLock) Unlock(key string) {
	l.locks.Delete(key)
}

type StatsManager struct {
	dbMu sync.RWMutex
	db   *ChatDB

	queue chan UsageEvent

	shards []statsShard

	recentMu    sync.RWMutex
	recentMeta  map[string]UsageRecentMeta
	dirtyRecent map[string]UsageRecentMeta

	flushMu   sync.Mutex
	flushKeys statsKeyLock

	closed atomic.Bool
	wg     sync.WaitGroup
}

func NewStatsManager(db *ChatDB) *StatsManager {
	m := &StatsManager{
		db:          db,
		queue:       make(chan UsageEvent, defaultStatsQueueSize),
		shards:      make([]statsShard, defaultStatsShardCount),
		recentMeta:  make(map[string]UsageRecentMeta),
		dirtyRecent: make(map[string]UsageRecentMeta),
	}
	for i := range m.shards {
		m.shards[i].pending = make(map[string]*UsageAggregate)
	}

	m.wg.Add(2)
	go m.runAggregator()
	go m.runFlusher()
	return m
}

func (m *StatsManager) runAggregator() {
	defer m.wg.Done()
	for event := range m.queue {
		m.applyEvent(event)
	}
}

func (m *StatsManager) runFlusher() {
	defer m.wg.Done()
	ticker := time.NewTicker(defaultStatsFlushInterval)
	defer ticker.Stop()
	for range ticker.C {
		if m.closed.Load() {
			return
		}
		if err := m.Flush(); err != nil {
			log.Printf("[stats] flush error: %v", err)
		}
	}
}

func (m *StatsManager) currentDB() *ChatDB {
	m.dbMu.RLock()
	defer m.dbMu.RUnlock()
	return m.db
}

func (m *StatsManager) RebindDB(db *ChatDB) {
	m.dbMu.Lock()
	m.db = db
	m.dbMu.Unlock()
}

func (m *StatsManager) Record(event UsageEvent) {
	if m == nil || m.closed.Load() {
		return
	}
	event.normalize()
	defer func() {
		_ = recover()
	}()
	select {
	case m.queue <- event:
	default:
		// Keep the bot path non-blocking; only fall back to direct shard updates
		// when the queue is saturated.
		m.applyEvent(event)
	}
}

func (m *StatsManager) applyEvent(event UsageEvent) {
	scopes := usageScopesForEvent(event)
	for _, granularity := range []UsageGranularity{UsageGranularityMinute, UsageGranularityDaily} {
		for _, scope := range scopes {
			pendingKey, ok := usagePendingKey(granularity, scope, event.ChatID, event.UserID, event.Timestamp)
			if !ok {
				continue
			}
			shard := m.shardFor(pendingKey)
			shard.mu.Lock()
			agg := shard.pending[pendingKey]
			if agg == nil {
				agg = &UsageAggregate{}
				shard.pending[pendingKey] = agg
			}
			agg.AddEvent(event)
			shard.mu.Unlock()

			m.touchRecent(scope, event)
		}
	}
}

func (m *StatsManager) shardFor(key string) *statsShard {
	var sum uint32
	for i := 0; i < len(key); i++ {
		sum = sum*31 + uint32(key[i])
	}
	return &m.shards[sum%uint32(len(m.shards))]
}

func (m *StatsManager) touchRecent(scope UsageScope, event UsageEvent) {
	key, ok := usageRecentKey(scope, event.ChatID, event.UserID)
	if !ok {
		return
	}
	meta := UsageRecentMeta{
		Scope:         scope,
		ChatID:        event.ChatID,
		UserID:        event.UserID,
		LastSeenAt:    event.Timestamp,
		LastModel:     event.Model,
		LastCallType:  event.CallType,
		LastRequestID: event.RequestID,
	}

	m.recentMu.Lock()
	defer m.recentMu.Unlock()
	if existing, ok := m.recentMeta[key]; ok && existing.LastSeenAt.After(meta.LastSeenAt) {
		return
	}
	m.recentMeta[key] = meta
	m.dirtyRecent[key] = meta
}

func (m *StatsManager) snapshotPending() map[string]UsageAggregate {
	out := make(map[string]UsageAggregate)
	for i := range m.shards {
		shard := &m.shards[i]
		shard.mu.Lock()
		for key, agg := range shard.pending {
			out[key] = agg.clone()
		}
		shard.pending = make(map[string]*UsageAggregate)
		shard.mu.Unlock()
	}
	return out
}

func (m *StatsManager) restorePending(items map[string]UsageAggregate) {
	for key, agg := range items {
		shard := m.shardFor(key)
		shard.mu.Lock()
		current := shard.pending[key]
		if current == nil {
			copyAgg := agg
			shard.pending[key] = &copyAgg
		} else {
			current.Merge(agg)
		}
		shard.mu.Unlock()
	}
}

func (m *StatsManager) snapshotDirtyRecent() map[string]UsageRecentMeta {
	m.recentMu.Lock()
	defer m.recentMu.Unlock()
	out := make(map[string]UsageRecentMeta, len(m.dirtyRecent))
	for key, meta := range m.dirtyRecent {
		out[key] = meta
	}
	m.dirtyRecent = make(map[string]UsageRecentMeta)
	return out
}

func (m *StatsManager) restoreDirtyRecent(items map[string]UsageRecentMeta) {
	if len(items) == 0 {
		return
	}
	m.recentMu.Lock()
	defer m.recentMu.Unlock()
	for key, meta := range items {
		if existing, ok := m.dirtyRecent[key]; ok && existing.LastSeenAt.After(meta.LastSeenAt) {
			continue
		}
		m.dirtyRecent[key] = meta
	}
}

func (m *StatsManager) Flush() error {
	if m == nil {
		return nil
	}
	m.flushMu.Lock()
	defer m.flushMu.Unlock()

	db := m.currentDB()
	if db == nil {
		return nil
	}

	pending := m.snapshotPending()
	dirtyRecent := m.snapshotDirtyRecent()
	if len(pending) == 0 && len(dirtyRecent) == 0 {
		return nil
	}

	minuteRows := make(map[string]UsageAggregate)
	dailyRows := make(map[string]UsageAggregate)
	recentRows := make(map[string]UsageRecentMeta)
	restorePending := make(map[string]UsageAggregate)
	restoreRecent := make(map[string]UsageRecentMeta)
	lockedKeys := make([]string, 0, len(pending)+len(dirtyRecent))

	for pendingKey, agg := range pending {
		granularity, dbKey, ok := usageStorageKeyFromPending(pendingKey)
		if !ok {
			continue
		}
		lockKey := string(granularity) + "|" + dbKey
		if !m.flushKeys.TryLock(lockKey) {
			restorePending[pendingKey] = agg
			continue
		}
		lockedKeys = append(lockedKeys, lockKey)
		switch granularity {
		case UsageGranularityMinute:
			minuteRows[dbKey] = agg
		case UsageGranularityDaily:
			dailyRows[dbKey] = agg
		}
	}

	for key, meta := range dirtyRecent {
		lockKey := "recent|" + key
		if !m.flushKeys.TryLock(lockKey) {
			restoreRecent[key] = meta
			continue
		}
		lockedKeys = append(lockedKeys, lockKey)
		recentRows[key] = meta
	}

	defer func() {
		for _, key := range lockedKeys {
			m.flushKeys.Unlock(key)
		}
	}()

	if len(minuteRows) == 0 && len(dailyRows) == 0 && len(recentRows) == 0 {
		m.restorePending(restorePending)
		m.restoreDirtyRecent(restoreRecent)
		return nil
	}

	if err := db.MergeUsageAggregates(minuteRows, dailyRows, recentRows); err != nil {
		m.restorePending(pending)
		m.restoreDirtyRecent(dirtyRecent)
		return err
	}

	if len(restorePending) > 0 {
		m.restorePending(restorePending)
	}
	if len(restoreRecent) > 0 {
		m.restoreDirtyRecent(restoreRecent)
	}
	return nil
}

func (m *StatsManager) Close() error {
	if m == nil {
		return nil
	}
	if !m.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(m.queue)
	m.wg.Wait()
	return m.Flush()
}

func (m *StatsManager) QueryUsage(query UsageQuery) (UsageOverview, error) {
	if m == nil {
		return UsageOverview{}, nil
	}
	if query.Granularity == "" {
		query.Granularity = UsageGranularityMinute
	}
	if query.From.IsZero() {
		query.From = time.Now().UTC().Add(-15 * time.Minute)
	}
	if query.To.IsZero() {
		query.To = time.Now().UTC()
	}
	query.From = query.From.UTC()
	query.To = query.To.UTC()

	pointsMap := make(map[string]UsagePoint)
	db := m.currentDB()
	if db != nil {
		rows, err := db.QueryUsageAggregates(query)
		if err != nil {
			return UsageOverview{}, err
		}
		for _, row := range rows {
			pointsMap[row.Stamp] = row
		}
	}

	for stamp, agg := range m.snapshotPendingForQuery(query) {
		pointTime, err := usageTimeFromStamp(query.Granularity, stamp)
		if err != nil {
			continue
		}
		point := pointsMap[stamp]
		point.Stamp = stamp
		point.Time = pointTime
		point.Aggregate.Merge(agg)
		pointsMap[stamp] = point
	}

	points := make([]UsagePoint, 0, len(pointsMap))
	var total UsageAggregate
	for _, point := range pointsMap {
		points = append(points, point)
		total.Merge(point.Aggregate)
	}
	sort.Slice(points, func(i, j int) bool {
		return points[i].Time.Before(points[j].Time)
	})
	return UsageOverview{Points: points, Total: total}, nil
}

func (m *StatsManager) snapshotPendingForQuery(query UsageQuery) map[string]UsageAggregate {
	out := make(map[string]UsageAggregate)
	for i := range m.shards {
		shard := &m.shards[i]
		shard.mu.Lock()
		for pendingKey, agg := range shard.pending {
			granularity, scope, chatID, userID, stamp, ok := parseUsagePendingKey(pendingKey)
			if !ok || granularity != query.Granularity {
				continue
			}
			if scope != query.Scope || chatID != query.ChatID || userID != query.UserID {
				continue
			}
			pointTime, err := usageTimeFromStamp(granularity, stamp)
			if err != nil {
				continue
			}
			if pointTime.Before(query.From) || pointTime.After(query.To) {
				continue
			}
			existing := out[stamp]
			existing.Merge(agg.clone())
			out[stamp] = existing
		}
		shard.mu.Unlock()
	}
	return out
}

func (m *StatsManager) ListRecent(scope UsageScope, limit int) ([]UsageRecentMeta, error) {
	if limit <= 0 {
		limit = 20
	}
	items := make(map[string]UsageRecentMeta)

	db := m.currentDB()
	if db != nil {
		rows, err := db.LoadRecentUsageMeta(scope, 0)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			key, ok := usageRecentKey(row.Scope, row.ChatID, row.UserID)
			if ok {
				items[key] = row
			}
		}
	}

	m.recentMu.RLock()
	for key, meta := range m.recentMeta {
		if scope != "" && meta.Scope != scope {
			continue
		}
		if existing, ok := items[key]; !ok || existing.LastSeenAt.Before(meta.LastSeenAt) {
			items[key] = meta
		}
	}
	m.recentMu.RUnlock()

	list := make([]UsageRecentMeta, 0, len(items))
	for _, item := range items {
		list = append(list, item)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].LastSeenAt.After(list[j].LastSeenAt)
	})
	if len(list) > limit {
		list = list[:limit]
	}
	return list, nil
}

func usageEvent(
	ctx UsageContext,
	callType UsageCallType,
	model string,
	streamed bool,
	toolIterations int,
	started time.Time,
	usage *openai.Usage,
	success bool,
) UsageEvent {
	event := UsageEvent{
		Timestamp:      time.Now().UTC(),
		ChatID:         ctx.ChatID,
		UserID:         ctx.UserID,
		Model:          strings.TrimSpace(model),
		Provider:       usageProviderOpenAICompat,
		CallType:       callType,
		Success:        success,
		LatencyMs:      time.Since(started).Milliseconds(),
		RequestID:      ctx.RequestID,
		MessageID:      ctx.MessageID,
		ToolIterations: toolIterations,
		Streamed:       streamed,
	}
	if usage != nil {
		event.PromptTokens = int64(usage.PromptTokens)
		event.CompletionTokens = int64(usage.CompletionTokens)
		event.TotalTokens = int64(usage.TotalTokens)
	}
	event.normalize()
	return event
}

func usageScopesForEvent(event UsageEvent) []UsageScope {
	scopes := make([]UsageScope, 0, 3)
	if event.ChatID != 0 {
		scopes = append(scopes, UsageScopeChat)
	}
	if event.UserID != 0 {
		scopes = append(scopes, UsageScopeUser)
	}
	if event.ChatID != 0 && event.UserID != 0 {
		scopes = append(scopes, UsageScopeChatUser)
	}
	return scopes
}

func usagePendingKey(granularity UsageGranularity, scope UsageScope, chatID, userID int64, ts time.Time) (string, bool) {
	entity, ok := usageEntity(scope, chatID, userID)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%s|%s|%s|%s", granularity, scope, entity, usageStamp(granularity, ts)), true
}

func parseUsagePendingKey(key string) (UsageGranularity, UsageScope, int64, int64, string, bool) {
	parts := strings.SplitN(key, "|", 4)
	if len(parts) != 4 {
		return "", "", 0, 0, "", false
	}
	scope := UsageScope(parts[1])
	chatID, userID, ok := parseUsageEntity(scope, parts[2])
	if !ok {
		return "", "", 0, 0, "", false
	}
	return UsageGranularity(parts[0]), scope, chatID, userID, parts[3], true
}

func usageStorageKeyFromPending(key string) (UsageGranularity, string, bool) {
	granularity, scope, chatID, userID, stamp, ok := parseUsagePendingKey(key)
	if !ok {
		return "", "", false
	}
	entity, ok := usageEntity(scope, chatID, userID)
	if !ok {
		return "", "", false
	}
	return granularity, fmt.Sprintf("%s|%s|%s", scope, entity, stamp), true
}

func usageEntity(scope UsageScope, chatID, userID int64) (string, bool) {
	switch scope {
	case UsageScopeChat:
		if chatID == 0 {
			return "", false
		}
		return strconv.FormatInt(chatID, 10), true
	case UsageScopeUser:
		if userID == 0 {
			return "", false
		}
		return strconv.FormatInt(userID, 10), true
	case UsageScopeChatUser:
		if chatID == 0 || userID == 0 {
			return "", false
		}
		return fmt.Sprintf("%d:%d", chatID, userID), true
	default:
		return "", false
	}
}

func parseUsageEntity(scope UsageScope, entity string) (int64, int64, bool) {
	switch scope {
	case UsageScopeChat:
		chatID, err := strconv.ParseInt(entity, 10, 64)
		return chatID, 0, err == nil
	case UsageScopeUser:
		userID, err := strconv.ParseInt(entity, 10, 64)
		return 0, userID, err == nil
	case UsageScopeChatUser:
		parts := strings.SplitN(entity, ":", 2)
		if len(parts) != 2 {
			return 0, 0, false
		}
		chatID, err1 := strconv.ParseInt(parts[0], 10, 64)
		userID, err2 := strconv.ParseInt(parts[1], 10, 64)
		return chatID, userID, err1 == nil && err2 == nil
	default:
		return 0, 0, false
	}
}

func usageRecentKey(scope UsageScope, chatID, userID int64) (string, bool) {
	entity, ok := usageEntity(scope, chatID, userID)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%s|%s", scope, entity), true
}

func usageStamp(granularity UsageGranularity, ts time.Time) string {
	ts = ts.UTC()
	switch granularity {
	case UsageGranularityDaily:
		return ts.Format("20060102")
	default:
		return ts.Format("200601021504")
	}
}

func usageTimeFromStamp(granularity UsageGranularity, stamp string) (time.Time, error) {
	layout := "200601021504"
	if granularity == UsageGranularityDaily {
		layout = "20060102"
	}
	return time.ParseInLocation(layout, stamp, time.UTC)
}

func nextRequestID() string {
	seq := requestSequence.Add(1)
	return strconv.FormatInt(time.Now().UTC().UnixNano(), 36) + "-" + strconv.FormatUint(seq, 36)
}
