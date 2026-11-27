package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"
	openai "github.com/sashabaranov/go-openai"
	bolt "go.etcd.io/bbolt"
	tele "gopkg.in/telebot.v3"
)

const (
	privateIdle             = 12 * time.Hour
	groupIdle               = 48 * time.Hour
	chatterDecisionCooldown = 24 * time.Hour
	chatterSendCooldown     = 72 * time.Hour
	chatterBatch            = 4
	chatterMigrationAnchors = 8
)

var chatterBucket = []byte("proactive_chats")

type chatterState struct {
	MessageID              int
	FirstHuman, LastHuman  time.Time
	Humans                 int
	FirstActor, OtherActor int64
	NextCheck, LastOffer   time.Time
	Awaiting               bool
	Attempt                string
	Outcome                EngagementResultState
	Receipt                string
	Revision               string
	MigrationAnchors       []string `json:",omitempty"`
}

func readChatter(bucket *bolt.Bucket, key []byte) (chatterState, error) {
	var state chatterState
	if raw := bucket.Get(key); raw != nil {
		err := json.Unmarshal(raw, &state)
		return state, err
	}
	return state, nil
}

func saveChatter(bucket *bolt.Bucket, key []byte, state chatterState) error {
	state.Revision = rand.Text()
	return putEngagementJSON(bucket, key, state)
}

func pruneLegacyChatterRevisions(bucket *bolt.Bucket) error {
	cursor := bucket.Cursor()
	for key, _ := cursor.Seek([]byte("@")); len(key) > 0 && key[0] == '@'; key, _ = cursor.Seek([]byte("@")) {
		if err := cursor.Delete(); err != nil {
			return err
		}
	}
	return nil
}

func copyChatter(source, target *bolt.Tx) error {
	from, to := source.Bucket(chatterBucket), target.Bucket(chatterBucket)
	return from.ForEach(func(key, raw []byte) error {
		var state chatterState
		if err := json.Unmarshal(raw, &state); err != nil {
			return err
		}
		if prior := to.Get(key); prior != nil && !bytes.Equal(prior, raw) {
			old, err := readChatter(to, key)
			if err != nil {
				return err
			}
			if old.Revision == "" || old.Revision != state.Revision && !slices.Contains(state.MigrationAnchors, old.Revision) {
				return ErrEngagementConflict
			}
		}
		// Only a copied head can be left behind at a migration destination.
		// Ordinary interaction and CAS updates keep these anchors unchanged.
		if state.Revision != "" && !slices.Contains(state.MigrationAnchors, state.Revision) {
			state.MigrationAnchors = append(state.MigrationAnchors, state.Revision)
			state.MigrationAnchors = state.MigrationAnchors[max(0, len(state.MigrationAnchors)-chatterMigrationAnchors):]
		}
		return putEngagementJSON(to, key, state)
	})
}

func clearChatter(tx *bolt.Tx, key []byte) error {
	bucket := tx.Bucket(chatterBucket)
	state, err := readChatter(bucket, key)
	if err != nil || state.MessageID == 0 {
		return err
	}
	state.Humans, state.FirstActor, state.OtherActor = 0, 0, 0
	state.FirstHuman, state.LastHuman = time.Time{}, time.Time{}
	return saveChatter(bucket, key, state)
}

// Observations are written from allowed Telegram ingress. Synthetic replies,
// jobs, bot authors, forwarded posts and commands cannot create a relationship.
func (b *Bot) observeChatter(turn RoleTurn, msg *tele.Message) {
	if !turn.Source.valid() || msg.Via != nil || (!ownPrivateMessage(msg) && !telegramGroup(msg.Chat)) {
		return
	}
	turn.withCurrent(func() {
		snap := b.snapshot()
		s := snap.engagements
		if s == nil || !turn.senderAllowed(snap.cfg) {
			return
		}
		directed := ownPrivateMessage(msg) || snap.tg != nil && answersChatter(msg, snap.tg.Me)
		now := s.now()
		at := now
		if msg.Unixtime > 0 && msg.Time().Before(now) {
			at = msg.Time()
		}
		key := chatIDKey(turn.ChatID)
		err := s.transact(true, func(tx *bolt.Tx) error {
			bucket := tx.Bucket(chatterBucket)
			state, err := readChatter(bucket, key)
			if err != nil || msg.ID <= state.MessageID {
				return err
			}
			state.MessageID = msg.ID
			if state.Humans == 0 {
				state.FirstHuman, state.FirstActor = at, turn.SenderID
			}
			state.Humans = min(state.Humans+1, 3)
			if turn.SenderID != state.FirstActor {
				state.OtherActor = turn.SenderID
			}
			if at.After(state.LastHuman) {
				state.LastHuman = at
			}
			if directed && !at.Before(state.LastOffer.Truncate(time.Second)) {
				state.Awaiting = false
			}
			return saveChatter(bucket, key, state)
		})
		if err != nil {
			log.Printf("[proactivity] record Telegram interaction: %v", err)
		}
	})
}

