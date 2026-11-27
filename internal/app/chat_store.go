package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"
	bolt "go.etcd.io/bbolt"
)

var (
	historyBucket     = []byte("chat_history")
	overflowBucket    = []byte("chat_overflow")
	summaryBucket     = []byte("chat_summaries")
	scheduleBucket    = []byte("chat_schedules")
	reminderBucket    = []byte("user_reminders")
	usageMinuteBucket = []byte("usage_minute")
	usageDailyBucket  = []byte("usage_daily")
	usageRecentBucket = []byte("usage_recent_meta")
)

// ChatDB wraps the bbolt database shared by the persistent chat stores.
type ChatDB struct {
	db *bolt.DB
}

// OpenChatDB opens (or creates) the bbolt database for chat persistence.
func OpenChatDB(path string) (*ChatDB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("create chat db dir: %w", err)
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open chat db: %w", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{
			historyBucket,
			overflowBucket,
			summaryBucket,
			scheduleBucket,
			reminderBucket,
			usageMinuteBucket,
			usageDailyBucket,
			usageRecentBucket,
			memoryEntryBucket,
			memoryEventBucket,
			engagementBucket,
			engagementRequestsBucket,
			engagementWorkBucket,
			engagementRevisionsBucket,
			conversationPreferenceBucket,
			chatterBucket,
		} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		if err := pruneLegacyEngagementRevisions(tx); err != nil {
			return err
		}
		return pruneLegacyChatterRevisions(tx.Bucket(chatterBucket))
	}); err != nil {
		db.Close()
		return nil, fmt.Errorf("init chat db buckets: %w", err)
	}
	return &ChatDB{db: db}, nil
}

// Close shuts down the underlying bbolt database.
func (c *ChatDB) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	return c.db.Close()
}

// chatIDKey converts a chat ID to a bbolt key.
func chatIDKey(chatID int64) []byte {
	return []byte(strconv.FormatInt(chatID, 10))
}

// ─── History Persistence ─────────────────────────────────────────────────────

// migrateDB copies the in-memory history, overflow and removals before redirecting writes.
func (s *HistoryStore) migrateDB(db *ChatDB) error {
	return migrateChatDB(db, s, nil)
}

func migrateChatDB(db *ChatDB, history *HistoryStore, memory *MemoryBook, summaryStores ...*SummaryStore) error {
	var summaries *SummaryStore
	if len(summaryStores) > 0 {
		summaries = summaryStores[0]
	}
	return migrateChatStores(db, history, summaries, memory, nil)
}

