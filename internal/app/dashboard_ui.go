package app

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const dashboardRefreshInterval = time.Second

type dashboardTickMsg time.Time

type dashboardOverviewMsg struct {
	overview DashboardOverview
	err      error
}

type dashboardUsersMsg struct {
	users []DashboardUserSummary
	err   error
}

type dashboardUserDetailMsg struct {
	detail DashboardUserDetail
	err    error
}

type dashboardSchedulesMsg struct {
	chats    []DashboardScheduleSummary
	upcoming []ScheduledTaskRef
	err      error
}

type dashboardChatSchedulesMsg struct {
	detail DashboardChatSchedules
	err    error
}

type dashboardEventsMsg struct {
	events []DashboardEvent
}

type dashboardModel struct {
	service  *DashboardService
	renderer *lipgloss.Renderer

	width  int
	height int

	tabIndex int
	tabs     []string

	overview DashboardOverview

	users          []DashboardUserSummary
	selectedUser   int
	userDetail     DashboardUserDetail
	detailViewport viewport.Model

	scheduleChats     []DashboardScheduleSummary
	selectedChat      int
	chatSchedules     DashboardChatSchedules
	upcomingSchedules []ScheduledTaskRef

	events      []DashboardEvent
	lastEventID uint64
	logViewport viewport.Model

	status  string
	lastErr error
}

type dashboardMetricCard struct {
	Label string
	Value string
	Hint  string
	Tone  lipgloss.Color
}

type dashboardTheme struct {
	title            lipgloss.Style
	subtitle         lipgloss.Style
	tabActive        lipgloss.Style
	tabInactive      lipgloss.Style
	panel            lipgloss.Style
	panelTitle       lipgloss.Style
	panelSubtitle    lipgloss.Style
	card             lipgloss.Style
	cardLabel        lipgloss.Style
	cardValue        lipgloss.Style
	cardHint         lipgloss.Style
	listItem         lipgloss.Style
	listItemSelected lipgloss.Style
	badge            lipgloss.Style
	badgeGood        lipgloss.Style
	badgeWarn        lipgloss.Style
	badgeBad         lipgloss.Style
	badgeInfo        lipgloss.Style
	text             lipgloss.Style
	muted            lipgloss.Style
	footer           lipgloss.Style
}

func newDashboardModel(service *DashboardService, renderer *lipgloss.Renderer, width, height int) dashboardModel {
	m := dashboardModel{
		service:  service,
		renderer: renderer,
		width:    width,
		height:   height,
		tabs:     []string{"Overview", "Users", "Schedules", "Logs"},
	}
	m.detailViewport = viewport.New(max(20, width/2), max(8, height-6))
	m.logViewport = viewport.New(max(20, width-4), max(8, height-6))
	return m
}

func (m dashboardModel) Init() tea.Cmd {
	return tea.Batch(m.refreshActiveTab(), dashboardTickCmd())
}

func dashboardTickCmd() tea.Cmd {
	return tea.Tick(dashboardRefreshInterval, func(t time.Time) tea.Msg {
		return dashboardTickMsg(t)
	})
}

