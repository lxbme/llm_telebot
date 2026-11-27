package app

import (
	"bufio"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"golang.org/x/crypto/ssh"
	tele "gopkg.in/telebot.v3"
)

const adminTestUserID = 42

func TestAdminAccessStartup(t *testing.T) {
	for _, test := range []struct {
		name, yaml, env string
		admin           bool
	}{
		{name: "explicit ID", yaml: "42", admin: true},
		{name: "YAML wildcard", yaml: "'*'"},
		{name: "environment wildcard overrides YAML", yaml: "42", env: " * "},
		{name: "wildcard in YAML list", yaml: "[42, '*']"},
		{name: "explicit environment overrides YAML wildcard", yaml: "'*'", env: "42", admin: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := adminTestConfig(t, test.yaml, test.env)
			b, telegram := newAdminTestBot(t, cfg)
			telegram.call(t, b, b.handleAdminCommand, "")
			telegram.wantReply(t, map[bool]string{true: "Admin Config Panel", false: "Only admins"}[test.admin])
			_, panelOpened := b.adminSessions.Get(adminTestUserID)
			if panelOpened != test.admin {
				t.Errorf("config panel opened = %t, want %t", panelOpened, test.admin)
			}

			b.stats.ApplyQuotaDelta(UsageEvent{UserID: adminTestUserID, TotalTokens: 10}, currentLocalDate(cfg.quotaResetLocation), true)
			_, quotaOK := b.checkDailyQuota(adminTestUserID, adminTestUserID)
			if quotaOK != test.admin {
				t.Errorf("quota bypass = %t, want %t", quotaOK, test.admin)
			}
			telegram.call(t, b, b.handleQuotaReset, "me")
			telegram.wantReply(t, map[bool]string{true: "Quota reset", false: "Only admins"}[test.admin])
		})
	}
}

