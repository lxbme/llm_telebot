package app

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	tele "gopkg.in/telebot.v3"
)

var (
	errResumeScope          = errors.New("resume requires a revised or clarified scope")
	errEngagementConnection = errors.New("could not verify background connection")
)

var (
	sharedInitiatorOnly    = regexp.MustCompile(`(?i)^(?:only (?:i|the initiator) (?:can |may )?(?:edit|change|manage|stop|end|pause)s?(?: this (?:task|agreement|tracking))?|(?:只有|仅)(?:我|发起人|发起者)(?:才)?(?:能|可以|可)(?:改|修改|管理|暂停|结束|停止)(?:这项(?:任务|委托|跟踪))?)[.。!！]?$`)
	sharedOpenManagement   = regexp.MustCompile(`(?i)^(?:everyone (?:can|may) (?:manage|edit)(?: this (?:task|agreement|tracking))?|restore shared management|(?:全群|大家)都可以(?:修改|管理)|恢复共同管理)[.。!！]?$`)
	negatedConsent         = regexp.MustCompile(`(?i)\b(?:not|never|don't|no longer|whether|example|hypothetical)\b|不要|不用|无需|别加|不授权|举例|假设|如果我说`)
	expansionConsent       = regexp.MustCompile(`(?i)^(?:please )?(?:expand|extend|reaccept|re-accept|add to)\b|^(?:请|帮我)?(?:扩约|扩大委托|重新承接|追加到这项委托|给(?:这项|这个)(?:任务|委托|跟踪)(?:也|再)?(?:加|追加))`)
	onlyInitiator          = regexp.MustCompile(`(?i)^(?:only i (?:can|may) (?:edit|change|manage|stop|end|pause)|only i should be able to (?:edit|change|manage|stop|end|pause)|(?:(?:这个|这项)(?:跟踪|委托|任务))?只有我(?:能|可以)(?:改|修改|管理|暂停|结束|停止)|仅我(?:能|可)(?:改|修改|管理|暂停|结束|停止))`)
	openManagement         = regexp.MustCompile(`(?i)^(?:everyone (?:can|may) (?:manage|edit)|restore shared management|(?:全群|大家)都可以(?:修改|管理)|恢复共同管理)`)
	managerApprovalEnglish = regexp.MustCompile(`(?i)^i (?:authorize|allow) (?:telegram )?(?:user )?([1-9][0-9]*) to (pause|resume|mute|unmute|end|edit|manage) this (?:task|agreement)( this week)?[.!]?$`)
	managerApprovalChinese = regexp.MustCompile(`^(?:我)?(?:允许|授权)(?:用户)?[ ]*([1-9][0-9]*)[ ]*(本周|这周)?[ ]*(暂停|恢复执行|静音|恢复通知|结束|修改|管理)(?:这项|此项|本项)(?:委托|任务|跟踪)[。！]?$`)
	managementShare        = regexp.MustCompile(`(?i)^(?:share (?:this management (?:term|summary) )?with group|把这项任务的管理结论分享给群)\s*(-[0-9]+)\s*[:：]\s*(.+)$`)
	termConsent            = regexp.MustCompile(`(?i)^(?:set (trigger|end condition) for this task(?: in (?:this chat|my private chat))?|在(?:本会话|我的私聊)(?:设置|替换)这项委托的(触发条件|结束条件))\s*[:：]\s*(.+)$`)
	unchangedResume        = regexp.MustCompile(`(?i)^(?:` +
		`(?:please )?(?:resume|restart)(?: (?:it|(?:this |the )?(?:tracking|task|agreement|watch))(?: as before| as agreed| unchanged| under the original agreement)?| as before| as agreed| under the original agreement)(?:,? please)?` +
		`|(?:请|請|帮我|幫我)?(?:按原[约約]定?)?(?:恢[复復]|[继繼][续續])(?:[这這该該此本][项項个個]?)?(?:跟[踪蹤]|追[踪蹤]|任[务務]|委[托託]|[执執]行)` +
		`|[请請]?按原[约約]定?恢[复復]` +
		`)[.。!！]?$`)
)

func dialogueQuote(text, quote string) string {
	quote = strings.TrimSpace(quote)
	if quote == "" || len([]rune(quote)) > 450 || !strings.Contains(text, quote) {
		return ""
	}
	return quote
}