func (m dashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resizeViewports()
		m.refreshViewportContent()
		return m, nil
	case dashboardTickMsg:
		return m, tea.Batch(dashboardTickCmd(), m.refreshActiveTab())
	case dashboardOverviewMsg:
		m.lastErr = msg.err
		if msg.err == nil {
			m.overview = msg.overview
			m.status = fmt.Sprintf("overview updated %s", time.Now().Format("15:04:05"))
		}
		return m, nil
	case dashboardUsersMsg:
		m.lastErr = msg.err
		if msg.err == nil {
			m.users = msg.users
			if len(m.users) == 0 {
				m.selectedUser = 0
				m.userDetail = DashboardUserDetail{}
			} else if m.selectedUser >= len(m.users) {
				m.selectedUser = len(m.users) - 1
				return m, m.loadSelectedUser()
			} else {
				return m, m.loadSelectedUser()
			}
		}
		return m, nil
	case dashboardUserDetailMsg:
		m.lastErr = msg.err
		if msg.err == nil {
			m.userDetail = msg.detail
			m.detailViewport.SetContent(m.renderUserDetailContent())
		}
		return m, nil
	case dashboardSchedulesMsg:
		m.lastErr = msg.err
		if msg.err == nil {
			m.scheduleChats = msg.chats
			m.upcomingSchedules = msg.upcoming
			if len(m.scheduleChats) == 0 {
				m.selectedChat = 0
				m.chatSchedules = DashboardChatSchedules{}
				m.detailViewport.SetContent(m.renderScheduleDetailContent())
			} else if m.selectedChat >= len(m.scheduleChats) {
				m.selectedChat = len(m.scheduleChats) - 1
				return m, m.loadSelectedChat()
			} else {
				return m, m.loadSelectedChat()
			}
		}
		return m, nil
	case dashboardChatSchedulesMsg:
		m.lastErr = msg.err
		if msg.err == nil {
			m.chatSchedules = msg.detail
			m.detailViewport.SetContent(m.renderScheduleDetailContent())
		}
		return m, nil
	case dashboardEventsMsg:
		if len(msg.events) > 0 {
			m.events = append(m.events, msg.events...)
			if len(m.events) > 500 {
				m.events = m.events[len(m.events)-500:]
			}
			m.lastEventID = m.events[len(m.events)-1].ID
			m.logViewport.SetContent(m.renderLogContent())
			m.logViewport.GotoBottom()
		}
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "tab", "l", "right":
			m.tabIndex = (m.tabIndex + 1) % len(m.tabs)
			return m, m.refreshActiveTab()
		case "shift+tab", "h", "left":
			m.tabIndex = (m.tabIndex - 1 + len(m.tabs)) % len(m.tabs)
			return m, m.refreshActiveTab()
		case "up", "k":
			switch m.tabIndex {
			case 1:
				if m.selectedUser > 0 {
					m.selectedUser--
					return m, m.loadSelectedUser()
				}
			case 2:
				if m.selectedChat > 0 {
					m.selectedChat--
					return m, m.loadSelectedChat()
				}
			case 3:
				m.logViewport, cmd = m.logViewport.Update(msg)
				return m, cmd
			}
		case "down", "j":
			switch m.tabIndex {
			case 1:
				if m.selectedUser < len(m.users)-1 {
					m.selectedUser++
					return m, m.loadSelectedUser()
				}
			case 2:
				if m.selectedChat < len(m.scheduleChats)-1 {
					m.selectedChat++
					return m, m.loadSelectedChat()
				}
			case 3:
				m.logViewport, cmd = m.logViewport.Update(msg)
				return m, cmd
			}
		case "pgup", "pgdown", "home", "end":
			if m.tabIndex == 3 {
				m.logViewport, cmd = m.logViewport.Update(msg)
				return m, cmd
			}
			m.detailViewport, cmd = m.detailViewport.Update(msg)
			return m, cmd
		case "r":
			return m, m.refreshActiveTab()
		}
	}

	if m.tabIndex == 3 {
		m.logViewport, cmd = m.logViewport.Update(msg)
		return m, cmd
	}
	m.detailViewport, cmd = m.detailViewport.Update(msg)
	return m, cmd
}

func (m dashboardModel) View() string {
	if m.width <= 0 || m.height <= 0 {
		return "loading dashboard..."
	}

	var body string
	switch m.tabIndex {
	case 0:
		body = m.renderOverviewTab()
	case 1:
		body = m.renderUsersTab()
	case 2:
		body = m.renderSchedulesTab()
	case 3:
		body = m.renderLogsTab()
	}

	status := m.status
	if m.lastErr != nil {
		status = "error: " + m.lastErr.Error()
	}

	return lipgloss.JoinVertical(
		lipgloss.Left,
		m.renderHeader(status),
		body,
		m.renderFooter(),
	)
}

func (m dashboardModel) refreshActiveTab() tea.Cmd {
	switch m.tabIndex {
	case 0:
		return loadDashboardOverviewCmd(m.service)
	case 1:
		return loadDashboardUsersCmd(m.service)
	case 2:
		return loadDashboardSchedulesCmd(m.service)
	case 3:
		return loadDashboardEventsCmd(m.service, m.lastEventID)
	default:
		return nil
	}
}

func (m dashboardModel) loadSelectedUser() tea.Cmd {
	if len(m.users) == 0 || m.selectedUser >= len(m.users) {
		return nil
	}
	return loadDashboardUserDetailCmd(m.service, m.users[m.selectedUser].UserID)
}

func (m dashboardModel) loadSelectedChat() tea.Cmd {
	if len(m.scheduleChats) == 0 || m.selectedChat >= len(m.scheduleChats) {
		return nil
	}
	return loadDashboardChatSchedulesCmd(m.service, m.scheduleChats[m.selectedChat].ChatID)
}

func (m *dashboardModel) resizeViewports() {
	leftWidth := m.sidebarPanelWidth()
	rightWidth := max(20, m.width-leftWidth-1)
	contentHeight := max(8, m.height-6)
	m.detailViewport.Width = dashboardPanelContentWidth(rightWidth)
	m.detailViewport.Height = contentHeight
	m.logViewport.Width = dashboardPanelContentWidth(m.width)
	m.logViewport.Height = contentHeight
}

func (m *dashboardModel) refreshViewportContent() {
	m.detailViewport.SetContent(m.renderUserDetailContent())
	if m.tabIndex == 2 {
		m.detailViewport.SetContent(m.renderScheduleDetailContent())
	}
	m.logViewport.SetContent(m.renderLogContent())
}