func TestAdminAccessEditorRejectsWildcard(t *testing.T) {
	cfg := adminTestConfig(t, "42", "")
	b, telegram := newAdminTestBot(t, cfg)
	telegram.call(t, b, b.handleAdminCommand, "")
	telegram.call(t, b, b.handleText, "32")

	before, err := os.ReadFile(cfg.ConfigFilePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, wildcard := range []string{"*", "42, *"} {
		telegram.call(t, b, b.handleText, wildcard)
		telegram.wantReply(t, "Invalid input")
		after, err := os.ReadFile(cfg.ConfigFilePath)
		if err != nil || string(after) != string(before) {
			t.Fatalf("invalid edit %q changed the config file, read error: %v", wildcard, err)
		}
	}
	telegram.call(t, b, b.handleText, "42, 77")
	telegram.wantReply(t, "Updated: ADMIN_ID")
	values, err := loadYAMLConfigValues(cfg.ConfigFilePath)
	if err != nil || values["ADMIN_ID"] != "42,77" || !b.isAdmin(77) {
		t.Fatalf("explicit edit did not take effect, saved ADMIN_ID = %q, error: %v", values["ADMIN_ID"], err)
	}
}

func TestAdminAccessEditorHonorsEnvironmentOverride(t *testing.T) {
	cfg := adminTestConfig(t, "77", "42")
	b, telegram := newAdminTestBot(t, cfg)
	telegram.call(t, b, b.handleAdminCommand, "")
	telegram.call(t, b, b.handleText, "32")
	telegram.call(t, b, b.handleText, "78")
	telegram.wantReply(t, "Updated: ADMIN_ID")
	if !b.isAdmin(42) || b.isAdmin(78) {
		t.Fatal("the persisted YAML value replaced the effective environment override")
	}
	values, err := loadYAMLConfigValues(cfg.ConfigFilePath)
	if err != nil || values["ADMIN_ID"] != "78" {
		t.Fatalf("saved ADMIN_ID = %q, error: %v", values["ADMIN_ID"], err)
	}
}

func TestAdminAccessSaveRejectsEffectiveWildcard(t *testing.T) {
	cfg := adminTestConfig(t, "42", "*")
	if err := saveConfigFile(cfg); err == nil {
		t.Fatal("saved a config containing a wildcard administrator setting")
	}
	values, err := loadYAMLConfigValues(cfg.ConfigFilePath)
	if err != nil || values["ADMIN_ID"] != "42" {
		t.Fatalf("saved ADMIN_ID = %q, error: %v", values["ADMIN_ID"], err)
	}
}

func TestAdminAccessReloadRevokesSessions(t *testing.T) {
	cfg := adminTestConfig(t, "42", "")
	provider := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
		if !req.Stream && strings.HasSuffix(req.Messages[0].Content, engagementReplyInstruction) {
			roleCompletion(w, `{"allow":true}`)
			return
		}
		einoModelSSE(w, einoReplyTextFrame("ordinary reply"))
		einoReplyFinish(w, "stop")
	})
	cfg.OpenAIBase = provider.url + "/v1"
	t.Setenv("OPENAI_API_BASE", cfg.OpenAIBase)
	b, telegram := newAdminTestBot(t, cfg)
	telegram.call(t, b, b.handleAdminCommand, "")
	telegram.call(t, b, b.handleText, "32")
	oldPanel, ok := b.adminSessions.Get(adminTestUserID)
	if !ok || oldPanel.Step != "edit" {
		t.Fatal("admin never reached the config edit step")
	}
	telegram.call(t, b, b.handleScheduleNewCommand, "")
	if _, ok := b.scheduleWizard.Get(adminTestUserID); !ok {
		t.Fatal("explicit admin could not start the schedule wizard")
	}
	// An explicit administrator can also change schedules through the JSON entry.
	adminSchedule := `{"schedule":{"action":"upsert","id":"brief","prompt":"test","time":{"cron":"0 9 * * *","timezone":"UTC"}}}`
	handled, err := b.handleScheduleMessage(telegram.message(b, adminSchedule), adminSchedule)
	if err != nil || !handled || len(b.tasks.List(adminTestUserID)) != 1 {
		t.Fatalf("explicit administrator could not save a schedule, handled = %t, error = %v", handled, err)
	}

	adminTestReload(t, b, "'*'")
	telegram.callback(t, b, oldPanel.PanelMessageID)
	telegram.wantReply(t, "Only admins")
	for _, command := range []struct {
		handler func(tele.Context) error
		text    string
	}{
		{b.handleScheduleNewCommand, ""},
		{b.handleSchedulePauseCommand, "brief"},
		{b.handleSpeechMode, "on"},
		{b.handleQuotaReset, "me"},
		{b.handleText, `{"schedule":{"action":"delete","id":"brief"}}`},
	} {
		telegram.call(t, b, command.handler, command.text)
		telegram.wantReply(t, "Only admins")
	}
	if _, ok := b.adminSessions.Get(adminTestUserID); ok {
		t.Error("reload retained the admin editor session")
	}
	if _, ok := b.scheduleWizard.Get(adminTestUserID); ok {
		t.Error("reload retained the schedule wizard session")
	}
	if tasks := b.tasks.List(adminTestUserID); len(tasks) != 1 || !tasks[0].Enabled {
		t.Errorf("wildcard allowed changing a schedule: %+v", tasks)
	}

	telegram.call(t, b, b.handleRemindersCommand, "")
	telegram.wantReply(t, "No reminders")
	telegram.call(t, b, b.handleText, "hello")
	history := waitEinoReplyHistory(t, b, adminTestUserID, 2)
	if history[1].Content != "ordinary reply" {
		t.Errorf("reply after revocation = %q", history[1].Content)
	}

	adminTestReload(t, b, "42")
	telegram.call(t, b, b.handleAdminCommand, "")
	newPanel, ok := b.adminSessions.Get(adminTestUserID)
	if !ok || newPanel.PanelMessageID == oldPanel.PanelMessageID {
		t.Fatal("restored explicit admin did not receive a fresh panel")
	}
	telegram.callback(t, b, oldPanel.PanelMessageID)
	if current, ok := b.adminSessions.Get(adminTestUserID); !ok || current != newPanel {
		t.Error("a revoked button changed the new admin session")
	}
	telegram.callback(t, b, newPanel.PanelMessageID)
	if _, ok := b.adminSessions.Get(adminTestUserID); ok {
		t.Error("the new panel's cancel button did not close the session")
	}
	telegram.call(t, b, b.handleSchedulePauseCommand, "brief")
	if tasks := b.tasks.List(adminTestUserID); len(tasks) != 1 || tasks[0].Enabled {
		t.Errorf("restored admin could not pause a schedule: %+v", tasks)
	}
}

