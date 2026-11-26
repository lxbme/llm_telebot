package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
	tele "gopkg.in/telebot.v3"
)

var conversationPreferenceBucket = []byte("conversation_preferences")

type ConversationPreferenceMode string

const (
	ConversationDefault ConversationPreferenceMode = "default"
	ConversationOn      ConversationPreferenceMode = "on"
	ConversationReduce  ConversationPreferenceMode = "reduce"
	ConversationOff     ConversationPreferenceMode = "off"
)

func (m ConversationPreferenceMode) valid() bool {
	return m == ConversationDefault || m == ConversationOn || m == ConversationReduce || m == ConversationOff
}

type conversationPreferenceSetting struct {
	Persistent ConversationPreferenceMode
	Temporary  ConversationPreferenceMode
	Until      time.Time
}

func (s conversationPreferenceSetting) at(now time.Time) ConversationPreferenceMode {
	if s.Temporary != "" && now.Before(s.Until) {
		return s.Temporary
	}
	if s.Persistent == "" {
		return ConversationDefault
	}
	return s.Persistent
}

func (s *conversationPreferenceSetting) set(mode ConversationPreferenceMode, until time.Time) {
	if until.IsZero() {
		*s = conversationPreferenceSetting{Persistent: mode}
	} else {
		s.Temporary, s.Until = mode, until
	}
}

type conversationPreferenceRecord struct {
	Version                uint64
	Chatter, Notifications conversationPreferenceSetting
	Source                 MemorySource
	Revision               string
}

// ConversationPreference is the effective policy for an audience and optional
// participant. A group worker passes actorID 0 for chat-wide delivery.
type ConversationPreference struct {
	Chatter, Notifications ConversationPreferenceMode
}

func preferenceKey(chatID, actorID int64) []byte {
	return fmt.Appendf(nil, "%d/%d", chatID, actorID)
}

func loadPreference(tx *bolt.Tx, key []byte) (conversationPreferenceRecord, error) {
	var r conversationPreferenceRecord
	raw := tx.Bucket(conversationPreferenceBucket).Get(key)
	if raw == nil {
		return r, nil
	}
	err := json.Unmarshal(raw, &r)
	return r, err
}

func (s *EngagementStore) conversationPreference(chatID, actorID int64) (conversationPreferenceRecord, error) {
	var r conversationPreferenceRecord
	err := s.transact(false, func(tx *bolt.Tx) error {
		var err error
		r, err = loadPreference(tx, preferenceKey(chatID, actorID))
		return err
	})
	return r, err
}

func (s *EngagementStore) ConversationPreferenceAt(chatID, actorID int64, now time.Time) (ConversationPreference, error) {
	if chatID == 0 || actorID < 0 || (chatID > 0 && actorID != 0 && actorID != chatID) || now.IsZero() {
		return ConversationPreference{}, ErrEngagementInvalid
	}
	p := ConversationPreference{Chatter: ConversationDefault, Notifications: ConversationDefault}
	err := s.transact(false, func(tx *bolt.Tx) error {
		r, err := loadPreference(tx, preferenceKey(chatID, 0))
		if err != nil {
			return err
		}
		p = ConversationPreference{Chatter: r.Chatter.at(now), Notifications: r.Notifications.at(now)}
		if actorID > 0 && chatID < 0 {
			r, err = loadPreference(tx, preferenceKey(chatID, actorID))
			if err != nil {
				return err
			}
			if mode := r.Chatter.at(now); mode != ConversationDefault {
				p.Chatter = mode
			}
			if mode := r.Notifications.at(now); mode != ConversationDefault {
				p.Notifications = mode
			}
		}
		return nil
	})
	return p, err
}

func preferenceRevisionKey(key []byte, revision string) []byte {
	return fmt.Appendf(nil, "@%s/%s", key, revision)
}

const preferenceEventPrefix = "@event/"

type conversationPreferenceSave struct {
	Chatter, Notifications          ConversationPreferenceMode
	ChatterUntil, NotificationUntil time.Time
}

