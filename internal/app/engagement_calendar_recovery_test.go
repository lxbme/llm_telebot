package app

import (
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func holdCalendarResponse(t *testing.T) (chan struct{}, chan struct{}, func()) {
	t.Helper()
	entered, reply := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(reply) })
	t.Cleanup(release)
	return entered, reply, release
}

func writeLegacyCalendarWork(t *testing.T, store *EngagementStore, ref EngagementRef, work EngagementWork) {
	t.Helper()
	work.CalendarDispatches = nil
	work.Results = slices.Clone(work.Results)
	work.Results[0].CalendarRequest = ""
	if err := store.transact(true, func(tx *bolt.Tx) error {
		return putEngagementJSON(tx.Bucket(engagementWorkBucket), engagementWorkKey(ref.ID, work.ChangeKey), work)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestEngagementRunnerCalendarOverlapArbitration(t *testing.T) {
	for _, order := range []string{"negative first", "success first", "reopen while replay pending"} {
		t.Run(order, func(t *testing.T) {
			h := newRunnerHarness(t, true)
			b, _ := h.open()
			ref := h.seed(b, engagementOwner, true, true, engagementOwner)
			c := h.connector
			c.calendarRequests = make(chan map[string]any, 8)
			entered, reply, releaseFirst := holdCalendarResponse(t)
			c.entered, c.reply = entered, reply
			replayed, replayReply, releaseReplay := holdCalendarResponse(t)
			c.replayEntered, c.replayReply = replayed, replayReply
			c.outcome.Store("not_applied")
			b.scheduler.tick(t.Context(), h.clock())
			roleReceive(t, entered)
			original := roleReceive(t, c.calendarRequests)
			before := getRunnerWork(t, b, ref)[0]
			if order == "success first" {
				writeLegacyCalendarWork(t, b.engagements, ref, before)
			}
			h.advance(engagementLease)
			c.outcome.Store("created")
			replayRunner := NewTaskRunner(b, nil)
			replayRunner.tick(t.Context(), h.clock())
			roleReceive(t, replayed)
			if replay := roleReceive(t, c.calendarRequests); !maps.Equal(original, replay) || c.applied.Load() != 1 {
				t.Fatalf("replay did not use the original request: original=%v replay=%v creates=%d", original, replay, c.applied.Load())
			}
			h.cfg.ChatDBPath = filepath.Join(t.TempDir(), "switch.db")
			h.cfg.SystemPrompt = "You are Alder. Use the current source."
			if err := b.applyRuntimeConfig(h.cfg); err != nil {
				t.Fatal(err)
			}
			changed := *before.Observation.Event
			changed.Title = "Rescheduled release review"
			changed.Start, changed.End = changed.Start.Add(time.Hour), changed.End.Add(time.Hour)
			c.events.Store(&[]engagementEvent{changed})
			if order == "reopen while replay pending" {
				releaseFirst()
				waitRunner(t, b.scheduler)
				work := getRunnerWork(t, b, ref)[0]
				if work.Results[0].State != EngagementReserved || len(work.CalendarDispatches) != 2 ||
					work.CalendarDispatches[0].State != EngagementNoEffect || work.CalendarDispatches[1].State != EngagementReserved {
					t.Fatalf("missing unreturned dispatch at interruption: %+v", work)
				}
				if err := b.closeChatDB(); err != nil {
					t.Fatal(err)
				}
				releaseReplay()
				waitRunner(t, replayRunner)
				b = h.restart(b)
				h.advance(engagementLease)
				h.tick(b)
				request, msg := roleReceive(t, c.calendarRequests), roleReceive(t, h.telegram.sends)
				item, after := getRunnerItem(t, b, ref), getRunnerWork(t, b, ref)[0]
				if !maps.Equal(original, request) || c.writes.Load() != 3 || c.applied.Load() != 1 || c.reads.Load() != 1 ||
					msg.chatID != engagementOwner || !strings.Contains(msg.text, "Release review 1") || len(h.telegram.sends) != 0 ||
					item.EndReason != EngagementCompleted || after.Results[0].Receipt != "calendar event calendar-event-123" ||
					after.Results[1].Receipt != "telegram:71:1" || len(after.CalendarDispatches) != 3 ||
					after.CalendarDispatches[1].State != EngagementReserved || after.CalendarDispatches[2].State != EngagementSucceeded {
					t.Fatalf("restart lost the unresolved binding: writes=%d creates=%d msg=%+v work=%+v", c.writes.Load(), c.applied.Load(), msg, after)
				}
				t.Logf("reads=%d writes=%d creates=%d calendar=%q fulfillment=%q", c.reads.Load(), c.writes.Load(), c.applied.Load(), after.Results[0].Receipt, after.Results[1].Receipt)
				return
			}
			var warningReceipt string
			if order == "negative first" {
				c.outcome.Store("not_applied")
				releaseFirst()
				waitRunner(t, b.scheduler)
				h.advance(engagementLease)
				// This worker runs after the first rejection while the successful
				// replay has still not returned. It can only use the pinned write.
				windowRunner := NewTaskRunner(b, nil)
				windowRunner.tick(t.Context(), h.clock())
				waitRunner(t, windowRunner)
				if c.writes.Load() != 3 || c.applied.Load() != 1 || c.reads.Load() != 1 {
					t.Fatalf("unreturned replay lost its binding: reads=%d writes=%d creates=%d", c.reads.Load(), c.writes.Load(), c.applied.Load())
				}
				if window := roleReceive(t, c.calendarRequests); !maps.Equal(original, window) {
					t.Fatalf("another worker changed the pending request: original=%v window=%v", original, window)
				}
				result := getRunnerWork(t, b, ref)[0]
				if msg := roleReceive(t, h.telegram.sends); msg.chatID != engagementOwner || !strings.Contains(msg.text, "couldn't confirm") ||
					result.Results[0].State != EngagementUnknown || getRunnerItem(t, b, ref).UncertainSteps != 1 || result.Results[1].Receipt != "telegram:71:1" {
					t.Fatalf("expected only an uncertainty notice: %+v, %+v", msg, result.Results)
				}
				owner := EngagementViewer{
					ChatID: engagementOwner, ActorID: engagementOwner,
					DataAccess: map[MemoryDataAccess]bool{result.Results[0].Step.Action.Access: true},
				}
				visible, err := b.engagements.ReadVisible(ref.ID, owner)
				if err != nil || !visible.OutcomeUncertain {
					t.Fatalf("calendar owner cannot see unresolved write after a rejected replay: %+v, %v", visible, err)
				}
				owner.DataAccess = nil
				visible, err = b.engagements.ReadVisible(ref.ID, owner)
				if err != nil || visible.OutcomeUncertain {
					t.Fatalf("unresolved private write without source access: %+v, %v", visible, err)
				}
				warningReceipt = result.Results[1].Receipt
			}
			releaseReplay()
			waitRunner(t, replayRunner)
			if order == "success first" {
				releaseFirst()
				waitRunner(t, b.scheduler)
			}
			if result := getRunnerWork(t, b, ref)[0].Results[0]; result.State != EngagementSucceeded || result.Key != original["idempotencyKey"] || result.Receipt != "calendar event calendar-event-123" {
				t.Fatalf("successful replay did not settle its original logical write: %+v", result)
			}
			if len(h.telegram.sends) != 0 {
				t.Fatal("retired worker delivered before recovery")
			}
			beforeRestart := c.writes.Load()
			b = h.restart(b)
			h.advance(engagementPoll)
			h.tick(b)
			msg := roleReceive(t, h.telegram.sends)
			item, works := getRunnerItem(t, b, ref), getRunnerWork(t, b, ref)
			if c.writes.Load() != beforeRestart || c.applied.Load() != 1 || c.reads.Load() != 1 || len(h.telegram.sends) != 0 ||
				msg.chatID != engagementOwner || !strings.Contains(msg.text, "Release review 1") || !strings.Contains(msg.text, "added to your calendar") ||
				item.Origin.SenderID != engagementOwner || item.EndReason != EngagementCompleted || item.CompletedChangeKey != before.ChangeKey ||
				len(works) != 1 || works[0].Results[len(works[0].Results)-1].State != EngagementSucceeded {
				t.Fatalf("reopen did not resume only the original fulfillment notice: writes=%d creates=%d msg=%+v item=%+v work=%+v", c.writes.Load(), c.applied.Load(), msg, item, works)
			}
			if warningReceipt != "" && works[0].Results[1].Receipt != warningReceipt {
				t.Fatal("lost original uncertainty notice receipt")
			}
			dispatches := works[0].CalendarDispatches
			if len(dispatches) != int(c.writes.Load()) || dispatches[0].State != EngagementNoEffect ||
				dispatches[0].AttemptRef != before.Results[0].AttemptRef || dispatches[0].Receipt != "calendar confirmed change was not applied" ||
				dispatches[1].State != EngagementSucceeded || dispatches[1].AttemptRef != works[0].Results[0].AttemptRef {
				t.Fatalf("missing original and replay receipts after reopen: %+v", dispatches)
			}
			if len(dispatches) == 3 && (dispatches[2].State != EngagementNoEffect || dispatches[2].Receipt != dispatches[0].Receipt) {
				t.Fatalf("missing window replay rejection: %+v", dispatches[2])
			}
			for i, dispatch := range dispatches {
				if dispatch.Key != before.Results[0].Key || dispatch.Step != before.Results[0].Step ||
					dispatch.Attempt != before.Results[0].Attempt || dispatch.CalendarReplay != uint64(i) ||
					dispatch.CalendarRequest != before.Results[0].CalendarRequest || dispatch.Observation.Evidence != before.Observation.Evidence ||
					dispatch.Observation.Event == nil || *dispatch.Observation.Event != *before.Observation.Event {
					t.Fatalf("dispatch does not identify the pinned request and evidence: %+v", dispatch)
				}
			}
			t.Logf("reads=%d writes=%d creates=%d calendar=%q warning=%q fulfillment=%q", c.reads.Load(), c.writes.Load(), c.applied.Load(), works[0].Results[0].Receipt, warningReceipt, works[0].Results[len(works[0].Results)-1].Receipt)
		})
	}
}

func TestEngagementRunnerCalendarRechecksAfterUncertainNotice(t *testing.T) {
	const missingWarningReceipt = "unconfirmed warning rescheduled"
	for _, scenario := range []string{"rescheduled", "legacy rescheduled", missingWarningReceipt, "canceled", "event elapsed"} {
		t.Run(scenario, func(t *testing.T) {
			h := newRunnerHarness(t, false)
			b, _ := h.open()
			ref := h.seed(b, engagementOwner, true, true, engagementOwner)
			c := h.connector
			c.calendarRequests = make(chan map[string]any, 4)
			entered, reply, release := holdCalendarResponse(t)
			c.entered, c.reply = entered, reply
			c.outcome.Store("not_applied")
			b.scheduler.tick(t.Context(), h.clock())
			roleReceive(t, entered)
			original := getRunnerWork(t, b, ref)[0]
			h.advance(engagementLease)
			wantWarningState := EngagementSucceeded
			wantWarningReceipt, wantFinalReceipt := "telegram:71:1", "telegram:71:2"
			if scenario == missingWarningReceipt {
				h.telegram.failTo.Store(engagementOwner)
				h.telegram.fail.Store(http.StatusBadGateway)
				wantWarningState = EngagementUnknown
				wantWarningReceipt, wantFinalReceipt = "Telegram delivery could not be verified", "telegram:71:1"
			}
			recovery := NewTaskRunner(b, nil)
			recovery.tick(t.Context(), h.clock())
			waitRunner(t, recovery)
			msg := roleReceive(t, h.telegram.sends)
			warning := getRunnerWork(t, b, ref)[0].Results[1]
			if c.writes.Load() != 1 || c.applied.Load() != 0 || msg.chatID != engagementOwner || !strings.Contains(msg.text, "couldn't confirm") ||
				!warning.Step.UncertainNotice || warning.State != wantWarningState || warning.Receipt != wantWarningReceipt || !getRunnerItem(t, b, ref).LastDeliveredAt.IsZero() {
				t.Fatalf("warning was used as a fulfillment receipt: %+v, %+v", msg, warning)
			}
			h.telegram.fail.Store(0)
			release()
			waitRunner(t, b.scheduler)
			negative := getRunnerWork(t, b, ref)[0].Results[0]
			if negative.AttemptRef != original.Results[0].AttemptRef || negative.State != EngagementNoEffect || negative.Receipt != "calendar confirmed change was not applied" {
				t.Fatalf("late receipt did not settle the original request: %+v", negative)
			}
			if scenario == "legacy rescheduled" {
				writeLegacyCalendarWork(t, b.engagements, ref, getRunnerWork(t, b, ref)[0])
			}
			b = h.restart(b)
			event := *original.Observation.Event
			switch scenario {
			case "rescheduled", "legacy rescheduled", missingWarningReceipt:
				event.Title = "New release review"
				event.Start, event.End = event.Start.Add(time.Hour), event.End.Add(time.Hour)
			case "canceled":
				event.Summary += " — organizer canceled the meeting"
				h.decision.Store(`{"matched_ids":[],"bound_rejected":true}`)
			case "event elapsed":
				h.advance(time.Hour)
				h.decision.Store(`{"matched_ids":[]}`)
			}
			c.events.Store(&[]engagementEvent{event})
			c.outcome.Store("created")
			h.advance(engagementPoll)
			h.tick(b)
			item, work := getRunnerItem(t, b, ref), getRunnerWork(t, b, ref)[0]
			if c.reads.Load() != 2 || h.model.calls.Load() != 2 || work.Results[1] != warning {
				t.Fatalf("did not recheck current source and preserve warning: reads=%d model=%d work=%+v", c.reads.Load(), h.model.calls.Load(), work)
			}
			var finalReceipt string
			if strings.HasSuffix(scenario, "rescheduled") {
				if c.writes.Load() != 2 || c.applied.Load() != 1 || item.EndReason != EngagementCompleted || item.Origin.SenderID != engagementOwner ||
					len(work.Results) != 3 || work.Results[0].Receipt != "calendar event calendar-event-123" || work.Results[2].Receipt != wantFinalReceipt {
					t.Fatalf("valid recheck did not fulfill: writes=%d creates=%d item=%+v work=%+v", c.writes.Load(), c.applied.Load(), item, work)
				}
				first, second := roleReceive(t, c.calendarRequests), roleReceive(t, c.calendarRequests)
				msg := roleReceive(t, h.telegram.sends)
				if first["title"] != "Release review 1" || second["title"] != event.Title || second["start"] != event.Start.Format(time.RFC3339) ||
					msg.chatID != engagementOwner || !strings.Contains(msg.text, event.Title) || !strings.Contains(msg.text, "added to your calendar") {
					t.Fatalf("incorrect revalidated request or recipient: %v, %+v", second, msg)
				}
				finalReceipt = work.Results[2].Receipt
				dispatches := work.CalendarDispatches
				if len(dispatches) != 2 || dispatches[0].EngagementResult != negative || dispatches[1].EngagementResult != work.Results[0] ||
					dispatches[0].Observation.Event.Title != "Release review 1" || dispatches[1].Observation.Event.Title != event.Title ||
					dispatches[0].Key == dispatches[1].Key {
					t.Fatalf("missing prior attempt or its original source: %+v", dispatches)
				}
			} else if c.writes.Load() != 1 || c.applied.Load() != 0 || work.Results[0] != negative || item.EndReason == EngagementCompleted {
				t.Fatalf("invalid activity triggered a write: writes=%d creates=%d item=%+v work=%+v", c.writes.Load(), c.applied.Load(), item, work)
			}
			if len(h.telegram.sends) != 0 {
				t.Fatal("unexpected extra Telegram send")
			}
			t.Logf("reads=%d writes=%d creates=%d negative=%q warning=%q fulfillment=%q", c.reads.Load(), c.writes.Load(), c.applied.Load(), negative.Receipt, warning.Receipt, finalReceipt)
		})
	}
}