func dialogueStatements(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		return r == '\n' || strings.ContainsRune(".。!！;；,，", r)
	})
}

func dialogueSomeStatement(text string, pattern *regexp.Regexp) string {
	if negatedConsent.MatchString(text) || strings.ContainsAny(text, "\"“”「」`") {
		return ""
	}
	for _, clause := range dialogueStatements(text) {
		if clause = strings.TrimSpace(clause); pattern.MatchString(clause) {
			return clause
		}
	}
	return ""
}

func dialogueLocation(cfg Config) *time.Location {
	location, err := time.LoadLocation(cfg.ReminderDefaultTimezone)
	if err != nil || cfg.ReminderDefaultTimezone == "" {
		return time.UTC
	}
	return location
}

func dialogueWeekEnd(at time.Time, cfg Config) time.Time {
	at = at.In(dialogueLocation(cfg))
	start := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, at.Location())
	return start.AddDate(0, 0, 7-(int(at.Weekday())+6)%7)
}

func dialogueUntil(duration string, sent, now time.Time, cfg Config) (time.Time, error) {
	var until time.Time
	switch duration {
	case "", "ongoing":
		return until, nil
	case "this_week":
		if !sent.IsZero() {
			until = dialogueWeekEnd(sent, cfg)
		}
	default:
		until, _ = time.Parse(time.RFC3339, duration)
	}
	if !until.After(now) {
		return time.Time{}, ErrEngagementInvalid
	}
	return until, nil
}

func receiptUntil(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return " (" + at.Format(time.RFC3339) + ")"
}

func managerConsent(text string) (actor int64, change EngagementChange, thisWeek bool) {
	var user, op, week string
	if m := managerApprovalEnglish.FindStringSubmatch(strings.TrimSpace(text)); m != nil {
		user, op, week = m[1], m[2], m[3]
	} else if m := managerApprovalChinese.FindStringSubmatch(strings.TrimSpace(text)); m != nil {
		user, op, week = m[1], m[3], m[2]
	}
	switch strings.ToLower(op) {
	case "暂停", "pause":
		change = EngagementPause
	case "恢复执行", "resume":
		change = EngagementResume
	case "静音", "mute":
		change = EngagementMute
	case "恢复通知", "unmute":
		change = EngagementUnmute
	case "结束", "end":
		change = EngagementEnd
	case "修改", "edit":
		change = EngagementAgree
	case "管理", "manage":
		change = EngagementManage
	}
	actor, _ = strconv.ParseInt(user, 10, 64)
	return actor, change, week != ""
}

func (d *engagementDialogue) managementBasis(item Engagement, p engagementProposal, decision *EngagementDecision) *MemorySource {
	m := item.Management
	if m == nil || p.Basis <= 0 || m.Source.MessageID != p.Basis ||
		m.Source.SenderID != item.Origin.SenderID || m.Source.ChatID != d.turn.ChatID ||
		m.Source.Access != (MemoryDataAccess{}) {
		return nil
	}
	actor, change, week := managerConsent(m.Summary)
	if actor != d.turn.SenderID || change != p.Change {
		return nil
	}
	for i := len(item.Decisions) - 1; i >= 0; i-- {
		r := item.Decisions[i].Request
		if r.Source != m.Source || dialogueQuote(r.Text, m.Summary) == "" {
			continue
		}
		if week && (r.SentAt.IsZero() || decision.At.Before(r.SentAt) ||
			!decision.At.Before(dialogueWeekEnd(r.SentAt, d.turn.bot.currentConfig()))) {
			return nil
		}
		return &m.Source
	}
	return nil
}

func (d *engagementDialogue) mayAccept(item Engagement, p engagementProposal, managementOutsideAudience bool, decision *EngagementDecision) bool {
	if item.Origin.SenderID == d.turn.SenderID {
		return true
	}
	if p.Basis != 0 {
		basis := d.managementBasis(item, p, decision)
		if basis == nil {
			return false
		}
		decision.Authorized.ManagementBasis = basis
		return true
	}
	// A private restriction can require confirmation without disclosing
	// the private material. Visible social terms are inputs to the role's
	// decision. Physical audience and account limits still apply.
	if item.InitiatorOnly && managementOutsideAudience {
		return false
	}
	a := item.Current
	return a != nil && d.turn.ChatID < 0 && item.Origin.ChatID == d.turn.ChatID &&
		len(a.Recipients) == 1 && a.Recipients[0] == d.turn.ChatID &&
		!slices.ContainsFunc(a.Actions, func(c EngagementCapability) bool { return c.Write || c.Access.OwnerID != 0 }) &&
		p.Change != EngagementManage && p.Change != EngagementReaccept
}

