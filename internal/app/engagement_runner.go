package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
	tele "gopkg.in/telebot.v3"
)

const (
	engagementLease = 2 * time.Minute
	engagementPoll  = time.Minute
	engagementBatch = 8
)

type engagementRun struct {
	bot   *Bot
	store *EngagementStore
	claim EngagementClaim
	item  Engagement
	snap  runtimeSnapshot
}

func (r *TaskRunner) runDueEngagements(ctx context.Context) {
	store := r.bot.snapshot().engagements
	if store == nil {
		return
	}
	// Claim as each item starts, so a slow connector cannot consume another
	// item's lease while it waits in this worker's queue.
	for range engagementBatch {
		if ctx.Err() != nil {
			return
		}
		claims, err := store.ClaimDue(store.now(), engagementLease, 1)
		if err != nil {
			log.Printf("[engagement] claim due: %v", err)
			return
		}
		if len(claims) == 0 {
			return
		}
		run := engagementRun{bot: r.bot, store: store, claim: claims[0]}
		run.execute(ctx)
	}
}

func (r *engagementRun) execute(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	var err error
	r.item, err = r.store.ReadClaim(r.claim, r.store.now())
	if err != nil {
		return
	}
	snap, generation := r.bot.snapshotForReply()
	defer r.bot.releaseReplyTools(generation)
	r.snap = snap
	check, err := r.check(ctx)
	switch {
	case errors.Is(err, errEngagementRoute):
		err = r.store.pauseClaim(r.claim, r.store.now(), EngagementRouteUnverified)
	case errors.Is(err, ErrEngagementDenied), errors.Is(err, errRoleAudience):
		if r.store.pauseClaim(r.claim, r.store.now(), EngagementAccessLost) == nil {
			r.reportPause(ctx)
		}
		return
	case errors.Is(err, ErrEngagementLease):
		return
	case errors.Is(err, ErrEngagementConflict):
		// A runtime-only change may have left this claim valid. Release it
		// without carrying a decision from the retired runtime forward.
		now := r.store.now()
		err = r.store.FinishCheck(r.claim, now, EngagementCheck{NextCheck: now.Add(15 * time.Second)})
	case ctx.Err() == nil || parent.Err() == nil:
		if err != nil {
			// Connector, model and transport errors may contain credentials.
			log.Printf("[engagement] check %s deferred", r.claim.ID)
		}
		if !check.NextCheck.After(r.store.now()) {
			check.NextCheck = r.store.now().Add(engagementPoll)
		}
		err = r.store.FinishCheck(r.claim, r.store.now(), check)
	}
	if err != nil && !errors.Is(err, ErrEngagementLease) {
		log.Printf("[engagement] finish %s: %v", r.claim.ID, err)
	}
}

var errEngagementRoute = errors.New("engagement destination needs clarification")

// Check the claim and runtime generation at each operation boundary. Runtime
// switches preserve claims and receipts, but fresh operations must discover
// the current tool and use the current role.
func (r *engagementRun) current(ctx context.Context) (Engagement, error) {
	if err := ctx.Err(); err != nil {
		return Engagement{}, err
	}
	live := r.bot.snapshot()
	if live.engagements != r.store || !sameBackgroundRole(r.snap, live) ||
		live.tools != r.snap.tools || live.userTools != r.snap.userTools {
		return Engagement{}, ErrEngagementConflict
	}
	item, err := r.store.ReadClaim(r.claim, r.store.now())
	if err == nil && item.Ref != r.item.Ref {
		return Engagement{}, ErrEngagementConflict
	}
	return item, err
}

