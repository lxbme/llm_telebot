package app

import (
	"context"
	"errors"
	"sync"

	tele "gopkg.in/telebot.v3"
)

var errCommandMCPAccess = errors.New("personal command MCP requires active administrator access")

type commandMCPChatKey struct{}
type commandMCPChat struct {
	chat   *tele.Chat
	sender *tele.User
}

// A policy is shared by the current tool manager and any retired managers an
// in-flight reply still holds. Revoked command connections never become valid
// again, even if the account later regains permission.
type commandMCPAccess struct {
	mu     sync.Mutex
	cfg    Config
	active map[*userCommandMCP]struct{}
}

type userCommandMCP struct {
	policy   *commandMCPAccess
	userID   int64
	lifetime context.Context
	cancel   context.CancelFunc
	registry *ToolRegistry
	client   *MCPClientManager
	close    func()
	calls    map[*userCommandMCPCall]struct{} // guarded by policy.mu
}

type userCommandMCPCall struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func (cfg Config) allowsCommandMCP(userID int64) bool {
	if !cfg.isAdmin(userID) {
		return false
	}
	if cfg.AllowedUsers[userID] || (cfg.AllowedUsers == nil && cfg.AllowedGroups == nil) {
		return true
	}
	// A stored account has no chat context. It can still use the bot in a
	// whitelisted group. Actual imports and calls check their specific chat.
	for _, allowed := range cfg.AllowedGroups {
		if allowed {
			return true
		}
	}
	return false
}

func (a *commandMCPAccess) allowsUser(userID int64) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.allowsCommandMCP(userID)
}

func (a *commandMCPAccess) authorize(userID int64) *userCommandMCP {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.cfg.allowsCommandMCP(userID) {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	conn := &userCommandMCP{
		policy: a, userID: userID, lifetime: ctx, cancel: cancel,
		registry: NewToolRegistry(), client: NewMCPClientManager(),
	}
	conn.client.stdioContext = ctx
	conn.close = sync.OnceFunc(conn.client.Close)
	if a.active == nil {
		a.active = make(map[*userCommandMCP]struct{})
	}
	a.active[conn] = struct{}{}
	return conn
}

func (a *commandMCPAccess) update(cfg Config) {
	a.mu.Lock()
	a.cfg = cfg
	var revoked []*userCommandMCP
	for conn := range a.active {
		if !cfg.allowsCommandMCP(conn.userID) {
			conn.cancel()
			delete(a.active, conn)
			revoked = append(revoked, conn)
		}
		conn.revokeCallsLocked()
	}
	a.mu.Unlock()
	for _, conn := range revoked {
		conn.close()
	}
}

func (conn *userCommandMCP) disconnect() {
	conn.policy.mu.Lock()
	conn.cancel()
	delete(conn.policy.active, conn)
	conn.revokeCallsLocked()
	conn.policy.mu.Unlock()
	conn.close()
}

func (conn *userCommandMCP) allowsCallLocked(ctx context.Context) bool {
	if conn.lifetime.Err() != nil {
		return false
	}
	if scope, ok := ctx.Value(commandMCPChatKey{}).(commandMCPChat); ok {
		return scope.sender != nil && scope.sender.ID == conn.userID &&
			conn.policy.cfg.allowsChat(scope.chat, scope.sender)
	}
	return true
}

func (conn *userCommandMCP) revokeCallsLocked() {
	for call := range conn.calls {
		if !conn.allowsCallLocked(call.ctx) {
			delete(conn.calls, call)
			call.cancel()
		}
	}
}

func (conn *userCommandMCP) beginCall(ctx context.Context) *userCommandMCPCall {
	conn.policy.mu.Lock()
	defer conn.policy.mu.Unlock()
	if !conn.allowsCallLocked(ctx) {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	call := &userCommandMCPCall{ctx: ctx, cancel: cancel}
	if conn.calls == nil {
		conn.calls = make(map[*userCommandMCPCall]struct{})
	}
	conn.calls[call] = struct{}{}
	return call
}

// finishCall checks authorization at delivery. The caller keeps the call
// registered until any result processing and completion observers have run.
func (conn *userCommandMCP) finishCall(call *userCommandMCPCall) bool {
	conn.policy.mu.Lock()
	_, allowed := conn.calls[call]
	delete(conn.calls, call)
	conn.policy.mu.Unlock()
	call.cancel()
	return allowed
}
