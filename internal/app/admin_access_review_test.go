package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	openai "github.com/sashabaranov/go-openai"
	tele "gopkg.in/telebot.v3"
	"gopkg.in/yaml.v3"
)

func TestAdminAccessSingleOptionPreservesYAML(t *testing.T) {
	cfg := adminTestConfig(t, "77", "42")
	signer, address := adminTestSSHConfig(t)
	original := fmt.Sprintf(`ADMIN_ID: &fallback_ids [77]
OPENAI_API_KEY: local-yaml-fallback
DASHBOARD_SSH_ENABLED: true
DASHBOARD_SSH_LISTEN: %q
DASHBOARD_SSH_HOST_KEY_PATH: %q
DASHBOARD_SSH_AUTHORIZED_KEYS_PATH: %q
DASHBOARD_SSH_IDLE_TIMEOUT: 45s
DASHBOARD_SSH_MAX_SESSIONS: 3
FUTURE_SETTING: {sequence: ["007", 7, false, null], fallback: *fallback_ids}
`, address, os.Getenv("DASHBOARD_SSH_HOST_KEY_PATH"), os.Getenv("DASHBOARD_SSH_AUTHORIZED_KEYS_PATH"))
	for _, key := range []string{"DASHBOARD_SSH_ENABLED", "DASHBOARD_SSH_LISTEN", "DASHBOARD_SSH_HOST_KEY_PATH", "DASHBOARD_SSH_AUTHORIZED_KEYS_PATH"} {
		t.Setenv(key, "")
	}
	if err := os.WriteFile(cfg.ConfigFilePath, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	b, tg := newAdminTestBot(t, loadConfig())
	tg.call(t, b, b.handleAdminCommand, "")
	tg.call(t, b, b.handleText, "32")
	tg.call(t, b, b.handleText, "78")
	tg.wantReply(t, "Updated: ADMIN_ID")
	tg.call(t, b, b.handleText, "6")
	tg.call(t, b, b.handleText, "11")
	tg.wantReply(t, "Updated: CONTEXT_MAX_MESSAGES")
	tg.call(t, b, b.handleText, "2")
	tg.call(t, b, b.handleText, adminTestSensitiveInput(tg.replyText(), "local-edit-under-env"))
	tg.wantReply(t, "Updated: OPENAI_API_KEY")

	var want, got map[string]any
	if err := yaml.Unmarshal([]byte(original), &want); err != nil {
		t.Fatal(err)
	}
	want["ADMIN_ID"] = 78
	want["CONTEXT_MAX_MESSAGES"] = 11
	raw, err := os.ReadFile(cfg.ConfigFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("YAML options = %#v, want %#v", got, want)
	}
	if info, err := os.Stat(cfg.ConfigFilePath); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("saved file permissions: %v, stat error: %v", info, err)
	}
	t.Setenv("ADMIN_ID", "")
	restart := loadConfig()
	if !restart.isAdmin(78) || restart.isAdmin(42) || !restart.DashboardSSHEnabled || restart.DashboardSSHListen != address || restart.DashboardSSHMaxSessions != 3 {
		t.Fatalf("restart config lost its YAML defaults: admin=%v, ssh=%t %q (%d)", restart.AdminIDs, restart.DashboardSSHEnabled, restart.DashboardSSHListen, restart.DashboardSSHMaxSessions)
	}
	if err := b.applyRuntimeConfig(restart); err != nil {
		t.Fatal(err)
	}
	startAdminTestBot(t, b)
	_ = dialAdminTestSSH(t, address, signer)
}

func TestAdminAccessFailedOptionRestoresOriginalYAML(t *testing.T) {
	cfg := adminTestConfig(t, "42", "")
	original := []byte("# local deployment\nADMIN_ID: [42]\nFUTURE_SETTING: {level: '001'}\n")
	if err := os.WriteFile(cfg.ConfigFilePath, original, 0600); err != nil {
		t.Fatal(err)
	}
	b, tg := newAdminTestBot(t, cfg)
	tg.call(t, b, b.handleAdminCommand, "")
	tg.call(t, b, b.handleText, "27")
	tg.call(t, b, b.handleText, t.TempDir())
	tg.wantReply(t, "Failed to apply config")
	raw, err := os.ReadFile(cfg.ConfigFilePath)
	if err != nil || !bytes.Equal(raw, original) || b.currentConfig().ChatDBPath != cfg.ChatDBPath {
		t.Fatalf("failed edit changed the YAML or running database, read error: %v", err)
	}
}

