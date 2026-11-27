package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	bolt "go.etcd.io/bbolt"
	tele "gopkg.in/telebot.v3"
)

func TestProactiveIdle(t *testing.T) {
	const quietGroup, activeGroup = int64(-810), int64(-811)
	var decisions atomic.Int32
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
		if req.Stream {
			engagementModelText(w, "收到")
			return
		}
		decisions.Add(1)
		input := fmt.Sprint(req.Messages)
		if len(req.Tools) != 0 || !strings.Contains(input, "Alder. Be reserved.") || strings.Contains(input, "私聊独有") && strings.Contains(input, "本群话题") ||
			strings.Contains(input, "本群话题") && strings.Contains(input, "私有记忆只在本人") {
			t.Errorf("unexpected idle decision input: %s", input)
		}
		text := "最近的本群话题，后来有新进展吗？"
		if strings.Contains(input, "我的话题已经结束") {
			text = ""
		}
		if strings.Contains(input, "私聊独有") {
			if !strings.Contains(input, "私有记忆只在本人") {
				t.Error("private idle decision missed its Memory Book entry")
			}
			text = "最近还在练习木工吗？"
		}
		_ = json.NewEncoder(w).Encode(openai.ChatCompletionResponse{Choices: []openai.ChatCompletionChoice{
			{Message: openai.ChatCompletionMessage{Content: dialogueJSON(map[string]string{"message": text})}},
		}, Usage: openai.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}})
	})
	b, deliveries := newRoleTestBot(t, model)
	b.cfg.ProfileEnabled = false
	b.cfg.ContextMode = "global"
	b.cfg.AdminWildcard = true
	b.cfg.AllowedUsers = map[int64]bool{engagementOwner: true, engagementPeer: true}
	b.cfg.AllowedGroups = map[int64]bool{quietGroup: true, activeGroup: true}
	var clock atomic.Int64
	start := engagementNow
	clock.Store(start.Unix())
	now := func() time.Time { return time.Unix(clock.Load(), 0).UTC() }
	b.engagements = NewEngagementStore(b.chatDB)
	b.engagements.now = now
	b.stats = NewStatsManager(b.chatDB)
	t.Cleanup(func() { _ = b.stats.Close() })

	send := func(chatID, userID int64, id int, text string) {
		msg := roleMessage(chatID, userID, id, text)
		msg.Unixtime = now().Unix()
		roleHandle(t, b, b.handleText, msg)
	}
	if _, err := b.memory.Learn(MemoryLearn{Subjects: []int64{engagementOwner}, Text: "私有记忆只在本人", Cue: MemoryFamiliar,
		Evidence: MemoryEvidence{Kind: MemoryStatement, Source: MemorySource{ChatID: engagementOwner, SenderID: engagementOwner, MessageID: 20}},
	}); err != nil {
		t.Fatal(err)
	}
	send(engagementOwner, engagementOwner, 1, "私聊独有，最近在练习木工")
	roleReceive(t, deliveries)
	send(engagementOwner, engagementOwner, 2, "私聊独有，做完了第一把椅子")
	roleReceive(t, deliveries)
	waitEinoReplyHistory(t, b, engagementOwner, 4)
	send(engagementPeer, engagementPeer, 1, "我的话题已经结束，谢谢")
	roleReceive(t, deliveries)
	send(engagementPeer, engagementPeer, 2, "我的话题已经结束")
	roleReceive(t, deliveries)
	waitEinoReplyHistory(t, b, engagementPeer, 4)
	for _, group := range []int64{quietGroup, activeGroup} {
		send(group, engagementOwner, 1, "本群话题：周末徒步")
		send(group, engagementPeer, 2, "本群话题：天气不错")
		send(group, engagementOwner, 3, "本群话题：我带地图")
	}
	_, err := b.engagements.setConversationPreference(
		engagementRequest(quietGroup, engagementOwner, 4, "本群先别主动闲聊").Source, 0, 0, "task7",
		conversationPreferenceSave{Chatter: ConversationOff, ChatterUntil: start.Add(55 * time.Hour)}, start,
	)
	if err != nil {
		t.Fatal(err)
	}
	tick := func() {
		r := NewTaskRunner(b, nil)
		r.tick(t.Context(), now())
		waitRunner(t, r)
		auditWaitLegacy(t, r, "proactivity:batch")
	}
	clock.Store(start.Add(13 * time.Hour).Unix())
	tick()
	dm := roleReceive(t, deliveries)
	if dm.chatID != engagementOwner || dm.path != "/botrole/sendMessage" || !strings.Contains(dm.text, "木工") || len(deliveries) != 0 {
		t.Fatalf("unexpected early delivery: %+v / %d additional", dm, len(deliveries))
	}
	clock.Store(start.Add(49 * time.Hour).Unix())
	tick()
	group := roleReceive(t, deliveries)
	if group.chatID != activeGroup || !strings.Contains(group.text, "本群话题") || len(deliveries) != 0 {
		t.Fatalf("unexpected group delivery: %+v / %d additional", group, len(deliveries))
	}
	date := now().Format("20060102")
	if chat := b.stats.GetQuotaBucket(UsageScopeChat, activeGroup, 0, date); chat.QuotaTokens != 10 {
		t.Fatalf("group autonomous usage: %+v", chat)
	}
	if user := b.stats.GetQuotaBucket(UsageScopeUser, activeGroup, engagementOwner, date); user.QuotaTokens != 0 {
		t.Fatalf("charged group chatter to a member: %+v", user)
	}

	// Switch the active database, then reopen it with a fresh store and runner.
	path := filepath.Join(t.TempDir(), "switch.db")
	target, err := OpenChatDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateChatStores(target, b.store, b.summaries, b.memory, b.engagements); err != nil {
		t.Fatal(err)
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	target, err = OpenChatDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	b.engagements = NewEngagementStore(target)
	b.engagements.now = now
	b.store = NewHistoryStore(target)
	b.memory = NewMemoryBook(target)
	b.summaries = NewSummaryStore(target)
	clock.Store(start.Add(96 * time.Hour).Unix())
	tick()
	group = roleReceive(t, deliveries)
	if group.chatID != quietGroup || len(deliveries) != 0 || decisions.Load() != 6 {
		t.Fatalf("expiry or restart lost the conversation state: %+v / %d decisions / %d additional", group, decisions.Load(), len(deliveries))
	}
}