func (m dashboardModel) theme() dashboardTheme {
	base := newDashboardRendererStyle(m.renderer)
	return dashboardTheme{
		title:            base.Bold(true).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("62")).Padding(0, 1),
		subtitle:         base.Foreground(lipgloss.Color("252")),
		tabActive:        base.Bold(true).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("63")).Padding(0, 2),
		tabInactive:      base.Foreground(lipgloss.Color("245")).Background(lipgloss.Color("236")).Padding(0, 2),
		panel:            base.BorderStyle(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("239")).Padding(0, 1),
		panelTitle:       base.Bold(true).Foreground(lipgloss.Color("223")),
		panelSubtitle:    base.Foreground(lipgloss.Color("244")),
		card:             base.BorderStyle(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("238")).Padding(0, 1),
		cardLabel:        base.Foreground(lipgloss.Color("244")),
		cardValue:        base.Bold(true).Foreground(lipgloss.Color("230")),
		cardHint:         base.Foreground(lipgloss.Color("245")),
		listItem:         base.BorderStyle(lipgloss.NormalBorder()).BorderLeft(true).BorderForeground(lipgloss.Color("238")).Padding(0, 1),
		listItemSelected: base.BorderStyle(lipgloss.NormalBorder()).BorderLeft(true).BorderForeground(lipgloss.Color("69")).Background(lipgloss.Color("236")).Bold(true).Padding(0, 1),
		badge:            base.Padding(0, 1).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("241")),
		badgeGood:        base.Padding(0, 1).Bold(true).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("35")),
		badgeWarn:        base.Padding(0, 1).Bold(true).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("136")),
		badgeBad:         base.Padding(0, 1).Bold(true).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("160")),
		badgeInfo:        base.Padding(0, 1).Bold(true).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("63")),
		text:             base.Foreground(lipgloss.Color("252")),
		muted:            base.Foreground(lipgloss.Color("244")),
		footer:           base.Foreground(lipgloss.Color("242")),
	}
}

func (m dashboardModel) renderHeader(status string) string {
	theme := m.theme()
	title := theme.title.Render("LLM Telebot Dashboard")
	meta := lipgloss.JoinHorizontal(
		lipgloss.Left,
		theme.badgeInfo.Render(strings.ToUpper(m.tabs[m.tabIndex])),
		" ",
		m.renderStatusBadge(status),
	)
	return lipgloss.JoinVertical(
		lipgloss.Left,
		dashboardJoinEdge(title, meta, m.width),
		m.renderTabBar(),
	)
}

func (m dashboardModel) renderStatusBadge(status string) string {
	theme := m.theme()
	switch {
	case m.lastErr != nil:
		return theme.badgeBad.Render(truncateDashboardText(status, 48))
	case status == "":
		return theme.badge.Render("idle")
	default:
		return theme.badgeGood.Render(truncateDashboardText(status, 48))
	}
}

func (m dashboardModel) renderTabBar() string {
	theme := m.theme()
	parts := make([]string, 0, len(m.tabs))
	for i, tab := range m.tabs {
		if i == m.tabIndex {
			parts = append(parts, theme.tabActive.Render(tab))
		} else {
			parts = append(parts, theme.tabInactive.Render(tab))
		}
	}
	return lipgloss.JoinHorizontal(lipgloss.Left, parts...)
}

func (m dashboardModel) renderFooter() string {
	theme := m.theme()
	help := "Tab/h/l 切换  j/k 选择  PgUp/PgDn 滚动  r 刷新  q 退出"
	scope := "概览聚焦整体负载"
	switch m.tabIndex {
	case 1:
		scope = "用户页查看画像、MCP 与最近事件"
	case 2:
		scope = "调度页查看聊天任务与下一次执行"
	case 3:
		scope = "日志页持续追踪实时事件流"
	}
	return dashboardJoinEdge(theme.footer.Render(help), theme.footer.Render(scope), m.width)
}