func (d *engagementDialogue) prepareManagement(ctx context.Context, snap runtimeSnapshot, item Engagement, p engagementProposal, decision *EngagementDecision) error {
	if p.Change != EngagementAgree && p.Change != EngagementReaccept && p.Change != EngagementManage {
		if p.Management != "" {
			return ErrEngagementInvalid
		}
		return nil
	}
	text, only := d.message.text, item.InitiatorOnly
	quote := dialogueQuote(text, p.Management)
	switch {
	case quote == "" && p.Management != "":
		return ErrEngagementDenied
	case p.Change == EngagementManage && quote == "":
		return ErrEngagementDenied
	case p.ID == "" && quote == "":
		quote = dialogueSomeStatement(text, onlyInitiator)
	}
	if quote == "" {
		return nil
	}
	if p.ID != "" && quote != strings.TrimSpace(text) {
		return ErrEngagementDenied
	}
	if clause := dialogueSomeStatement(quote, onlyInitiator); clause != "" {
		only = true
	} else if clause := dialogueSomeStatement(quote, openManagement); clause != "" {
		only = false
	} else if actor, _, _ := managerConsent(quote); actor != 0 {
		if p.ID != "" && item.Origin.SenderID != d.turn.SenderID {
			return ErrEngagementDenied
		}
	} else if parts := managementShare.FindStringSubmatch(quote); p.Change == EngagementManage && len(parts) == 3 {
		group, _ := strconv.ParseInt(parts[1], 10, 64)
		if d.turn.ChatID != d.turn.SenderID || item.Origin.SenderID != d.turn.SenderID ||
			!item.visibleTo(EngagementViewer{ChatID: group}) || d.verifyGroup(ctx, snap, group) != nil {
			return ErrEngagementDenied
		}
		quote = strings.TrimSpace(parts[2])
		switch {
		case sharedInitiatorOnly.MatchString(quote):
			only = true
		case sharedOpenManagement.MatchString(quote):
			only = false
		default:
			return ErrEngagementDenied
		}
		decision.Authorized.ManagementShare = &EngagementManagementShare{ChatID: group, Summary: quote}
	} else {
		return ErrEngagementDenied
	}
	decision.Management = &EngagementManagementEdit{Summary: quote, InitiatorOnly: only}
	return nil
}

func (d *engagementDialogue) verifyGroup(ctx context.Context, snap runtimeSnapshot, id int64) error {
	chat, err := snap.tg.ChatByID(id)
	user := &tele.User{ID: d.turn.SenderID}
	if err != nil || !telegramGroup(chat) || chat.ID != id || !snap.cfg.allowsChat(chat, nil) ||
		!verifyTelegramGroupMember(snap.tg, chat, user) || ctx.Err() != nil {
		return ErrEngagementDenied
	}
	return nil
}

func (d *engagementDialogue) recipients(item Engagement, requested, stated string) ([]int64, error) {
	if stated != "" {
		if stated != "here" && stated != "private" {
			return nil, ErrEngagementDenied
		}
		// Here and private identify the same audience in the owner's DM.
		if requested != "" && requested != stated && d.turn.ChatID != d.turn.SenderID {
			return nil, ErrEngagementDenied
		}
		requested = stated
	}
	switch requested {
	case "":
		if item.Current != nil {
			return slices.Clone(item.Current.Recipients), nil
		}
		return []int64{d.turn.ChatID}, nil
	case "here":
		if item.Current != nil && !slices.Contains(item.Current.Recipients, d.turn.ChatID) && stated != "here" {
			return nil, ErrEngagementDenied
		}
		return []int64{d.turn.ChatID}, nil
	case "private":
		if d.turn.ChatID == d.turn.SenderID || stated == "private" {
			return []int64{d.turn.SenderID}, nil
		}
	}
	return nil, ErrEngagementDenied
}