func TestProactiveIdleConcurrentUnknownDoesNotReplay(t *testing.T) {
	var decisions, attempted atomic.Int32
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, req openai.ChatCompletionRequest) {
		if req.Stream {
			engagementModelText(w, "I see.")
			return
		}
		decisions.Add(1)
		roleCompletion(w, `{"message":"Did you get to try the new tea?"}`)
	})
	b, replies := newRoleTestBotWithTelegram(t, model, func(w http.ResponseWriter, _ *http.Request, d roleTestDelivery) bool {
		if d.path != "/botrole/sendMessage" || !strings.Contains(d.text, "new tea") {
			return false
		}
		if d.chatID != engagementOwner {
			t.Errorf("active opening to unexpected chat: %+v", d)
		}
		attempted.Add(1)
		http.Error(w, "receipt lost after send", http.StatusBadGateway)
		return true
	})
	b.cfg.ProfileEnabled = false
	var clock atomic.Int64
	clock.Store(engagementNow.Unix())
	now := func() time.Time { return time.Unix(clock.Load(), 0).UTC() }
	b.engagements = NewEngagementStore(b.chatDB)
	b.engagements.now = now
	for i := 1; i <= 2; i++ {
		msg := roleMessage(engagementOwner, engagementOwner, i, "I will try a new tea.")
		msg.Unixtime = now().Unix()
		roleHandle(t, b, b.handleText, msg)
		roleReceive(t, replies)
		waitEinoReplyHistory(t, b, engagementOwner, i*2)
	}
	clock.Store(engagementNow.Add(13 * time.Hour).Unix())
	a, c := NewTaskRunner(b, nil), NewTaskRunner(b, nil)
	a.tick(t.Context(), now())
	c.tick(t.Context(), now())
	waitTask7Runner(t, a)
	waitTask7Runner(t, c)
	if decisions.Load() != 1 || attempted.Load() != 1 || len(b.store.Get(engagementOwner)) != 4 {
		t.Fatalf("idle claim or receipt was not respected: decisions=%d sends=%d history=%d", decisions.Load(), attempted.Load(), len(b.store.Get(engagementOwner)))
	}

	if err := b.chatDB.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := OpenChatDB(b.cfg.ChatDBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	b.engagements = NewEngagementStore(db)
	b.engagements.now = now
	b.store = NewHistoryStore(db)
	b.memory = NewMemoryBook(db)
	clock.Store(engagementNow.Add(7 * 24 * time.Hour).Unix())
	c.tick(t.Context(), now())
	waitTask7Runner(t, c)
	if decisions.Load() != 1 || attempted.Load() != 1 {
		t.Fatal("outcome unknown caused another proactive attempt after restart")
	}
}

func waitTask7Runner(t *testing.T, r *TaskRunner) {
	t.Helper()
	waitRunner(t, r)
	auditWaitLegacy(t, r, "proactivity:batch")
}