func (r *engagementRun) check(ctx context.Context) (EngagementCheck, error) {
	a := r.item.Current
	if _, err := r.item.recipientSources(); err != nil {
		return EngagementCheck{}, errEngagementRoute
	}
	if a.Observe.Operation == engagementPrivateFollowup {
		return r.checkPrivateFollowup(ctx)
	}
	quotaAvailable := r.bot.backgroundQuota(r.billingUser(), a.Observe.DataChatID, r.store.now())
	if a.Once {
		return r.checkOneShot(ctx, quotaAvailable)
	}
	if len(a.Actions) == 0 {
		if !quotaAvailable {
			return EngagementCheck{}, nil
		}
		return r.checkOngoing(ctx)
	}
	// Resume pending work first. A completed action and pending Telegram
	// delivery do not need another upstream tool call or model decision.
	pending := false
	var check EngagementCheck
	for cursor := ""; ; {
		if err := ctx.Err(); err != nil {
			return EngagementCheck{}, err
		}
		works, err := r.store.ClaimWork(r.claim, r.store.now(), cursor, 32)
		if err != nil {
			return EngagementCheck{}, err
		}
		if len(works) == 0 {
			break
		}
		for _, work := range works {
			cursor = work.ChangeKey
			if work.completed(*a) || !quotaAvailable {
				continue
			}
			complete, err := r.fulfill(ctx, work, false)
			if err != nil {
				return EngagementCheck{}, err
			}
			if complete {
				check.CompletedChange = work.ChangeKey
			}
			pending = pending || !complete
		}
	}
	if !quotaAvailable {
		return check, nil
	}
	events, err := r.observe(ctx, "")
	if err != nil {
		return check, err
	}
	check.EndConditionMet = events.ended && !pending
	if _, err := r.current(ctx); err != nil {
		return EngagementCheck{}, err
	}
	if len(events.matched) == 0 {
		_, _, err := r.store.Observe(r.claim, r.store.now(), EngagementObservation{Evidence: events.checkpoint})
		return check, err
	}
	for _, event := range events.matched {
		observation := EngagementObservation{
			ChangeKey: event.ID, Event: &event,
			Evidence: EngagementText{ChatID: a.Observe.DataChatID, Access: a.Observe.Access, Text: event.Title},
		}
		work, fresh, err := r.store.Observe(r.claim, r.store.now(), observation)
		if err != nil {
			return EngagementCheck{}, err
		}
		if !fresh {
			continue // It was already considered on this check or explicitly invalidated.
		}
		complete, err := r.fulfill(ctx, work, false)
		if err != nil {
			return EngagementCheck{}, err
		}
		if complete {
			check.CompletedChange = work.ChangeKey
		} else {
			check.EndConditionMet = false
		}
	}
	return check, nil
}

func (r *engagementRun) checkOneShot(ctx context.Context, quotaAvailable bool) (EngagementCheck, error) {
	a := r.item.Current
	works, err := r.store.ClaimWork(r.claim, r.store.now(), "", 1)
	if err != nil {
		return EngagementCheck{}, err
	}
	var boundID string
	if len(works) != 0 {
		work := works[0]
		if work.completed(*a) {
			return EngagementCheck{CompletedChange: work.ChangeKey}, nil
		}
		if work.ChangeKey == r.item.OnceChangeKey {
			boundID = work.Observation.ChangeKey
		}
		switch {
		case len(a.Actions) == 0:
			canSend := false
			for _, id := range a.Recipients {
				i := work.resultIndex(EngagementStep{Recipient: id})
				if i < 0 || work.Results[i].State == EngagementNoEffect {
					canSend = canSend || engagementChatIDAllowed(r.snap.cfg, id, r.item.Origin.SenderID)
				} else if result := work.Results[i]; result.State == EngagementReserved {
					if _, err := r.recordUnknown(result.bind(r.claim.ID, work.ChangeKey), "Telegram delivery could not be verified"); err != nil {
						return EngagementCheck{}, err
					}
				}
			}
			if !canSend {
				return EngagementCheck{}, nil
			}
		case quotaAvailable && work.mayHaveTakenEffect():
			// Reconcile only the existing action or finish its delivery.
			// A fresh source decision cannot displace this attempt.
			return r.fulfillOneShot(ctx, work, true)
		}
	}
	if !quotaAvailable {
		return EngagementCheck{}, nil
	}
	// A new calendar attempt or deferred read-only delivery requires current
	// source evidence. Failures leave the old observation and receipts intact.
	events, err := r.observe(ctx, boundID)
	if err != nil {
		return EngagementCheck{}, err
	}
	if _, err := r.current(ctx); err != nil {
		return EngagementCheck{}, err
	}
	now := r.store.now()
	observation := EngagementObservation{Evidence: events.checkpoint}
	if !events.ended && len(events.matched) != 0 {
		event := events.matched[0]
		if a.oneShotEligible(event, now) {
			observation.ChangeKey, observation.Event = event.ID, &event
		}
	}
	// Inconclusive evidence can update the checkpoint without releasing the binding.
	revalidate := !events.boundUncertain || len(a.Actions) != 0 && events.ended
	work, _, err := r.store.observe(r.claim, now, observation, revalidate)
	check := EngagementCheck{EndConditionMet: events.ended}
	if err != nil || work.ChangeKey == "" {
		return check, err
	}
	return r.fulfillOneShot(ctx, work, false)
}