func (m dashboardModel) renderOverviewTab() string {
	theme := m.theme()
	successRate := "0%"
	if m.overview.Metrics.RequestCount > 0 {
		successRate = fmt.Sprintf("%.0f%%",
			float64(m.overview.Metrics.SuccessCount)*100/float64(m.overview.Metrics.RequestCount))
	}
	cards := []dashboardMetricCard{
		{Label: "Time Window", Value: m.overview.Window.String(), Hint: "", Tone: lipgloss.Color("63")},
		{Label: "Requests", Value: fmt.Sprintf("%d", m.overview.Metrics.RequestCount), Hint: "recent total requests", Tone: lipgloss.Color("69")},
		{Label: "Success Rate", Value: successRate, Hint: fmt.Sprintf("%d ok / %d err", m.overview.Metrics.SuccessCount, m.overview.Metrics.ErrorCount), Tone: lipgloss.Color("35")},
		{Label: "Avg Latency", Value: fmt.Sprintf("%d ms", avgLatencyMs(m.overview.Metrics)), Hint: "mean request latency", Tone: lipgloss.Color("111")},
		{Label: "Tokens", Value: fmt.Sprintf("%d", m.overview.Metrics.TotalTokens), Hint: fmt.Sprintf("prompt %d / completion %d", m.overview.Metrics.PromptTokens, m.overview.Metrics.CompletionTokens), Tone: lipgloss.Color("141")},
		{Label: "Schedules", Value: fmt.Sprintf("%d", m.overview.ScheduleCount), Hint: "registered scheduled tasks", Tone: lipgloss.Color("75")},
		{Label: "MCP Users", Value: fmt.Sprintf("%d", m.overview.MCPUserCount), Hint: "users with MCP servers", Tone: lipgloss.Color("99")},
		{Label: "SSH Sessions", Value: fmt.Sprintf("%d", m.overview.SSHSessions), Hint: fmt.Sprintf("dropped events %d", m.overview.DroppedEvents), Tone: lipgloss.Color("136")},
	}

	metrics := m.renderMetricGrid(cards, m.width)
	topChats := m.renderTopChatsPanel()
	models := m.renderModelSummaryPanel()
	recentChats := m.renderRecentChatsPanel()
	recentUsers := m.renderRecentUsersPanel()

	if m.width >= 110 {
		leftWidth := max(36, (m.width-3)/2)
		rightWidth := max(36, m.width-leftWidth-3)
		row1 := lipgloss.JoinHorizontal(lipgloss.Top,
			m.renderPanel("Top Chats", "哪些 chat 最活跃", topChats, leftWidth),
			" ",
			m.renderPanel("Models", "最近窗口内的模型热度", models, rightWidth),
		)
		row2 := lipgloss.JoinHorizontal(lipgloss.Top,
			m.renderPanel("Recent Chats", "最近交互的 chat", recentChats, leftWidth),
			" ",
			m.renderPanel("Recent Users", "最近活跃用户", recentUsers, rightWidth),
		)
		return lipgloss.JoinVertical(lipgloss.Left, metrics, row1, row2)
	}

	return lipgloss.JoinVertical(
		lipgloss.Left,
		metrics,
		m.renderPanel("Top Chats", "哪些 chat 最活跃", topChats, m.width),
		m.renderPanel("Models", "最近窗口内的模型热度", models, m.width),
		m.renderPanel("Recent Chats", "最近交互的 chat", recentChats, m.width),
		m.renderPanel("Recent Users", "最近活跃用户", recentUsers, m.width),
		theme.muted.Render(""),
	)
}

func (m dashboardModel) renderUsersTab() string {
	leftWidth := m.sidebarPanelWidth()
	rightWidth := max(32, m.width-leftWidth-1)
	left := m.renderPanel(
		"Users",
		fmt.Sprintf("%d users, current %d/%d", len(m.users), min(m.selectedUser+1, len(m.users)), max(1, len(m.users))),
		m.renderUserList(dashboardPanelContentWidth(leftWidth)),
		leftWidth,
	)
	right := m.renderPanel(
		"User Detail",
		"画像、使用量、MCP 与事件",
		m.detailViewport.View(),
		rightWidth,
	)
	return lipgloss.JoinHorizontal(lipgloss.Top, left, " ", right)
}

func (m dashboardModel) renderUserList(contentWidth int) string {
	theme := m.theme()
	if len(m.users) == 0 {
		return theme.muted.Render("No users yet.")
	}

	itemWidth := max(16, contentWidth)
	items := make([]string, 0, len(m.users))
	for i, user := range m.users {
		name := formatProfileIdentity(user.UserID, user.Username)
		if user.DisplayName != "" {
			name += " | " + user.DisplayName
		}
		badges := make([]string, 0, 2)
		if user.HasProfile {
			badges = append(badges, theme.badgeInfo.Render("PROFILE"))
		}
		if user.HasMCP {
			badges = append(badges, theme.badgeGood.Render("MCP"))
		}
		style := theme.listItem
		if i == m.selectedUser {
			style = theme.listItemSelected
		}
		items = append(items, renderDashboardListItem(
			style,
			itemWidth,
			name,
			renderDashboardBadgeLine(theme, badges, "last active "+formatRelativeTime(user.LastSeenAt)),
		))
	}
	return lipgloss.JoinVertical(lipgloss.Left, items...)
}