// migrateChatStores commits all stores before rebinding any handle. Acquire
// the history, summary, memory, engagement and reminder locks in that order.
func migrateChatStores(db *ChatDB, history *HistoryStore, summaries *SummaryStore, memory *MemoryBook, engagements *EngagementStore, reminderStores ...*ReminderStore) error {
	var reminders *ReminderStore
	if len(reminderStores) > 0 {
		reminders = reminderStores[0]
	}
	if history != nil {
		history.mu.Lock()
		defer history.mu.Unlock()
	}
	if summaries != nil {
		summaries.mu.Lock()
		defer summaries.mu.Unlock()
	}
	if memory != nil {
		memory.mu.Lock()
		defer memory.mu.Unlock()
	}
	if engagements != nil {
		engagements.mu.Lock()
		defer engagements.mu.Unlock()
	}
	if reminders != nil {
		reminders.mu.Lock()
		defer reminders.mu.Unlock()
	}

	if err := db.db.Update(func(tx *bolt.Tx) error {
		if history != nil {
			if err := history.copyTo(tx); err != nil {
				return fmt.Errorf("migrate chat history: %w", err)
			}
		}
		if summaries != nil {
			for chatID, summary := range summaries.summaries {
				if err := tx.Bucket(summaryBucket).Put(chatIDKey(chatID), []byte(summary)); err != nil {
					return fmt.Errorf("migrate chat summaries: %w", err)
				}
			}
		}
		if memory != nil && memory.db != nil && memory.db != db {
			if err := memory.db.db.View(func(source *bolt.Tx) error {
				return copyMemoryBook(source, tx)
			}); err != nil {
				return fmt.Errorf("migrate memory book: %w", err)
			}
		}
		if engagements != nil && engagements.db != nil && engagements.db != db {
			if err := engagements.db.db.View(func(source *bolt.Tx) error {
				if err := copyEngagements(source, tx); err != nil {
					return err
				}
				if err := copyConversationPreferences(source, tx); err != nil {
					return err
				}
				return copyChatter(source, tx)
			}); err != nil {
				return fmt.Errorf("migrate engagements: %w", err)
			}
		}
		if reminders != nil {
			if err := reminders.copyTo(tx); err != nil {
				return fmt.Errorf("migrate reminders: %w", err)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if history != nil {
		history.db = db
	}
	if summaries != nil {
		summaries.db = db
	}
	if memory != nil {
		memory.db = db
	}
	if engagements != nil {
		engagements.db = db
	}
	if reminders != nil {
		reminders.db = db
	}
	return nil
}

func (s *HistoryStore) copyTo(tx *bolt.Tx) error {
	for _, state := range []struct {
		bucket   []byte
		messages map[int64][]openai.ChatCompletionMessage
	}{
		{historyBucket, s.history},
		{overflowBucket, s.overflow},
	} {
		bucket := tx.Bucket(state.bucket)
		for chatID, messages := range state.messages {
			if err := putChatMessages(bucket, chatIDKey(chatID), messages); err != nil {
				return err
			}
		}
	}
	return nil
}

// clearContext resets the three stores in one transaction. Empty persisted
// values carry the removal across later runtime database switches.
func (s *HistoryStore) clearContext(chatID int64, summaries *SummaryStore) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if summaries != nil {
		summaries.mu.Lock()
		defer summaries.mu.Unlock()
	}
	if s.db != nil {
		key := chatIDKey(chatID)
		if err := s.db.db.Update(func(tx *bolt.Tx) error {
			if err := putChatMessages(tx.Bucket(historyBucket), key, nil); err != nil {
				return err
			}
			if err := putChatMessages(tx.Bucket(overflowBucket), key, nil); err != nil {
				return err
			}
			if err := clearChatter(tx, key); err != nil {
				return err
			}
			return tx.Bucket(summaryBucket).Put(key, []byte{})
		}); err != nil {
			return err
		}
	}
	s.history[chatID], s.overflow[chatID] = nil, nil
	s.overflowRevision[chatID]++
	if summaries != nil {
		summaries.summaries[chatID] = ""
	}
	return nil
}

// persistChat saves the sliding window and any known overflow state in one transaction.
// Must be called with s.mu held.
func (s *HistoryStore) persistChat(chatID int64) {
	if s.db == nil {
		return
	}
	key := chatIDKey(chatID)
	if err := s.db.db.Update(func(tx *bolt.Tx) error {
		if err := putChatMessages(tx.Bucket(historyBucket), key, s.history[chatID]); err != nil {
			return err
		}
		if overflow, known := s.overflow[chatID]; known {
			return putChatMessages(tx.Bucket(overflowBucket), key, overflow)
		}
		return nil
	}); err != nil {
		log.Printf("[chat-db] save history and overflow error for chat %d: %v", chatID, err)
	}
}

func putChatMessages(bucket *bolt.Bucket, key []byte, messages []openai.ChatCompletionMessage) error {
	// A nil slice encodes as JSON null. The loaders retain the key with a
	// nil slice so a removal can overwrite older messages on the next rebind.
	data, err := json.Marshal(messages)
	if err != nil {
		return err
	}
	return bucket.Put(key, data)
}

// LoadAllHistory loads all persisted chat histories into a map.
func (c *ChatDB) LoadAllHistory() map[int64][]openai.ChatCompletionMessage {
	if c == nil {
		return nil
	}
	result := make(map[int64][]openai.ChatCompletionMessage)
	c.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(historyBucket)
		return b.ForEach(func(k, v []byte) error {
			chatID, err := strconv.ParseInt(string(k), 10, 64)
			if err != nil {
				return nil
			}
			var msgs []openai.ChatCompletionMessage
			if err := json.Unmarshal(v, &msgs); err != nil {
				log.Printf("[chat-db] unmarshal history for chat %d: %v", chatID, err)
				return nil
			}
			result[chatID] = msgs
			return nil
		})
	})
	return result
}

// ─── Overflow Persistence ────────────────────────────────────────────────────

type overflowBatch struct {
	messages []openai.ChatCompletionMessage
	revision uint64
}

// pendingOverflow selects the current prefix. Appends do not change the
// selected messages or revision, even if they reallocate the queue.
func (s *HistoryStore) pendingOverflow(chatID int64) overflowBatch {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return overflowBatch{s.overflow[chatID], s.overflowRevision[chatID]}
}

// commitSummary publishes the new summary and removes only the selected
// prefix. Later appends remain pending. The caller holds the role generation
// guard, and the lock order matches clearContext and migrateChatDB.
func (s *HistoryStore) commitSummary(chatID int64, summaries *SummaryStore, batch overflowBatch, existing, summary string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	summaries.mu.Lock()
	defer summaries.mu.Unlock()

	if len(batch.messages) == 0 || s.overflowRevision[chatID] != batch.revision || summaries.summaries[chatID] != existing {
		return false, nil
	}
	remaining := append([]openai.ChatCompletionMessage(nil), s.overflow[chatID][len(batch.messages):]...)
	if s.db != nil {
		key := chatIDKey(chatID)
		if err := s.db.db.Update(func(tx *bolt.Tx) error {
			if err := putChatMessages(tx.Bucket(overflowBucket), key, remaining); err != nil {
				return err
			}
			return tx.Bucket(summaryBucket).Put(key, []byte(summary))
		}); err != nil {
			return false, err
		}
	}
	s.overflow[chatID] = remaining
	s.overflowRevision[chatID]++
	summaries.summaries[chatID] = summary
	return true, nil
}

func (c *ChatDB) markOverflowDrained(chatID int64) {
	if c == nil {
		return
	}
	if err := c.db.Update(func(tx *bolt.Tx) error {
		return putChatMessages(tx.Bucket(overflowBucket), chatIDKey(chatID), nil)
	}); err != nil {
		log.Printf("[chat-db] mark overflow drained error for chat %d: %v", chatID, err)
	}
}

// LoadAllOverflow loads all persisted overflow buffers into a map.
func (c *ChatDB) LoadAllOverflow() map[int64][]openai.ChatCompletionMessage {
	if c == nil {
		return nil
	}
	result := make(map[int64][]openai.ChatCompletionMessage)
	c.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(overflowBucket)
		return b.ForEach(func(k, v []byte) error {
			chatID, err := strconv.ParseInt(string(k), 10, 64)
			if err != nil {
				return nil
			}
			var msgs []openai.ChatCompletionMessage
			if err := json.Unmarshal(v, &msgs); err != nil {
				log.Printf("[chat-db] unmarshal overflow for chat %d: %v", chatID, err)
				return nil
			}
			result[chatID] = msgs
			return nil
		})
	})
	return result
}