func TestProactiveReplyIdentity(t *testing.T) {
	const caption = "🥾 @TESTBOT"
	for _, tc := range []struct {
		name, text string
		entity     tele.Entities
		caption    bool
		replyID    int64
		want       bool
	}{
		{name: "ordinary discussion", text: "Testbot was mentioned yesterday"},
		{name: "other bot", text: "@testbot_other hello", entity: tele.Entities{{Type: tele.EntityMention, Length: 14}}},
		{name: "partial entity", text: "@testbot_other hello", entity: tele.Entities{{Type: tele.EntityMention, Length: 8}}},
		{name: "fallback other bot", text: "@testbot_other hello"},
		{name: "fallback username", text: "Hi, @TESTBOT!", want: true},
		{name: "email-like token", text: "not@testbot"},
		{name: "code", text: "@testbot", entity: tele.Entities{{Type: tele.EntityCode, Length: 8}}},
		{name: "UTF-16 offset", text: caption, entity: tele.Entities{{Type: tele.EntityMention, Offset: 3, Length: 8}}, want: true},
		{name: "UTF-16 caption", text: caption, entity: tele.Entities{{Type: tele.EntityMention, Offset: 3, Length: 8}}, caption: true, want: true},
		{name: "incorrect offset", text: caption, entity: tele.Entities{{Type: tele.EntityMention, Offset: 2, Length: 8}}},
		{name: "reply to someone else", text: "Thanks", replyID: 98},
		{name: "reply to the bot", text: "Thanks", replyID: 99, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := roleMessage(engagementGroup, engagementOwner, 1, tc.text)
			if tc.caption {
				msg.Text, msg.Caption, msg.CaptionEntities = "", tc.text, tc.entity
			} else {
				msg.Entities = tc.entity
			}
			if tc.replyID != 0 {
				msg.ReplyTo = roleMessage(engagementGroup, tc.replyID, 1, "Hello")
			}
			if got := answersChatter(msg, &tele.User{ID: 99, Username: "testbot"}); got != tc.want {
				t.Fatalf("directed=%t, want %t", got, tc.want)
			}
		})
	}
}

func TestProactiveBatchUsesAvailableQuota(t *testing.T) {
	const exhausted, eligible = 4, 6
	var decisions atomic.Int32
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ openai.ChatCompletionRequest) {
		decisions.Add(1)
		_ = json.NewEncoder(w).Encode(openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{Content: `{"message":"Tea, anyone?"}`}}},
			Usage:   openai.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10},
		})
	})
	b, replies := newRoleTestBot(t, model)
	b.cfg.ContextMode, b.cfg.ChatDailyTokenLimit = "global", 20
	b.cfg.AllowedGroups = map[int64]bool{}
	b.stats = NewStatsManager(b.chatDB)
	t.Cleanup(func() { _ = b.stats.Close() })
	b.engagements = NewEngagementStore(b.chatDB)
	now := engagementNow
	b.engagements.now = func() time.Time { return now }
	b.registerHandlers()
	at := now.Add(60 * time.Hour)
	day := at.Format("20060102")
	for i := range exhausted + eligible {
		chat := int64(-900 - i)
		b.cfg.AllowedGroups[chat] = true
		for j, actor := range []int64{engagementOwner, engagementPeer, engagementOwner} {
			msg := roleMessage(chat, actor, j+1, "Group discussion")
			msg.Unixtime = now.Unix()
			b.tg.ProcessUpdate(tele.Update{Message: msg})
		}
		if i < exhausted {
			b.stats.ApplyQuotaDelta(UsageEvent{ChatID: chat, TotalTokens: 20}, day, true)
		}
		now = now.Add(time.Hour)
	}
	now = at
	r := NewTaskRunner(b, nil)
	for _, want := range []int32{4, 6, 6} {
		r.tick(t.Context(), at)
		waitTask7Runner(t, r)
		if decisions.Load() != want || int32(len(replies)) != want {
			t.Fatalf("batch wanted %d cumulative decisions and deliveries, got %d/%d", want, decisions.Load(), len(replies))
		}
	}
	for i := range eligible {
		d := roleReceive(t, replies)
		if want := int64(-900 - exhausted - i); d.chatID != want {
			t.Fatalf("oldest eligible chat wanted %d; got %+v", want, d)
		}
		if tokens := b.stats.GetQuotaBucket(UsageScopeChat, d.chatID, 0, day).QuotaTokens; tokens != 10 {
			t.Fatalf("chat %d charged %d tokens", d.chatID, tokens)
		}
	}
	if tokens := b.stats.GetQuotaBucket(UsageScopeUser, 0, engagementOwner, day).QuotaTokens; tokens != 0 {
		t.Fatalf("autonomous decisions charged a user %d tokens", tokens)
	}
}

