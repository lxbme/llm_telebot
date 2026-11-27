package app

import (
	"errors"
	"log"
	"net"
	"sync"
	"sync/atomic"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	ssh "github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	wishbubbletea "github.com/charmbracelet/wish/bubbletea"
)

type DashboardSSHServer struct {
	cfg     Config
	service *DashboardService
	events  *DashboardEventHub
	server  *ssh.Server

	mu       sync.Mutex
	listener net.Listener
	conns    map[net.Conn]struct{}
	closed   bool
	sessions atomic.Int64
}

func (cfg Config) dashboardSSHAllowed() bool {
	return cfg.DashboardSSHEnabled && !cfg.AdminWildcard
}

func NewDashboardSSHServer(service *DashboardService, events *DashboardEventHub, cfg Config) (*DashboardSSHServer, error) {
	server, err := wish.NewServer(
		wish.WithAddress(cfg.DashboardSSHListen),
		wish.WithHostKeyPath(cfg.DashboardSSHHostKeyPath),
		wish.WithAuthorizedKeys(cfg.DashboardSSHAuthorizedKeysPath),
		wish.WithIdleTimeout(cfg.DashboardSSHIdleTimeout),
	)
	if err != nil {
		return nil, err
	}

	d := &DashboardSSHServer{
		cfg:     cfg,
		service: service,
		events:  events,
		server:  server,
		conns:   make(map[net.Conn]struct{}),
	}
	server.Handler = d.handleSession
	server.ConnCallback = d.trackConn
	service.SetSessionCounter(d.SessionCount)
	return d, nil
}

func (d *DashboardSSHServer) Start() error {
	if d == nil || d.server == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return ssh.ErrServerClosed
	}
	if d.listener != nil {
		return nil
	}
	listener, err := net.Listen("tcp", d.cfg.DashboardSSHListen)
	if err != nil {
		return err
	}
	d.listener = listener
	log.Printf("[dashboard] SSH dashboard listening on %s", listener.Addr())
	go func() {
		if err := d.server.Serve(listener); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, ssh.ErrServerClosed) {
			log.Printf("[dashboard] SSH server error: %v", err)
		}
	}()
	return nil
}

func (d *DashboardSSHServer) Close() error {
	if d == nil || d.server == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	var err error
	if d.listener != nil {
		err = d.listener.Close()
	}
	for conn := range d.conns {
		_ = conn.Close()
	}
	clear(d.conns)
	return err
}

type dashboardSSHConn struct {
	net.Conn
	dashboard *DashboardSSHServer
}

func (conn *dashboardSSHConn) Close() error {
	err := conn.Conn.Close()
	conn.dashboard.mu.Lock()
	delete(conn.dashboard.conns, conn.Conn)
	conn.dashboard.mu.Unlock()
	return err
}

func (d *DashboardSSHServer) trackConn(_ ssh.Context, conn net.Conn) net.Conn {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	// Track the socket before authentication. The SSH server tracks only
	// authenticated connections and can accept a socket just before Close.
	d.conns[conn] = struct{}{}
	return &dashboardSSHConn{Conn: conn, dashboard: d}
}

func (d *DashboardSSHServer) matchesConfig(cfg Config) bool {
	return cfg.dashboardSSHAllowed() &&
		d.cfg.DashboardSSHListen == cfg.DashboardSSHListen &&
		d.cfg.DashboardSSHHostKeyPath == cfg.DashboardSSHHostKeyPath &&
		d.cfg.DashboardSSHAuthorizedKeysPath == cfg.DashboardSSHAuthorizedKeysPath &&
		d.cfg.DashboardSSHIdleTimeout == cfg.DashboardSSHIdleTimeout &&
		d.cfg.DashboardSSHMaxSessions == cfg.DashboardSSHMaxSessions
}

func (b *Bot) startDashboardSSH() {
	b.runtimeConfigMu.Lock()
	defer b.runtimeConfigMu.Unlock()
	b.dashboardRunning = true
	if err := b.configureDashboardSSH(b.currentConfig()); err != nil {
		log.Printf("[dashboard] failed to start SSH server: %v", err)
	}
}

func (b *Bot) stopDashboardSSH() {
	b.runtimeConfigMu.Lock()
	defer b.runtimeConfigMu.Unlock()
	b.dashboardRunning = false
	b.closeDashboardSSH()
}

// Dashboard configuration changes are serialized with publication of the bot's config.
func (b *Bot) closeChangedDashboardSSH(cfg Config) {
	if b.dashboardSSH != nil && !b.dashboardSSH.matchesConfig(cfg) {
		b.closeDashboardSSH()
	}
}

func (b *Bot) closeDashboardSSH() {
	if b.dashboardSSH == nil {
		return
	}
	if err := b.dashboardSSH.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Printf("[dashboard] failed to close SSH server: %v", err)
	}
	b.dashboardSSH = nil
	b.dashboardService.SetSessionCounter(nil)
}

func (b *Bot) configureDashboardSSH(cfg Config) error {
	if !cfg.dashboardSSHAllowed() {
		return nil
	}
	if b.dashboardSSH == nil {
		server, err := NewDashboardSSHServer(b.dashboardService, b.dashboardEvents, cfg)
		if err != nil {
			return err
		}
		b.dashboardSSH = server
	}
	if b.dashboardRunning {
		if err := b.dashboardSSH.Start(); err != nil {
			b.closeDashboardSSH()
			return err
		}
	}
	return nil
}

func (d *DashboardSSHServer) SessionCount() int {
	if d == nil {
		return 0
	}
	return int(d.sessions.Load())
}

func (d *DashboardSSHServer) handleSession(sess ssh.Session) {
	if d.cfg.DashboardSSHMaxSessions > 0 && d.SessionCount() >= d.cfg.DashboardSSHMaxSessions {
		wish.Fatalln(sess, "dashboard is busy, try again later")
		return
	}

	pty, windowChanges, ok := sess.Pty()
	if !ok {
		wish.Fatalln(sess, "no active terminal, skipping")
		return
	}

	d.sessions.Add(1)
	defer d.sessions.Add(-1)

	if d.events != nil {
		d.events.Publish(DashboardEvent{
			Type:    DashboardEventSSHLogin,
			Summary: "dashboard ssh login",
			Detail:  sess.User() + "@" + sess.RemoteAddr().String(),
			Success: true,
		})
	}

	renderer := wishbubbletea.MakeRenderer(sess)
	model := newDashboardModel(d.service, renderer, pty.Window.Width, pty.Window.Height)
	opts := append([]tea.ProgramOption{tea.WithAltScreen(), tea.WithContext(sess.Context())}, wishbubbletea.MakeOptions(sess)...)
	program := tea.NewProgram(model, opts...)

	go func() {
		for w := range windowChanges {
			program.Send(tea.WindowSizeMsg{Width: w.Width, Height: w.Height})
		}
	}()

	if _, err := program.Run(); err != nil && sess.Context().Err() == nil {
		log.Printf("[dashboard] bubbletea exited with error: %v", err)
	}
	program.Kill()
}

func newDashboardRendererStyle(renderer *lipgloss.Renderer) lipgloss.Style {
	if renderer != nil {
		return renderer.NewStyle()
	}
	return lipgloss.NewStyle()
}