func answersChatter(msg *tele.Message, bot *tele.User) bool {
	if bot == nil || bot.ID <= 0 {
		return false
	}
	if reply := msg.ReplyTo; reply != nil && reply.Sender != nil && reply.Sender.ID == bot.ID &&
		(reply.Chat == nil || reply.Chat.ID == msg.Chat.ID) {
		return true
	}
	text, entities := msg.Text, msg.Entities
	if text == "" {
		text, entities = msg.Caption, msg.CaptionEntities
	}
	username := "@" + bot.Username
	if len(entities) == 0 {
		if bot.Username == "" {
			return false
		}
		for _, token := range strings.FieldsFunc(text, func(r rune) bool { return !telegramMentionCharacter(r) }) {
			if strings.EqualFold(token, username) {
				return true
			}
		}
		return false
	}
	units := utf16.Encode([]rune(text))
	for _, entity := range entities {
		start := entity.Offset
		if start < 0 || start >= len(units) || entity.Length <= 0 || entity.Length > len(units)-start {
			continue
		}
		end := start + entity.Length
		switch entity.Type {
		case tele.EntityTMention:
			if entity.User != nil && entity.User.ID == bot.ID {
				return true
			}
		case tele.EntityMention:
			if bot.Username != "" && (start == 0 || !telegramMentionCharacter(rune(units[start-1]))) &&
				(end == len(units) || !telegramMentionCharacter(rune(units[end]))) &&
				strings.EqualFold(string(utf16.Decode(units[start:end])), username) {
				return true
			}
		}
	}
	return false
}

