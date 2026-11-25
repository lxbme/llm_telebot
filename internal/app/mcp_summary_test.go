package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	bolt "go.etcd.io/bbolt"
)

func TestMCPSummaryPromptUsesLegacyEntryOnlyForMatchingSchema(t *testing.T) {
	const userID = 42
	store := newMCPSummaryTestStore(t)
	old := newMCPSummaryTestRegistry(t, `{"type":"object","properties":{"title":{"type":"string"}}}`)
	current := newMCPSummaryTestRegistry(t, `{"type":"object","properties":{"id":{"type":"integer"}}}`)
	entry := mcpSummaryEntry{Hash: mcpToolsHash(old.OpenAIToolsForServer("docs")), Summary: "[docs] Legacy title search."}
	value, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(mcpSummaryBucket).Put([]byte("u42:docs"), value)
	}); err != nil {
		t.Fatal(err)
	}
	assertMCPSummaryPrompt(t, NewMergedToolView(nil, old), store, userID, "[docs] Legacy title search. _(personal)_")
	assertMCPSummaryPrompt(t, NewMergedToolView(nil, current), store, userID, "**docs** (1 tools) _(personal)_")
}

func TestMCPSummaryLateWritePreservesCurrentSchema(t *testing.T) {
	oldStore := newMCPSummaryTestStore(t)
	currentStore := NewMCPSummaryStore(oldStore.db)
	old := newMCPSummaryTestRegistry(t, `{"type":"object","properties":{"title":{"type":"string"}}}`)
	current := newMCPSummaryTestRegistry(t, `{"type":"object","properties":{"id":{"type":"integer"}}}`)
	oldStarted := make(chan struct{})
	resumeOld := make(chan struct{})
	releaseOld := sync.OnceFunc(func() { close(resumeOld) })
	provider := newEinoReplyProvider(t, func(w http.ResponseWriter, r *http.Request, _ int, req openai.ChatCompletionRequest) {
		var summary string
		switch req.Model {
		case "old-model":
			close(oldStarted)
			select {
			case <-resumeOld:
			case <-r.Context().Done():
				return
			}
			summary = "[docs] Search by title."
		case "current-model":
			summary = "[docs] Look up by ID."
		default:
			t.Errorf("unexpected summary request model %q", req.Model)
			http.Error(w, "unexpected model", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q}}]}`, summary)
	})
	oldCfg := Config{ToolsSummaryEnabled: true, OpenAIKey: "test-key", OpenAIBase: provider.url + "/v1", OpenAIModel: "old-model"}
	currentCfg := oldCfg
	currentCfg.OpenAIModel = "current-model"
	oldGen := NewMCPSummaryGenerator(oldCfg, oldStore)
	currentGen := NewMCPSummaryGenerator(currentCfg, currentStore)
	oldDone := make(chan struct{})
	t.Cleanup(func() {
		releaseOld()
		waitReplySignal(t, oldDone, "old summary cleanup")
	})
	go func() {
		defer close(oldDone)
		oldGen.EnsureSummary(t.Context(), "g", "docs", old.OpenAIToolsForServer("docs"))
	}()
	waitReplySignal(t, oldStarted, "old summary request")
	currentGen.EnsureSummary(t.Context(), "g", "docs", current.OpenAIToolsForServer("docs"))
	oldView, currentView := NewMergedToolView(old, nil), NewMergedToolView(current, nil)
	assertMCPSummaryPrompt(t, currentView, currentStore, 0, "[docs] Look up by ID.")
	assertMCPSummaryPrompt(t, oldView, oldStore, 0, "**docs** (1 tools)")
	releaseOld()
	waitReplySignal(t, oldDone, "old summary write")
	assertMCPSummaryPrompt(t, oldView, oldStore, 0, "[docs] Search by title.")
	assertMCPSummaryPrompt(t, currentView, currentStore, 0, "[docs] Look up by ID.")
}

func newMCPSummaryTestStore(t *testing.T) *MCPSummaryStore {
	t.Helper()
	store, err := NewUserMCPStore(filepath.Join(t.TempDir(), "users.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return NewMCPSummaryStore(store.db)
}

func newMCPSummaryTestRegistry(t *testing.T, schema string) *ToolRegistry {
	t.Helper()
	registry := NewToolRegistry()
	addEinoTestMCPServer(t, registry, "docs", einoTestMCPTool("search", "Search documents.", schema))
	return registry
}

func assertMCPSummaryPrompt(t *testing.T, view *MergedToolView, store *MCPSummaryStore, userID int64, want string) {
	t.Helper()
	section := buildMCPSummarySection(view, store, userID)
	if !strings.HasSuffix(section, "\n• "+want+"\n") {
		t.Errorf("prompt = %q, want %q in the tool groups", section, want)
	}
}