func TestAdminAccessInFlightPanelsStayRevoked(t *testing.T) {
	for _, name := range []string{"config", "schedule"} {
		t.Run(name, func(t *testing.T) {
			cfg := adminTestConfig(t, "42", "")
			b, telegram := newAdminTestBot(t, cfg)
			handler := b.handleAdminCommand
			if name == "schedule" {
				handler = b.handleScheduleNewCommand
			}
			started, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			telegram.mu.Lock()
			telegram.beforeSend = func() { close(started); <-release }
			telegram.mu.Unlock()
			done := make(chan error, 1)
			go func() { done <- handler(telegram.message(b, "")) }()
			t.Cleanup(unblock)
			waitReplySignal(t, started, "first panel send")

			configPath := b.currentConfig().ConfigFilePath
			writeAdminTestYAML(t, configPath, "'*'")
			next := loadConfig()
			reloaded := make(chan error, 1)
			go func() { reloaded <- b.applyRuntimeConfig(next) }()
			select {
			case err := <-reloaded:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("revocation waited for Telegram panel response")
			}
			adminTestReload(t, b, "42")
			unblock()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("panel response did not finish")
			}
			if _, exists := b.adminSessions.Get(adminTestUserID); exists {
				t.Error("an in-flight request restored a revoked config session")
			}
			if _, exists := b.scheduleWizard.Get(adminTestUserID); exists {
				t.Error("an in-flight request restored a revoked schedule session")
			}
		})
	}
}

func TestDashboardSSHRevocation(t *testing.T) {
	cfg := adminTestConfig(t, "42", "")
	signer, address := adminTestSSHConfig(t)
	b, _ := newAdminTestBot(t, loadConfig())
	startAdminTestBot(t, b)
	originalDashboard := b.dashboardSSH

	client := dialAdminTestSSH(t, address, signer)
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	if err := session.RequestPty("xterm", 24, 80, nil); err != nil {
		t.Fatal(err)
	}
	if err := session.Shell(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for b.dashboardService.currentSessionCount() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("SSH did not start a dashboard session")
		}
		time.Sleep(time.Millisecond)
	}
	adminTestReload(t, b, "42")
	if _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
		t.Fatalf("reload with unchanged SSH settings dropped the current connection: %v", err)
	}
	// A connection that has started the handshake must also be closed.
	pending, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pending.Close() })
	if err := pending.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(pending)
	if banner, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(banner, "SSH-") {
		t.Fatalf("SSH handshake banner = %q, error = %v", banner, err)
	}

	adminTestReload(t, b, "'*'")
	closed := make(chan struct{})
	go func() { _ = client.Wait(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("authenticated dashboard connection survived revocation")
	}
	deadline = time.Now().Add(3 * time.Second)
	for originalDashboard.SessionCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the revoked dashboard TUI is still running")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("unauthenticated SSH connection survived revocation")
	} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		t.Fatal("revoked SSH handshake remained open")
	}
	tcp, err := net.DialTimeout("tcp", address, time.Second)
	if err == nil {
		_ = tcp.Close()
		t.Fatal("SSH listener remained open after revocation")
	}

	writeAdminTestYAML(t, cfg.ConfigFilePath, "42")
	if err := b.applyRuntimeConfig(loadConfig()); err != nil {
		t.Fatal(err)
	}
	_ = dialAdminTestSSH(t, address, signer)
}

