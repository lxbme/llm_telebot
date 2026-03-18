package app

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	openai "github.com/sashabaranov/go-openai"
	bolt "go.etcd.io/bbolt"
)

var mcpBucket = []byte("user_mcp")

// ─── User MCP Store (persistent) ────────────────────────────────────────────

// UserMCPStore persists per-user MCP server configurations in bbolt,
// keyed by Telegram user ID. Each user's value is a JSON-encoded MCPConfig.
type UserMCPStore struct {
	db *bolt.DB
}

// NewUserMCPStore opens (or creates) the bbolt database for user MCP configs.
func NewUserMCPStore(path string) (*UserMCPStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("create user mcp db dir: %w", err)
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open user mcp db: %w", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(mcpBucket)
		return err
	}); err != nil {
		db.Close()
		return nil, fmt.Errorf("init user mcp bucket: %w", err)
	}
	return &UserMCPStore{db: db}, nil
}

// Close shuts down the bbolt database.
func (s *UserMCPStore) Close() error {
	return s.db.Close()
}

func userKey(userID int64) []byte {
	return []byte(strconv.FormatInt(userID, 10))
}

// Get retrieves the persisted MCP config for a user. Returns nil if none.
func (s *UserMCPStore) Get(userID int64) (*MCPConfig, error) {
	var cfg *MCPConfig
	err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(mcpBucket).Get(userKey(userID))
		if data == nil {
			return nil
		}
		cfg = &MCPConfig{}
		return json.Unmarshal(data, cfg)
	})
	return cfg, err
}

// Save persists the full MCP config for a user.
func (s *UserMCPStore) Save(userID int64, cfg *MCPConfig) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal user mcp config: %w", err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(mcpBucket).Put(userKey(userID), data)
	})
}

// Delete removes all MCP config for a user.
func (s *UserMCPStore) Delete(userID int64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(mcpBucket).Delete(userKey(userID))
	})
}

// AddServers merges new server entries into the user's existing config
// and persists the result. Returns the names of added/updated servers.
func (s *UserMCPStore) AddServers(userID int64, incoming map[string]MCPServerConfig) ([]string, error) {
	existing, err := s.Get(userID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		existing = &MCPConfig{MCPServers: make(map[string]MCPServerConfig)}
	}
	if existing.MCPServers == nil {
		existing.MCPServers = make(map[string]MCPServerConfig)
	}

	var names []string
	for name, srv := range incoming {
		existing.MCPServers[name] = srv
		names = append(names, name)
	}

	if err := s.Save(userID, existing); err != nil {
		return nil, err
	}
	return names, nil
}

// RemoveServer removes a single named server from the user's config.
// Returns true if the server existed.
func (s *UserMCPStore) RemoveServer(userID int64, serverName string) (bool, error) {
	existing, err := s.Get(userID)
	if err != nil {
		return false, err
	}
	if existing == nil || existing.MCPServers == nil {
		return false, nil
	}
	if _, ok := existing.MCPServers[serverName]; !ok {
		return false, nil
	}
	delete(existing.MCPServers, serverName)
	if len(existing.MCPServers) == 0 {
		return true, s.Delete(userID)
	}
	return true, s.Save(userID, existing)
}

// AllUserIDs returns all user IDs that have stored MCP configs.
func (s *UserMCPStore) AllUserIDs() ([]int64, error) {
	var ids []int64
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(mcpBucket).ForEach(func(k, v []byte) error {
			id, err := strconv.ParseInt(string(k), 10, 64)
			if err == nil {
				ids = append(ids, id)
			}
			return nil
		})
	})
	return ids, err
}

// ─── User Tool Manager ──────────────────────────────────────────────────────

// UserToolManager manages per-user MCP client connections and tool registries.
// Each user has their own set of connected MCP servers and discovered tools.
type UserToolManager struct {
	mu         sync.RWMutex
	store      *UserMCPStore
	registries map[int64]*ToolRegistry     // userID → personal tool registry
	clients    map[int64]*MCPClientManager // userID → personal MCP connections
}