type conversationPreferenceEvent struct {
	Attempt string
	Sender  int64
	Saves   map[int64]conversationPreferenceSave
}

func loadPreferenceEvent(tx *bolt.Tx, source MemorySource) (conversationPreferenceEvent, error) {
	var event conversationPreferenceEvent
	key := append([]byte(preferenceEventPrefix), source.eventKey()...)
	if raw := tx.Bucket(conversationPreferenceBucket).Get(key); raw != nil {
		if err := json.Unmarshal(raw, &event); err != nil {
			return event, err
		}
		if event.Sender != source.SenderID {
			return event, ErrEngagementDenied
		}
	}
	return event, nil
}

func (s *EngagementStore) conversationPreferenceReplay(source MemorySource, attempt string) (map[int64]conversationPreferenceSave, error) {
	var saves map[int64]conversationPreferenceSave
	err := s.transact(false, func(tx *bolt.Tx) error {
		event, err := loadPreferenceEvent(tx, source)
		if err == nil && event.Attempt != attempt {
			saves = event.Saves
		}
		return err
	})
	return saves, err
}

func (s *EngagementStore) setConversationPreference(source MemorySource, subject int64, version uint64, attempt string, requested conversationPreferenceSave, now time.Time) (conversationPreferenceSave, error) {
	if !source.valid() || source.Access != (MemoryDataAccess{}) ||
		(source.ChatID > 0 && (source.ChatID != source.SenderID || subject != 0)) ||
		(subject != 0 && subject != source.SenderID) || attempt == "" {
		return conversationPreferenceSave{}, ErrEngagementInvalid
	}
	key := preferenceKey(source.ChatID, subject)
	eventKey := append([]byte(preferenceEventPrefix), source.eventKey()...)
	var saved conversationPreferenceSave
	err := s.transact(true, func(tx *bolt.Tx) error {
		bucket := tx.Bucket(conversationPreferenceBucket)
		event, err := loadPreferenceEvent(tx, source)
		if err != nil {
			return err
		}
		if event.Attempt != "" {
			prior, exists := event.Saves[subject]
			if exists || event.Attempt != attempt {
				saved = prior
				return ErrEngagementProcessed
			}
		} else {
			event = conversationPreferenceEvent{Attempt: attempt, Sender: source.SenderID, Saves: make(map[int64]conversationPreferenceSave)}
		}
		r, err := loadPreference(tx, key)
		if err != nil {
			return err
		}
		if r.Source.MessageID >= source.MessageID {
			return ErrEngagementProcessed
		}
		if r.Version != version {
			return ErrEngagementConflict
		}
		chatter, notifications := requested.Chatter, requested.Notifications
		chatterUntil, notificationUntil := requested.ChatterUntil, requested.NotificationUntil
		if (chatter == "" && notifications == "") || (chatter != "" && !chatter.valid()) ||
			(notifications != "" && !notifications.valid()) || (!chatterUntil.IsZero() && !chatterUntil.After(now)) ||
			(!notificationUntil.IsZero() && !notificationUntil.After(now)) {
			return ErrEngagementInvalid
		}
		if r.Revision != "" {
			if err := bucket.Put(preferenceRevisionKey(key, r.Revision), []byte{1}); err != nil {
				return err
			}
		}
		if chatter != "" {
			r.Chatter.set(chatter, chatterUntil)
		}
		if notifications != "" {
			r.Notifications.set(notifications, notificationUntil)
		}
		r.Version++
		r.Source, r.Revision = source, rand.Text()
		event.Saves[subject] = requested
		if err := putEngagementJSON(bucket, eventKey, event); err != nil {
			return err
		}
		return putEngagementJSON(bucket, key, r)
	})
	if err == nil {
		saved = requested
	}
	return saved, err
}