func (d *engagementDialogue) recipientAllowed(cfg Config, id int64, item Engagement) bool {
	if id > 0 {
		return id == d.turn.SenderID && cfg.allowsChat(&tele.Chat{ID: id, Type: tele.ChatPrivate}, &tele.User{ID: id})
	}
	if id == d.turn.ChatID {
		return cfg.allowsChat(&tele.Chat{ID: id, Type: d.turn.chatType}, nil)
	}
	// Whitelist rules are the same for both group types. verifyGroup
	// binds the live Telegram chat and membership before this local check.
	return id < 0 && item.Origin.SenderID == d.turn.SenderID &&
		cfg.allowsChat(&tele.Chat{ID: id, Type: tele.ChatSuperGroup}, nil)
}

func (d *engagementDialogue) verifyRecipients(ctx context.Context, snap runtimeSnapshot, item Engagement, recipients []int64) error {
	for _, id := range recipients {
		if id == d.turn.ChatID {
			if !snap.cfg.allowsChat(&tele.Chat{ID: id, Type: d.turn.chatType}, nil) && id < 0 {
				return ErrEngagementDenied
			}
			continue
		}
		if id == d.turn.SenderID {
			chat, err := snap.tg.ChatByID(id)
			if err != nil || chat == nil || chat.Type != tele.ChatPrivate || chat.ID != id ||
				!snap.cfg.allowsChat(chat, &tele.User{ID: id}) {
				return ErrEngagementDenied
			}
			continue
		}
		// Retaining a previously approved group from the initiator's
		// private chat requires a live group and membership lookup.
		if id >= 0 || item.Current == nil || !slices.Contains(item.Current.Recipients, id) ||
			item.Origin.SenderID != d.turn.SenderID || d.verifyGroup(ctx, snap, id) != nil {
			return ErrEngagementDenied
		}
	}
	return ctx.Err()
}

type verifiedEngagementScope struct {
	initiator int64
	tools     map[EngagementCapability]*MCPRemoteTool
	chats     map[int64]*tele.Chat
}

func (d *engagementDialogue) prepare(ctx context.Context, snap runtimeSnapshot, viewer EngagementViewer, item Engagement, p engagementProposal, decision *EngagementDecision, verified *verifiedEngagementScope) ([]int64, error) {
	if err := d.prepareManagement(ctx, snap, item, p, decision); err != nil {
		return nil, err
	}
	var manage []int64
	if d.turn.ChatID < 0 && p.ID == "" {
		// Only the authenticated sender can enter this private viewer.
		manage = []int64{d.turn.SenderID}
	}
	if p.Change == EngagementMute {
		until, err := dialogueUntil(p.Duration, d.message.sentAt, decision.At, snap.cfg)
		decision.MuteUntil = until
		return manage, err
	}
	a := EngagementAgreement{Once: true, NextCheck: decision.At}
	if item.Current != nil {
		a = *item.Current
	}
	switch p.Change {
	case EngagementAgree, EngagementReaccept:
		options := snap.engagementCapabilities(viewer.ActorID, viewer.ChatID)
		intent, err := d.currentIntent(ctx, snap)
		if err != nil {
			return nil, ErrEngagementDenied
		}
		route := parseEngagementRoute(decision.Request.Text)
		requested, stated := p.Recipient, intent.Recipient
		if destination := route.actualRecipient(decision.Request.Source); destination != "" {
			requested, stated = destination, destination
		}
		a.Recipients, err = d.recipients(item, requested, stated)
		if err != nil {
			return nil, errEngagementRoute
		}
		decision.Authorized.RecipientSources, err = d.bindRecipients(item, *decision, a.Recipients)
		if err != nil {
			return nil, errEngagementRoute
		}
		if err := d.prepareAgreement(options, snap, item, p, decision, &a, intent); err != nil {
			return nil, err
		}
		decision.Agreement = &a
	case EngagementResume:
		if err := d.verifyResumeScope(item, p); err != nil {
			return nil, err
		}
	default:
		return manage, nil
	}
	verified.initiator = item.Origin.SenderID
	if item.Current == nil {
		verified.initiator = decision.Request.Source.SenderID
	}
	for _, c := range append([]EngagementCapability{a.Observe}, a.Actions...) {
		if c.Write && !a.Once {
			return nil, ErrEngagementDenied
		}
		if verified.chats[c.DataChatID] == nil {
			chat, err := snap.engagementChat(ctx, c.DataChatID, verified.initiator)
			if err != nil {
				return nil, errRoleAudience
			}
			verified.chats[c.DataChatID] = chat
		}
		if c.Operation == engagementPrivateFollowup {
			decision.Authorized.Capabilities = append(decision.Authorized.Capabilities, c)
			continue
		}
		binding, err := snap.resolveEngagementCapability(c, verified.initiator)
		if err != nil {
			return nil, err
		}
		if err = binding.verify(ctx); err != nil {
			return nil, fmt.Errorf("%w: %w", errEngagementConnection, err)
		}
		verified.tools[c] = binding.tool
		decision.Authorized.Capabilities = append(decision.Authorized.Capabilities, c)
	}
	if err := d.verifyRecipients(ctx, snap, item, a.Recipients); err != nil {
		return nil, err
	}
	decision.Authorized.Recipients = slices.Clone(a.Recipients)
	return manage, nil
}