func (r *engagementRun) fulfillOneShot(ctx context.Context, work EngagementWork, reconcileOnly bool) (EngagementCheck, error) {
	var check EngagementCheck
	complete, err := r.fulfill(ctx, work, reconcileOnly)
	if complete {
		check.CompletedChange = work.ChangeKey
	}
	return check, err
}

func (r *engagementRun) verifyCapability(ctx context.Context, cap EngagementCapability) (resolvedEngagementCapability, error) {
	item, err := r.current(ctx)
	if err != nil {
		return resolvedEngagementCapability{}, err
	}
	if !r.bot.backgroundQuota(r.billingUser(), cap.DataChatID, r.store.now()) {
		return resolvedEngagementCapability{}, errEngagementQuota
	}
	resolved, err := r.bot.verifyEngagementCapability(ctx, item, cap)
	if err != nil {
		return resolvedEngagementCapability{}, err
	}
	return resolved, r.admitTool(ctx, cap, resolved)
}

var errEngagementQuota = errors.New("background quota exhausted")

func (r *engagementRun) admitTool(ctx context.Context, cap EngagementCapability, verified resolvedEngagementCapability) error {
	if _, err := r.current(ctx); err != nil {
		return err
	}
	live := r.bot.snapshot()
	current, err := live.resolveEngagementCapability(cap, r.item.Origin.SenderID)
	if err != nil || verified.tool != current.tool {
		return ErrEngagementDenied
	}
	if !engagementChatIDAllowed(live.cfg, cap.DataChatID, r.item.Origin.SenderID) {
		return errRoleAudience
	}
	if !r.bot.backgroundQuota(r.billingUser(), cap.DataChatID, r.store.now()) {
		return errEngagementQuota
	}
	return nil
}

func (r *engagementRun) fulfill(ctx context.Context, work EngagementWork, reconcileOnly bool) (bool, error) {
	a := r.item.Current
	actionsPending := false
	var unknownChats []int64
	for _, cap := range a.Actions {
		step := EngagementStep{Action: cap}
		if !work.permits(*a, step) {
			continue
		}
		i := work.resultIndex(step)
		if i >= 0 && work.Results[i].State == EngagementSucceeded {
			continue
		}
		if reconcileOnly && (i < 0 || work.Results[i].State == EngagementNoEffect) {
			actionsPending = true
			continue
		}
		state, err := r.executeAction(ctx, work, step, reconcileOnly)
		if err != nil {
			return false, err
		}
		actionsPending = actionsPending || state != EngagementSucceeded
		if state == EngagementUnknown || state == EngagementReserved {
			unknownChats = append(unknownChats, cap.DataChatID)
		}
	}
	complete := !actionsPending
	for _, id := range a.Recipients {
		step := EngagementStep{Recipient: id, UncertainNotice: slices.Contains(unknownChats, id)}
		if actionsPending && !step.UncertainNotice || !work.permits(*a, step) {
			continue
		}
		i := work.resultIndex(step)
		if i >= 0 && work.Results[i].State == EngagementSucceeded {
			continue
		}
		state, err := r.deliver(ctx, work, step, "")
		// An unavailable recipient must not withhold other, independently
		// authorized recipients' delivery. Retry only the unsettled step.
		if err != nil && !errors.Is(err, errRoleAudience) && !errors.Is(err, errEngagementQuota) {
			return false, err
		}
		complete = complete && err == nil && state == EngagementSucceeded
	}
	return complete, nil
}

