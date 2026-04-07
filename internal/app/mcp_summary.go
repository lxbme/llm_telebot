package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	openai "github.com/sashabaranov/go-openai"
	bolt "go.etcd.io/bbolt"
)

// ─── MCP Summary Store ────────────────────────────────────────────────────────

var mcpSummaryBucket = []byte("mcp_summaries")

type mcpSummaryEntry struct {
	Hash    string `json:"hash"`    // SHA-256 of tool schemas to detect changes
	Summary string `json:"summary"` // compact paragraph injected into system prompt
}

// MCPSummaryStore persists per-server summaries in the existing user_mcp.db.
// Keys: "{prefix}:{serverName}" where prefix = "g" (global) or "u{userID}" (per-user).
type MCPSummaryStore struct {
	db *bolt.DB
}

func NewMCPSummaryStore(db *bolt.DB) *MCPSummaryStore {
	_ = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(mcpSummaryBucket)
		return err
	})
	return &MCPSummaryStore{db: db}
}

func (s *MCPSummaryStore) get(key string) (mcpSummaryEntry, bool) {
	var entry mcpSummaryEntry
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(mcpSummaryBucket)
		if b == nil {
			return nil
		}
		v := b.Get([]byte(key))
		if v == nil {
			return nil
		}
		return json.Unmarshal(v, &entry)
	})
	return entry, err == nil && entry.Hash != ""
}

func (s *MCPSummaryStore) set(key string, entry mcpSummaryEntry) {
	_ = s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(mcpSummaryBucket)
		if err != nil {
			return err
		}
		v, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		return b.Put([]byte(key), v)
	})
}

// deleteByPrefix removes all keys that start with the given prefix string.
func (s *MCPSummaryStore) deleteByPrefix(prefix string) {
	_ = s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(mcpSummaryBucket)
		if b == nil {
			return nil
		}
		var keys [][]byte
		_ = b.ForEach(func(k, _ []byte) error {
			if strings.HasPrefix(string(k), prefix) {
				keys = append(keys, append([]byte{}, k...))
			}
			return nil
		})
		for _, k := range keys {
			_ = b.Delete(k)
		}
		return nil
	})
}

// GetSummary returns the cached summary text for a server, or "" if not found / not yet generated.
func (s *MCPSummaryStore) GetSummary(prefix, serverName string) string {
	entry, ok := s.get(prefix + ":" + serverName)
	if !ok {
		return ""
	}
	return entry.Summary
}

// ─── Hash ─────────────────────────────────────────────────────────────────────

// mcpToolsHash computes a stable SHA-256 fingerprint of an OpenAI tool list.
// Used to detect whether cached summaries are stale.
func mcpToolsHash(tools []openai.Tool) string {
	type slim struct {
		Name   string `json:"name"`
		Desc   string `json:"desc"`
		Params any    `json:"params"`
	}
	slims := make([]slim, len(tools))
	for i, t := range tools {
		slims[i] = slim{
			Name:   t.Function.Name,
			Desc:   t.Function.Description,
			Params: t.Function.Parameters,
		}
	}
	b, _ := json.Marshal(slims)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

// ─── Summary Generator ────────────────────────────────────────────────────────

var mcpSummarySystemPrompt = `Role: Minimalist Technical Writer.Task: Summarize the following MCP tool group in a single paragraph (≤80 words).Format: Start with [Group Name].Requirements: Synthesize the tools' collective capabilities into a high-density functional overview. Focus exclusively on the core utility and technical outcomes (what it enables the user to achieve). Use precise, active verbs. Do not list individual tool names or use introductory filler like "This group is designed to..." or "These tools provide...".`

// MCPSummaryGenerator calls a small LLM to produce one-paragraph server summaries.
type MCPSummaryGenerator struct {
	client *openai.Client
	model  string
	store  *MCPSummaryStore
}

func NewMCPSummaryGenerator(cfg Config, store *MCPSummaryStore) *MCPSummaryGenerator {
	if !cfg.ToolsSummaryEnabled || store == nil {
		return nil
	}
	apiKey := firstNonEmpty(cfg.ToolsSummaryAPIKey, cfg.OpenAIKey)
	if apiKey == "" {
		return nil
	}
	oaiCfg := openai.DefaultConfig(apiKey)
	if base := firstNonEmpty(cfg.ToolsSummaryAPIBase, cfg.OpenAIBase); base != "" {
		oaiCfg.BaseURL = base
	}
	model := firstNonEmpty(cfg.ToolsSummaryModel, cfg.OpenAIModel)
	return &MCPSummaryGenerator{
		client: openai.NewClientWithConfig(oaiCfg),
		model:  model,
		store:  store,
	}
}

// EnsureSummary checks the cache; regenerates if missing or stale.
// Designed to be called in a goroutine — runs synchronously and logs errors.
func (g *MCPSummaryGenerator) EnsureSummary(ctx context.Context, prefix, serverName string, tools []openai.Tool) {
	if g == nil || len(tools) == 0 {
		return
	}
	hash := mcpToolsHash(tools)
	key := prefix + ":" + serverName
	if entry, ok := g.store.get(key); ok && entry.Hash == hash {
		return // cache hit
	}

	var sb strings.Builder
	sb.WriteString("Tool group: " + serverName + "\nTools:\n")
	for _, t := range tools {
		sb.WriteString("- " + t.Function.Name + ": " + t.Function.Description + "\n")
	}

	summaryReq := openai.ChatCompletionRequest{
		Model: g.model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: mcpSummarySystemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: sb.String()},
		},
		Temperature: 0.2,
	}
	applyMaxTokens(&summaryReq, 150)
	resp, err := g.client.CreateChatCompletion(ctx, summaryReq)
	if err != nil {
		log.Printf("[mcp_summary] failed to generate summary for %q: %v", serverName, err)
		return
	}
	if len(resp.Choices) == 0 {
		log.Printf("[mcp_summary] empty response for %q", serverName)
		return
	}
	summary := strings.TrimSpace(resp.Choices[0].Message.Content)
	g.store.set(key, mcpSummaryEntry{Hash: hash, Summary: summary})
	log.Printf("[mcp_summary] cached summary for %q (%d chars)", serverName, len(summary))
}