func copyConversationPreferences(source, target *bolt.Tx) error {
	from, to := source.Bucket(conversationPreferenceBucket), target.Bucket(conversationPreferenceBucket)
	return from.ForEach(func(key, raw []byte) error {
		if prior := to.Get(key); prior != nil && !bytes.Equal(prior, raw) {
			if bytes.HasPrefix(key, []byte(preferenceEventPrefix)) {
				return ErrEngagementConflict
			}
			if key[0] != '@' {
				r, err := loadPreference(target, key)
				if err != nil {
					return err
				}
				if from.Get(preferenceRevisionKey(key, r.Revision)) == nil {
					return ErrEngagementConflict
				}
			}
		}
		return to.Put(key, raw)
	})
}

type conversationPreferenceProposal struct {
	Scope                string
	Version              uint64
	Verdict              EngagementVerdict
	Chatter              ConversationPreferenceMode
	Notifications        ConversationPreferenceMode
	Duration             string
	ChatterDuration      string `json:"chatter_duration"`
	NotificationDuration string `json:"notification_duration"`
	Reply                string
}

func (d *engagementDialogue) preferenceRuntime(ctx context.Context) (runtimeSnapshot, error) {
	source := d.turn.Source
	chat := &tele.Chat{ID: d.turn.ChatID, Type: d.turn.chatType}
	if !d.message.trusted || !source.valid() || source.Access != (MemoryDataAccess{}) ||
		source.ChatID != chat.ID || source.SenderID != d.turn.SenderID ||
		(!telegramGroup(chat) && (chat.Type != tele.ChatPrivate || chat.ID != source.SenderID)) {
		return runtimeSnapshot{}, ErrEngagementDenied
	}
	return d.active(ctx)
}

// replayPreferences restores only the channels committed for this event. The
// current proposal and current preference values do not describe that save.
func (d *engagementDialogue) replayPreferences(store *EngagementStore) (bool, error) {
	prior, err := store.conversationPreferenceReplay(d.turn.Source, d.preferenceAttempt)
	if err != nil || len(prior) == 0 {
		return false, err
	}
	if d.turn.chatType == tele.ChatPrivate {
		if saved, ok := prior[0]; ok {
			d.preferenceReceipt(d.outcome(dialogueDecisionKey{preference: "private"}), saved, true)
		}
	} else {
		if saved, ok := prior[0]; ok {
			d.preferenceReceipt(d.outcome(dialogueDecisionKey{preference: "group"}), saved, true)
		}
		if saved, ok := prior[d.turn.SenderID]; ok {
			d.preferenceReceipt(d.outcome(dialogueDecisionKey{preference: "self"}), saved, true)
		}
	}
	return true, nil
}