func TestDashboardSSHRevocationAtStartup(t *testing.T) {
	adminTestConfig(t, "'*'", "")
	signer, address := adminTestSSHConfig(t)
	cfg := loadConfig()
	b, _ := newAdminTestBot(t, cfg)
	if _, err := os.Stat(cfg.DashboardSSHHostKeyPath); !os.IsNotExist(err) {
		t.Errorf("wildcard initialized SSH host keys, stat error: %v", err)
	}
	startAdminTestBot(t, b)
	if client, err := tryAdminTestSSH(address, signer); err == nil {
		_ = client.Close()
		t.Fatal("valid SSH key authenticated with wildcard admin config")
	}
	adminTestReload(t, b, "''")
	_ = dialAdminTestSSH(t, address, signer)
}

func adminTestConfig(t *testing.T, yamlValue, env string) Config {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	for _, option := range allConfigOptions() {
		t.Setenv(option.EnvKey, "")
	}
	for key, value := range map[string]string{
		"CONFIG_FILE": filepath.Join(dir, "config.yaml"), "ADMIN_ID": env,
		"TELEGRAM_BOT_TOKEN": "admin-test", "OPENAI_API_KEY": "test-key",
		"PROFILE_ENABLED": "false", "SUMMARY_ENABLED": "false", "USER_DAILY_TOKEN_LIMIT": "5",
		"DASHBOARD_SSH_ENABLED": "", "DASHBOARD_SSH_LISTEN": "",
		"DASHBOARD_SSH_HOST_KEY_PATH": "", "DASHBOARD_SSH_AUTHORIZED_KEYS_PATH": "",
		"DASHBOARD_SSH_IDLE_TIMEOUT": "", "DASHBOARD_SSH_MAX_SESSIONS": "",
	} {
		t.Setenv(key, value)
	}
	writeAdminTestYAML(t, filepath.Join(dir, "config.yaml"), yamlValue)
	return loadConfig()
}

func writeAdminTestYAML(t *testing.T, path, adminValue string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("ADMIN_ID: "+adminValue+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func adminTestReload(t *testing.T, b *Bot, yamlValue string) {
	t.Helper()
	writeAdminTestYAML(t, b.currentConfig().ConfigFilePath, yamlValue)
	if err := b.applyRuntimeConfig(loadConfig()); err != nil {
		t.Fatal(err)
	}
}

type adminTestTransport func(*http.Request) (*http.Response, error)

func (f adminTestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type adminTestTelegram struct {
	nextID          atomic.Int32
	mu              sync.Mutex
	lastText        string
	beforeSend      func()
	beforeEdit      func(string)
	rejectMessage   func(path, text string) bool
	echoedSensitive bool
}

func newAdminTestBot(t *testing.T, cfg Config) (*Bot, *adminTestTelegram) {
	t.Helper()
	telegram := &adminTestTelegram{}
	server := httptest.NewServer(http.HandlerFunc(telegram.serveHTTP))
	t.Cleanup(server.Close)
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	base := http.DefaultTransport
	http.DefaultTransport = adminTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "api.telegram.org" {
			r = r.Clone(r.Context())
			r.URL.Scheme, r.URL.Host = endpoint.Scheme, endpoint.Host
		}
		return base.RoundTrip(r)
	})
	t.Cleanup(func() { http.DefaultTransport = base })

	b, err := NewBot(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		b.stopDashboardSSH()
		_ = b.stats.Close()
		_ = b.chatDB.Close()
	})
	return b, telegram
}

