package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

// ─── Profile Extraction ──────────────────────────────────────────────────────

// profileExtractSystemPrompt is the system-level instruction for the profile
// extraction LLM call. %s placeholders: subjectLabel, displayName, existingFacts.
const profileExtractSystemPrompt = `You are a user-profile extractor. Analyze the conversation and extract or update concise factual tags about user %s (%s).

Existing profile:
%s

Rules:
1. Each tag must be a short phrase (e.g. "Go developer", "Lives in Shanghai", "Cat owner", "College student", "25 years old").
2. Focus on: occupation, skills, hobbies/interests, location, age, personality traits, life-style preferences, notable personal context.
3. ONLY record facts the user explicitly stated or strongly implied about THEMSELVES. Never guess from questions they asked.
4. Merge with existing tags: retain still-valid ones, update changed ones, drop contradicted ones.
5. Maximum 10 tags. Prioritise the most important and stable traits.
6. Do NOT record transient states, conversation topics, or questions asked.
7. Use the same language as the user's messages for the tags.

Return ONLY a JSON array of strings. No explanation, no markdown fences.
Example: ["Go developer", "Based in Shanghai", "Cat owner"]
If nothing new, return the existing tags unchanged. If nothing at all, return: []`

// extractProfile runs a background LLM call to extract/update the user's
// profile from the recent conversation. Safe to call from a goroutine.
func (b *Bot) extractProfile(chatID, userID int64, username, displayName string, conversation []openai.ChatCompletionMessage) {
	snap := b.snapshot()
	if userID == 0 || snap.profiles == nil {
		return
	}
	usageCtx := newUsageContext(chatID, userID, 0)
	subjectLabel := formatProfileIdentity(userID, username)

	// Acquire per-user lock.
	if !snap.profiles.MarkExtracting(userID) {
		return // another goroutine is already extracting
	}
	defer snap.profiles.DoneExtracting(userID)

	// Load existing profile facts.
	existing, err := snap.profiles.Get(userID)
	if err != nil {
		log.Printf("[profile] read error for %s: %v", subjectLabel, err)
		return
	}
	existingFacts := "None"
	if existing != nil && len(existing.Facts) > 0 {
		data, _ := json.Marshal(existing.Facts)
		existingFacts = string(data)
	}

	// Build a trimmed conversation transcript.
	var convBuf strings.Builder
	for _, m := range conversation {
		if m.Role == openai.ChatMessageRoleSystem {
			continue
		}
		tag := "assistant"
		if m.Role == openai.ChatMessageRoleUser {
			tag = "user"
		}
		// Truncate very long messages to save tokens.
		content := m.Content
		if len([]rune(content)) > 500 {
			content = string([]rune(content)[:500]) + "…"
		}
		convBuf.WriteString(fmt.Sprintf("[%s] %s\n", tag, content))
	}

	sysPrompt := fmt.Sprintf(profileExtractSystemPrompt, subjectLabel, displayName, existingFacts)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	profileReq := openai.ChatCompletionRequest{
		Model: snap.profileModel,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: sysPrompt},
			{Role: openai.ChatMessageRoleUser, Content: convBuf.String()},
		},
		Temperature: 0.1,
	}
	applyMaxTokens(&profileReq, 500)
	started := time.Now()
	resp, err := snap.profileAI.CreateChatCompletion(ctx, profileReq)
	b.recordUsageEvent(usageEvent(usageCtx, UsageCallProfileExtract, firstNonEmpty(resp.Model, snap.profileModel), false, 0, started, &resp.Usage, err == nil))
	if err != nil {
		log.Printf("[profile] LLM error for %s: %v", subjectLabel, err)
		return
	}
	if len(resp.Choices) == 0 {
		log.Printf("[profile] empty LLM response for %s", subjectLabel)
		return
	}

	raw := strings.TrimSpace(resp.Choices[0].Message.Content)
	facts, err := parseJSONArray(raw)
	if err != nil {
		log.Printf("[profile] parse error for %s: %v (raw: %s)", subjectLabel, err, raw)
		return
	}
	if len(facts) > 10 {
		facts = facts[:10]
	}

	profile := &UserProfile{
		UserID:      userID,
		Username:    username,
		DisplayName: displayName,
		Facts:       facts,
		UpdatedAt:   time.Now(),
	}
	if err := snap.profiles.Save(profile); err != nil {
		log.Printf("[profile] save error for %s: %v", subjectLabel, err)
		return
	}

	b.recordDashboardEvent(DashboardEvent{
		Type:    DashboardEventProfileUpdated,
		ChatID:  chatID,
		UserID:  userID,
		Model:   firstNonEmpty(resp.Model, snap.profileModel),
		Summary: truncateDashboardText(strings.Join(facts, "; "), 200),
		Success: true,
	})
	log.Printf("[profile] updated %s: %v", subjectLabel, facts)
}