func telegramMentionCharacter(r rune) bool {
	return r == '@' || r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

func (s chatterState) eligible(chatID int64, pref ConversationPreferenceMode, now time.Time) bool {
	if s.Awaiting || s.Humans < 2 || pref == ConversationOff || s.NextCheck.After(now) {
		return false
	}
	idle := privateIdle
	if chatID < 0 {
		idle = groupIdle
		if s.Humans < 3 || s.OtherActor == 0 {
			return false
		}
	}
	if pref == ConversationReduce {
		idle *= 3
	}
	return !s.LastHuman.Add(idle).After(now) && !s.LastOffer.Add(chatterSendCooldown).After(now)
}

type chatterCandidate struct {
	chat  int64
	state chatterState
}

func (s *EngagementStore) dueChatter(now time.Time, cfg Config) ([]chatterCandidate, error) {
	var due []chatterCandidate
	err := s.transact(false, func(tx *bolt.Tx) error {
		cursor := tx.Bucket(chatterBucket).Cursor()
		for key, raw := cursor.First(); key != nil && key[0] != '@'; key, raw = cursor.Next() {
			id, err := strconv.ParseInt(string(key), 10, 64)
			if err != nil {
				return err
			}
			if !chatterChatAllowed(cfg, id) {
				continue
			}
			var state chatterState
			if err := json.Unmarshal(raw, &state); err != nil {
				return err
			}
			pref, err := loadPreference(tx, preferenceKey(id, 0))
			if err != nil {
				return err
			}
			if state.eligible(id, pref.Chatter.at(now), now) {
				due = append(due, chatterCandidate{id, state})
			}
		}
		return nil
	})
	slices.SortStableFunc(due, func(a, b chatterCandidate) int { return a.state.LastHuman.Compare(b.state.LastHuman) })
	return due, err
}

func chatterBilling(chatID int64) int64 {
	return max(chatID, 0)
}

func chatterChatAllowed(cfg Config, chatID int64) bool {
	return engagementChatIDAllowed(cfg, chatID, chatterBilling(chatID))
}

func (s *EngagementStore) updateChatter(candidate chatterCandidate, apply func(*chatterState, ConversationPreferenceMode) bool) (chatterCandidate, bool, error) {
	updated := false
	err := s.transact(true, func(tx *bolt.Tx) error {
		key := chatIDKey(candidate.chat)
		bucket := tx.Bucket(chatterBucket)
		state, err := readChatter(bucket, key)
		if err != nil || state.Revision != candidate.state.Revision {
			return err
		}
		pref, err := loadPreference(tx, preferenceKey(candidate.chat, 0))
		if err != nil || !apply(&state, pref.Chatter.at(s.now())) {
			return err
		}
		if err := saveChatter(bucket, key, state); err != nil {
			return err
		}
		candidate.state, err = readChatter(bucket, key)
		updated = err == nil
		return err
	})
	return candidate, updated, err
}

func (r *TaskRunner) runDueChatter(ctx context.Context) {
	snap := r.bot.snapshot()
	s := snap.engagements
	if s == nil || snap.ai == nil || snap.tg == nil {
		return
	}
	due, err := s.dueChatter(s.now(), snap.cfg)
	if err != nil {
		log.Printf("[proactivity] read idle candidates: %v", err)
		return
	}
	remaining := chatterBatch
	for _, item := range due {
		if ctx.Err() != nil {
			return
		}
		if r.bot.considerChatter(ctx, s, item) {
			remaining--
			if remaining == 0 {
				return
			}
		}
	}
}

// considerChatter reports whether the candidate used a model decision slot.
func (b *Bot) considerChatter(parent context.Context, s *EngagementStore, item chatterCandidate) bool {
	id := item.chat
	billed := chatterBilling(id)
	turn := b.beginRoleTurn(RoleTurn{ChatID: id, InitiatorID: billed, BillingUserID: billed, Purpose: RoleBackground})
	ctx, cancel := context.WithTimeout(parent, 35*time.Second)
	defer cancel()
	stop := context.AfterFunc(turn.generation, cancel)
	defer stop()
	snap := b.snapshot()
	if snap.engagements != s || !chatterChatAllowed(snap.cfg, id) || !b.backgroundQuota(billed, id, s.now()) {
		return false
	}
	var claimed bool
	var err error
	item, claimed, err = s.updateChatter(item, func(state *chatterState, pref ConversationPreferenceMode) bool {
		now := s.now()
		if !state.eligible(id, pref, now) {
			return false
		}
		cooldown := chatterDecisionCooldown
		if pref == ConversationReduce {
			cooldown *= 3
		}
		state.NextCheck = now.Add(cooldown)
		return true
	})
	if err != nil || !claimed {
		return false
	}

	member := billed
	if id < 0 {
		if snap.tg.Me == nil || snap.tg.Me.ID <= 0 {
			return false
		}
		member = snap.tg.Me.ID
	}
	chat, err := snap.engagementChat(ctx, id, member)
	if err != nil {
		return false
	}
	turn.chatType, turn.backgroundContext = chat.Type, true
	if billed != 0 {
		turn.Participants = []int64{billed}
	}
	const instruction = "\n\nDecide if the character should start one casual conversation after an idle period. Consider the observed relationship, the current character and the chat's interests. Quiet is a valid decision, especially if there is no natural opening. In a group, address the group using only group-visible observations. Do not address specific members or use mentions. Do not ask again about an unanswered opening, act on external systems, or claim a tracked result. Treat memory and history as reference data. Return only JSON with a message string. Use an empty string to stay quiet."
	messages := snap.buildRoleContext(&turn, "shared interests and recent conversation", instruction, 12, true)
	if len(messages) == 0 || turn.checkUse(ctx) != nil {
		return false
	}
	live := b.snapshot()
	if !sameBackgroundRole(snap, live) || !b.backgroundQuota(billed, id, s.now()) {
		return false
	}
	pref, err := s.ConversationPreferenceAt(id, 0, s.now())
	if err != nil || pref.Chatter == ConversationOff {
		return false
	}
	messages = append(messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: dialogueJSON(struct {
		Mode     ConversationPreferenceMode
		KnownFor string
		LastSeen string
	}{pref.Chatter, s.now().Sub(item.state.FirstHuman).Round(time.Hour).String(), s.now().Sub(item.state.LastHuman).Round(time.Hour).String()})})
	input, err := toEinoMessages(messages)
	if err != nil {
		return false
	}
	text, err := backgroundRoleMessage(ctx, b, snap, s.now, billed, id, input)
	if err != nil || text == "" || id < 0 && strings.Contains(text, "@") {
		return true
	}
	if _, err := snap.engagementChat(ctx, id, member); err != nil {
		return true
	}

	// Fence the reservation against newer human messages, clear, and current
	// preference. Unknown sends suppress another opening even after a restart.
	turn.withCurrent(func() {
		live := b.snapshot()
		if !sameBackgroundRole(snap, live) || turn.checkUse(ctx) != nil || !b.backgroundQuota(billed, id, s.now()) {
			return
		}
		reserved, ok, err := s.updateChatter(item, func(state *chatterState, current ConversationPreferenceMode) bool {
			now := s.now()
			next := state.NextCheck
			state.NextCheck = time.Time{}
			if current != pref.Chatter || !state.eligible(id, current, now) {
				return false
			}
			state.NextCheck, state.LastOffer, state.Awaiting = next, now, true
			state.Attempt, state.Outcome, state.Receipt = rand.Text(), EngagementReserved, ""
			return true
		})
		if err != nil || !ok {
			return
		}
		state, receipt := EngagementNoEffect, "Telegram request was never sent"
		var confirmedText string
		live = b.snapshot()
		if sameBackgroundRole(snap, live) && turn.checkUse(ctx) == nil && b.backgroundQuota(billed, id, s.now()) {
			state, receipt, confirmedText = snap.sendEngagementText(ctx, id, text)
		}
		if err := s.settleChatter(id, reserved.state.Attempt, state, receipt); err != nil {
			log.Printf("[proactivity] archive Telegram outcome: %v", err)
		}
		if state == EngagementSucceeded && snap.store != nil {
			snap.store.Append(id, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: confirmedText}, snap.cfg.ContextMaxMsgs)
		}
	})
	return true
}

