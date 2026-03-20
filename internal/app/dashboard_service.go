package app

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultDashboardCacheTTL       = 2 * time.Second
	defaultDashboardOverviewWindow = 15 * time.Minute
	defaultDashboardUserWindow     = 24 * time.Hour
)

type DashboardHotChat struct {
	ChatID      int64
	Requests    int64
	TotalTokens int64
	LastSeenAt  time.Time
}

type DashboardModelSummary struct {
	Model       string
	Count       int
	TotalTokens int64
}

type DashboardUserSummary struct {
	UserID      int64
	Username    string
	DisplayName string
	LastSeenAt  time.Time
	HasProfile  bool
	HasMCP      bool
}

type DashboardOverview struct {
	Window         time.Duration
	Metrics        UsageAggregate
	RecentChats    []UsageRecentMeta
	RecentUsers    []DashboardUserSummary
	TopChats       []DashboardHotChat
	ModelSummaries []DashboardModelSummary
	ScheduleCount  int
	MCPUserCount   int
	SSHSessions    int
	DroppedEvents  uint64
}

type DashboardUserDetail struct {
	User       DashboardUserSummary
	Profile    *UserProfile
	MCPServers []ServerInfo
	Usage      UsageOverview
	Recent     *UsageRecentMeta
	Events     []DashboardEvent
}

type DashboardScheduleSummary struct {
	ChatID       int64
	Count        int
	EnabledCount int
	NextRunAt    time.Time
}

type DashboardChatSchedules struct {
	ChatID int64
	Tasks  []ScheduledTask
}

type dashboardCacheEntry struct {
	expires time.Time
	value   any
}

type DashboardService struct {
	bot *Bot

	events *DashboardEventHub

	mu           sync.Mutex
	cache        map[string]dashboardCacheEntry
	sessionCount func() int
}

func NewDashboardService(bot *Bot, events *DashboardEventHub) *DashboardService {
	return &DashboardService{
		bot:    bot,
		events: events,
		cache:  make(map[string]dashboardCacheEntry),
	}
}

func (s *DashboardService) SetSessionCounter(fn func() int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionCount = fn
}

func (s *DashboardService) currentSessionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionCount == nil {
		return 0
	}
	return s.sessionCount()
}

func (s *DashboardService) getCached(key string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.cache[key]
	if !ok || time.Now().After(entry.expires) {
		return nil, false
	}
	return entry.value, true
}

func (s *DashboardService) setCached(key string, value any, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[key] = dashboardCacheEntry{
		expires: time.Now().Add(ttl),
		value:   value,
	}
}

func (s *DashboardService) QueryUserUsage(userID int64, window time.Duration) (UsageOverview, error) {
	if window <= 0 {
		window = defaultDashboardUserWindow
	}
	snap := s.bot.snapshot()
	if snap.stats == nil || userID == 0 {
		return UsageOverview{}, nil
	}
	return snap.stats.QueryUsage(UsageQuery{
		Granularity: UsageGranularityMinute,
		Scope:       UsageScopeUser,
		UserID:      userID,
		From:        time.Now().UTC().Add(-window),
		To:          time.Now().UTC(),
	})
}

func (s *DashboardService) QueryChatUsage(chatID int64, window time.Duration) (UsageOverview, error) {
	if window <= 0 {
		window = defaultDashboardOverviewWindow
	}
	snap := s.bot.snapshot()
	if snap.stats == nil || chatID == 0 {
		return UsageOverview{}, nil
	}
	return snap.stats.QueryUsage(UsageQuery{
		Granularity: UsageGranularityMinute,
		Scope:       UsageScopeChat,
		ChatID:      chatID,
		From:        time.Now().UTC().Add(-window),
		To:          time.Now().UTC(),
	})
}

func (s *DashboardService) ListRecentChats(limit int) ([]UsageRecentMeta, error) {
	snap := s.bot.snapshot()
	if snap.stats == nil {
		return nil, nil
	}
	return snap.stats.ListRecent(UsageScopeChat, limit)
}

