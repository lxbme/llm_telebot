package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

// ─── Summary Store ───────────────────────────────────────────────────────────

// SummaryStore keeps per-chat conversation summaries backed by an optional
// persistent ChatDB. When the sliding window overflows, older messages are
// compressed into a running summary so the LLM retains long-term memory.
type SummaryStore struct {
	mu          sync.RWMutex
	summaries   map[int64]string // chatID → summary text
	summarizing sync.Map         // chatID → bool (prevent concurrent summarisations)
	db          *ChatDB          // optional persistent backend
}

// NewSummaryStore creates a summary store, optionally restoring persisted data.
func NewSummaryStore(db *ChatDB) *SummaryStore {
	s := &SummaryStore{
		summaries: make(map[int64]string),
		db:        db,
	}
	if db != nil {
		for chatID, summary := range db.LoadAllSummaries() {
			s.summaries[chatID] = summary
		}
		if len(s.summaries) > 0 {
			log.Printf("[chat-db] restored summaries for %d chat(s)", len(s.summaries))
		}
	}
	return s
}

// Get returns the current summary for a chat (empty string if none).
func (s *SummaryStore) Get(chatID int64) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.summaries[chatID]
}

// Set stores (or replaces) the summary for a chat.
func (s *SummaryStore) Set(chatID int64, summary string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.summaries[chatID] = summary
	if s.db != nil {
		s.db.SaveSummary(chatID, summary)
	}
}

// Clear removes the summary for a chat.
func (s *SummaryStore) Clear(chatID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.summaries, chatID)
	if s.db != nil {
		s.db.DeleteSummary(chatID)
	}
}

// MarkSummarizing acquires a per-chat lock to prevent duplicate goroutines.
// Returns false if summarisation is already running for this chat.
func (s *SummaryStore) MarkSummarizing(chatID int64) bool {
	_, loaded := s.summarizing.LoadOrStore(chatID, true)
	return !loaded
}

// DoneSummarizing releases the per-chat lock.
func (s *SummaryStore) DoneSummarizing(chatID int64) {
	s.summarizing.Delete(chatID)
}

// ─── Summary Extraction ──────────────────────────────────────────────────────

const summaryExtractSystemPrompt = `You are a conversation summarizer. Compress the given conversation history into a concise yet information-rich summary.

Rules:
1. Preserve key facts, decisions, questions asked, and answers provided.
2. Preserve who said what when it matters.
3. Be concise but information-dense — do not lose important details.
4. If an existing summary is provided, merge the new information into it seamlessly, producing a single unified summary.
5. Use the same language as the conversation messages.
6. Maximum 300 words.
7. Output ONLY the summary text — no markdown fences, no meta-commentary.`

// summarizeOverflow compresses overflow messages (those that fell out of the
// sliding window) into the existing summary via a background LLM call.
// Safe to call from a goroutine.
func (b *Bot) summarizeOverflow(chatID int64) {
	// Acquire per-chat lock.
	if !b.summaries.MarkSummarizing(chatID) {
		return // another goroutine is already summarizing this chat
	}
	defer b.summaries.DoneSummarizing(chatID)

	// Drain accumulated overflow.
	overflow := b.store.DrainOverflow(chatID)
	if len(overflow) == 0 {
		return
	}

	existing := b.summaries.Get(chatID)

	// Build the user prompt with existing summary + overflow messages.
	var userPrompt strings.Builder
	if existing != "" {
		userPrompt.WriteString("=== Existing Summary ===\n")
		userPrompt.WriteString(existing)
		userPrompt.WriteString("\n\n")
	}
	userPrompt.WriteString("=== New Messages to Incorporate ===\n")
	for _, m := range overflow {
		if m.Role == openai.ChatMessageRoleSystem {
			continue
		}
		tag := "assistant"
		if m.Role == openai.ChatMessageRoleUser {
			tag = "user"
		}
		content := m.Content
		if len([]rune(content)) > 500 {
			content = string([]rune(content)[:500]) + "…"
		}
		userPrompt.WriteString(fmt.Sprintf("[%s] %s\n", tag, content))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := b.summaryAI.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: b.summaryModel,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: summaryExtractSystemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: userPrompt.String()},
		},
		MaxTokens:   800,
		Temperature: 0.2,
	})
	if err != nil {
		log.Printf("[summary] LLM error for chat %d: %v", chatID, err)
		return
	}
	if len(resp.Choices) == 0 {
		log.Printf("[summary] empty LLM response for chat %d", chatID)
		return
	}

	summary := strings.TrimSpace(resp.Choices[0].Message.Content)
	if summary == "" {
		return
	}

	b.summaries.Set(chatID, summary)
	log.Printf("[summary] updated chat %d (%d chars)", chatID, len(summary))
}