func (s *EngagementStore) settleChatter(id int64, attempt string, outcome EngagementResultState, receipt string) error {
	return s.transact(true, func(tx *bolt.Tx) error {
		key := chatIDKey(id)
		bucket := tx.Bucket(chatterBucket)
		state, err := readChatter(bucket, key)
		if err != nil || state.Attempt != attempt || state.Outcome != EngagementReserved {
			return err
		}
		state.Outcome, state.Receipt = outcome, receipt
		if outcome == EngagementNoEffect {
			state.Awaiting = false
		}
		return saveChatter(bucket, key, state)
	})
}

func sameBackgroundRole(before, live runtimeSnapshot) bool {
	return before.engagements == live.engagements && before.tg == live.tg && before.ai == live.ai &&
		before.cfg.OpenAIModel == live.cfg.OpenAIModel && before.cfg.SystemPrompt == live.cfg.SystemPrompt
}

func backgroundRoleMessage(ctx context.Context, b *Bot, snap runtimeSnapshot, now func() time.Time, billed, chat int64, input []*schema.Message) (string, error) {
	model := &einoChatModel{
		client: snap.ai, modelName: snap.cfg.OpenAIModel, maxTokens: 350,
		retry: snap.cfg.EinoModelRetryEnabled, backupModel: snap.cfg.EinoFallbackModel,
		usageCtx: newUsageContext(chat, billed, 0),
		recordUsage: func(event UsageEvent) {
			event.Timestamp = now()
			b.recordUsageEventFor(snap, event)
		},
	}
	reply, err := model.Generate(ctx, input)
	if err != nil {
		return "", err
	}
	var output struct{ Message *string }
	if !dialogueArgs(reply.Content, &output) || output.Message == nil || utf8.RuneCountInString(*output.Message) > 800 {
		return "", ErrEngagementInvalid
	}
	return strings.TrimSpace(*output.Message), nil
}

const privateFollowupDelay = 7 * 24 * time.Hour

func privateFollowupCapability(owner int64) EngagementCapability {
	return EngagementCapability{
		Access: MemoryDataAccess{OwnerID: owner, Key: engagementPrivateFollowup}, Operation: engagementPrivateFollowup,
		Target: "original private chat", DataChatID: owner,
	}
}