func (m dashboardModel) renderUserDetailContent() string {
	theme := m.theme()
	if m.userDetail.User.UserID == 0 {
		return theme.muted.Render("Select a user to inspect profile, MCP and recent events.")
	}

	identityLines := []string{
		fmt.Sprintf("User: %s", formatProfileIdentity(m.userDetail.User.UserID, m.userDetail.User.Username)),
	}
	if m.userDetail.User.DisplayName != "" {
		identityLines = append(identityLines, "Display: "+m.userDetail.User.DisplayName)
	}
	if m.userDetail.Recent != nil {
		identityLines = append(identityLines, "Last Seen: "+formatTime(m.userDetail.Recent.LastSeenAt))
	}

	usageCards := []dashboardMetricCard{
		{Label: "Requests", Value: fmt.Sprintf("%d", m.userDetail.Usage.Total.RequestCount), Hint: "24h total", Tone: lipgloss.Color("69")},
		{Label: "Success", Value: fmt.Sprintf("%d", m.userDetail.Usage.Total.SuccessCount), Hint: fmt.Sprintf("%d errors", m.userDetail.Usage.Total.ErrorCount), Tone: lipgloss.Color("35")},
		{Label: "Tokens", Value: fmt.Sprintf("%d", m.userDetail.Usage.Total.TotalTokens), Hint: "prompt + completion", Tone: lipgloss.Color("141")},
		{Label: "Latency", Value: fmt.Sprintf("%d ms", avgLatencyMs(m.userDetail.Usage.Total)), Hint: "average latency", Tone: lipgloss.Color("111")},
	}

	var sb strings.Builder
	sb.WriteString(m.renderSubsection("Identity", strings.Join(identityLines, "\n")))
	sb.WriteString("\n")
	sb.WriteString(m.renderMetricGrid(usageCards, max(24, m.detailViewport.Width)))
	if m.userDetail.Profile != nil && len(m.userDetail.Profile.Facts) > 0 {
		lines := make([]string, 0, len(m.userDetail.Profile.Facts))
		for i, fact := range m.userDetail.Profile.Facts {
			lines = append(lines, fmt.Sprintf("%d. %s", i+1, fact))
		}
		sb.WriteString("\n")
		sb.WriteString(m.renderSubsection("Profile Facts", strings.Join(lines, "\n")))
	}
	if len(m.userDetail.MCPServers) > 0 {
		lines := make([]string, 0, len(m.userDetail.MCPServers))
		for _, item := range m.userDetail.MCPServers {
			lines = append(lines,
				fmt.Sprintf("%s  [%s]  tools=%d", item.Name, firstNonEmpty(item.Type, "unknown"), item.ToolCount),
				"  "+firstNonEmpty(item.URL, "-"),
			)
		}
		sb.WriteString("\n")
		sb.WriteString(m.renderSubsection("MCP Servers", strings.Join(lines, "\n")))
	}
	if len(m.userDetail.Events) > 0 {
		lines := make([]string, 0, len(m.userDetail.Events))
		for _, event := range m.userDetail.Events {
			lines = append(lines,
				fmt.Sprintf("[%s] %s", event.Time.Format("15:04:05"), formatDashboardEventLabel(event.Type)),
				"  "+truncateDashboardText(event.Summary, max(24, m.detailViewport.Width-6)),
			)
		}
		sb.WriteString("\n")
		sb.WriteString(m.renderSubsection("Recent Events", strings.Join(lines, "\n")))
	}
	return sb.String()
}

func (m dashboardModel) renderSchedulesTab() string {
	leftWidth := m.sidebarPanelWidth()
	rightWidth := max(32, m.width-leftWidth-1)
	left := m.renderPanel(
		"Schedules",
		fmt.Sprintf("%d chats, %d upcoming", len(m.scheduleChats), len(m.upcomingSchedules)),
		m.renderScheduleList(dashboardPanelContentWidth(leftWidth)),
		leftWidth,
	)
	right := m.renderPanel(
		"Task Detail",
		"所选 chat 的任务列表",
		m.detailViewport.View(),
		rightWidth,
	)
	return lipgloss.JoinHorizontal(lipgloss.Top, left, " ", right)
}

func (m dashboardModel) renderScheduleList(contentWidth int) string {
	theme := m.theme()
	if len(m.scheduleChats) == 0 {
		return theme.muted.Render("No schedules.")
	}

	itemWidth := max(16, contentWidth)
	chatLines := make([]string, 0, len(m.scheduleChats))
	for i, item := range m.scheduleChats {
		title := fmt.Sprintf("chat %d", item.ChatID)
		badges := []string{
			theme.badgeInfo.Render(fmt.Sprintf("%d tasks", item.Count)),
			theme.badgeGood.Render(fmt.Sprintf("%d enabled", item.EnabledCount)),
		}
		style := theme.listItem
		if i == m.selectedChat {
			style = theme.listItemSelected
		}
		chatLines = append(chatLines, renderDashboardListItem(
			style,
			itemWidth,
			title,
			renderDashboardBadgeLine(theme, badges, "next "+firstNonEmpty(formatRelativeTime(item.NextRunAt), "-")),
		))
	}
	upcomingLines := make([]string, 0, min(6, len(m.upcomingSchedules)))
	for _, item := range m.upcomingSchedules {
		upcomingLines = append(upcomingLines,
			fmt.Sprintf("chat %d  %s", item.ChatID, item.ID),
			"  "+formatTime(item.Task.NextRunAt),
		)
	}

	var sb strings.Builder
	sb.WriteString(m.renderSubsection("Chats", lipgloss.JoinVertical(lipgloss.Left, chatLines...)))
	if len(upcomingLines) > 0 {
		sb.WriteString("\n")
		sb.WriteString(m.renderSubsection("Upcoming", strings.Join(upcomingLines, "\n")))
	}
	return sb.String()
}

