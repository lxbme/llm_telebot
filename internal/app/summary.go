package app

import (
	"fmt"
	"log"
	"strings"
	"sync"

	openai "github.com/sashabaranov/go-openai"
)

// ─── Summary Store ───────────────────────────────────────────────────────────

// SummaryStore keeps per-chat conversation summaries backed by an optional
// persistent ChatDB. When the sliding window overflows, older messages are
// compressed into a running summary so the LLM retains long-term memory.
type SummaryStore struct {
	mu          sync.RWMutex
	summaries   map[int64]string // chatID → summary text
	summarizing sync.Map         // generation -> bool (one active summary per chat generation)
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

// Limit the prompt text per request independently of how long the queue has
// grown. One message can still be summarized alongside a long custom persona.
const summaryInputBudgetBytes = 6 * 1024

// summarizeOverflow compresses overflow messages (those that fell out of the
// sliding window) into the existing summary via a background LLM call.
// Safe to call from a goroutine.
func (b *Bot) summarizeOverflow(turn RoleTurn) {
	turn.Purpose = RoleSummary
	// These inputs are the same-chat transcript, already delivered legally.
	turn.memoryUsed, turn.groupTool = nil, nil
	snap := b.snapshot()
	if !snap.cfg.SummaryEnabled || snap.summaries == nil || snap.summaryAI == nil {
		return
	}
	chatID := turn.ChatID
	if _, busy := snap.summaries.summarizing.LoadOrStore(turn.generation, true); busy {
		return
	}
	defer snap.summaries.summarizing.Delete(turn.generation)

	var batch overflowBatch
	var existing string
	turn.withCurrent(func() {
		batch = snap.store.pendingOverflow(chatID)
		existing = snap.summaries.Get(chatID)
	})
	if len(batch.messages) == 0 {
		return
	}
	messages := snap.buildRoleContext(&turn, "", "\n\n"+summaryExtractSystemPrompt, 0, false)
	if len(messages) == 0 {
		return
	}

	// Build the user prompt with existing summary + overflow messages.
	var userPrompt strings.Builder
	if existing != "" {
		userPrompt.WriteString("=== Existing Summary ===\n")
		userPrompt.WriteString(existing)
		userPrompt.WriteString("\n\n")
	}
	userPrompt.WriteString("=== New Messages to Incorporate ===\n")
	headerBytes := userPrompt.Len()
	userBudget := summaryInputBudgetBytes - len(messages[0].Content)
	for i, m := range batch.messages {
		if m.Role == openai.ChatMessageRoleSystem {
			continue
		}
		tag := "assistant"
		if m.Role == openai.ChatMessageRoleUser {
			tag = "user"
		}
		line := fmt.Sprintf("[%s] %s\n", tag, truncate(userMessageText(m), 500))
		if userPrompt.Len() > headerBytes && userPrompt.Len()+len(line) > userBudget {
			batch.messages = batch.messages[:i]
			break
		}
		userPrompt.WriteString(line)
	}

	messages = append(messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: userPrompt.String()})
	req := openai.ChatCompletionRequest{Model: snap.summaryModel, Messages: messages, Temperature: 0.2}
	turn.BillingUserID = 0
	resp, err := b.completeRoleDecision(turn, snap, snap.summaryAI, req, UsageCallSummary, 800)
	if err != nil {
		if turn.generation.Err() == nil {
			log.Printf("[summary] LLM error for chat %d: %v", chatID, err)
		}
		return
	}
	if len(resp.Choices) == 0 {
		log.Printf("[summary] empty LLM response for chat %d", chatID)
		return
	}

	choice := resp.Choices[0]
	summary := strings.TrimSpace(choice.Message.Content)
	if summary == "" || choice.Message.Refusal != "" ||
		(choice.FinishReason != "" && choice.FinishReason != openai.FinishReasonStop) {
		return
	}

	var committed bool
	turn.withCurrent(func() {
		if turn.checkUse(turn.generation) != nil {
			return
		}
		var err error
		committed, err = snap.store.commitSummary(chatID, snap.summaries, batch, existing, summary)
		if err != nil {
			log.Printf("[summary] save error for chat %d: %v", chatID, err)
		}
	})
	if !committed {
		return
	}
	b.recordDashboardEvent(DashboardEvent{
		Type:    DashboardEventSummaryUpdated,
		ChatID:  chatID,
		Model:   firstNonEmpty(resp.Model, snap.summaryModel),
		Summary: truncateDashboardText(summary, 200),
		Success: true,
	})
	log.Printf("[summary] updated chat %d (%d chars)", chatID, len(summary))
}