// ─── Summary Persistence ─────────────────────────────────────────────────────

// SaveSummary persists a conversation summary for a chat.
// An empty string deletes the key.
func (c *ChatDB) SaveSummary(chatID int64, summary string) {
	if c == nil {
		return
	}
	if summary == "" {
		c.db.Update(func(tx *bolt.Tx) error {
			return tx.Bucket(summaryBucket).Delete(chatIDKey(chatID))
		})
		return
	}
	if err := c.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(summaryBucket).Put(chatIDKey(chatID), []byte(summary))
	}); err != nil {
		log.Printf("[chat-db] save summary error for chat %d: %v", chatID, err)
	}
}

// LoadAllSummaries loads all persisted summaries into a map.
func (c *ChatDB) LoadAllSummaries() map[int64]string {
	if c == nil {
		return nil
	}
	result := make(map[int64]string)
	c.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(summaryBucket)
		return b.ForEach(func(k, v []byte) error {
			chatID, err := strconv.ParseInt(string(k), 10, 64)
			if err != nil {
				return nil
			}
			result[chatID] = string(v)
			return nil
		})
	})
	return result
}

// DeleteSummary removes a persisted summary for a chat.
func (c *ChatDB) DeleteSummary(chatID int64) {
	if c == nil {
		return
	}
	c.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(summaryBucket).Delete(chatIDKey(chatID))
	})
}

// ─── Schedule Persistence ────────────────────────────────────────────────────

// SaveSchedules persists the scheduled task list for a chat.
// An empty slice deletes the key.
func (c *ChatDB) SaveSchedules(chatID int64, tasks []ScheduledTask) {
	if c == nil {
		return
	}
	if len(tasks) == 0 {
		c.DeleteSchedules(chatID)
		return
	}
	data, err := json.Marshal(tasks)
	if err != nil {
		log.Printf("[chat-db] marshal schedules error for chat %d: %v", chatID, err)
		return
	}
	if err := c.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(scheduleBucket).Put(chatIDKey(chatID), data)
	}); err != nil {
		log.Printf("[chat-db] save schedules error for chat %d: %v", chatID, err)
	}
}

// LoadAllSchedules loads all persisted scheduled tasks into a map.
func (c *ChatDB) LoadAllSchedules() map[int64][]ScheduledTask {
	if c == nil {
		return nil
	}
	result := make(map[int64][]ScheduledTask)
	c.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(scheduleBucket)
		return b.ForEach(func(k, v []byte) error {
			chatID, err := strconv.ParseInt(string(k), 10, 64)
			if err != nil {
				return nil
			}
			var tasks []ScheduledTask
			if err := json.Unmarshal(v, &tasks); err != nil {
				log.Printf("[chat-db] unmarshal schedules for chat %d: %v", chatID, err)
				return nil
			}
			result[chatID] = tasks
			return nil
		})
	})
	return result
}