func (s *DashboardService) ListUsers(limit int, filter string) ([]DashboardUserSummary, error) {
	cacheKey := fmt.Sprintf("users:%d:%s", limit, strings.ToLower(strings.TrimSpace(filter)))
	if cached, ok := s.getCached(cacheKey); ok {
		if items, ok := cached.([]DashboardUserSummary); ok {
			return items, nil
		}
	}

	snap := s.bot.snapshot()
	idSet := make(map[int64]struct{})
	recentByUser := make(map[int64]UsageRecentMeta)

	if snap.stats != nil {
		recentUsers, err := snap.stats.ListRecent(UsageScopeUser, max(128, limit*4))
		if err != nil {
			return nil, err
		}
		for _, item := range recentUsers {
			idSet[item.UserID] = struct{}{}
			recentByUser[item.UserID] = item
		}
	}

	mcpIDs := make(map[int64]struct{})
	if snap.userTools != nil && snap.userTools.store != nil {
		ids, err := snap.userTools.store.AllUserIDs()
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			idSet[id] = struct{}{}
			mcpIDs[id] = struct{}{}
		}
	}

	profileIDs := make(map[int64]struct{})
	if snap.profiles != nil {
		ids, err := snap.profiles.ListUserIDs()
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			idSet[id] = struct{}{}
			profileIDs[id] = struct{}{}
		}
	}

	summaries := make([]DashboardUserSummary, 0, len(idSet))
	filter = strings.ToLower(strings.TrimSpace(filter))
	for userID := range idSet {
		summary := DashboardUserSummary{
			UserID:     userID,
			HasProfile: hasID(profileIDs, userID),
			HasMCP:     hasID(mcpIDs, userID),
		}
		if recent, ok := recentByUser[userID]; ok {
			summary.LastSeenAt = recent.LastSeenAt
		}
		if snap.profiles != nil {
			profile, err := snap.profiles.Get(userID)
			if err != nil {
				return nil, err
			}
			if profile != nil {
				summary.Username = profile.Username
				summary.DisplayName = profile.DisplayName
			}
		}

		if filter != "" {
			text := strings.ToLower(fmt.Sprintf("%d %s %s", summary.UserID, summary.Username, summary.DisplayName))
			if !strings.Contains(text, filter) {
				continue
			}
		}
		summaries = append(summaries, summary)
	}

	sort.Slice(summaries, func(i, j int) bool {
		if !summaries[i].LastSeenAt.Equal(summaries[j].LastSeenAt) {
			return summaries[i].LastSeenAt.After(summaries[j].LastSeenAt)
		}
		return summaries[i].UserID < summaries[j].UserID
	})
	if limit > 0 && len(summaries) > limit {
		summaries = summaries[:limit]
	}
	s.setCached(cacheKey, summaries, defaultDashboardCacheTTL)
	return summaries, nil
}

func (s *DashboardService) GetUserDetail(userID int64) (DashboardUserDetail, error) {
	detail := DashboardUserDetail{}
	users, err := s.ListUsers(256, "")
	if err != nil {
		return detail, err
	}
	for _, item := range users {
		if item.UserID == userID {
			detail.User = item
			break
		}
	}

	snap := s.bot.snapshot()
	if snap.profiles != nil {
		detail.Profile, err = snap.profiles.Get(userID)
		if err != nil {
			return detail, err
		}
		if detail.Profile != nil {
			detail.User.Username = firstNonEmpty(detail.User.Username, detail.Profile.Username)
			detail.User.DisplayName = firstNonEmpty(detail.User.DisplayName, detail.Profile.DisplayName)
			detail.User.HasProfile = true
		}
	}
	if snap.userTools != nil {
		detail.MCPServers, err = snap.userTools.ListServers(userID)
		if err != nil {
			return detail, err
		}
		if len(detail.MCPServers) > 0 {
			detail.User.HasMCP = true
		}
	}
	if snap.stats != nil {
		recentItems, err := snap.stats.ListRecent(UsageScopeUser, 256)
		if err != nil {
			return detail, err
		}
		for _, item := range recentItems {
			if item.UserID == userID {
				copyItem := item
				detail.Recent = &copyItem
				detail.User.LastSeenAt = item.LastSeenAt
				break
			}
		}
		detail.Usage, err = s.QueryUserUsage(userID, defaultDashboardUserWindow)
		if err != nil {
			return detail, err
		}
	}
	if s.events != nil {
		for _, event := range s.events.Tail(0, 200) {
			if event.UserID == userID {
				detail.Events = append(detail.Events, event)
			}
		}
		if len(detail.Events) > 20 {
			detail.Events = detail.Events[len(detail.Events)-20:]
		}
	}
	return detail, nil
}