func (r *engagementRun) executeAction(ctx context.Context, work EngagementWork, step EngagementStep, reconcileOnly bool) (EngagementResultState, error) {
	cap := step.Action
	source, err := r.verifyCapability(ctx, work.Observe)
	if err != nil {
		return "", err
	}
	verified, err := r.verifyCapability(ctx, cap)
	if err != nil {
		return "", err
	}
	if cap.DataChatID != work.Observe.DataChatID {
		return "", ErrEngagementDenied
	}
	event := work.Observation.Event
	if event == nil || !event.valid() {
		return "", ErrEngagementInvalid
	}
	now := r.store.now()
	eligible := r.item.Current.oneShotEligible(*event, now)
	if !reconcileOnly && !eligible {
		return "", ErrEngagementInvalid
	}
	binding := verified.tool.background
	result, start, err := r.store.beginStep(r.claim, now, work.ChangeKey, step, reconcileOnly, binding.Idempotent && eligible)
	if err != nil || result.State == EngagementSucceeded {
		return result.State, err
	}
	if !start {
		return r.recordUnknown(result, "calendar outcome could not be verified")
	}
	base := result.CalendarRequest
	if base == "" {
		// Older reservations stored their typed arguments in the original observation.
		base, err = calendarCreateArgs(binding.Target, event.calendarEvent())
	}
	var args string
	if err == nil {
		args, err = verified.withIdempotencyKey(base, result.Key)
	}
	if err == nil {
		err = r.admitTool(ctx, work.Observe, source)
	}
	if err == nil {
		err = r.admitTool(ctx, cap, verified)
	}
	if err == nil && !r.item.Current.oneShotEligible(*event, r.store.now()) {
		err = ErrEngagementInvalid
	}
	if err != nil {
		settled, saveErr := r.store.archiveOutcome(result.AttemptRef, r.store.now(), EngagementNoEffect, "request was never sent")
		return settled, errors.Join(err, saveErr)
	}
	response, callErr := verified.tool.call(ctx, args)
	state, receipt := calendarOutcome(binding, result.Key, response, callErr)
	return r.store.archiveOutcome(result.AttemptRef, r.store.now(), state, receipt)
}

func (r *engagementRun) recordUnknown(result EngagementResult, reason string) (EngagementResultState, error) {
	if result.State == EngagementUnknown {
		return EngagementUnknown, nil
	}
	return EngagementUnknown, r.saveResult(result, EngagementUnknown, reason)
}

func (r *engagementRun) saveResult(attempt EngagementResult, state EngagementResultState, receipt string) error {
	return r.store.ArchiveOutcome(attempt.AttemptRef, r.store.now(), state, receipt)
}

func (r *engagementRun) recipient(ctx context.Context, item Engagement, id int64) (*tele.Chat, error) {
	grants, err := item.recipientSources()
	if err != nil || !slices.ContainsFunc(grants, func(g EngagementRecipientSource) bool { return g.ChatID == id }) {
		return nil, errEngagementRoute
	}
	chat, err := r.snap.engagementChat(ctx, id, item.Origin.SenderID)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	live := r.bot.snapshot()
	if !engagementChatAllowed(live.cfg, chat, item.Origin.SenderID) {
		return nil, errRoleAudience
	}
	return chat, nil
}

func (r *engagementRun) billingUser() int64 {
	if r.item.Kind == EngagementTracking && r.item.Origin.ChatID < 0 {
		return 0
	}
	return r.item.Origin.SenderID
}

func (r *engagementRun) notificationAllowed(item Engagement, id int64) (bool, error) {
	now := r.store.now()
	if item.Muted && (item.MuteUntil.IsZero() || item.MuteUntil.After(now)) {
		return false, nil
	}
	pref, err := r.store.ConversationPreferenceAt(id, 0, now)
	return pref.Notifications != ConversationOff &&
		(pref.Notifications != ConversationReduce || !item.DeliveredTo[id].Add(24*time.Hour).After(now)), err
}

func (r *engagementRun) deliver(ctx context.Context, work EngagementWork, step EngagementStep, report string) (EngagementResultState, error) {
	id := step.Recipient
	item, err := r.current(ctx)
	if err != nil {
		return "", err
	}
	chat, err := r.recipient(ctx, item, id)
	if err != nil {
		return "", err
	}
	// Personal source data never crosses into another chat. Public source
	// facts can go to a separately verified, accepted recipient. Do not
	// expose restricted trigger text or calendar arguments in this report.
	if work.Observe.Access.OwnerID != 0 && work.Observe.DataChatID != id {
		return "", errRoleAudience
	}
	item, err = r.current(ctx)
	if err != nil {
		return "", err
	}
	allowed, err := r.notificationAllowed(item, id)
	if err != nil || !allowed {
		return "", err
	}
	if !r.bot.backgroundQuota(r.billingUser(), id, r.store.now()) {
		return "", errEngagementQuota
	}
	result, start, err := r.store.BeginStep(r.claim, r.store.now(), work.ChangeKey, step)
	if err != nil || result.State == EngagementSucceeded {
		return result.State, err
	}
	if !start {
		return r.recordUnknown(result, "Telegram delivery could not be verified")
	}
	item, err = r.current(ctx)
	if err == nil && !engagementChatAllowed(r.bot.snapshot().cfg, chat, item.Origin.SenderID) {
		err = errRoleAudience
	}
	if err == nil && !r.bot.backgroundQuota(r.billingUser(), id, r.store.now()) {
		err = errEngagementQuota
	}
	if err == nil {
		allowed, err = r.notificationAllowed(item, id)
	}
	if err != nil || !allowed {
		return EngagementNoEffect, errors.Join(err, r.saveResult(result, EngagementNoEffect, "Telegram request was never sent"))
	}
	if report == "" {
		report = r.report(work, step)
	}
	state, receipt, _ := r.snap.sendEngagementText(ctx, chat.ID, report)
	return state, r.saveResult(result, state, receipt)
}

