package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	openai "github.com/sashabaranov/go-openai"
	tele "gopkg.in/telebot.v3"
)

func useAdminTestTelegramRouter(t *testing.T, b *Bot, tg *adminTestTelegram, onError func(error, tele.Context)) {
	t.Helper()
	bot, err := tele.NewBot(tele.Settings{
		Token: b.currentConfig().TelegramToken, Offline: true, Synchronous: true, OnError: onError,
		Client: &http.Client{Transport: adminTestTransport(func(req *http.Request) (*http.Response, error) {
			w := httptest.NewRecorder()
			tg.serveHTTP(w, req)
			return w.Result(), nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	b.tg = bot
	startAdminTestBot(t, b)
}

func TestAdminAccessDelayedOrdinaryRetryKeepsVisibleField(t *testing.T) {
	cfg := adminTestConfig(t, "42", "")
	b, tg := newAdminTestBot(t, cfg)
	synctest.Test(t, func(t *testing.T) {
		useAdminTestTelegramRouter(t, b, tg, nil)
		process := func(text string) {
			b.tg.ProcessUpdate(tele.Update{Message: tg.message(b, text).Message()})
		}
		process("/admin")
		process("6")
		tg.wantReply(t, "Editing: CONTEXT_MAX_MESSAGES")
		panel, _ := b.adminSessions.Get(adminTestUserID)
		started, release := make(chan struct{}), make(chan struct{})
		tg.mu.Lock()
		tg.beforeEdit = func(text string) {
			if strings.Contains(text, "Invalid input") && strings.Contains(text, "Editing: CONTEXT_MAX_MESSAGES") {
				close(started)
				<-release
			}
		}
		tg.mu.Unlock()
		retried := make(chan struct{})
		go func() {
			defer close(retried)
			process("0")
		}()
		<-started

		reopened := make(chan struct{})
		go func() {
			defer close(reopened)
			process("/admin")
			process("7")
		}()
		// Let the new panel either reach Telegram or wait behind the blocked edit.
		synctest.Wait()
		close(release)
		<-retried
		<-reopened

		prompt := tg.replyText()
		active, _ := b.adminSessions.Get(adminTestUserID)
		if active.PanelMessageID != panel.PanelMessageID {
			t.Fatalf("the reopen used panel %d, want the original %d", active.PanelMessageID, panel.PanelMessageID)
		}
		process("123")
		got := b.currentConfig()
		if !strings.Contains(prompt, "Editing: MAX_TOKENS") || got.ContextMaxMsgs != cfg.ContextMaxMsgs || got.MaxTokens != 123 {
			_, field, _ := strings.Cut(prompt, "Editing: ")
			t.Fatalf("visible prompt was for %q, submission left CONTEXT_MAX_MESSAGES=%d and MAX_TOKENS=%d",
				strings.SplitN(field, "\n", 2)[0], got.ContextMaxMsgs, got.MaxTokens)
		}
	})
}

func TestAdminAccessOrdinaryRetryAfterBothPanelRequestsFail(t *testing.T) {
	adminTestConfig(t, "42", "")
	var modelCalled atomic.Bool
	provider := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ openai.ChatCompletionRequest) {
		modelCalled.Store(true)
		einoModelSSE(w, einoReplyTextFrame("ordinary reply"))
		einoReplyFinish(w, "stop")
	})
	t.Setenv("OPENAI_API_BASE", provider.url+"/v1")
	t.Setenv("USER_DAILY_TOKEN_LIMIT", "0")
	cfg := loadConfig()
	b, tg := newAdminTestBot(t, cfg)
	var routeErrors []error
	useAdminTestTelegramRouter(t, b, tg, func(err error, _ tele.Context) {
		routeErrors = append(routeErrors, err)
	})
	process := func(text string) {
		b.tg.ProcessUpdate(tele.Update{Message: tg.message(b, text).Message()})
	}
	process("/admin")
	process("6")
	tg.wantReply(t, "Editing: CONTEXT_MAX_MESSAGES")
	var failures []string
	tg.rejectMessage = func(path, text string) bool {
		if strings.Contains(text, "Invalid input") {
			failures = append(failures, path)
			return true
		}
		return false
	}
	process("0")
	if len(routeErrors) != 1 || len(failures) != 2 ||
		!strings.HasSuffix(failures[0], "/editMessageText") || !strings.HasSuffix(failures[1], "/sendMessage") {
		t.Fatalf("retry did not fail at both Telegram operations: paths=%v errors=%v", failures, routeErrors)
	}
	tg.wantReply(t, "Editing: CONTEXT_MAX_MESSAGES")
	process("12")
	if got := b.currentConfig().ContextMaxMsgs; got != 12 {
		history := waitEinoReplyHistory(t, b, adminTestUserID, 2)
		t.Fatalf("correction from the still-visible prompt: CONTEXT_MAX_MESSAGES=%d, chat history starts with %q",
			got, history[0].Content)
	}
	if modelCalled.Load() || len(b.snapshot().store.Get(adminTestUserID)) != 0 {
		t.Fatal("the correction also entered regular chat")
	}
	if got := loadConfig().ContextMaxMsgs; got != 12 {
		t.Fatalf("the corrected value was not persisted: CONTEXT_MAX_MESSAGES=%d", got)
	}
}

func TestAdminAccessPersonalMCPDeliveryAfterCompletionObserver(t *testing.T) {
	const (
		privateText  = "local command result"
		privateError = "local private tool error"
		denied       = "error: personal command MCP requires active administrator access"
	)
	for _, tc := range []struct {
		name             string
		args             string
		revoke           bool
		restore          bool
		cancel           bool
		cancelAtDelivery bool
		want             string
		wantErr          error
		eventSuccess     bool
	}{
		{name: "authorized", args: "{}", want: privateText, eventSuccess: true},
		{name: "authorized tool error", args: `{"fail":true}`, want: "error: " + privateError},
		{name: "revoked", args: "{}", revoke: true, want: denied, eventSuccess: true},
		{name: "revoked then restored", args: "{}", revoke: true, restore: true, want: denied, eventSuccess: true},
		{name: "tool error revoked then restored", args: `{"fail":true}`, revoke: true, restore: true, want: denied},
		{name: "caller canceled", args: "{}", cancel: true, wantErr: context.Canceled, eventSuccess: true},
		{name: "caller canceled during final authorization", args: "{}", cancelAtDelivery: true, wantErr: context.Canceled, eventSuccess: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var loggedPayload atomic.Bool
			logOutput := log.Writer()
			log.SetOutput(adminTestLogWriter(func(p []byte) (int, error) {
				if text := string(p); strings.Contains(text, privateText) || strings.Contains(text, privateError) {
					loggedPayload.Store(true)
				}
				return len(p), nil
			}))
			t.Cleanup(func() { log.SetOutput(logOutput) })

			cfg := adminTestConfig(t, "42", "")
			cfg.ToolsEnabled = true
			cfg.ToolsSummaryEnabled = false
			cfg.UserMCPDBPath = filepath.Join(t.TempDir(), "users.db")
			cfg.MCPConfigPath = ""
			stdio, _, _ := adminTestCommandMCP(t)
			store, err := NewUserMCPStore(cfg.UserMCPDBPath)
			if err != nil {
				t.Fatal(err)
			}
			err = store.Save(adminTestUserID, &MCPConfig{MCPServers: map[string]MCPServerConfig{"local": stdio}})
			store.Close()
			if err != nil {
				t.Fatal(err)
			}
			b, _ := newAdminTestBot(t, cfg)
			t.Cleanup(func() { snap := b.snapshot(); closeToolResources(snap.mcpClients, snap.userTools) })
			snap := b.snapshot()
			tools, err := newEinoTools(NewMergedToolView(snap.tools, snap.userTools.GetRegistry(adminTestUserID)), false)
			if err != nil {
				t.Fatal(err)
			}
			assertEinoToolNames(t, tools, "local_command")
			privileged := tools.tools[0]

			ctx, cancel := context.WithCancel(context.WithValue(t.Context(), commandMCPChatKey{}, commandMCPChat{
				chat: &tele.Chat{ID: adminTestUserID, Type: tele.ChatPrivate}, sender: &tele.User{ID: adminTestUserID},
			}))
			defer cancel()
			finished, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			var events []DashboardEvent
			ctx = withEinoToolEvents(ctx, UsageContext{}, func(event DashboardEvent) {
				events = append(events, event)
				if event.Type == DashboardEventToolCallFinished {
					close(finished)
					<-release
				}
			})
			var result string
			var callErr error
			delivered := make(chan struct{})
			go func() {
				defer close(delivered)
				result, callErr = privileged.InvokableRun(ctx, tc.args)
			}()
			t.Cleanup(func() {
				unblock()
				waitReplySignal(t, delivered, "Eino call cleanup")
			})
			waitReplySignal(t, finished, "Eino tool completion observer")
			if tc.revoke {
				limited := cfg
				limited.AllowedUsers = map[int64]bool{78: true}
				limited.AllowedGroups = map[int64]bool{-50: true}
				reconfigured := make(chan error, 1)
				go func() {
					err := b.applyRuntimeConfig(limited)
					if err == nil && tc.restore {
						err = b.applyRuntimeConfig(cfg)
					}
					reconfigured <- err
				}()
				select {
				case err := <-reconfigured:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("changing access waited for the completion observer")
				}
			}
			if tc.cancel {
				cancel()
			}
			if tc.cancelAtDelivery {
				// Cancel as finishCall releases the registered RPC, after the
				// completion observer and before the caller receives the result.
				command := privileged.registered.(*MCPRemoteTool).command
				command.policy.mu.Lock()
				if len(command.calls) != 1 {
					command.policy.mu.Unlock()
					t.Fatal("expected one registered RPC at delivery")
				}
				for call := range command.calls {
					releaseRPC := call.cancel
					call.cancel = func() {
						cancel()
						releaseRPC()
					}
				}
				command.policy.mu.Unlock()
			}
			unblock()
			waitReplySignal(t, delivered, "InvokableRun return")
			if result != tc.want || !errors.Is(callErr, tc.wantErr) {
				t.Errorf("InvokableRun returned (%q, %v), want (%q, %v)", result, callErr, tc.want, tc.wantErr)
			}
			if len(events) != 2 || events[1].Success != tc.eventSuccess {
				t.Fatalf("unexpected tool events: %+v", events)
			}
			for _, event := range events {
				if content := fmt.Sprintf("%+v", event); strings.Contains(content, privateText) || strings.Contains(content, privateError) {
					t.Error("dashboard event included private MCP content")
				}
			}
			if loggedPayload.Load() {
				t.Error("private MCP content reached the log")
			}
		})
	}
}