func (m dashboardModel) renderScheduleDetailContent() string {
	theme := m.theme()
	if len(m.chatSchedules.Tasks) == 0 {
		return theme.muted.Render("No tasks for selected chat.")
	}

	lines := make([]string, 0, len(m.chatSchedules.Tasks))
	for _, task := range m.chatSchedules.Tasks {
		status := theme.badgeBad.Render("DISABLED")
		if task.Enabled {
			status = theme.badgeGood.Render("ENABLED")
		}
		lines = append(lines,
			fmt.Sprintf("%s  %s", status, firstNonEmpty(task.Name, task.Prompt)),
			fmt.Sprintf("  id=%s", truncateDashboardText(task.ID, 24)),
			fmt.Sprintf("  cron=%s  tz=%s", task.CronExpr, firstNonEmpty(task.Timezone, "UTC")),
			fmt.Sprintf("  next=%s  last=%s", formatTime(task.NextRunAt), formatTime(task.LastRunAt)),
		)
		if task.LastError != "" {
			lines = append(lines, "  error="+truncateDashboardText(task.LastError, max(24, m.detailViewport.Width-8)))
		}
		lines = append(lines, "")
	}
	return m.renderSubsection(fmt.Sprintf("Chat %d", m.chatSchedules.ChatID), strings.Join(lines, "\n"))
}

func (m dashboardModel) renderLogsTab() string {
	return m.renderPanel(
		"Live Event Stream",
		fmt.Sprintf("%d buffered events", len(m.events)),
		m.logViewport.View(),
		m.width,
	)
}

func (m dashboardModel) renderLogContent() string {
	theme := m.theme()
	if len(m.events) == 0 {
		return theme.muted.Render("No events yet.")
	}
	lines := make([]string, 0, len(m.events)*2)
	for _, event := range m.events {
		meta := make([]string, 0, 4)
		if event.ChatID != 0 {
			meta = append(meta, fmt.Sprintf("chat=%d", event.ChatID))
		}
		if event.UserID != 0 {
			meta = append(meta, fmt.Sprintf("user=%d", event.UserID))
		}
		if event.Model != "" {
			meta = append(meta, "model="+event.Model)
		}
		if event.LatencyMs > 0 {
			meta = append(meta, fmt.Sprintf("%dms", event.LatencyMs))
		}
		header := lipgloss.JoinHorizontal(
			lipgloss.Left,
			theme.muted.Render(event.Time.Format("15:04:05")),
			" ",
			m.renderEventBadge(event.Type),
			" ",
			theme.text.Render(truncateDashboardText(event.Summary, max(24, m.logViewport.Width-30))),
		)
		lines = append(lines, header)
		if len(meta) > 0 {
			lines = append(lines, "  "+theme.muted.Render(strings.Join(meta, "  ")))
		}
	}
	return strings.Join(lines, "\n")
}

func (m dashboardModel) renderMetricGrid(cards []dashboardMetricCard, availableWidth int) string {
	if len(cards) == 0 {
		return ""
	}
	if availableWidth <= 0 {
		availableWidth = m.width
	}
	cols := 2
	switch {
	case availableWidth >= 130:
		cols = 4
	case availableWidth >= 100:
		cols = 3
	case availableWidth < 56:
		cols = 1
	}
	cardWidth := max(16, (availableWidth-(cols-1))/cols)
	rows := make([]string, 0, (len(cards)+cols-1)/cols)
	for i := 0; i < len(cards); i += cols {
		rowCards := make([]string, 0, cols)
		for j := i; j < min(i+cols, len(cards)); j++ {
			rowCards = append(rowCards, m.renderMetricCard(cards[j], cardWidth))
		}
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, append([]string{rowCards[0]}, prefixDashboardStrings(" ", rowCards[1:])...)...))
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

func (m dashboardModel) renderMetricCard(card dashboardMetricCard, width int) string {
	theme := m.theme()
	contentWidth := max(12, width-5)
	valueStyle := theme.cardValue.Copy().Foreground(card.Tone)
	content := lipgloss.JoinVertical(
		lipgloss.Left,
		theme.cardLabel.Render(card.Label),
		valueStyle.Render(card.Value),
		theme.cardHint.Render(truncateDashboardText(card.Hint, contentWidth)),
	)
	return theme.card.Copy().Width(contentWidth).Render(content)
}