func (r *engagementRun) localized(en, zh string) string {
	if r.item.Initial != nil {
		return engagementLanguage(r.item.Initial.Request.Text, en, zh)
	}
	return en
}

func (r *engagementRun) report(work EngagementWork, step EngagementStep) string {
	if step.UncertainNotice {
		return fmt.Sprintf(r.localized("Tracking %s — I couldn't confirm the calendar result. Check the calendar before adding the event again.",
			"跟踪 %s：无法确认日历操作结果。再次添加前请先核对日历。"), r.claim.ID)
	}
	text := fmt.Sprintf(r.localized("Tracking %s — confirmed a matching update.", "跟踪 %s：已核实一项符合条件的更新。"), r.claim.ID)
	if event := work.Observation.Event; event != nil {
		text = fmt.Sprintf(r.localized("Tracking %s — observed %s.", "跟踪 %s：已观察到 %s。"), r.claim.ID, event.Title)
		text += event.evidenceText()
	}
	for _, action := range r.item.Current.Actions {
		// Only render a success proven by the completed action path, and
		// identify it solely to the owner of the account.
		if action.DataChatID == step.Recipient && work.permits(*r.item.Current, EngagementStep{Action: action}) {
			text += "\n" + r.localized("The event was added to your calendar.", "已将该活动添加到你的日历。")
		}
	}
	return text
}

func (event engagementEvent) evidenceText() string {
	var evidence string
	if summary := strings.TrimSpace(event.Summary); summary != "" {
		evidence += "\n" + summary
	}
	if url := event.evidenceURL(); url != "" {
		evidence += "\n" + url
	}
	return evidence
}

// Ongoing read-only agreements use the current source snapshot. A version
// no longer present or an elapsed event cannot fill a catch-up notification.
func (r *engagementRun) checkOngoing(ctx context.Context) (EngagementCheck, error) {
	events, err := r.observe(ctx, "")
	if err != nil {
		return EngagementCheck{}, err
	}
	if _, err := r.current(ctx); err != nil {
		return EngagementCheck{}, err
	}
	now := r.store.now()
	if events.ended || len(events.matched) == 0 {
		_, _, err := r.store.Observe(r.claim, now, EngagementObservation{Evidence: events.checkpoint})
		return EngagementCheck{EndConditionMet: events.ended}, err
	}
	var available []EngagementWork
	for _, event := range events.matched {
		observation := EngagementObservation{ChangeKey: event.ID, Event: &event,
			Evidence: EngagementText{ChatID: r.item.Current.Observe.DataChatID, Access: r.item.Current.Observe.Access, Text: event.Title}}
		work, _, err := r.store.Observe(r.claim, now, observation)
		if err != nil {
			return EngagementCheck{}, err
		}
		if work.InvalidatedVersion == 0 && (event.End.IsZero() || event.End.After(now)) {
			available = append(available, work)
		}
	}
	for _, id := range r.item.Current.Recipients {
		if err := r.sendCombinedNotice(ctx, id, available); err != nil &&
			!errors.Is(err, errEngagementQuota) && !errors.Is(err, errRoleAudience) {
			return EngagementCheck{}, err
		}
	}
	var check EngagementCheck
	err = r.store.withClaim(r.claim, r.store.now(), false, func(tx *bolt.Tx, row *engagementRecord) error {
		for _, candidate := range available {
			work, err := loadEngagementWork(tx, row.Ref.ID, candidate.ChangeKey)
			if err != nil {
				return err
			}
			if work.completed(*row.Current) {
				check.CompletedChange = work.ChangeKey
			}
		}
		return nil
	})
	return check, err
}

