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

// ChatDB wraps a bbolt database shared by HistoryStore and SummaryStore for
// persistent chat storage that survives restarts.
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
		} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
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

// SaveHistory persists the sliding-window message history for a chat.
func (c *ChatDB) SaveHistory(chatID int64, msgs []openai.ChatCompletionMessage) {
	if c == nil {
		return
	}
	data, err := json.Marshal(msgs)
	if err != nil {
		log.Printf("[chat-db] marshal history error for chat %d: %v", chatID, err)
		return
	}
	if err := c.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(historyBucket).Put(chatIDKey(chatID), data)
	}); err != nil {
		log.Printf("[chat-db] save history error for chat %d: %v", chatID, err)
	}
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

// DeleteHistory removes persisted history for a chat.
func (c *ChatDB) DeleteHistory(chatID int64) {
	if c == nil {
		return
	}
	c.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(historyBucket).Delete(chatIDKey(chatID))
	})
}

// ─── Overflow Persistence ────────────────────────────────────────────────────

// SaveOverflow persists the overflow buffer for a chat.
// An empty slice deletes the key.
func (c *ChatDB) SaveOverflow(chatID int64, msgs []openai.ChatCompletionMessage) {
	if c == nil {
		return
	}
	if len(msgs) == 0 {
		c.db.Update(func(tx *bolt.Tx) error {
			return tx.Bucket(overflowBucket).Delete(chatIDKey(chatID))
		})
		return
	}
	data, err := json.Marshal(msgs)
	if err != nil {
		log.Printf("[chat-db] marshal overflow error for chat %d: %v", chatID, err)
		return
	}
	if err := c.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(overflowBucket).Put(chatIDKey(chatID), data)
	}); err != nil {
		log.Printf("[chat-db] save overflow error for chat %d: %v", chatID, err)
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

// DeleteOverflow removes persisted overflow for a chat.
func (c *ChatDB) DeleteOverflow(chatID int64) {
	if c == nil {
		return
	}
	c.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(overflowBucket).Delete(chatIDKey(chatID))
	})
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

// SaveReminders persists the reminder list for a user. An empty slice deletes
// the key. Reminders are keyed by userID (not chatID) so the private-chat
// "show me all my reminders across chats" listing is a single bucket lookup.
func (c *ChatDB) SaveReminders(userID int64, reminders []Reminder) {
	if c == nil {
		return
	}
	if len(reminders) == 0 {
		c.DeleteRemindersForUser(userID)
		return
	}
	data, err := json.Marshal(reminders)
	if err != nil {
		log.Printf("[chat-db] marshal reminders error for user %d: %v", userID, err)
		return
	}
	if err := c.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(reminderBucket).Put(userIDKey(userID), data)
	}); err != nil {
		log.Printf("[chat-db] save reminders error for user %d: %v", userID, err)
	}
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

// DeleteRemindersForUser removes persisted reminders owned by a user.
func (c *ChatDB) DeleteRemindersForUser(userID int64) {
	if c == nil {
		return
	}
	c.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(reminderBucket).Delete(userIDKey(userID))
	})
}