// parseJSONArray extracts a JSON string-array from possibly noisy LLM output.
func parseJSONArray(s string) ([]string, error) {
	var result []string
	if err := json.Unmarshal([]byte(s), &result); err == nil {
		return result, nil
	}
	// Fallback: locate the first [...] in the text.
	start := strings.Index(s, "[")
	end := strings.LastIndex(s, "]")
	if start >= 0 && end > start {
		if err := json.Unmarshal([]byte(s[start:end+1]), &result); err == nil {
			return result, nil
		}
	}
	return nil, fmt.Errorf("no valid JSON array found")
}

// ─── Profile Injection ───────────────────────────────────────────────────────

// buildProfileSection builds a text block describing the profiles of all users
// who appear in the given message history. It is appended to the system prompt
// so the LLM knows who the participants are.
func (b *Bot) buildProfileSection(history []openai.ChatCompletionMessage) string {
	snap := b.snapshot()
	if snap.profiles == nil {
		return ""
	}

	// Collect unique participants from message metadata headers.
	participants := make(map[int64]string)
	for _, msg := range history {
		userID := extractUserIDFromContent(msg.Content)
		if userID == 0 {
			continue
		}
		participants[userID] = extractUsernameFromContent(msg.Content)
	}
	if len(participants) == 0 {
		return ""
	}

	var sb strings.Builder
	count := 0
	for userID, username := range participants {
		profile, err := snap.profiles.Get(userID)
		if err != nil || profile == nil || len(profile.Facts) == 0 {
			continue
		}
		if count == 0 {
			sb.WriteString("\n\n=== Participant Profiles ===\n")
		}
		count++
		sb.WriteString(fmt.Sprintf("%s: %s\n", formatProfileIdentity(userID, firstNonEmpty(profile.Username, username)), strings.Join(profile.Facts, "; ")))
	}

	return sb.String()
}

func formatProfileIdentity(userID int64, username string) string {
	username = strings.TrimSpace(username)
	if username != "" {
		return fmt.Sprintf("@%s (%d)", username, userID)
	}
	return fmt.Sprintf("user_%d", userID)
}

// extractUsernameFromContent pulls a @username from the metadata header that
// buildUserContent() embeds in every user message.
// Expected format: [user_id:123 username:@alice name:"Alice" time:...]
func extractUsernameFromContent(content string) string {
	const prefix = "username:@"
	idx := strings.Index(content, prefix)
	if idx < 0 {
		return ""
	}
	rest := content[idx+len(prefix):]
	end := strings.IndexAny(rest, " ]\n")
	if end <= 0 {
		return ""
	}
	return rest[:end]
}

func extractUserIDFromContent(content string) int64 {
	const prefix = "user_id:"
	idx := strings.Index(content, prefix)
	if idx < 0 {
		return 0
	}
	rest := content[idx+len(prefix):]
	end := strings.IndexAny(rest, " ]\n")
	if end <= 0 {
		return 0
	}
	userID, err := strconv.ParseInt(rest[:end], 10, 64)
	if err != nil {
		return 0
	}
	return userID
}
