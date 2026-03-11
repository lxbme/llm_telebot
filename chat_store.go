package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	openai "github.com/sashabaranov/go-openai"
	bolt "go.etcd.io/bbolt"
)

var (
	historyBucket  = []byte("chat_history")
	overflowBucket = []byte("chat_overflow")
	summaryBucket  = []byte("chat_summaries")
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
		for _, b := range [][]byte{historyBucket, overflowBucket, summaryBucket} {
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
