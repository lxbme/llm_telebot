package main

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"

	openai "github.com/sashabaranov/go-openai"
)

// ─── Tool Interface ──────────────────────────────────────────────────────────

// MCPTool defines the interface for any tool that the LLM can invoke.
// To add a new tool, implement this interface and register it with the
// ToolRegistry via Register().
type MCPTool interface {
	// Name returns the unique function name the LLM will use to call this tool.
	Name() string

	// Description returns a brief description shown to the LLM.
	Description() string

	// Parameters returns the JSON Schema describing the function's arguments.
	// Can return a jsonschema.Definition, a map, or any JSON-serializable value.
	Parameters() any

	// Execute runs the tool with the given JSON arguments string and returns
	// a textual result (or an error). The result is fed back to the LLM as a
	// tool-call response.
	Execute(args string) (string, error)
}

// ─── Tool Registry ───────────────────────────────────────────────────────────

// ToolRegistry holds all registered tools and converts them to OpenAI format.
type ToolRegistry struct {
	mu    sync.RWMutex
	tools map[string]MCPTool
	order []string // preserves registration order
}

// NewToolRegistry creates an empty tool registry.
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		tools: make(map[string]MCPTool),
	}
}

// Register adds a tool to the registry. Duplicate names overwrite silently.
func (r *ToolRegistry) Register(t MCPTool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := t.Name()
	if _, exists := r.tools[name]; !exists {
		r.order = append(r.order, name)
	}
	r.tools[name] = t
	log.Printf("[tools] registered: %s", name)
}

// Get returns a registered tool by name, or nil if not found.
func (r *ToolRegistry) Get(name string) MCPTool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tools[name]
}

// OpenAITools converts all registered tools into the []openai.Tool format
// expected by the ChatCompletionRequest.
func (r *ToolRegistry) OpenAITools() []openai.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.order) == 0 {
		return nil
	}
	tools := make([]openai.Tool, 0, len(r.order))
	for _, name := range r.order {
		t := r.tools[name]
		tools = append(tools, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.Parameters(),
			},
		})
	}
	return tools
}

// Count returns the number of registered tools.
func (r *ToolRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}

// ─── Tool Execution ──────────────────────────────────────────────────────────

// ExecuteToolCall looks up and executes a single tool call, returning the
// result string. On any error the result contains the error message so the
// LLM can self-correct.
func (r *ToolRegistry) ExecuteToolCall(call openai.ToolCall) string {
	tool := r.Get(call.Function.Name)
	if tool == nil {
		return fmt.Sprintf("error: unknown tool %q", call.Function.Name)
	}
	result, err := tool.Execute(call.Function.Arguments)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	return result
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// ParseArgs is a convenience helper that unmarshals a JSON arguments string
// into the given struct pointer. Tools can use this in their Execute method.
func ParseArgs(argsJSON string, dest any) error {
	if err := json.Unmarshal([]byte(argsJSON), dest); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}
