package app

import (
	"errors"
	"log"
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

	started  atomic.Bool
	sessions atomic.Int64
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
	}
	server.Handler = d.handleSession
	service.SetSessionCounter(d.SessionCount)
	return d, nil
}

func (d *DashboardSSHServer) Start() error {
	if d == nil || d.server == nil {
		return nil
	}
	if !d.started.CompareAndSwap(false, true) {
		return nil
	}
	go func() {
		log.Printf("[dashboard] SSH dashboard listening on %s", d.cfg.DashboardSSHListen)
		if err := d.server.ListenAndServe(); err != nil && !errors.Is(err, ssh.ErrServerClosed) {
			log.Printf("[dashboard] SSH server error: %v", err)
		}
	}()
	return nil
}

func (d *DashboardSSHServer) Close() error {
	if d == nil || d.server == nil {
		return nil
	}
	return d.server.Close()
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
	opts := append([]tea.ProgramOption{tea.WithAltScreen()}, wishbubbletea.MakeOptions(sess)...)
	program := tea.NewProgram(model, opts...)

	go func() {
		for w := range windowChanges {
			program.Send(tea.WindowSizeMsg{Width: w.Width, Height: w.Height})
		}
	}()

	if _, err := program.Run(); err != nil {
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