func (m dashboardModel) renderPanel(title, subtitle, content string, width int) string {
	theme := m.theme()
	contentWidth := dashboardPanelContentWidth(width)
	header := theme.panelTitle.Render(title)
	if subtitle != "" {
		header = lipgloss.JoinVertical(lipgloss.Left, header, theme.panelSubtitle.Render(subtitle))
	}
	if strings.TrimSpace(content) == "" {
		content = theme.muted.Render("No data.")
	}
	return theme.panel.Copy().Width(contentWidth).Render(
		lipgloss.JoinVertical(lipgloss.Left, header, content),
	)
}

func (m dashboardModel) renderSubsection(title, content string) string {
	theme := m.theme()
	if strings.TrimSpace(content) == "" {
		content = theme.muted.Render("No data.")
	}
	return lipgloss.JoinVertical(
		lipgloss.Left,
		theme.panelTitle.Render(title),
		theme.text.Render(content),
	)
}

func (m dashboardModel) renderTopChatsPanel() string {
	if len(m.overview.TopChats) == 0 {
		return m.theme().muted.Render("No active chats in the selected window.")
	}
	maxRequests := int64(1)
	for _, item := range m.overview.TopChats {
		if item.Requests > maxRequests {
			maxRequests = item.Requests
		}
	}
	lines := make([]string, 0, min(6, len(m.overview.TopChats))*2)
	for i, item := range m.overview.TopChats[:min(6, len(m.overview.TopChats))] {
		lines = append(lines,
			fmt.Sprintf("%d. chat %d  %s", i+1, item.ChatID, renderDashboardBar(item.Requests, maxRequests, 12)),
			fmt.Sprintf("   req=%d  tokens=%d  last=%s", item.Requests, item.TotalTokens, formatRelativeTime(item.LastSeenAt)),
		)
	}
	return strings.Join(lines, "\n")
}

func (m dashboardModel) renderModelSummaryPanel() string {
	if len(m.overview.ModelSummaries) == 0 {
		return m.theme().muted.Render("No model usage recorded yet.")
	}
	maxCount := 1
	for _, item := range m.overview.ModelSummaries {
		if item.Count > maxCount {
			maxCount = item.Count
		}
	}
	lines := make([]string, 0, min(6, len(m.overview.ModelSummaries)))
	for _, item := range m.overview.ModelSummaries[:min(6, len(m.overview.ModelSummaries))] {
		lines = append(lines,
			fmt.Sprintf("%s  %s  %d req", truncateDashboardText(item.Model, 20), renderDashboardBar(int64(item.Count), int64(maxCount), 12), item.Count),
		)
	}
	return strings.Join(lines, "\n")
}

func (m dashboardModel) renderRecentChatsPanel() string {
	if len(m.overview.RecentChats) == 0 {
		return m.theme().muted.Render("No recent chats.")
	}
	lines := make([]string, 0, min(6, len(m.overview.RecentChats)))
	for _, item := range m.overview.RecentChats[:min(6, len(m.overview.RecentChats))] {
		lines = append(lines,
			fmt.Sprintf("chat %d  %s", item.ChatID, formatRelativeTime(item.LastSeenAt)),
			fmt.Sprintf("  model=%s  call=%s", firstNonEmpty(item.LastModel, "-"), firstNonEmpty(string(item.LastCallType), "-")),
		)
	}
	return strings.Join(lines, "\n")
}

func (m dashboardModel) renderRecentUsersPanel() string {
	if len(m.overview.RecentUsers) == 0 {
		return m.theme().muted.Render("No recent users.")
	}
	lines := make([]string, 0, min(6, len(m.overview.RecentUsers)))
	for _, item := range m.overview.RecentUsers[:min(6, len(m.overview.RecentUsers))] {
		name := formatProfileIdentity(item.UserID, item.Username)
		if item.DisplayName != "" {
			name += " | " + item.DisplayName
		}
		lines = append(lines,
			truncateDashboardText(name, 28),
			fmt.Sprintf("  %s  profile=%t mcp=%t", formatRelativeTime(item.LastSeenAt), item.HasProfile, item.HasMCP),
		)
	}
	return strings.Join(lines, "\n")
}

func (m dashboardModel) renderEventBadge(eventType DashboardEventType) string {
	theme := m.theme()
	label := formatDashboardEventLabel(eventType)
	switch eventType {
	case DashboardEventConversationError, DashboardEventScheduleFailed:
		return theme.badgeBad.Render(label)
	case DashboardEventConversationFinished, DashboardEventUsageRecorded, DashboardEventProfileUpdated, DashboardEventSummaryUpdated:
		return theme.badgeGood.Render(label)
	case DashboardEventSSHLogin, DashboardEventConfigReloaded, DashboardEventMCPChanged:
		return theme.badgeInfo.Render(label)
	default:
		return theme.badgeWarn.Render(label)
	}
}