func TestAdminAccessAnchorEditPreservesLoadedValues(t *testing.T) {
	for _, key := range []string{"DASHBOARD_SSH_MAX_SESSIONS", "ADMIN_ID"} {
		t.Run(key, func(t *testing.T) {
			cfg := adminTestConfig(t, "42", "42")
			original := []byte("CONTEXT_MAX_MESSAGES: &n 42\n" + key + ": *n\n")
			if err := os.WriteFile(cfg.ConfigFilePath, original, 0600); err != nil {
				t.Fatal(err)
			}
			b, tg := newAdminTestBot(t, loadConfig())
			tg.call(t, b, b.handleAdminCommand, "")
			tg.call(t, b, b.handleText, "6")
			tg.call(t, b, b.handleText, "11")
			t.Setenv("ADMIN_ID", "")
			restarted := loadConfig()
			if restarted.DashboardSSHMaxSessions != 8 ||
				(key == "ADMIN_ID" && restarted.isAdmin(adminTestUserID)) {
				t.Fatalf("editing context activated an existing alias: SSH limit=%d, admin=%t",
					restarted.DashboardSSHMaxSessions, restarted.isAdmin(adminTestUserID))
			}
			if restarted.ContextMaxMsgs == 42 {
				tg.wantReply(t, "Failed to apply config")
				raw, err := os.ReadFile(cfg.ConfigFilePath)
				if err != nil || !bytes.Equal(raw, original) || b.currentConfig().ContextMaxMsgs != 42 {
					t.Fatalf("rejected edit changed disk or runtime: %v", err)
				}
			} else if restarted.ContextMaxMsgs != 11 || b.currentConfig().ContextMaxMsgs != 11 {
				t.Fatal("accepted edit did not update context on disk and in memory")
			}
		})
	}
}

type adminTestLogWriter func([]byte) (int, error)

func (f adminTestLogWriter) Write(p []byte) (int, error) { return f(p) }