func TestProactiveMigrationKeepsBoundedAncestry(t *testing.T) {
	model := newEinoReplyProvider(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ openai.ChatCompletionRequest) {
		t.Error("ordinary group ingress called a model")
	})
	b, replies := newRoleTestBot(t, model)
	b.cfg.ContextMode, b.cfg.ProfileEnabled = "global", false
	b.engagements = NewEngagementStore(b.chatDB)
	b.engagements.now = func() time.Time { return engagementNow }
	b.registerHandlers()
	const chat = engagementGroup
	key := chatIDKey(chat)
	send := func(id int, text string) {
		b.tg.ProcessUpdate(tele.Update{Message: roleMessage(chat, engagementOwner, id, text)})
	}
	state := func(db *ChatDB, wantAnchors int) chatterState {
		t.Helper()
		var got chatterState
		err := db.db.View(func(tx *bolt.Tx) error {
			bucket := tx.Bucket(chatterBucket)
			if keys := bucket.Stats().KeyN; keys != 1 {
				t.Errorf("DB has %d proactive keys, want 1", keys)
			}
			var err error
			got, err = readChatter(bucket, key)
			return err
		})
		if err != nil || len(got.MigrationAnchors) != wantAnchors {
			t.Fatalf("state=%+v (%v), want %d anchors", got, err, wantAnchors)
		}
		return got
	}
	open := func() *ChatDB {
		t.Helper()
		db, err := OpenChatDB(filepath.Join(t.TempDir(), "next.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	migrate := func(db *ChatDB) error { return migrateChatStores(db, b.store, b.summaries, b.memory, b.engagements) }
	for id := 1; id <= 525; id++ {
		send(id, "Ordinary group discussion")
	}
	initial := state(b.chatDB, 0)
	alternate := open()
	if err := migrate(alternate); err != nil {
		t.Fatal(err)
	}
	if moved := state(alternate, 1); moved.Revision != initial.Revision {
		t.Fatalf("migration changed an active candidate revision: %+v", moved)
	}
	for id := 526; id <= 1050; id++ {
		send(id, "More ordinary discussion")
	}
	send(1051, "/clear")
	roleReceive(t, replies)
	if cleared := state(alternate, 1); cleared.Humans != 0 || cleared.Revision == initial.Revision {
		t.Fatalf("clear retained eligible or stale state: %+v", cleared)
	}
	if err := migrate(b.chatDB); err != nil {
		t.Fatal(err)
	}
	want := state(b.chatDB, 2)
	t.Log("525 messages before switch and 1,050 messages plus /clear after switch: one current key per copy, zero historical keys")

	// Seed the old key format, then reopen through normal DB initialization.
	if err := alternate.db.Update(func(tx *bolt.Tx) error {
		for i := range 525 {
			if err := tx.Bucket(chatterBucket).Put([]byte(fmt.Sprintf("@%dlegacy-%d", chat, i)), []byte{1}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	path := alternate.db.Path()
	if err := alternate.Close(); err != nil {
		t.Fatal(err)
	}
	alternate, err := OpenChatDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer alternate.Close()
	stale := state(alternate, 1)
	if stale.Revision != want.Revision || stale.MessageID != 1050 {
		t.Fatalf("legacy cleanup changed the active row: %+v", stale)
	}
	b.engagements = NewEngagementStore(b.chatDB)
	b.engagements.now = func() time.Time { return engagementNow }
	send(1050, "Repeated Telegram update")
	if got := state(b.chatDB, 2); got.Revision != want.Revision {
		t.Fatal("old update changed the version after reopen")
	}
	fork := NewEngagementStore(alternate)
	if _, ok, err := fork.updateChatter(chatterCandidate{chat, stale}, func(row *chatterState, _ ConversationPreferenceMode) bool {
		row.MessageID += 2
		return true
	}); err != nil || !ok {
		t.Fatalf("could not produce a divergent branch: %t / %v", ok, err)
	}
	send(1052, "Active branch input")
	forked := state(alternate, 1)
	if err := migrate(alternate); !errors.Is(err, ErrEngagementConflict) {
		t.Fatalf("distinct revisions at identical message IDs merged: %v", err)
	}
	if current := state(b.chatDB, 2); current.MessageID != forked.MessageID || current.Revision == forked.Revision ||
		b.engagements.db != b.chatDB || b.store.db != b.chatDB || len(NewHistoryStore(alternate).Get(chat)) != 0 {
		t.Fatalf("rejected switch changed the stores: source=%+v target=%+v", current, forked)
	}
	for i := range chatterMigrationAnchors + 2 {
		next := open()
		if err := migrate(next); err != nil {
			t.Fatal(err)
		}
		state(next, min(3+i, chatterMigrationAnchors))
		send(1053+i, "Active branch input")
	}
}