// NewUserToolManager creates a new manager backed by the given store.
func NewUserToolManager(store *UserMCPStore) *UserToolManager {
	return &UserToolManager{
		store:      store,
		registries: make(map[int64]*ToolRegistry),
		clients:    make(map[int64]*MCPClientManager),
	}
}

// RestoreAll reconnects all users' MCP servers from persisted configs.
// Called once at startup.
func (m *UserToolManager) RestoreAll() {
	ids, err := m.store.AllUserIDs()
	if err != nil {
		log.Printf("[user-mcp] failed to list users: %v", err)
		return
	}

	for _, uid := range ids {
		cfg, err := m.store.Get(uid)
		if err != nil || cfg == nil || len(cfg.MCPServers) == 0 {
			continue
		}
		failures := m.connectUser(uid, cfg)
		if len(failures) > 0 {
			for name, ferr := range failures {
				log.Printf("[user-mcp] restore user %d: server %q failed: %v", uid, name, ferr)
			}
		}
		log.Printf("[user-mcp] restored user %d (%d servers, %d failed)", uid, len(cfg.MCPServers), len(failures))
	}
}

// connectUser creates connections for a user's MCP config.
// Must be called with m.mu held or before concurrent access begins.
// Returns a map of server name → error for servers that failed to connect.
func (m *UserToolManager) connectUser(userID int64, cfg *MCPConfig) map[string]error {
	// Close existing connections if any.
	m.disconnectUserLocked(userID)

	registry := NewToolRegistry()
	manager := NewMCPClientManager()
	failures := manager.ConnectAll(cfg, registry)

	m.registries[userID] = registry
	m.clients[userID] = manager
	return failures
}

// disconnectUserLocked closes existing connections for a user.
// Caller must hold m.mu.
func (m *UserToolManager) disconnectUserLocked(userID int64) {
	if mgr, ok := m.clients[userID]; ok {
		mgr.Close()
		delete(m.clients, userID)
	}
	delete(m.registries, userID)
}

// MCPAddResult holds the outcome of an AddServers operation.
type MCPAddResult struct {
	Succeeded []string          // server names that connected successfully
	Failed    map[string]string // server name → error message for failures
}