func TestAdminAccessRevokedSensitiveInput(t *testing.T) {
	for _, mode := range []string{"shown", "prompt in flight", "received during prompt send"} {
		t.Run(mode, func(t *testing.T) {
			cfg := adminTestConfig(t, "42", "")
			cfg.ContextMode = "global"
			t.Setenv("CONTEXT_MODE", "global")
			t.Setenv("USER_DAILY_TOKEN_LIMIT", "0")
			const sentinel = "local-sensitive-input-sentinel"
			var loggedSensitive atomic.Bool
			logOutput := log.Writer()
			log.SetOutput(adminTestLogWriter(func(p []byte) (int, error) {
				if bytes.Contains(p, []byte(sentinel)) {
					loggedSensitive.Store(true)
				}
				return len(p), nil
			}))
			t.Cleanup(func() { log.SetOutput(logOutput) })
			provider := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
				for _, msg := range req.Messages {
					if strings.Contains(msg.Content, sentinel) {
						t.Error("stale sensitive input reached the model")
					}
				}
				if !req.Stream && strings.HasSuffix(req.Messages[0].Content, engagementReplyInstruction) {
					roleCompletion(w, `{"allow":true}`)
					return
				}
				einoModelSSE(w, einoReplyTextFrame("ordinary reply"))
				einoReplyFinish(w, "stop")
			})
			cfg.OpenAIBase = provider.url + "/v1"
			t.Setenv("OPENAI_API_BASE", cfg.OpenAIBase)
			b, tg := newAdminTestBot(t, cfg)
			tg.call(t, b, b.handleAdminCommand, "")

			var prompt string
			var finishPrompt func()
			if mode != "shown" {
				requested, release := make(chan string, 1), make(chan struct{})
				unblock := sync.OnceFunc(func() { close(release) })
				tg.mu.Lock()
				tg.beforeEdit = func(text string) {
					if strings.Contains(text, "Editing: OPENAI_API_KEY") {
						requested <- text
						<-release
					}
				}
				tg.mu.Unlock()
				done := make(chan struct{})
				var promptErr error
				go func() {
					defer close(done)
					promptErr = b.handleText(tg.message(b, "2"))
				}()
				t.Cleanup(func() {
					unblock()
					waitReplySignal(t, done, "sensitive prompt cleanup")
				})
				select {
				case prompt = <-requested:
				case <-time.After(3 * time.Second):
					t.Fatal("sensitive prompt was not requested")
				}
				finishPrompt = func() {
					unblock()
					waitReplySignal(t, done, "sensitive prompt response")
					if promptErr != nil {
						t.Fatal(promptErr)
					}
				}
			} else {
				tg.call(t, b, b.handleText, "2")
				tg.wantReply(t, "Editing: OPENAI_API_KEY")
				prompt = tg.replyText()
			}

			adminTestReload(t, b, "'*'")
			tg.call(t, b, b.handleText, "unrelated private chat after revocation")
			waitEinoReplyHistory(t, b, adminTestUserID, 2)
			if mode == "prompt in flight" {
				finishPrompt()
			}
			// The same sender's ordinary messages in another chat keep following normal routing.
			group := tg.message(b, "group context")
			group.Message().Chat = &tele.Chat{ID: -50, Type: tele.ChatSuperGroup}
			if err := b.handleText(group); err != nil || len(b.snapshot().store.Get(-50)) != 1 {
				t.Fatalf("group chat was intercepted: %v", err)
			}
			other := tg.message(b, "another user's chat")
			other.Message().Sender = &tele.User{ID: 78}
			other.Message().Chat = &tele.Chat{ID: 78, Type: tele.ChatPrivate}
			if err := b.handleText(other); err != nil {
				t.Fatal(err)
			}
			waitEinoReplyHistory(t, b, 78, 2)
			tg.call(t, b, b.handleText, adminTestSensitiveInput(prompt, sentinel))
			tg.wantReply(t, "No active config edit")
			if mode == "received during prompt send" {
				finishPrompt()
			}
			if _, exists := b.adminSessions.Get(adminTestUserID); exists {
				t.Error("late prompt restored the revoked edit")
			}
			tg.call(t, b, b.handleText, adminTestSensitiveInput(prompt, sentinel))
			tg.wantReply(t, "No active config edit")
			tg.call(t, b, b.handleText, "ordinary private chat")
			history := waitEinoReplyHistory(t, b, adminTestUserID, 4)
			if !strings.Contains(history[2].Content, "ordinary private chat") || history[3].Content != "ordinary reply" {
				t.Fatal("ordinary chat did not resume after stale inputs")
			}
			tg.mu.Lock()
			echoed := tg.echoedSensitive
			tg.mu.Unlock()
			if echoed {
				t.Error("Telegram received the stale sensitive input")
			}
			if loggedSensitive.Load() {
				t.Error("stale sensitive input reached the log")
			}
		})
	}
}

func TestAdminAccessRevokedSensitiveInputAcrossTelegramRoutes(t *testing.T) {
	cfg := adminTestConfig(t, "42", "")
	var modelCalled atomic.Bool
	provider := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ openai.ChatCompletionRequest) {
		modelCalled.Store(true)
		http.Error(w, "unexpected model call for stale config input", http.StatusBadRequest)
	})
	t.Setenv("OPENAI_API_BASE", provider.url+"/v1")
	b, tg := newAdminTestBot(t, loadConfig())
	// Use the real Telebot router with synchronous delivery for assertions.
	synchronous, err := tele.NewBot(tele.Settings{Token: cfg.TelegramToken, Synchronous: true})
	if err != nil {
		t.Fatal(err)
	}
	b.tg = synchronous
	startAdminTestBot(t, b)
	tg.call(t, b, b.handleAdminCommand, "")
	tg.call(t, b, b.handleText, "2")
	prompt := tg.replyText()
	adminTestReload(t, b, "'*'")
	input := adminTestSensitiveInput(prompt, "/remind local-sensitive-input-sentinel")
	msg := tg.message(b, input).Message()
	b.tg.ProcessUpdate(tele.Update{Message: msg})
	tg.wantReply(t, "No active config edit")
	tg.call(t, b, b.handleRemindersCommand, "")
	tg.wantReply(t, "No reminders")
	photo := tg.message(b, "").Message()
	photo.Photo = &tele.Photo{}
	photo.Caption = input
	b.tg.ProcessUpdate(tele.Update{Message: photo})
	tg.wantReply(t, "No active config edit")
	if modelCalled.Load() {
		t.Fatal("the reminder command sent a stale config value to the model")
	}
}

