package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

// ─── Profile Extraction ──────────────────────────────────────────────────────

// profileExtractSystemPrompt is the system-level instruction for the profile
// extraction LLM call. %s placeholders: username, displayName, existingFacts.
const profileExtractSystemPrompt = `You are a user-profile extractor. Analyze the conversation and extract or update concise factual tags about user @%s (%s).

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
func (b *Bot) extractProfile(username, displayName string, conversation []openai.ChatCompletionMessage) {
	snap := b.snapshot()
	if username == "" || snap.profiles == nil {
		return
	}

	// Acquire per-user lock.
	if !snap.profiles.MarkExtracting(username) {
		return // another goroutine is already extracting
	}
	defer snap.profiles.DoneExtracting(username)

	// Load existing profile facts.
	existing, err := snap.profiles.Get(username)
	if err != nil {
		log.Printf("[profile] read error for @%s: %v", username, err)
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

	sysPrompt := fmt.Sprintf(profileExtractSystemPrompt, username, displayName, existingFacts)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := snap.profileAI.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: snap.profileModel,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: sysPrompt},
			{Role: openai.ChatMessageRoleUser, Content: convBuf.String()},
		},
		MaxTokens:   500,
		Temperature: 0.1,
	})
	if err != nil {
		log.Printf("[profile] LLM error for @%s: %v", username, err)
		return
	}
	if len(resp.Choices) == 0 {
		log.Printf("[profile] empty LLM response for @%s", username)
		return
	}

	raw := strings.TrimSpace(resp.Choices[0].Message.Content)
	facts, err := parseJSONArray(raw)
	if err != nil {
		log.Printf("[profile] parse error for @%s: %v (raw: %s)", username, err, raw)
		return
	}
	if len(facts) > 10 {
		facts = facts[:10]
	}

	profile := &UserProfile{
		Username:    username,
		DisplayName: displayName,
		Facts:       facts,
		UpdatedAt:   time.Now(),
	}
	if err := snap.profiles.Save(profile); err != nil {
		log.Printf("[profile] save error for @%s: %v", username, err)
		return
	}

	log.Printf("[profile] updated @%s: %v", username, facts)
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

	// Collect unique usernames from message metadata headers.
	usernames := make(map[string]struct{})
	for _, msg := range history {
		if uname := extractUsernameFromContent(msg.Content); uname != "" {
			usernames[uname] = struct{}{}
		}
	}
	if len(usernames) == 0 {
		return ""
	}

	var sb strings.Builder
	count := 0
	for uname := range usernames {
		profile, err := snap.profiles.Get(uname)
		if err != nil || profile == nil || len(profile.Facts) == 0 {
			continue
		}
		if count == 0 {
			sb.WriteString("\n\n=== Participant Profiles ===\n")
		}
		count++
		sb.WriteString(fmt.Sprintf("@%s: %s\n", uname, strings.Join(profile.Facts, "; ")))
	}

	return sb.String()
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
