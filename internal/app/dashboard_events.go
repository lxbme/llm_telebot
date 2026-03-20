package app

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const defaultDashboardEventBufferSize = 4096

type DashboardEventType string

const (
	DashboardEventConversationStarted  DashboardEventType = "conversation_started"
	DashboardEventConversationFinished DashboardEventType = "conversation_finished"
	DashboardEventConversationError    DashboardEventType = "conversation_error"
	DashboardEventUsageRecorded        DashboardEventType = "usage_recorded"
	DashboardEventToolCallStarted      DashboardEventType = "tool_call_started"
	DashboardEventToolCallFinished     DashboardEventType = "tool_call_finished"
	DashboardEventMCPChanged           DashboardEventType = "mcp_changed"
	DashboardEventScheduleTriggered    DashboardEventType = "schedule_triggered"
	DashboardEventScheduleFailed       DashboardEventType = "schedule_failed"
	DashboardEventProfileUpdated       DashboardEventType = "profile_updated"
	DashboardEventSummaryUpdated       DashboardEventType = "summary_updated"
	DashboardEventConfigReloaded       DashboardEventType = "config_reloaded"
	DashboardEventSSHLogin             DashboardEventType = "ssh_login"
)

type DashboardEvent struct {
	ID        uint64             `json:"id"`
	Time      time.Time          `json:"time"`
	Type      DashboardEventType `json:"type"`
	ChatID    int64              `json:"chat_id,omitempty"`
	UserID    int64              `json:"user_id,omitempty"`
	Model     string             `json:"model,omitempty"`
	RequestID string             `json:"request_id,omitempty"`
	Summary   string             `json:"summary,omitempty"`
	Detail    string             `json:"detail,omitempty"`
	ToolName  string             `json:"tool_name,omitempty"`
	LatencyMs int64              `json:"latency_ms,omitempty"`
	Success   bool               `json:"success,omitempty"`
}

type DashboardEventHub struct {
	mu      sync.RWMutex
	buffer  []DashboardEvent
	head    int
	count   int
	dropped atomic.Uint64
	nextID  atomic.Uint64
}

func NewDashboardEventHub(size int) *DashboardEventHub {
	if size <= 0 {
		size = defaultDashboardEventBufferSize
	}
	return &DashboardEventHub{
		buffer: make([]DashboardEvent, size),
	}
}

func (h *DashboardEventHub) Publish(event DashboardEvent) DashboardEvent {
	if h == nil || len(h.buffer) == 0 {
		return event
	}
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	} else {
		event.Time = event.Time.UTC()
	}
	event.ID = h.nextID.Add(1)
	event.Summary = truncateDashboardText(strings.TrimSpace(event.Summary), 240)
	event.Detail = truncateDashboardText(strings.TrimSpace(event.Detail), 400)

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.count < len(h.buffer) {
		idx := (h.head + h.count) % len(h.buffer)
		h.buffer[idx] = event
		h.count++
		return event
	}

	h.buffer[h.head] = event
	h.head = (h.head + 1) % len(h.buffer)
	h.dropped.Add(1)
	return event
}

func (h *DashboardEventHub) Tail(afterID uint64, limit int) []DashboardEvent {
	if h == nil {
		return nil
	}
	if limit <= 0 {
		limit = 100
	}

	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.count == 0 {
		return nil
	}

	events := make([]DashboardEvent, 0, min(limit, h.count))
	for i := 0; i < h.count; i++ {
		idx := (h.head + i) % len(h.buffer)
		event := h.buffer[idx]
		if afterID > 0 && event.ID <= afterID {
			continue
		}
		events = append(events, event)
	}
	if afterID == 0 && len(events) > limit {
		events = events[len(events)-limit:]
	}
	return events
}

func (h *DashboardEventHub) DroppedCount() uint64 {
	if h == nil {
		return 0
	}
	return h.dropped.Load()
}

func NewDashboardEventFromUsage(event UsageEvent) DashboardEvent {
	return DashboardEvent{
		Time:      event.Timestamp,
		Type:      DashboardEventUsageRecorded,
		ChatID:    event.ChatID,
		UserID:    event.UserID,
		Model:     event.Model,
		RequestID: event.RequestID,
		LatencyMs: event.LatencyMs,
		Success:   event.Success,
		Summary: fmt.Sprintf(
			"%s prompt=%d completion=%d total=%d success=%t",
			event.CallType,
			event.PromptTokens,
			event.CompletionTokens,
			event.TotalTokens,
			event.Success,
		),
	}
}

func truncateDashboardText(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