// Resume can re-enable only the exact agreement fenced by the item's CAS.
// Require the entire authenticated message to authorize a plain resume.
// Omitted limits in either model's interpretation cannot authorize it.
func (d *engagementDialogue) verifyResumeScope(item Engagement, p engagementProposal) error {
	if item.Current == nil || item.Status != EngagementPaused {
		return ErrEngagementDenied
	}
	if _, err := item.recipientSources(); err != nil {
		return errResumeScope
	}
	recipients, err := d.recipients(item, p.Recipient, "")
	if err != nil || !slices.Equal(recipients, item.Current.Recipients) {
		return errResumeScope
	}
	if !unchangedResume.MatchString(strings.Join(strings.Fields(d.message.text), " ")) ||
		p.Observe != "" || p.Actions != nil || p.Trigger != nil ||
		p.EndCondition != nil || p.Once != nil || p.Duration != "" {
		return errResumeScope
	}
	return nil
}

func (d *engagementDialogue) prepareAgreement(options map[string]EngagementCapability, snap runtimeSnapshot, item Engagement, p engagementProposal, decision *EngagementDecision, a *EngagementAgreement, intent engagementIntent) error {
	if p.Observe != "" {
		c, ok := options[p.Observe]
		if !ok || c.Write {
			return ErrEngagementDenied
		}
		a.Observe = c
	}
	quote := dialogueQuote(d.message.text, p.Authorization)
	explicitExpansion := quote != "" && !negatedConsent.MatchString(d.message.text) && expansionConsent.MatchString(d.message.text)
	if p.Change == EngagementReaccept {
		if !explicitExpansion || item.Origin.SenderID != d.turn.SenderID {
			return ErrEngagementDenied
		}
		decision.Request.ExplicitReaccept = true
	}
	if p.Once != nil {
		a.Once = *p.Once
	}
	if p.Duration != "" {
		until, err := dialogueUntil(p.Duration, d.message.sentAt, decision.At, snap.cfg)
		if err != nil {
			return err
		}
		a.ExpiresAt = until
	}
	if p.Actions != nil {
		a.Actions = nil
		for _, id := range *p.Actions {
			c, ok := options[id]
			if !ok {
				return ErrEngagementDenied
			}
			a.Actions = append(a.Actions, c)
		}
	}
	if p.Trigger != nil {
		if err := d.bindTerm(item, decision, EngagementTriggerTerm, *p.Trigger, &a.Trigger); err != nil {
			return err
		}
	}
	if p.EndCondition != nil {
		if err := d.bindTerm(item, decision, EngagementEndTerm, *p.EndCondition, &a.EndCondition); err != nil {
			return err
		}
	}
	prior := item.Current
	changedWriteScope := prior == nil || a.Observe != prior.Observe || a.Trigger != prior.Trigger ||
		a.EndCondition != prior.EndCondition || a.Once != prior.Once || !a.ExpiresAt.Equal(prior.ExpiresAt) ||
		!slices.Equal(a.Recipients, prior.Recipients)
	var actions []EngagementCapability
	hasWrite := false
	for _, c := range a.Actions {
		if c.Write {
			if intent.RemoveCalendar {
				if p.Actions != nil {
					return ErrEngagementDenied
				}
				continue
			}
			if !a.Once || changedWriteScope || !slices.Contains(prior.Actions, c) {
				if !a.Once || !d.permitsCalendar(options, c, p.Kind, quote, intent, a.Trigger, prior) {
					if p.Actions != nil {
						return ErrEngagementDenied
					}
					// An old action does not come along when its scope is changed.
					continue
				}
			}
			if hasWrite {
				return ErrEngagementDenied
			}
			hasWrite = true
		}
		actions = append(actions, c)
	}
	a.Actions = actions
	if a.Observe.Operation == engagementPrivateFollowup {
		source := decision.Request.Source
		if !a.permitsPrivateFollowup(p.Kind, source) {
			return ErrEngagementDenied
		}
		if item.Current == nil || item.Current.Observe != a.Observe || p.CheckAfter != "" {
			earliest := decision.At.Add(privateFollowupDelay)
			if p.CheckAfter != "" {
				at, err := time.Parse(time.RFC3339, p.CheckAfter)
				if err != nil || at.Before(decision.At.Add(24*time.Hour)) || at.After(decision.At.Add(365*24*time.Hour)) {
					return ErrEngagementInvalid
				}
				earliest = at
			}
			a.NextCheck = earliest
		}
		if a.ExpiresAt.IsZero() {
			a.ExpiresAt = a.NextCheck.Add(30 * 24 * time.Hour)
		}
	} else if p.CheckAfter != "" {
		return ErrEngagementInvalid
	}
	return nil
}