func (a EngagementAgreement) permitsPrivateFollowup(kind EngagementKind, source MemorySource) bool {
	return kind == EngagementTracking && source.ChatID > 0 && source.ChatID == source.SenderID &&
		a.Observe == privateFollowupCapability(source.ChatID) && a.Once && len(a.Actions) == 0 &&
		a.Trigger.ChatID == source.ChatID && a.EndCondition == (EngagementText{}) &&
		slices.Equal(a.Recipients, []int64{source.ChatID})
}

func (r *engagementRun) checkPrivateFollowup(ctx context.Context) (EngagementCheck, error) {
	a, origin := r.item.Current, r.item.Origin
	if !a.permitsPrivateFollowup(r.item.Kind, origin) || !r.canUseTerm(a.Trigger) {
		return EngagementCheck{}, ErrEngagementDenied
	}
	step := EngagementStep{Recipient: origin.ChatID}
	now := r.store.now()
	check := EngagementCheck{NextCheck: now.Add(24 * time.Hour)}
	if r.item.MuteUntil.After(now) {
		check.NextCheck = minTime(check.NextCheck, r.item.MuteUntil)
	}
	pref, err := r.store.conversationPreference(origin.ChatID, 0)
	if err != nil {
		return check, err
	}
	if pref.Notifications.Until.After(now) {
		check.NextCheck = minTime(check.NextCheck, pref.Notifications.Until)
	}
	works, err := r.store.ClaimWork(r.claim, now, "", 1)
	if err != nil {
		return check, err
	}
	if len(works) != 0 {
		work := works[0]
		if i := work.resultIndex(step); i >= 0 {
			prior := work.Results[i]
			switch prior.State {
			case EngagementSucceeded:
				check.CompletedChange = work.ChangeKey
				return check, nil
			case EngagementReserved, EngagementUnknown:
				_, err := r.recordUnknown(prior.bind(r.claim.ID, work.ChangeKey), "Telegram delivery could not be verified")
				return check, err
			}
		}
	}
	allowed, err := r.notificationAllowed(r.item, origin.ChatID)
	if err != nil || !allowed || !r.bot.backgroundQuota(origin.SenderID, origin.ChatID, now) {
		return check, err
	}
	chat, err := r.recipient(ctx, r.item, origin.ChatID)
	if err != nil {
		return check, err
	}
	turn := r.bot.beginRoleTurn(RoleTurn{ChatID: chat.ID, InitiatorID: origin.SenderID, BillingUserID: origin.SenderID,
		Purpose: RoleBackground, chatType: chat.Type, backgroundContext: true, Participants: []int64{origin.SenderID}})
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(turn.generation, cancel)
	defer stop()
	const instruction = "\n\nDecide whether it is appropriate to ask this person once, privately, about the personal outcome described in the data. You cannot know the outcome. If they already told you the result, the date has not passed, or the question is no longer helpful, stay quiet. Obey the current character and return only JSON with a message string, empty when staying quiet."
	messages := r.snap.buildRoleContext(&turn, a.Trigger.Text, instruction, 12, true)
	if len(messages) == 0 {
		return check, errRoleAudience
	}
	messages = append(messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: dialogueJSON(struct {
		Topic, Context string
		Now            time.Time
	}{a.Trigger.Text, r.item.Reason.Text, now})})
	input, err := toEinoMessages(messages)
	if err != nil {
		return check, err
	}
	if _, err := r.current(ctx); err != nil || turn.checkUse(ctx) != nil {
		return check, err
	}
	text, err := backgroundRoleMessage(ctx, r.bot, r.snap, r.store.now, origin.SenderID, origin.ChatID, input)
	if err != nil || text == "" || turn.checkUse(ctx) != nil {
		return check, err
	}
	turn.withCurrent(func() {
		if _, err = r.current(ctx); err != nil || turn.checkUse(ctx) != nil {
			return
		}
		work, _, observeErr := r.store.Observe(r.claim, r.store.now(), EngagementObservation{ChangeKey: engagementPrivateFollowup})
		if err = observeErr; err != nil {
			return
		}
		var state EngagementResultState
		state, err = r.deliver(ctx, work, step, text)
		if state == EngagementSucceeded {
			check.CompletedChange = work.ChangeKey
		}
	})
	return check, err
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