func (tg *adminTestTelegram) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/botadmin-test/getMe":
		fmt.Fprint(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"test","username":"testbot"}}`)
	case "/botadmin-test/deleteMessage", "/botadmin-test/answerCallbackQuery":
		fmt.Fprint(w, `{"ok":true,"result":true}`)
	case "/botadmin-test/sendMessage", "/botadmin-test/editMessageText":
		tg.mu.Lock()
		beforeSend := tg.beforeSend
		tg.mu.Unlock()
		if r.URL.Path == "/botadmin-test/sendMessage" && beforeSend != nil {
			beforeSend()
		}
		var body struct {
			MessageID string `json:"message_id"`
			Text      string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		tg.mu.Lock()
		beforeEdit := tg.beforeEdit
		rejectMessage := tg.rejectMessage
		tg.mu.Unlock()
		if r.URL.Path == "/botadmin-test/editMessageText" && beforeEdit != nil {
			beforeEdit(body.Text)
		}
		if rejectMessage != nil && rejectMessage(r.URL.Path, body.Text) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: local message rejection"}`)
			return
		}
		tg.mu.Lock()
		tg.lastText = body.Text
		tg.echoedSensitive = tg.echoedSensitive || strings.Contains(body.Text, "local-sensitive-input-sentinel")
		tg.mu.Unlock()
		id := body.MessageID
		if id == "" {
			id = fmt.Sprint(tg.nextID.Add(1))
		}
		fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%s,"chat":{"id":42,"type":"private"}}}`, id)
	default:
		http.NotFound(w, r)
	}
}

func (tg *adminTestTelegram) message(b *Bot, text string) tele.Context {
	return b.tg.NewContext(tele.Update{Message: &tele.Message{
		ID: 100, Chat: &tele.Chat{ID: adminTestUserID, Type: tele.ChatPrivate},
		Sender: &tele.User{ID: adminTestUserID}, Text: text, Payload: text,
	}})
}

func (tg *adminTestTelegram) call(t *testing.T, b *Bot, handler func(tele.Context) error, text string) {
	t.Helper()
	if err := handler(tg.message(b, text)); err != nil {
		t.Fatal(err)
	}
}

func (tg *adminTestTelegram) callback(t *testing.T, b *Bot, panelID int) {
	t.Helper()
	ctx := b.tg.NewContext(tele.Update{Callback: &tele.Callback{
		ID: "callback", Sender: &tele.User{ID: adminTestUserID},
		Message: &tele.Message{ID: panelID, Chat: &tele.Chat{ID: adminTestUserID, Type: tele.ChatPrivate}},
	}})
	if err := b.handleAdminCancel(ctx); err != nil {
		t.Fatal(err)
	}
}

func (tg *adminTestTelegram) wantReply(t *testing.T, text string) {
	t.Helper()
	lastText := tg.replyText()
	if !strings.Contains(lastText, text) {
		t.Fatalf("Telegram reply = %q, want %q", truncate(lastText, 180), text)
	}
}

func (tg *adminTestTelegram) replyText() string {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	return tg.lastText
}

type adminTestPoller struct{ started chan struct{} }

func (p adminTestPoller) Poll(_ *tele.Bot, _ chan tele.Update, stop chan struct{}) {
	close(p.started)
	<-stop
}

func startAdminTestBot(t *testing.T, b *Bot) {
	t.Helper()
	poller := adminTestPoller{started: make(chan struct{})}
	b.tg.Poller, b.scheduler = poller, nil
	stopped := make(chan struct{})
	go func() { b.Run(); close(stopped) }()
	waitReplySignal(t, poller.started, "bot polling")
	t.Cleanup(func() {
		b.tg.Stop()
		waitReplySignal(t, stopped, "bot shutdown")
	})
}

func adminTestSSHConfig(t *testing.T) (ssh.Signer, string) {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	authorizedKeys := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(authorizedKeys, ssh.MarshalAuthorizedKey(signer.PublicKey()), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DASHBOARD_SSH_LISTEN", address)
	t.Setenv("DASHBOARD_SSH_ENABLED", "true")
	t.Setenv("DASHBOARD_SSH_HOST_KEY_PATH", filepath.Join(dir, "host_key"))
	t.Setenv("DASHBOARD_SSH_AUTHORIZED_KEYS_PATH", authorizedKeys)
	return signer, address
}

func dialAdminTestSSH(t *testing.T, address string, signer ssh.Signer) *ssh.Client {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		client, err := tryAdminTestSSH(address, signer)
		if err == nil {
			t.Cleanup(func() { _ = client.Close() })
			return client
		}
		if time.Now().After(deadline) {
			t.Fatalf("could not authenticate to dashboard SSH: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func tryAdminTestSSH(address string, signer ssh.Signer) (*ssh.Client, error) {
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	client, channels, requests, err := ssh.NewClientConn(conn, address, &ssh.ClientConfig{
		User: "local-test", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(client, channels, requests), nil
}