func (d *engagementDialogue) permitsCalendar(options map[string]EngagementCapability, c EngagementCapability, kind EngagementKind, quote string, intent engagementIntent, trigger EngagementText, prior *EngagementAgreement) bool {
	if kind != EngagementDelegation || c.Access.OwnerID != d.turn.SenderID || d.turn.ChatID != d.turn.SenderID ||
		intent.Calendar == nil || !intent.Calendar.Once || quote == "" || dialogueQuote(d.message.text, intent.Calendar.Quote) == "" ||
		!strings.Contains(intent.Calendar.Quote, quote) || strings.ContainsAny(intent.Calendar.Quote, "\"“”「」`") ||
		negatedConsent.MatchString(intent.Calendar.Quote) {
		return false
	}
	if condition := strings.TrimSpace(intent.Calendar.Trigger); condition != "" {
		if trigger.ChatID != d.turn.ChatID || trigger.Text != dialogueQuote(d.message.text, condition) {
			return false
		}
	} else if prior == nil || trigger != prior.Trigger {
		return false
	}
	if target := strings.TrimSpace(intent.Calendar.Target); target != "" {
		return strings.EqualFold(target, c.Target) && strings.Contains(strings.ToLower(intent.Calendar.Quote), strings.ToLower(target))
	}
	count := 0
	for _, option := range options {
		if option.Write {
			count++
		}
	}
	return count == 1
}

func (d *engagementDialogue) bindTerm(item Engagement, decision *EngagementDecision, key EngagementTerm, text string, existing *EngagementText) error {
	text = strings.TrimSpace(text)
	if text == existing.Text {
		return nil
	}
	value := EngagementText{}
	if text != "" {
		if dialogueQuote(d.message.text, text) == "" {
			return ErrEngagementDenied
		}
		value = EngagementText{ChatID: d.turn.ChatID, Text: text}
	}
	if existing.Text != "" && existing.ChatID != d.turn.ChatID {
		// Cross-audience replacement is a new, explicitly authorized
		// statement from the initiator. It cannot disclose the old term.
		if item.Origin.SenderID != d.turn.SenderID || d.turn.ChatID != d.turn.SenderID {
			return ErrEngagementDenied
		}
		parts := termConsent.FindStringSubmatch(d.message.text)
		if len(parts) != 4 || strings.TrimSpace(parts[3]) != text ||
			(key == EngagementTriggerTerm && parts[1] != "trigger" && parts[2] != "触发条件") ||
			(key == EngagementEndTerm && parts[1] != "end condition" && parts[2] != "结束条件") {
			return ErrEngagementDenied
		}
		decision.Authorized.TermEdits = append(decision.Authorized.TermEdits, EngagementTermGrant{Term: key, Value: value, Source: d.turn.Source})
	}
	*existing = value
	return nil
}