func (c *ChatDB) MergeUsageAggregates(minuteRows, dailyRows map[string]UsageAggregate, recentRows map[string]UsageRecentMeta) error {
	if c == nil {
		return nil
	}
	return c.db.Update(func(tx *bolt.Tx) error {
		if err := mergeUsageAggregateBucket(tx.Bucket(usageMinuteBucket), minuteRows); err != nil {
			return err
		}
		if err := mergeUsageAggregateBucket(tx.Bucket(usageDailyBucket), dailyRows); err != nil {
			return err
		}
		if err := mergeUsageRecentBucket(tx.Bucket(usageRecentBucket), recentRows); err != nil {
			return err
		}
		return nil
	})
}

func mergeUsageAggregateBucket(bucket *bolt.Bucket, rows map[string]UsageAggregate) error {
	for key, incoming := range rows {
		var current UsageAggregate
		raw := bucket.Get([]byte(key))
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &current); err != nil {
				return fmt.Errorf("decode usage aggregate %s: %w", key, err)
			}
		}
		current.Merge(incoming)
		encoded, err := json.Marshal(current)
		if err != nil {
			return fmt.Errorf("encode usage aggregate %s: %w", key, err)
		}
		if err := bucket.Put([]byte(key), encoded); err != nil {
			return fmt.Errorf("write usage aggregate %s: %w", key, err)
		}
	}
	return nil
}

func mergeUsageRecentBucket(bucket *bolt.Bucket, rows map[string]UsageRecentMeta) error {
	for key, incoming := range rows {
		var current UsageRecentMeta
		raw := bucket.Get([]byte(key))
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &current); err != nil {
				return fmt.Errorf("decode usage recent meta %s: %w", key, err)
			}
		}
		if current.LastSeenAt.After(incoming.LastSeenAt) {
			continue
		}
		encoded, err := json.Marshal(incoming)
		if err != nil {
			return fmt.Errorf("encode usage recent meta %s: %w", key, err)
		}
		if err := bucket.Put([]byte(key), encoded); err != nil {
			return fmt.Errorf("write usage recent meta %s: %w", key, err)
		}
	}
	return nil
}

func (c *ChatDB) QueryUsageAggregates(query UsageQuery) ([]UsagePoint, error) {
	if c == nil {
		return nil, nil
	}
	prefix, err := usageQueryPrefix(query.Scope, query.ChatID, query.UserID)
	if err != nil {
		return nil, err
	}
	if query.From.IsZero() {
		query.From = time.Unix(0, 0).UTC()
	}
	if query.To.IsZero() {
		query.To = time.Now().UTC()
	}

	result := make([]UsagePoint, 0)
	err = c.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(usageBucketName(query.Granularity))
		cursor := bucket.Cursor()
		prefixBytes := []byte(prefix)
		for key, value := cursor.Seek(prefixBytes); key != nil && bytes.HasPrefix(key, prefixBytes); key, value = cursor.Next() {
			_, _, stamp, ok := splitUsageStorageKey(string(key))
			if !ok {
				continue
			}
			pointTime, err := usageTimeFromStamp(query.Granularity, stamp)
			if err != nil || pointTime.Before(query.From.UTC()) || pointTime.After(query.To.UTC()) {
				continue
			}
			var aggregate UsageAggregate
			if err := json.Unmarshal(value, &aggregate); err != nil {
				return fmt.Errorf("decode usage aggregate %s: %w", string(key), err)
			}
			result = append(result, UsagePoint{
				Stamp:     stamp,
				Time:      pointTime,
				Aggregate: aggregate,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Time.Before(result[j].Time)
	})
	return result, nil
}

func (c *ChatDB) LoadRecentUsageMeta(scope UsageScope, limit int) ([]UsageRecentMeta, error) {
	if c == nil {
		return nil, nil
	}
	result := make([]UsageRecentMeta, 0)
	err := c.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(usageRecentBucket)
		cursor := bucket.Cursor()
		prefix := ""
		if scope != "" {
			prefix = string(scope) + "|"
		}
		prefixBytes := []byte(prefix)
		for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
			if prefix != "" && !bytes.HasPrefix(key, prefixBytes) {
				continue
			}
			var meta UsageRecentMeta
			if err := json.Unmarshal(value, &meta); err != nil {
				return fmt.Errorf("decode usage recent meta %s: %w", string(key), err)
			}
			result = append(result, meta)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].LastSeenAt.After(result[j].LastSeenAt)
	})
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func usageBucketName(granularity UsageGranularity) []byte {
	if granularity == UsageGranularityDaily {
		return usageDailyBucket
	}
	return usageMinuteBucket
}