func (s *DashboardService) GetOverview(window time.Duration, topN int) (DashboardOverview, error) {
	if window <= 0 {
		window = defaultDashboardOverviewWindow
	}
	if topN <= 0 {
		topN = 10
	}

	cacheKey := fmt.Sprintf("overview:%s:%d", window, topN)
	if cached, ok := s.getCached(cacheKey); ok {
		if overview, ok := cached.(DashboardOverview); ok {
			return overview, nil
		}
	}

	snap := s.bot.snapshot()
	overview := DashboardOverview{
		Window:        window,
		SSHSessions:   s.currentSessionCount(),
		DroppedEvents: s.events.DroppedCount(),
	}

	recentChats, err := s.ListRecentChats(max(topN*4, 20))
	if err != nil {
		return overview, err
	}
	overview.RecentChats = recentChats

	recentUsers, err := s.ListUsers(max(topN*3, 20), "")
	if err != nil {
		return overview, err
	}
	overview.RecentUsers = recentUsers

	hotChats := make([]DashboardHotChat, 0, len(recentChats))
	for _, item := range recentChats {
		usage, err := s.QueryChatUsage(item.ChatID, window)
		if err != nil {
			return overview, err
		}
		if usage.Total.RequestCount == 0 && usage.Total.TotalTokens == 0 {
			continue
		}
		overview.Metrics.Merge(usage.Total)
		hotChats = append(hotChats, DashboardHotChat{
			ChatID:      item.ChatID,
			Requests:    usage.Total.RequestCount,
			TotalTokens: usage.Total.TotalTokens,
			LastSeenAt:  item.LastSeenAt,
		})
	}
	sort.Slice(hotChats, func(i, j int) bool {
		if hotChats[i].Requests != hotChats[j].Requests {
			return hotChats[i].Requests > hotChats[j].Requests
		}
		if hotChats[i].TotalTokens != hotChats[j].TotalTokens {
			return hotChats[i].TotalTokens > hotChats[j].TotalTokens
		}
		return hotChats[i].LastSeenAt.After(hotChats[j].LastSeenAt)
	})
	if len(hotChats) > topN {
		hotChats = hotChats[:topN]
	}
	overview.TopChats = hotChats

	if snap.tasks != nil {
		snap.tasks.mu.RLock()
		for _, chatTasks := range snap.tasks.tasks {
			overview.ScheduleCount += len(chatTasks)
		}
		snap.tasks.mu.RUnlock()
	}
	if snap.userTools != nil && snap.userTools.store != nil {
		ids, err := snap.userTools.store.AllUserIDs()
		if err != nil {
			return overview, err
		}
		overview.MCPUserCount = len(ids)
	}

	modelCounts := make(map[string]*DashboardModelSummary)
	if s.events != nil {
		cutoff := time.Now().UTC().Add(-window)
		for _, event := range s.events.Tail(0, 512) {
			if event.Type != DashboardEventUsageRecorded || event.Time.Before(cutoff) || event.Model == "" {
				continue
			}
			item := modelCounts[event.Model]
			if item == nil {
				item = &DashboardModelSummary{Model: event.Model}
				modelCounts[event.Model] = item
			}
			item.Count++
			if event.Detail != "" {
				// no-op placeholder to keep detail available in future extensions
			}
			if event.Type == DashboardEventUsageRecorded {
				// tokens are already encoded in summary text, but model histogram only
				// needs relative activity at this stage.
			}
		}
	}
	for _, item := range modelCounts {
		overview.ModelSummaries = append(overview.ModelSummaries, *item)
	}
	sort.Slice(overview.ModelSummaries, func(i, j int) bool {
		return overview.ModelSummaries[i].Count > overview.ModelSummaries[j].Count
	})

	s.setCached(cacheKey, overview, defaultDashboardCacheTTL)
	return overview, nil
}

func (s *DashboardService) ListScheduleChats(limit int) ([]DashboardScheduleSummary, error) {
	snap := s.bot.snapshot()
	if snap.tasks == nil {
		return nil, nil
	}
	snap.tasks.mu.RLock()
	defer snap.tasks.mu.RUnlock()

	summaries := make([]DashboardScheduleSummary, 0, len(snap.tasks.tasks))
	for chatID, chatTasks := range snap.tasks.tasks {
		summary := DashboardScheduleSummary{ChatID: chatID, Count: len(chatTasks)}
		for _, task := range chatTasks {
			if task.Enabled {
				summary.EnabledCount++
			}
			if summary.NextRunAt.IsZero() || (!task.NextRunAt.IsZero() && task.NextRunAt.Before(summary.NextRunAt)) {
				summary.NextRunAt = task.NextRunAt
			}
		}
		summaries = append(summaries, summary)
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].EnabledCount != summaries[j].EnabledCount {
			return summaries[i].EnabledCount > summaries[j].EnabledCount
		}
		if summaries[i].Count != summaries[j].Count {
			return summaries[i].Count > summaries[j].Count
		}
		return summaries[i].ChatID < summaries[j].ChatID
	})
	if limit > 0 && len(summaries) > limit {
		summaries = summaries[:limit]
	}
	return summaries, nil
}

func (s *DashboardService) GetChatSchedules(chatID int64) (DashboardChatSchedules, error) {
	snap := s.bot.snapshot()
	if snap.tasks == nil {
		return DashboardChatSchedules{ChatID: chatID}, nil
	}
	return DashboardChatSchedules{
		ChatID: chatID,
		Tasks:  snap.tasks.List(chatID),
	}, nil
}

func (s *DashboardService) ListUpcomingSchedules(limit int) ([]ScheduledTaskRef, error) {
	snap := s.bot.snapshot()
	if snap.tasks == nil {
		return nil, nil
	}
	items := snap.tasks.Due(time.Now().UTC())
	sort.Slice(items, func(i, j int) bool {
		return items[i].Task.NextRunAt.Before(items[j].Task.NextRunAt)
	})
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (s *DashboardService) TailEvents(afterID uint64, limit int) []DashboardEvent {
	if s.events == nil {
		return nil
	}
	return s.events.Tail(afterID, limit)
}

func hasID(set map[int64]struct{}, id int64) bool {
	_, ok := set[id]
	return ok
}