// Accept the prompt's advertised format. The bare-value fallback exercises
// the previous prompt protocol when running these regressions on the baseline.
func adminTestSensitiveInput(prompt, value string) string {
	for _, line := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(line, "/admin_value ") {
			return strings.Replace(line, "<value>", value, 1)
		}
	}
	return value
}

func TestAdminAccessSensitiveEditWaitsForPrompt(t *testing.T) {
	cfg := adminTestConfig(t, "42", "")
	t.Setenv("OPENAI_API_KEY", "")
	if err := os.WriteFile(cfg.ConfigFilePath, []byte("ADMIN_ID: 42\nOPENAI_API_KEY: initial-local-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg = loadConfig()
	b, tg := newAdminTestBot(t, cfg)
	tg.call(t, b, b.handleAdminCommand, "")
	requested := make(chan string, 1)
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	tg.mu.Lock()
	tg.beforeEdit = func(text string) {
		if strings.Contains(text, "Editing: OPENAI_API_KEY") {
			requested <- text
			<-release
		}
	}
	tg.mu.Unlock()
	done := make(chan struct{})
	var promptErr error
	go func() {
		defer close(done)
		promptErr = b.handleText(tg.message(b, "2"))
	}()
	t.Cleanup(func() {
		unblock()
		waitReplySignal(t, done, "prompt cleanup")
	})
	var prompt string
	select {
	case prompt = <-requested:
	case <-time.After(3 * time.Second):
		t.Fatal("sensitive prompt was not requested")
	}
	tg.call(t, b, b.handleText, "3")
	if b.currentConfig().OpenAIKey != cfg.OpenAIKey {
		t.Fatal("the second list selection was saved as the API key")
	}
	tg.call(t, b, b.handleText, adminTestSensitiveInput(prompt, "early-key"))
	if b.currentConfig().OpenAIKey != cfg.OpenAIKey {
		t.Fatal("the displayed token was accepted before the prompt response")
	}
	unblock()
	waitReplySignal(t, done, "sensitive prompt response")
	if promptErr != nil {
		t.Fatal(promptErr)
	}
	tg.call(t, b, b.handleText, adminTestSensitiveInput(prompt, "updated-local-key"))
	tg.wantReply(t, "Updated: OPENAI_API_KEY")
	if b.currentConfig().OpenAIKey != "updated-local-key" {
		t.Fatal("acknowledged prompt could not update the API key")
	}
	if restarted := loadConfig(); restarted.OpenAIKey != "updated-local-key" {
		t.Fatal("acknowledged prompt did not persist the API key")
	}
	tg.call(t, b, b.handleText, "9")
	tg.call(t, b, b.handleText, "global")
	tg.wantReply(t, "Updated: CONTEXT_MODE")
	tg.call(t, b, b.handleText, "2")
	newPrompt := tg.replyText()
	tg.call(t, b, b.handleText, adminTestSensitiveInput(prompt, "expired-key"))
	tg.wantReply(t, "No active config edit")
	session, _ := b.adminSessions.Get(adminTestUserID)
	tg.callback(t, b, session.PanelMessageID)
	tg.wantReply(t, "Admin Config Panel")
	tg.call(t, b, b.handleText, adminTestSensitiveInput(newPrompt, "canceled-key"))
	tg.wantReply(t, "No active config edit")
	if b.currentConfig().OpenAIKey != "updated-local-key" || b.currentConfig().ContextMode != "global" {
		t.Fatal("canceled input changed the key or the normal mode edit failed")
	}
}

func TestAdminAccessSensitiveInputAfterRestart(t *testing.T) {
	cfg := adminTestConfig(t, "42", "")
	t.Setenv("USER_DAILY_TOKEN_LIMIT", "0")
	const sentinel = "local-sensitive-input-sentinel"
	var leaked atomic.Bool
	provider := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
		for _, msg := range req.Messages {
			if strings.Contains(msg.Content, sentinel) {
				leaked.Store(true)
			}
		}
		einoModelSSE(w, einoReplyTextFrame("ordinary reply"))
		einoReplyFinish(w, "stop")
	})
	t.Setenv("OPENAI_API_BASE", provider.url+"/v1")
	previous, previousTG := newAdminTestBot(t, cfg)
	previousTG.call(t, previous, previous.handleAdminCommand, "")
	previousTG.call(t, previous, previous.handleText, "2")
	input := adminTestSensitiveInput(previousTG.replyText(), sentinel)
	if err := previous.stats.Close(); err != nil {
		t.Fatal(err)
	}
	if err := previous.chatDB.Close(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ADMIN_ID", "*")
	b, tg := newAdminTestBot(t, loadConfig())
	synchronous, err := tele.NewBot(tele.Settings{Token: cfg.TelegramToken, Synchronous: true})
	if err != nil {
		t.Fatal(err)
	}
	b.tg = synchronous
	startAdminTestBot(t, b)
	process := func(text string) {
		b.tg.ProcessUpdate(tele.Update{Message: tg.message(b, text).Message()})
	}
	process("ordinary after restart")
	waitEinoReplyHistory(t, b, adminTestUserID, 2)
	process(input)
	for _, msg := range b.snapshot().store.Get(adminTestUserID) {
		if strings.Contains(msg.Content, sentinel) {
			t.Fatal("stale sensitive input reached history after restart")
		}
	}
	tg.wantReply(t, "No active config edit")
	if leaked.Load() || b.currentConfig().OpenAIKey != cfg.OpenAIKey {
		t.Fatal("stale sensitive input reached the model or changed the API key")
	}
}

type adminAccessBarrierContext struct {
	tele.Context
	calls   int
	at      int
	started chan struct{}
	resume  chan struct{}
}

func (c *adminAccessBarrierContext) Sender() *tele.User {
	c.calls++
	if c.calls == c.at {
		close(c.started)
		<-c.resume
	}
	return c.Context.Sender()
}

func TestAdminAccessConcurrentWhitelistRevocation(t *testing.T) {
	for _, name := range []string{"config", "schedule private", "schedule group"} {
		t.Run(name, func(t *testing.T) {
			cfg := adminTestConfig(t, "42", "")
			cfg.AllowedUsers = map[int64]bool{adminTestUserID: true}
			cfg.AllowedGroups = map[int64]bool{-50: true}
			b, tg := newAdminTestBot(t, cfg)
			telegramContext := tg.message(b, "11")
			handler, at := b.handleAdminTextIfNeeded, 3
			if name == "config" {
				tg.call(t, b, b.handleAdminCommand, "")
				tg.call(t, b, b.handleText, "6")
			} else {
				handler, at = b.handleScheduleTextIfNeeded, 4
				telegramContext.Message().Text = "off"
				if name == "schedule group" {
					telegramContext.Message().Chat = &tele.Chat{ID: -50, Type: tele.ChatSuperGroup}
				}
				b.scheduleWizard.Set(adminTestUserID, ScheduleWizardSession{
					Step: "context", ChatID: telegramContext.Chat().ID, ID: "revoked",
					CronExpr: "0 9 * * *", Timezone: "UTC", Prompt: "local fixture",
				})
			}
			before, err := os.ReadFile(cfg.ConfigFilePath)
			if err != nil {
				t.Fatal(err)
			}

			ctx := &adminAccessBarrierContext{Context: telegramContext, at: at, started: make(chan struct{}), resume: make(chan struct{})}
			unblock := sync.OnceFunc(func() { close(ctx.resume) })
			done := make(chan struct{})
			var handled bool
			go func() {
				defer close(done)
				handled, err = handler(ctx, telegramContext.Text())
			}()
			t.Cleanup(func() {
				unblock()
				waitReplySignal(t, done, "write cleanup")
			})
			waitReplySignal(t, ctx.started, "authorized handler before its write")
			next := cfg
			next.AllowedUsers = map[int64]bool{78: true}
			next.AllowedGroups = map[int64]bool{-51: true}
			if err := b.applyRuntimeConfig(next); err != nil {
				t.Fatal(err)
			}
			unblock()
			waitReplySignal(t, done, "attempted write")
			if err != nil || !handled || !b.isAdmin(adminTestUserID) {
				t.Fatalf("fixture did not reach the still-admin handler: handled=%t err=%v", handled, err)
			}
			after, err := os.ReadFile(cfg.ConfigFilePath)
			if err != nil || !bytes.Equal(before, after) || len(b.tasks.List(telegramContext.Chat().ID)) != 0 {
				t.Fatalf("an in-flight privileged edit committed after whitelist revocation, read error: %v", err)
			}
		})
	}
}

func TestAdminAccessPersonalMCPMutationAfterChatRevocation(t *testing.T) {
	for _, command := range []string{"delete", "clear"} {
		t.Run(command, func(t *testing.T) {
			cfg := adminTestConfig(t, "42", "")
			cfg.ToolsEnabled, cfg.ToolsSummaryEnabled = true, false
			cfg.AllowedUsers = map[int64]bool{adminTestUserID: true}
			cfg.AllowedGroups = map[int64]bool{-50: true}
			stdio, marker, _ := adminTestCommandMCP(t)
			network := newReplyMCPServer(t, "ordinary_personal")
			b, tg := newAdminTestBot(t, cfg)
			t.Cleanup(func() { snap := b.snapshot(); closeToolResources(snap.mcpClients, snap.userTools) })
			manager := b.snapshot().userTools
			added, err := manager.AddServers(adminTestUserID, map[string]MCPServerConfig{
				"ordinary": {Type: "http", URL: network.url},
				"command":  stdio,
			})
			if err != nil || len(added.Succeeded) != 2 {
				t.Fatalf("could not create test servers: %+v, %v", added, err)
			}
			registry := manager.GetRegistry(adminTestUserID)
			handler := b.handleMCPDel
			if command == "clear" {
				handler = b.handleMCPClear
			}
			ctx := &adminAccessBarrierContext{
				Context: tg.message(b, "ordinary"), at: 2, started: make(chan struct{}), resume: make(chan struct{}),
			}
			unblock := sync.OnceFunc(func() { close(ctx.resume) })
			done := make(chan struct{})
			var handlerErr error
			go func() {
				defer close(done)
				handlerErr = handler(ctx)
			}()
			t.Cleanup(func() {
				unblock()
				waitReplySignal(t, done, "personal MCP write cleanup")
			})
			waitReplySignal(t, ctx.started, "personal MCP command after the old whitelist check")

			limited := cfg
			limited.AllowedUsers = map[int64]bool{78: true}
			if err := b.applyRuntimeConfig(limited); err != nil {
				t.Fatal(err)
			}
			unblock()
			waitReplySignal(t, done, "revoked personal MCP command")
			stored, err := manager.store.Get(adminTestUserID)
			if handlerErr != nil || err != nil || stored == nil || stored.MCPServers["ordinary"].URL != network.url ||
				manager.GetRegistry(adminTestUserID) != registry {
				t.Errorf("revoked private command modified the store or registry: handler error=%v, read error=%v", handlerErr, err)
			}
			if events, err := os.ReadFile(marker); err != nil || string(events) != "start\n" {
				t.Errorf("revoked command restarted the stdio process: events=%q, error=%v", events, err)
			}
			if t.Failed() {
				return
			}

			allowed := tg.message(b, "ordinary")
			allowed.Message().Chat = &tele.Chat{ID: -50, Type: tele.ChatSuperGroup}
			if err := handler(allowed); err != nil {
				t.Fatal(err)
			}
			stored, err = manager.store.Get(adminTestUserID)
			if err != nil || (command == "clear" && stored != nil) ||
				(command == "delete" && (stored == nil || len(stored.MCPServers) != 1 || stored.MCPServers["command"].Command != stdio.Command)) {
				t.Fatalf("authorized group could not remove the requested personal configuration: %v", err)
			}
		})
	}
}

func TestAdminAccessMCPFixture(t *testing.T) {
	path := os.Getenv("ADMIN_ACCESS_MCP_FIXTURE")
	if path == "" {
		return
	}
	tracker, err := net.DialTimeout("tcp", os.Getenv("ADMIN_ACCESS_MCP_TRACKER"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer tracker.Close()
	record := func(event string) {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
		if err != nil {
			panic(err)
		}
		if _, err := fmt.Fprintln(f, event); err != nil {
			panic(err)
		}
		if err := f.Close(); err != nil {
			panic(err)
		}
	}
	record("start")
	s := server.NewMCPServer("local command fixture", "1")
	s.AddTool(mcp.NewTool("local_command", mcp.WithBoolean("block"), mcp.WithBoolean("fail")), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if req.GetBool("block", false) {
			if _, err := tracker.Write([]byte{1}); err != nil {
				return nil, err
			}
			var signal [1]byte
			if _, err := io.ReadFull(tracker, signal[:]); err != nil {
				return nil, err
			}
			return mcp.NewToolResultText("in-flight private result"), nil
		}
		record("call")
		if req.GetBool("fail", false) {
			return mcp.NewToolResultError("local private tool error"), nil
		}
		return mcp.NewToolResultText("local command result"), nil
	})
	if err := server.ServeStdio(s); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func adminTestCommandMCP(t *testing.T) (config MCPServerConfig, marker string, connection func() *net.TCPConn) {
	t.Helper()
	marker = filepath.Join(t.TempDir(), "command-events")
	tracker, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tracker.Close() })
	commandConnection := func() *net.TCPConn {
		t.Helper()
		if err := tracker.SetDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		conn, err := tracker.AcceptTCP()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return MCPServerConfig{
		Type: "stdio", Command: executable, Args: []string{"-test.run=^TestAdminAccessMCPFixture$"},
		Env: FlexEnv{Pairs: []string{"ADMIN_ACCESS_MCP_FIXTURE=" + marker, "ADMIN_ACCESS_MCP_TRACKER=" + tracker.Addr().String()}},
	}, marker, commandConnection
}

func TestAdminAccessHistoricalPersonalMCP(t *testing.T) {
	cfg := adminTestConfig(t, "'*'", "")
	cfg.ToolsEnabled = true
	cfg.ToolsSummaryEnabled = false
	cfg.StickerRulesPath = filepath.Join(t.TempDir(), "rules.json")
	cfg.UserMCPDBPath = filepath.Join(t.TempDir(), "users.db")
	cfg.MCPConfigPath = filepath.Join(t.TempDir(), "global.json")
	stdio, marker, commandConnection := adminTestCommandMCP(t)
	network := newReplyMCPServer(t, "ordinary_personal")
	global := newReplyMCPServer(t, "ordinary_global")
	writeReplyMCPConfig(t, cfg.MCPConfigPath, global.url)
	store, err := NewUserMCPStore(cfg.UserMCPDBPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(adminTestUserID, &MCPConfig{MCPServers: map[string]MCPServerConfig{
		"history":  stdio,
		"ordinary": {Type: "http", URL: network.url},
	}}); err != nil {
		t.Fatal(err)
	}
	store.Close()

	b, tg := newAdminTestBot(t, cfg)
	t.Cleanup(func() { snap := b.snapshot(); closeToolResources(snap.mcpClients, snap.userTools) })
	events := func() string {
		t.Helper()
		raw, err := os.ReadFile(marker)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return string(raw)
	}
	if got := events(); got != "" {
		t.Errorf("unauthorized connection started at startup: %q", got)
	}
	tg.call(t, b, b.handleText, fmt.Sprintf(`{"mcpServers":{"temporary":{"type":"http","url":%q}}}`, network.url))
	tg.call(t, b, b.handleMCPDel, "temporary")
	if got := events(); got != "" {
		t.Errorf("ordinary import/delete started unauthorized historical command: %q", got)
	}
	cfg.MCPConfigPath = filepath.Join(t.TempDir(), "global-after-reload.json")
	writeReplyMCPConfig(t, cfg.MCPConfigPath, global.url)
	if err := b.applyRuntimeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if got := events(); got != "" {
		t.Errorf("unauthorized configuration reload started a historical command: %q", got)
	}
	oldOrdinary := b.userTools.GetRegistry(adminTestUserID).Get("ordinary_personal")
	oldGlobal := b.tools.Get("ordinary_global")
	if oldOrdinary == nil || oldGlobal == nil {
		t.Fatal("ordinary network tools were not restored")
	}

	authorized := cfg
	authorized.AdminWildcard, authorized.AdminIDs = false, map[int64]bool{adminTestUserID: true}
	if err := b.applyRuntimeConfig(authorized); err != nil {
		t.Fatal(err)
	}
	firstConnection := commandConnection()
	oldSnap, generation := b.snapshotForReply()
	defer b.releaseReplyTools(generation)
	reg := oldSnap.userTools.GetRegistry(adminTestUserID)
	if reg.Get("ordinary_personal") != oldOrdinary || oldSnap.tools.Get("ordinary_global") != oldGlobal {
		t.Fatal("authorizing the command replaced ordinary MCP connections")
	}
	tools, err := newEinoTools(NewMergedToolView(oldSnap.tools, reg), false)
	if err != nil {
		t.Fatal(err)
	}
	tools.RoundToolInfos()
	var privileged *einoTool
	for _, tool := range tools.tools {
		if tool.info.Name == "local_command" {
			privileged = tool
			break
		}
	}
	if privileged == nil {
		t.Fatal("explicit authorization did not restore the stored command tool")
	}
	user := &tele.User{ID: adminTestUserID}
	privateCtx := context.WithValue(t.Context(), commandMCPChatKey{}, commandMCPChat{&tele.Chat{ID: adminTestUserID, Type: tele.ChatPrivate}, user})
	groupCtx := context.WithValue(t.Context(), commandMCPChatKey{}, commandMCPChat{&tele.Chat{ID: -50, Type: tele.ChatSuperGroup}, user})
	call := func(ctx context.Context, want string) {
		t.Helper()
		if got, err := privileged.InvokableRun(ctx, "{}"); got != want || err != nil {
			t.Errorf("command call = %q, %v, want %q", got, err, want)
		}
	}
	call(privateCtx, "local command result")

	limited := authorized
	limited.AllowedUsers = map[int64]bool{78: true}
	limited.AllowedGroups = map[int64]bool{-50: true}

	// Start a call in the private chat and let the same command process
	// survive loss of that chat's access. A later grant must not revive it.
	inFlight := make(chan struct{})
	var result string
	var callErr error
	go func() {
		defer close(inFlight)
		result, callErr = privileged.InvokableRun(privateCtx, `{"block":true}`)
	}()
	t.Cleanup(func() {
		_ = firstConnection.Close()
		waitReplySignal(t, inFlight, "personal MCP call cleanup")
	})
	if err := firstConnection.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var started [1]byte
	if _, err := io.ReadFull(firstConnection, started[:]); err != nil {
		t.Fatalf("private MCP call did not start: %v", err)
	}
	if err := b.applyRuntimeConfig(limited); err != nil {
		t.Fatal(err)
	}
	if err := b.applyRuntimeConfig(authorized); err != nil {
		t.Fatal(err)
	}
	if _, err := firstConnection.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	waitReplySignal(t, inFlight, "personal MCP result after revocation")
	const denied = "error: personal command MCP requires active administrator access"
	if result != denied || callErr != nil {
		t.Errorf("revoked private MCP RPC returned %q, %v to Eino", result, callErr)
	}
	if err := b.applyRuntimeConfig(limited); err != nil {
		t.Fatal(err)
	}
	call(privateCtx, denied)
	call(groupCtx, "local command result")

	// Retire the current tool generation while an existing reply still holds it.
	retired := authorized
	retired.MCPConfigPath = filepath.Join(t.TempDir(), "new-global.json")
	writeReplyMCPConfig(t, retired.MCPConfigPath, global.url)
	if err := b.applyRuntimeConfig(retired); err != nil {
		t.Fatal(err)
	}
	secondConnection := commandConnection()
	revoked := retired
	revoked.AllowedUsers = map[int64]bool{78: true}
	if err := b.applyRuntimeConfig(revoked); err != nil {
		t.Fatal(err)
	}
	call(privateCtx, denied)
	for _, conn := range []*net.TCPConn{firstConnection, secondConnection} {
		if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		var buffer [1]byte
		if _, err := conn.Read(buffer[:]); err != io.EOF {
			t.Errorf("revoked command process kept its tracker open: %v", err)
		}
	}
	current := b.snapshot()
	if current.userTools.GetRegistry(adminTestUserID).Get("local_command") != nil {
		t.Error("revoked command remained available in the current registry")
	}
	for _, tool := range []MCPTool{oldOrdinary, oldGlobal} {
		if _, err := tool.Execute("{}"); err != nil {
			t.Errorf("in-flight ordinary tool %s was disconnected: %v", tool.Name(), err)
		}
	}
	if got := events(); got != "start\ncall\ncall\nstart\n" {
		t.Errorf("command events after revocation = %q", got)
	}
	stored, err := current.userTools.store.Get(adminTestUserID)
	if err != nil || stored == nil || stored.MCPServers["history"].Command != stdio.Command {
		t.Fatalf("revocation changed the stored personal command, error=%v", err)
	}
	if err := b.applyRuntimeConfig(retired); err != nil {
		t.Fatal(err)
	}
	call(privateCtx, denied)
	if got := events(); got != "start\ncall\ncall\nstart\nstart\n" {
		t.Errorf("command events after restoration = %q", got)
	}
}