func formatDashboardEventLabel(eventType DashboardEventType) string {
	switch eventType {
	case DashboardEventConversationStarted:
		return "CHAT START"
	case DashboardEventConversationFinished:
		return "CHAT DONE"
	case DashboardEventConversationError:
		return "CHAT ERR"
	case DashboardEventUsageRecorded:
		return "USAGE"
	case DashboardEventToolCallStarted:
		return "TOOL START"
	case DashboardEventToolCallFinished:
		return "TOOL DONE"
	case DashboardEventMCPChanged:
		return "MCP"
	case DashboardEventScheduleTriggered:
		return "SCHEDULE"
	case DashboardEventScheduleFailed:
		return "SCHED ERR"
	case DashboardEventProfileUpdated:
		return "PROFILE"
	case DashboardEventSummaryUpdated:
		return "SUMMARY"
	case DashboardEventConfigReloaded:
		return "CONFIG"
	case DashboardEventSSHLogin:
		return "SSH"
	default:
		return strings.ToUpper(string(eventType))
	}
}

func renderDashboardBar(value, total int64, width int) string {
	if width <= 0 {
		return ""
	}
	if total <= 0 {
		total = 1
	}
	filled := int(value * int64(width) / total)
	if filled <= 0 && value > 0 {
		filled = 1
	}
	if filled > width {
		filled = width
	}
	return "[" + strings.Repeat("=", filled) + strings.Repeat(".", width-filled) + "]"
}

func dashboardJoinEdge(left, right string, width int) string {
	gap := max(1, width-lipgloss.Width(left)-lipgloss.Width(right))
	return left + strings.Repeat(" ", gap) + right
}

func (m dashboardModel) sidebarPanelWidth() int {
	return max(30, min(42, m.width/3))
}

func dashboardPanelContentWidth(width int) int {
	return max(16, width-4)
}

func dashboardStyleContentWidth(style lipgloss.Style, totalWidth int) int {
	return max(1, totalWidth-style.GetHorizontalFrameSize())
}

func renderDashboardBadgeLine(theme dashboardTheme, badges []string, meta string) string {
	if len(badges) == 0 {
		return theme.muted.Render(meta)
	}
	return lipgloss.JoinHorizontal(lipgloss.Left, strings.Join(badges, " "), " ", theme.muted.Render(meta))
}

func renderDashboardListItem(style lipgloss.Style, totalWidth int, lines ...string) string {
	contentWidth := dashboardStyleContentWidth(style, totalWidth)
	return style.Width(contentWidth).Render(lipgloss.JoinVertical(lipgloss.Left, lines...))
}

func prefixDashboardStrings(prefix string, items []string) []string {
	if len(items) == 0 {
		return nil
	}
	out := make([]string, 0, len(items)*2)
	for _, item := range items {
		out = append(out, prefix, item)
	}
	return out
}

func formatRelativeTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("01-02 15:04")
	}
}

func loadDashboardOverviewCmd(service *DashboardService) tea.Cmd {
	return func() tea.Msg {
		overview, err := service.GetOverview(defaultDashboardOverviewWindow, 10)
		return dashboardOverviewMsg{overview: overview, err: err}
	}
}

func loadDashboardUsersCmd(service *DashboardService) tea.Cmd {
	return func() tea.Msg {
		users, err := service.ListUsers(200, "")
		return dashboardUsersMsg{users: users, err: err}
	}
}

func loadDashboardUserDetailCmd(service *DashboardService, userID int64) tea.Cmd {
	return func() tea.Msg {
		detail, err := service.GetUserDetail(userID)
		return dashboardUserDetailMsg{detail: detail, err: err}
	}
}

func loadDashboardSchedulesCmd(service *DashboardService) tea.Cmd {
	return func() tea.Msg {
		chats, err := service.ListScheduleChats(200)
		if err != nil {
			return dashboardSchedulesMsg{err: err}
		}
		upcoming, err := service.ListUpcomingSchedules(20)
		return dashboardSchedulesMsg{chats: chats, upcoming: upcoming, err: err}
	}
}

func loadDashboardChatSchedulesCmd(service *DashboardService, chatID int64) tea.Cmd {
	return func() tea.Msg {
		detail, err := service.GetChatSchedules(chatID)
		return dashboardChatSchedulesMsg{detail: detail, err: err}
	}
}

func loadDashboardEventsCmd(service *DashboardService, afterID uint64) tea.Cmd {
	return func() tea.Msg {
		return dashboardEventsMsg{events: service.TailEvents(afterID, 200)}
	}
}

func avgLatencyMs(aggregate UsageAggregate) int64 {
	if aggregate.RequestCount == 0 {
		return 0
	}
	return aggregate.TotalLatencyMs / aggregate.RequestCount
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02 15:04:05")
}