// ─── Context Injection ────────────────────────────────────────────────────────

// buildMCPSummarySection generates the "=== Available Tool Groups ===" block
// appended to the system prompt when lazy loading is enabled.
// userID is used for per-user server prefix lookups.
func buildMCPSummarySection(toolView *MergedToolView, store *MCPSummaryStore, userID int64) string {
	if toolView == nil || store == nil {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n\n=== Available Tool Groups ===\n")
	sb.WriteString("Call get_tool_details({\"groups\":[\"name\"]}) to load full schemas before using a tool.\n")

	wrote := 0

	// Global servers
	if toolView.global != nil {
		for _, srv := range toolView.global.ServerNames() {
			summary := store.GetSummary("g", srv)
			if summary == "" {
				summary = fmt.Sprintf("**%s** (%d tools)", srv, toolView.global.CountForServer(srv))
			}
			sb.WriteString("• " + summary + "\n")
			wrote++
		}
	}

	// Per-user servers
	if toolView.user != nil {
		prefix := fmt.Sprintf("u%d", userID)
		for _, srv := range toolView.user.ServerNames() {
			summary := store.GetSummary(prefix, srv)
			if summary == "" {
				summary = fmt.Sprintf("**%s** (%d tools)", srv, toolView.user.CountForServer(srv))
			}
			sb.WriteString("• " + summary + " _(personal)_\n")
			wrote++
		}
	}

	if wrote == 0 {
		return ""
	}
	return sb.String()
}

// ─── Virtual Tool ─────────────────────────────────────────────────────────────

// getToolDetailsVirtualTool is the single tool injected in summary mode.
// The LLM calls it to request full schemas for specific server groups.
var getToolDetailsVirtualTool = openai.Tool{
	Type: openai.ToolTypeFunction,
	Function: &openai.FunctionDefinition{
		Name:        "get_tool_details",
		Description: "Load the full parameter schemas for one or more tool groups. You MUST call this before using any tool from that group.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"groups": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Names of tool groups to expand (as listed under === Available Tool Groups ===).",
				},
			},
			"required": []string{"groups"},
		},
	},
}

// ─── Active Tool Builder ──────────────────────────────────────────────────────

// buildActiveTools returns the []openai.Tool slice for a single LLM request.
//
//   - summaryMode=false: returns all tool schemas (original behaviour).
//   - summaryMode=true:  returns the virtual get_tool_details tool plus full
//     schemas for any servers the LLM has already expanded.
func buildActiveTools(toolView *MergedToolView, expanded map[string]bool, summaryMode bool) []openai.Tool {
	if toolView == nil {
		return nil
	}
	if !summaryMode {
		return toolView.OpenAITools()
	}
	tools := []openai.Tool{getToolDetailsVirtualTool}
	for srv := range expanded {
		tools = append(tools, toolView.OpenAIToolsForServer(srv)...)
	}
	return tools
}