func (r *engagementRun) sendCombinedNotice(ctx context.Context, id int64, available []EngagementWork) error {
	item, err := r.current(ctx)
	if err != nil {
		return err
	}
	if !r.bot.backgroundQuota(r.billingUser(), id, r.store.now()) {
		return errEngagementQuota
	}
	if allowed, err := r.notificationAllowed(item, id); !allowed || err != nil {
		return err
	}
	step := EngagementStep{Recipient: id}
	text := fmt.Sprintf(r.localized("Tracking %s — verified updates:", "跟踪 %s：已核实以下更新"), item.Ref.ID)
	var changes []string
	for _, work := range available {
		if !work.permits(*item.Current, step) || work.Observe.Access.OwnerID != 0 && work.Observe.DataChatID != id {
			continue
		}
		if i := work.resultIndex(step); i >= 0 {
			prior := work.Results[i]
			if prior.State == EngagementReserved {
				_, err := r.recordUnknown(prior.bind(item.Ref.ID, work.ChangeKey), "Telegram delivery could not be verified")
				if err != nil {
					return err
				}
			}
			if prior.State != EngagementNoEffect {
				continue
			}
		}
		event := work.Observation.Event
		entry := "\n\n" + event.Title + event.evidenceText()
		if !fitsTelegramMessage(text + entry) {
			break
		}
		text += entry
		changes = append(changes, work.ChangeKey)
	}
	if len(changes) == 0 {
		return nil
	}
	chat, err := r.recipient(ctx, item, id)
	if err != nil {
		return err
	}
	attempts, err := r.store.beginCombinedNotice(r.claim, r.store.now(), changes, step)
	if err != nil {
		return err
	}
	state, receipt := EngagementNoEffect, "Telegram request was never sent"
	item, err = r.current(ctx)
	if err == nil && !engagementChatAllowed(r.bot.snapshot().cfg, chat, item.Origin.SenderID) {
		err = errRoleAudience
	}
	if err == nil && !r.bot.backgroundQuota(r.billingUser(), id, r.store.now()) {
		err = errEngagementQuota
	}
	if err == nil {
		var allowed bool
		allowed, err = r.notificationAllowed(item, id)
		if err == nil && allowed {
			state, receipt, _ = r.snap.sendEngagementText(ctx, id, text)
		}
	}
	_, saveErr := r.store.archiveOutcomes(attempts, r.store.now(), state, receipt)
	return errors.Join(err, saveErr)
}

func (r *engagementRun) reportPause(ctx context.Context) {
	// The pause itself is durable. At most one worker receives the transition;
	// an ambiguous notification is never replayed on the wrong audience.
	item, err := r.store.Get(r.claim.ID)
	if err != nil || item.Status != EngagementPaused || item.Ref.Version != r.item.Ref.Version+1 {
		return
	}
	for _, id := range item.Current.Recipients {
		if !r.bot.backgroundQuota(r.billingUser(), id, r.store.now()) {
			continue
		}
		chat, err := r.recipient(ctx, item, id)
		if err != nil {
			continue
		}
		live, err := r.store.Get(r.claim.ID)
		if err != nil || live.Ref != item.Ref || live.Status != EngagementPaused {
			return
		}
		allowed, err := r.notificationAllowed(live, id)
		if err != nil || !allowed || ctx.Err() != nil {
			continue
		}
		attempt, err := r.store.beginPauseNotice(item.Ref, r.store.now(), id)
		if err != nil {
			continue
		}
		liveSnap := r.bot.snapshot()
		live, err = r.store.Get(r.claim.ID)
		state, receipt := EngagementNoEffect, "Telegram request was never sent"
		if err == nil && live.Ref == item.Ref && (live.Current.ExpiresAt.IsZero() || live.Current.ExpiresAt.After(r.store.now())) &&
			ctx.Err() == nil && liveSnap.tg == r.snap.tg && engagementChatAllowed(liveSnap.cfg, chat, item.Origin.SenderID) {
			if allowed, err := r.notificationAllowed(live, id); err == nil && allowed {
				text := fmt.Sprintf(r.localized("Tracking %s is paused. I couldn't verify access required by the agreement.",
					"跟踪 %s 已暂停。无法核验这项约定所需的访问权限。"), item.Ref.ID)
				state, receipt, _ = r.snap.sendEngagementText(ctx, chat.ID, text)
			}
		}
		if err := r.store.archivePauseNotice(item.Ref.ID, attempt, r.store.now(), state, receipt); err != nil {
			log.Printf("[engagement] could not archive pause notice for %s", item.Ref.ID)
		}
	}
}