// DeleteUsageAggregate removes a single entry from the stats buckets.
// Used by /quota_reset to purge both the in-memory counter and the
// persisted row so a restart doesn't resurrect the old counters.
func (c *ChatDB) DeleteUsageAggregate(granularity UsageGranularity, scope UsageScope, chatID, userID int64, stamp string) error {
	if c == nil {
		return nil
	}
	entity, ok := usageEntity(scope, chatID, userID)
	if !ok {
		return fmt.Errorf("invalid usage delete scope=%s chat_id=%d user_id=%d", scope, chatID, userID)
	}
	key := fmt.Sprintf("%s|%s|%s", scope, entity, stamp)
	return c.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(usageBucketName(granularity)).Delete([]byte(key))
	})
}

// ForEachDailyChatAggregate iterates every entry in the daily usage bucket
// whose scope is "chat" and whose stamp falls within [from, to] (UTC),
// invoking fn with the decoded aggregate. This powers the dashboard's
// "Today USD" / "Month USD" summaries without double-counting user and
// chat_user scopes.
func (c *ChatDB) ForEachDailyChatAggregate(from, to time.Time, fn func(UsageAggregate)) error {
	if c == nil || fn == nil {
		return nil
	}
	prefix := []byte(string(UsageScopeChat) + "|")
	return c.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(usageDailyBucket)
		cursor := bucket.Cursor()
		for key, value := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, value = cursor.Next() {
			_, _, stamp, ok := splitUsageStorageKey(string(key))
			if !ok {
				continue
			}
			pointTime, err := usageTimeFromStamp(UsageGranularityDaily, stamp)
			if err != nil || pointTime.Before(from) || pointTime.After(to) {
				continue
			}
			var agg UsageAggregate
			if err := json.Unmarshal(value, &agg); err != nil {
				return fmt.Errorf("decode daily chat aggregate %s: %w", string(key), err)
			}
			fn(agg)
		}
		return nil
	})
}

func usageQueryPrefix(scope UsageScope, chatID, userID int64) (string, error) {
	entity, ok := usageEntity(scope, chatID, userID)
	if !ok {
		return "", fmt.Errorf("invalid usage query scope=%s chat_id=%d user_id=%d", scope, chatID, userID)
	}
	return fmt.Sprintf("%s|%s|", scope, entity), nil
}

func splitUsageStorageKey(key string) (UsageScope, string, string, bool) {
	parts := strings.SplitN(key, "|", 3)
	if len(parts) != 3 {
		return "", "", "", false
	}
	return UsageScope(parts[0]), parts[1], parts[2], true
}

// DeleteSchedules removes persisted scheduled tasks for a chat.
func (c *ChatDB) DeleteSchedules(chatID int64) {
	if c == nil {
		return
	}
	c.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(scheduleBucket).Delete(chatIDKey(chatID))
	})
}

// ─── Reminder Persistence ────────────────────────────────────────────────────

// userIDKey converts a user ID to a bbolt key.
func userIDKey(userID int64) []byte {
	return []byte(strconv.FormatInt(userID, 10))
}

// putReminders replaces an owner's reminders inside the caller's transaction.
func putReminders(bucket *bolt.Bucket, key []byte, reminders []Reminder) error {
	if len(reminders) == 0 {
		return bucket.Delete(key)
	}
	sort.Slice(reminders, func(i, j int) bool { return reminders[i].ID < reminders[j].ID })
	data, err := json.Marshal(reminders)
	if err != nil {
		return err
	}
	return bucket.Put(key, data)
}

// LoadAllReminders loads all persisted reminders grouped by owner user ID.
func (c *ChatDB) LoadAllReminders() map[int64][]Reminder {
	if c == nil {
		return nil
	}
	result := make(map[int64][]Reminder)
	c.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(reminderBucket)
		return b.ForEach(func(k, v []byte) error {
			userID, err := strconv.ParseInt(string(k), 10, 64)
			if err != nil {
				return nil
			}
			var rems []Reminder
			if err := json.Unmarshal(v, &rems); err != nil {
				log.Printf("[chat-db] unmarshal reminders for user %d: %v", userID, err)
				return nil
			}
			result[userID] = rems
			return nil
		})
	})
	return result
}