func (d *engagementDialogue) changePreference(ctx context.Context, raw string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var p conversationPreferenceProposal
	valid := dialogueArgs(raw, &p)
	key := dialogueDecisionKey{preference: firstNonEmpty(p.Scope, "invalid")}
	snap, err := d.preferenceRuntime(ctx)
	if err != nil {
		return d.failed(d.outcome(key), err)
	}
	store := snap.engagements
	replayed, err := d.replayPreferences(store)
	if err != nil {
		return d.failed(d.outcome(key), err)
	}
	if replayed {
		if out := d.outcomes[key]; out != nil && out.finished {
			return out.text, nil
		}
		return d.localized("This message was already processed. No additional preference change was made.", "这条消息已处理。本次没有新增偏好变更。"), nil
	}
	out := d.outcome(key)
	if out.finished {
		return out.text, nil
	}
	if !valid {
		return d.failed(out, ErrEngagementInvalid)
	}
	switch p.Verdict {
	case EngagementReject, EngagementClarify:
		note := d.localized("Conversation preferences were unchanged.", "会话偏好保持原状。")
		if p.Reply != "" && d.reviewReply(ctx, snap, p.Reply) {
			note = truncate(p.Reply, 450) + "\n\n" + note
		}
		return out.result(note)
	case EngagementAccept:
	default:
		return d.failed(out, ErrEngagementInvalid)
	}
	var subject int64
	switch {
	case d.turn.chatType == tele.ChatPrivate && p.Scope == "private":
	case d.turn.ChatID < 0 && p.Scope == "self":
		subject = d.turn.SenderID
	case d.turn.ChatID < 0 && p.Scope == "group":
	default:
		return d.failed(out, ErrEngagementDenied)
	}
	if d.preferenceAttempt == "" {
		d.preferenceAttempt = rand.Text()
	}
	if p.Scope == "group" {
		intent, err := d.currentIntent(ctx, snap)
		if err != nil || intent.Preference != "group" {
			return d.failed(out, ErrEngagementDenied)
		}
	}
	now := store.now()
	chatterUntil, err := dialogueUntil(firstNonEmpty(p.ChatterDuration, p.Duration), d.message.sentAt, now, snap.cfg)
	if err != nil {
		return d.failed(out, err)
	}
	notificationUntil, err := dialogueUntil(firstNonEmpty(p.NotificationDuration, p.Duration), d.message.sentAt, now, snap.cfg)
	if err != nil {
		return d.failed(out, err)
	}
	requested := conversationPreferenceSave{p.Chatter, p.Notifications, chatterUntil, notificationUntil}
	var saved conversationPreferenceSave
	if !d.turn.withCurrent(func() {
		d.turn.bot.runtimeConfigMu.Lock()
		defer d.turn.bot.runtimeConfigMu.Unlock()
		current := d.turn.bot.snapshot()
		if d.turn.checkUse(ctx) != nil || !d.turn.senderAllowed(current.cfg) || current.cfg.SystemPrompt != d.rolePrompt || current.engagements != store {
			err = ErrEngagementDenied
			return
		}
		saved, err = store.setConversationPreference(d.turn.Source, subject, p.Version, d.preferenceAttempt, requested, now)
	}) {
		return d.failed(out, ErrEngagementDenied)
	}
	if errors.Is(err, ErrEngagementConflict) {
		return d.retry(ctx, out, "")
	}
	replayed = errors.Is(err, ErrEngagementProcessed)
	if err != nil && !replayed {
		return d.failed(out, err)
	}
	return d.preferenceReceipt(out, saved, replayed)
}

func (d *engagementDialogue) preferenceReceipt(out *dialogueOutcome, saved conversationPreferenceSave, replayed bool) (string, error) {
	out.chatter, out.notifications = saved.Chatter != "", saved.Notifications != ""
	if replayed && !out.chatter && !out.notifications {
		return out.result(d.localized("No additional preference change was made. I couldn't confirm an earlier save for this scope.", "本次没有新增偏好变更。无法确认这个范围先前保存了哪些设置。"))
	}
	scope := d.localized("private chat", "私聊")
	if out.key.preference == "group" {
		scope = d.localized("this group", "全群")
	} else if out.key.preference == "self" {
		scope = d.localized("you in this group", "你在本群")
	}
	receipt := d.localized("Conversation preference saved for ", "已保存会话偏好，适用于") + scope + "."
	if replayed {
		receipt = d.localized("This message was already processed. The original preference changes for ", "这条消息已处理。原先保存的会话偏好适用于") + scope + d.localized(" were as follows.", "。")
	}
	if out.chatter {
		receipt += "\n" + d.localized("Proactive chatter", "主动闲聊") + " — " + d.preferenceMode(saved.Chatter) + receiptUntil(saved.ChatterUntil)
	}
	if out.notifications {
		receipt += "\n" + d.localized("Tracking notifications", "跟踪通知") + " — " + d.preferenceMode(saved.Notifications) + receiptUntil(saved.NotificationUntil)
	}
	return out.result(receipt)
}

func (d *engagementDialogue) preferenceMode(mode ConversationPreferenceMode) string {
	switch mode {
	case ConversationOn:
		return d.localized("on", "开启")
	case ConversationReduce:
		return d.localized("reduced", "减少")
	case ConversationOff:
		return d.localized("off", "关闭")
	default:
		return d.localized("default", "默认")
	}
}