// AddServers adds new MCP servers for a user, validates connectivity,
// and persists only the servers that connect successfully.
// Servers that fail to connect are rolled back from the DB.
func (m *UserToolManager) AddServers(userID int64, incoming map[string]MCPServerConfig) (*MCPAddResult, error) {
	result := &MCPAddResult{}

	// First, persist all incoming servers.
	_, err := m.store.AddServers(userID, incoming)
	if err != nil {
		return nil, err
	}

	// Reload and reconnect all servers for this user.
	cfg, err := m.store.Get(userID)
	if err != nil {
		return nil, fmt.Errorf("added but failed to reload: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	failures := m.connectUser(userID, cfg)

	// Classify newly-added servers into succeeded vs. failed.
	for name := range incoming {
		if ferr, ok := failures[name]; ok {
			if result.Failed == nil {
				result.Failed = make(map[string]string)
			}
			result.Failed[name] = ferr.Error()
		} else {
			result.Succeeded = append(result.Succeeded, name)
		}
	}

	// Roll back failed servers from the DB so they don't linger.
	if len(result.Failed) > 0 {
		for name := range result.Failed {
			if _, err := m.store.RemoveServer(userID, name); err != nil {
				log.Printf("[user-mcp] rollback server %q for user %d failed: %v", name, userID, err)
			}
		}
		// Reconnect without the failed servers.
		updatedCfg, err := m.store.Get(userID)
		if err == nil && updatedCfg != nil && len(updatedCfg.MCPServers) > 0 {
			m.connectUser(userID, updatedCfg)
		} else if err == nil {
			m.disconnectUserLocked(userID)
		}
	}

	return result, nil
}

// RemoveServer removes a single named MCP server from a user's config,
// persists the change, and reconnects remaining servers.
func (m *UserToolManager) RemoveServer(userID int64, serverName string) (bool, error) {
	found, err := m.store.RemoveServer(userID, serverName)
	if err != nil || !found {
		return found, err
	}

	// Reload.
	cfg, err := m.store.Get(userID)
	if err != nil {
		return true, fmt.Errorf("removed but failed to reload: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if cfg == nil || len(cfg.MCPServers) == 0 {
		m.disconnectUserLocked(userID)
		return true, nil
	}
	failures := m.connectUser(userID, cfg)
	if len(failures) > 0 {
		var errNames []string
		for name := range failures {
			errNames = append(errNames, name)
		}
		return true, fmt.Errorf("removed but some servers failed to reconnect: %s", strings.Join(errNames, ", "))
	}
	return true, nil
}

// ClearAll removes all MCP servers for a user.
func (m *UserToolManager) ClearAll(userID int64) error {
	if err := m.store.Delete(userID); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.disconnectUserLocked(userID)
	return nil
}

// GetRegistry returns the user's personal ToolRegistry, or nil if none.
func (m *UserToolManager) GetRegistry(userID int64) *ToolRegistry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.registries[userID]
}

// ListServers returns the names and types of a user's configured MCP servers.
func (m *UserToolManager) ListServers(userID int64) ([]ServerInfo, error) {
	cfg, err := m.store.Get(userID)
	if err != nil {
		return nil, err
	}
	if cfg == nil || len(cfg.MCPServers) == 0 {
		return nil, nil
	}

	m.mu.RLock()
	reg := m.registries[userID]
	m.mu.RUnlock()

	var infos []ServerInfo
	for name, srv := range cfg.MCPServers {
		info := ServerInfo{
			Name: name,
			Type: srv.Type,
			URL:  srv.URL,
		}
		// Count tools from this server's registry.
		if reg != nil {
			info.ToolCount = reg.Count()
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// Close shuts down all user MCP connections.
func (m *UserToolManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for uid := range m.clients {
		m.disconnectUserLocked(uid)
	}
	m.store.Close()
}

// ServerInfo holds display information about a configured MCP server.
type ServerInfo struct {
	Name      string
	Type      string
	URL       string
	ToolCount int
}

// ─── Merged Tool View ────────────────────────────────────────────────────────

// MergedToolView provides a unified view of global tools + user-specific tools
// for a single request. It implements tool lookup and OpenAI tool list generation.
type MergedToolView struct {
	global *ToolRegistry
	user   *ToolRegistry // may be nil
}

// NewMergedToolView creates a merged view. user may be nil.
func NewMergedToolView(global, user *ToolRegistry) *MergedToolView {
	return &MergedToolView{global: global, user: user}
}

// Count returns the total number of available tools.
func (v *MergedToolView) Count() int {
	n := 0
	if v.global != nil {
		n += v.global.Count()
	}
	if v.user != nil {
		n += v.user.Count()
	}
	return n
}

// Get looks up a tool by name, checking user tools first (higher priority),
// then global tools.
func (v *MergedToolView) Get(name string) MCPTool {
	if v.user != nil {
		if t := v.user.Get(name); t != nil {
			return t
		}
	}
	if v.global != nil {
		return v.global.Get(name)
	}
	return nil
}

// OpenAITools returns the combined tool definitions for the OpenAI API.
func (v *MergedToolView) OpenAITools() []openai.Tool {
	var tools []openai.Tool
	if v.global != nil {
		tools = append(tools, v.global.OpenAITools()...)
	}
	if v.user != nil {
		tools = append(tools, v.user.OpenAITools()...)
	}
	return tools
}

// ExecuteToolCall looks up and executes a tool call from either registry.
func (v *MergedToolView) ExecuteToolCall(call openai.ToolCall) string {
	tool := v.Get(call.Function.Name)
	if tool == nil {
		return fmt.Sprintf("error: unknown tool %q", call.Function.Name)
	}
	result, err := tool.Execute(call.Function.Arguments)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	return result
}

// ─── JSON Detection Helper ──────────────────────────────────────────────────

// TryParseMCPConfig attempts to parse a message as MCP server configuration.
// Returns the parsed config and true if the message looks like a valid
// {"mcpServers": {...}} JSON block.
func TryParseMCPConfig(text string) (*MCPConfig, bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "{") {
		return nil, false
	}
	var cfg MCPConfig
	if err := json.Unmarshal([]byte(text), &cfg); err != nil {
		return nil, false
	}
	if len(cfg.MCPServers) == 0 {
		return nil, false
	}
	// Remove disabled servers (Claude Desktop compat).
	for name, srv := range cfg.MCPServers {
		if srv.Disabled {
			delete(cfg.MCPServers, name)
		}
	}
	if len(cfg.MCPServers) == 0 {
		return nil, false
	}
	// Auto-infer transport type when omitted.
	for name, srv := range cfg.MCPServers {
		if srv.Type == "" {
			inferred := InferTransportType(&srv)
			if inferred == "" {
				log.Printf("[user-mcp] server %q: cannot infer type (need command or url)", name)
				return nil, false
			}
			srv.Type = inferred
			cfg.MCPServers[name] = srv
		}
	}
	return &cfg, true
}

// FormatServerList builds a human-readable list of MCP servers for display.
func FormatServerList(servers []ServerInfo) string {
	if len(servers) == 0 {
		return "📭 No personal MCP servers configured."
	}
	var sb strings.Builder
	sb.WriteString("🔌 Your MCP servers:\n\n")
	for i, s := range servers {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, s.Name))
		sb.WriteString(fmt.Sprintf("   Type: %s\n", s.Type))
		if s.URL != "" {
			sb.WriteString(fmt.Sprintf("   URL: %s\n", s.URL))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// formatMCPAddResult builds a human-readable message from an MCPAddResult,
// reporting which servers succeeded and which failed (with error details).
func formatMCPAddResult(r *MCPAddResult) string {
	var sb strings.Builder
	if len(r.Succeeded) > 0 {
		sb.WriteString(fmt.Sprintf("✅ Added MCP servers: %s\n", strings.Join(r.Succeeded, ", ")))
	}
	if len(r.Failed) > 0 {
		sb.WriteString("\n❌ Failed to connect:\n")
		for name, errMsg := range r.Failed {
			sb.WriteString(fmt.Sprintf("  • %s: %s\n", name, errMsg))
		}
		sb.WriteString("\nFailed servers were not saved.")
	}
	if len(r.Succeeded) > 0 {
		sb.WriteString("\n\nUse /mcp_list to see all your servers.")
	}
	if len(r.Succeeded) == 0 && len(r.Failed) == 0 {
		sb.WriteString("⚠️ No servers to add.")
	}
	return sb.String()
}

// IsCommandMCP returns true if the server config uses a command-based
// (stdio) transport — i.e. it executes a local process.
func IsCommandMCP(srv MCPServerConfig) bool {
	switch strings.ToLower(InferTransportType(&srv)) {
	case "stdio":
		return true
	default:
		return false
	}
}

// filterCommandMCPs removes any command-based (stdio) servers from the config
// in-place and returns the names of the removed entries. If no command-based
// servers are found, the returned slice is nil.
func filterCommandMCPs(cfg *MCPConfig) []string {
	var rejected []string
	for name, srv := range cfg.MCPServers {
		if IsCommandMCP(srv) {
			rejected = append(rejected, name)
			delete(cfg.MCPServers, name)
		}
	}
	return rejected
}
