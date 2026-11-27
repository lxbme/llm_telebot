package app

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

const (
	memoryUser  int64 = 41
	memoryGroup int64 = -10041
	memoryOther int64 = -10042
)

func openMemoryTestDB(t *testing.T, path string) *ChatDB {
	t.Helper()
	db, err := OpenChatDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func learnMemory(t *testing.T, book *MemoryBook, req MemoryLearn) MemoryRef {
	t.Helper()
	ref, err := book.Learn(req)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func memoryObservation(chatID, messageID int64, text string) MemoryLearn {
	return MemoryLearn{
		Subjects: []int64{memoryUser},
		Evidence: MemoryEvidence{
			Source: MemorySource{ChatID: chatID, MessageID: messageID, SenderID: memoryUser},
			Kind:   MemoryStatement,
			Text:   "material about " + text,
		},
		Text: text,
	}
}

func visibleMemory(t *testing.T, book *MemoryBook, query MemoryQuery) []MemoryProjection {
	t.Helper()
	got, err := book.Visible(query)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestMemoryAudienceBeforeSelection(t *testing.T) {
	book := NewMemoryBook(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
	private := learnMemory(t, book, memoryObservation(memoryUser, 7, "reading list and private reason"))
	learnMemory(t, book, memoryObservation(memoryGroup, 7, "the group joke"))
	learnMemory(t, book, memoryObservation(memoryOther, 7, "unrelated group"))

	query := MemoryQuery{
		ChatID: memoryGroup, Participants: []int64{memoryUser},
		Query: "private reading list", MaxItems: 1, MaxBytes: 120,
	}
	if got := visibleMemory(t, book, query); len(got) != 1 || got[0].Text != "the group joke" {
		t.Fatalf("group candidates with one available slot = %+v", got)
	}
	query.ChatID = memoryUser
	query.Query = "group joke"
	if got := visibleMemory(t, book, query); len(got) != 1 || got[0].Text != "reading list and private reason" {
		t.Fatalf("private candidates = %+v", got)
	}
	query.Participants = []int64{42}
	if got := visibleMemory(t, book, query); len(got) != 0 {
		t.Fatalf("private conversation without the verified peer = %+v", got)
	}

	if _, err := book.Share(private, 42, memoryGroup, "reading list"); !errors.Is(err, ErrMemoryDenied) {
		t.Fatalf("share by a different person = %v", err)
	}
	if _, err := book.Share(private, memoryUser, memoryGroup, "reading list"); err != nil {
		t.Fatal(err)
	}
	query.ChatID = memoryGroup
	query.Participants = []int64{memoryUser}
	query.Query = "reading list"
	got := visibleMemory(t, book, query)
	if len(got) != 1 || got[0].Text != "reading list" {
		t.Fatalf("explicitly shared group candidates = %+v", got)
	}
	query.ChatID = memoryOther
	if got := visibleMemory(t, book, query); len(got) != 1 || got[0].Text != "unrelated group" {
		t.Fatalf("other group candidates = %+v", got)
	}
	query.ChatID = memoryGroup
	query.MaxItems, query.MaxBytes = 5, len("reading list")
	if got := visibleMemory(t, book, query); len(got) != 1 || got[0].Text != "reading list" {
		t.Fatalf("byte-budgeted group candidates = %+v", got)
	}
}

func TestMemoryFiniteCuesFollowTrustedParticipants(t *testing.T) {
	book := NewMemoryBook(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
	relation := memoryObservation(memoryUser, 7, "private reason for trusting this person")
	relation.Cue = MemoryFamiliar
	learnMemory(t, book, relation)
	habit := memoryObservation(memoryUser, 8, "private explanation for a habit")
	habit.Cue = MemoryAskFirst
	learnMemory(t, book, habit)

	query := MemoryQuery{ChatID: memoryGroup, Participants: []int64{memoryUser}, MaxItems: 5, MaxBytes: 120}
	for _, queryText := range []string{"", "private trusting explanation"} {
		query.Query = queryText
		got := visibleMemory(t, book, query)
		if len(got) != 2 || got[0].Cue != MemoryAskFirst || got[0].SubjectID != 0 ||
			got[1].Cue != MemoryFamiliar || got[1].SubjectID != memoryUser {
			t.Fatalf("cross-chat cues = %+v", got)
		}
		for _, projection := range got {
			if projection.Text != "" || projection.Ref != (MemoryRef{}) {
				t.Fatalf("cross-chat cue contains raw entry data: %+v", projection)
			}
		}
	}
	query.Participants = []int64{42}
	if got := visibleMemory(t, book, query); len(got) != 1 || got[0].Cue != MemoryAskFirst {
		t.Fatalf("unverified relationship projected: %+v", got)
	}

	relation = memoryObservation(memoryGroup, 7, "reason for a more reserved tone in the group")
	relation.Cue = MemoryReserved
	learnMemory(t, book, relation)
	query.ChatID, query.Participants, query.Query = memoryUser, []int64{memoryUser}, ""
	var relationCues []MemoryCue
	for _, projection := range visibleMemory(t, book, query) {
		if projection.SubjectID == memoryUser {
			relationCues = append(relationCues, projection.Cue)
		}
		if strings.Contains(projection.Text, "more reserved tone") {
			t.Fatalf("group text projected in private: %+v", projection)
		}
	}
	if !reflect.DeepEqual(relationCues, []MemoryCue{MemoryReserved}) {
		t.Fatalf("relationship projected back into the private chat = %v", relationCues)
	}
}

func TestMemoryDataAccessAppliesBeforeProjectionAndSharing(t *testing.T) {
	book := NewMemoryBook(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
	access := MemoryDataAccess{OwnerID: memoryUser, Key: "calendar-access"}
	req := memoryObservation(memoryUser, 7, "calendar conclusion and appointment details")
	req.Evidence.Source.Access = access
	req.Cue = MemoryFamiliar
	ref := learnMemory(t, book, req)
	if _, err := book.Share(ref, memoryUser, memoryGroup, "appointment confirmed"); err != nil {
		t.Fatal(err)
	}

	query := MemoryQuery{ChatID: memoryGroup, Participants: []int64{memoryUser}, MaxItems: 5, MaxBytes: 120}
	if got := visibleMemory(t, book, query); len(got) != 0 {
		t.Fatalf("without current group data permission = %+v", got)
	}
	query.DataAccess = map[MemoryDataAccess]bool{access: true}
	got := visibleMemory(t, book, query)
	if len(got) != 2 || got[0].Text != "appointment confirmed" || got[1].Cue != MemoryFamiliar {
		t.Fatalf("group permission only admits the approved text and finite cue = %+v", got)
	}
	delete(query.DataAccess, access)
	query.ChatID = memoryUser
	if got := visibleMemory(t, book, query); len(got) != 0 {
		t.Fatalf("after revocation in original chat = %+v", got)
	}
}

func TestMemoryManualEditAndReplay(t *testing.T) {
	book := NewMemoryBook(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
	start := memoryObservation(memoryUser, 7, "initial inference")
	start.Evidence.Kind = MemoryInference
	original := learnMemory(t, book, start)
	correction := memoryObservation(memoryUser, 8, "observed absence had a reason")
	correction.Expect = original
	correction.Evidence.Kind = MemoryCorrection
	corrected := learnMemory(t, book, correction)
	shared, err := book.Share(corrected, memoryUser, memoryGroup, "approved prior wording")
	if err != nil {
		t.Fatal(err)
	}
	revised, err := book.Revise(shared, MemoryEdit{Text: "manually corrected"})
	if err != nil {
		t.Fatal(err)
	}
	if revised.ID != original.ID || revised.Version != original.Version+3 {
		t.Fatalf("reference after corrections and share = %+v; original = %+v", revised, original)
	}

	stale := memoryObservation(memoryUser, 9, "stale async overwrite")
	stale.Expect = corrected
	if _, err := book.Learn(stale); !errors.Is(err, ErrMemoryConflict) {
		t.Fatalf("late learning after manual edit = %v", err)
	}
	otherChat := memoryObservation(memoryGroup, 8, "different audience")
	otherChat.Expect = revised
	if _, err := book.Learn(otherChat); !errors.Is(err, ErrMemoryDenied) {
		t.Fatalf("cross-chat overwrite = %v", err)
	}
	correction.Expect = MemoryRef{}
	correction.Subjects = []int64{42}
	for _, replay := range []MemoryLearn{start, correction} {
		if _, err := book.Learn(replay); !errors.Is(err, ErrMemoryProcessed) {
			t.Fatalf("source %d created another entry: %v", replay.Evidence.Source.MessageID, err)
		}
	}
	got, err := book.Get(original.ID)
	if err != nil || got.Ref != revised || got.Text != "manually corrected" || len(got.Evidence) != 2 ||
		got.Evidence[0] != start.Evidence || got.Evidence[1] != (MemoryEvidence{
		Source: correction.Evidence.Source, Kind: MemoryCorrection, Text: "material about observed absence had a reason",
	}) {
		t.Fatalf("raw evidence and revision = %+v, %v", got, err)
	}
	list, err := book.List("", 1)
	if err != nil || len(list) != 1 || !reflect.DeepEqual(list[0], got) {
		t.Fatalf("admin list = %+v, %v", list, err)
	}
	if next, err := book.List(original.ID, 1); err != nil || len(next) != 0 {
		t.Fatalf("admin list after last ID = %+v, %v", next, err)
	}
	if got := visibleMemory(t, book, MemoryQuery{ChatID: memoryGroup, MaxItems: 5, MaxBytes: 120}); len(got) != 0 {
		t.Fatalf("old share survived a content revision: %+v", got)
	}
}

func TestMemoryReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.db")
	db := openMemoryTestDB(t, path)
	book := NewMemoryBook(db)
	first := memoryObservation(memoryUser, 7, "to be deleted")
	first.Evidence.Kind = MemoryExperience
	ref := learnMemory(t, book, first)
	followup := memoryObservation(memoryUser, 8, "evidence to be deleted")
	followup.Expect = ref
	ref = learnMemory(t, book, followup)
	if err := book.Delete(ref); err != nil {
		t.Fatal(err)
	}
	kept := learnMemory(t, book, memoryObservation(memoryUser, 9, "kept after restart"))
	if _, err := book.Share(kept, memoryUser, memoryGroup, "share kept after restart"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	book = NewMemoryBook(openMemoryTestDB(t, path))
	if _, err := book.Get(ref.ID); !errors.Is(err, ErrMemoryNotFound) {
		t.Fatalf("get deleted entry after restart = %v", err)
	}
	followup.Expect = MemoryRef{}
	for _, req := range []MemoryLearn{first, followup} {
		if _, err := book.Learn(req); !errors.Is(err, ErrMemoryProcessed) {
			t.Fatalf("deleted source %d replay after restart = %v", req.Evidence.Source.MessageID, err)
		}
	}
	query := MemoryQuery{ChatID: memoryGroup, MaxItems: 5, MaxBytes: 120}
	if got := visibleMemory(t, book, query); len(got) != 1 || got[0].Text != "share kept after restart" {
		t.Fatalf("visible group memories after restart = %+v", got)
	}
	fresh := learnMemory(t, book, memoryObservation(memoryUser, 10, "new independent event"))
	list, err := book.List("", 5)
	if err != nil || len(list) != 2 || fresh.ID == ref.ID {
		t.Fatalf("list after independent new event = %+v, %v", list, err)
	}
}

func TestMemoryConcurrentDeleteAndLearn(t *testing.T) {
	book := NewMemoryBook(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
	for i := int64(1); i <= 16; i++ {
		first := memoryObservation(memoryUser, i*2, "old text")
		ref := learnMemory(t, book, first)
		followup := memoryObservation(memoryUser, i*2+1, "new text")
		followup.Expect = ref
		start := make(chan struct{})
		deleted, learned := make(chan error, 1), make(chan error, 1)
		go func() {
			<-start
			deleted <- book.Delete(ref)
		}()
		go func() {
			<-start
			_, err := book.Learn(followup)
			learned <- err
		}()
		close(start)
		deleteErr, learnErr := <-deleted, <-learned
		switch {
		case deleteErr == nil && errors.Is(learnErr, ErrMemoryNotFound):
		case errors.Is(deleteErr, ErrMemoryConflict) && learnErr == nil:
			entry, err := book.Get(ref.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := book.Delete(entry.Ref); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("concurrent delete and Learn = %v, %v", deleteErr, learnErr)
		}
		if _, err := book.Learn(first); !errors.Is(err, ErrMemoryProcessed) {
			t.Fatalf("replay after deletion = %v", err)
		}
	}
	if got, err := book.List("", 20); err != nil || len(got) != 0 {
		t.Fatalf("live entries after deletion = %+v, %v", got, err)
	}
}

func TestMemoryRuntimeDBSwitch(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{ChatDBPath: filepath.Join(dir, "a.db"), StickerRulesPath: filepath.Join(dir, "stickers.json"),
		OpenAIKey: "test-key", OpenAIModel: "test-model"}
	db := openMemoryTestDB(t, cfg.ChatDBPath)
	b := &Bot{cfg: cfg, chatDB: db, store: NewHistoryStore(db), memory: NewMemoryBook(db)}
	t.Cleanup(func() { _ = b.closeChatDB() })
	book := b.snapshot().memory
	first := memoryObservation(memoryUser, 7, "old text")
	ref := learnMemory(t, book, first)
	b.store.Append(memoryUser, openai.ChatCompletionMessage{Role: "user", Content: "history before switch"}, 3)

	next := cfg
	next.ChatDBPath = filepath.Join(dir, "b.db")
	target := openMemoryTestDB(t, next.ChatDBPath)
	targetOnly := learnMemory(t, NewMemoryBook(target), memoryObservation(memoryGroup, 7, "destination-only text"))
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.applyRuntimeConfig(next); err != nil {
		t.Fatal(err)
	}
	if b.snapshot().memory != book {
		t.Fatal("a captured MemoryBook no longer points at the active database")
	}

	start := make(chan struct{})
	work := make(chan error, 1)
	go func() {
		<-start
		if err := book.Delete(ref); err != nil {
			work <- err
			return
		}
		_, err := book.Learn(memoryObservation(memoryUser, 8, "survives concurrent switch"))
		work <- err
	}()
	close(start)
	switchErr := b.applyRuntimeConfig(cfg)
	if workErr := <-work; switchErr != nil || workErr != nil {
		t.Fatalf("switch and concurrent writes = %v, %v", switchErr, workErr)
	}
	if _, err := book.Get(targetOnly.ID); err != nil {
		t.Fatalf("destination-only entry across round trip: %v", err)
	}
	if err := b.closeChatDB(); err != nil {
		t.Fatal(err)
	}
	reopened := openMemoryTestDB(t, cfg.ChatDBPath)
	if history := reopened.LoadAllHistory()[memoryUser]; len(history) != 1 || history[0].Content != "history before switch" {
		t.Fatalf("history after round trip = %+v", history)
	}
	book = NewMemoryBook(reopened)
	if _, err := book.Learn(first); !errors.Is(err, ErrMemoryProcessed) {
		t.Fatalf("event index after round trip = %v", err)
	}
	query := MemoryQuery{ChatID: memoryUser, Participants: []int64{memoryUser}, MaxItems: 5, MaxBytes: 120}
	if got := visibleMemory(t, book, query); len(got) != 1 || got[0].Text != "survives concurrent switch" {
		t.Fatalf("private memories after round trip = %+v", got)
	}
}

func TestMemoryFailedMigrationKeepsSourceAndRollsBackHistory(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{ChatDBPath: filepath.Join(dir, "a.db"), StickerRulesPath: filepath.Join(dir, "stickers.json"),
		OpenAIKey: "test-key", OpenAIModel: "test-model"}
	db := openMemoryTestDB(t, cfg.ChatDBPath)
	b := &Bot{cfg: cfg, chatDB: db, store: NewHistoryStore(db), memory: NewMemoryBook(db)}
	t.Cleanup(func() { _ = b.closeChatDB() })
	b.store.Append(memoryUser, openai.ChatCompletionMessage{Role: "user", Content: "active history"}, 3)
	req := memoryObservation(memoryUser, 7, "active memory")
	active := learnMemory(t, b.memory, req)

	next := cfg
	next.ChatDBPath = filepath.Join(dir, "b.db")
	target := openMemoryTestDB(t, next.ChatDBPath)
	targetHistory := NewHistoryStore(target)
	targetHistory.Append(memoryUser, openai.ChatCompletionMessage{Role: "user", Content: "target history"}, 3)
	// Both databases independently learned the same Telegram event.
	competing := learnMemory(t, NewMemoryBook(target), req)
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.applyRuntimeConfig(next); !errors.Is(err, ErrMemoryConflict) || b.snapshot().chatDB != db || b.currentConfig().ChatDBPath != cfg.ChatDBPath {
		t.Fatalf("failed memory migration changed active resources: %v", err)
	}
	learnMemory(t, b.memory, memoryObservation(memoryUser, 8, "after failed migration"))
	target = openMemoryTestDB(t, next.ChatDBPath)
	if history := target.LoadAllHistory()[memoryUser]; len(history) != 1 || history[0].Content != "target history" {
		t.Fatalf("failed memory migration overwrote target history: %+v", history)
	}
	targetBook := NewMemoryBook(target)
	if _, err := targetBook.Get(competing.ID); err != nil {
		t.Fatalf("failed migration changed the target entry: %v", err)
	}
	if _, err := targetBook.Get(active.ID); !errors.Is(err, ErrMemoryNotFound) {
		t.Fatalf("failed migration copied the active entry: %v", err)
	}
	next.ChatDBPath = filepath.Join(dir, "recovery.db")
	if err := b.applyRuntimeConfig(next); err != nil {
		t.Fatalf("retrying with a compatible database: %v", err)
	}
	if got, err := b.memory.List("", 5); err != nil || len(got) != 2 {
		t.Fatalf("records after successful retry = %+v, %v", got, err)
	}
}

func TestMemoryPrivateCuesRequireTheActualPeer(t *testing.T) {
	book := NewMemoryBook(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
	req := memoryObservation(memoryGroup, 7, "group interaction with user 42")
	req.Subjects = []int64{42}
	req.Cue = MemoryTrusted
	learnMemory(t, book, req)
	query := MemoryQuery{
		ChatID: memoryUser, Participants: []int64{memoryUser, 42}, MaxItems: 5, MaxBytes: 120,
	}
	if got := visibleMemory(t, book, query); len(got) != 0 {
		t.Fatalf("another person's relationship cue appeared in a one-to-one chat: %+v", got)
	}
}

func TestMemoryChineseQueryRanksRelevantText(t *testing.T) {
	book := NewMemoryBook(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
	learnMemory(t, book, memoryObservation(memoryUser, 7, "用户偏爱普洱茶"))
	learnMemory(t, book, memoryObservation(memoryUser, 8, "新买了登山鞋"))
	query := MemoryQuery{
		ChatID: memoryUser, Participants: []int64{memoryUser}, Query: "普洱茶要怎么买？",
		MaxItems: 1, MaxBytes: 100,
	}
	if got := visibleMemory(t, book, query); len(got) != 1 || got[0].Text != "用户偏爱普洱茶" {
		t.Fatalf("single-slot query ranked unrelated text first: %+v", got)
	}
}

func TestMemoryShareRevocationIsVersionedAndLimitedToOneGroup(t *testing.T) {
	book := NewMemoryBook(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
	initial := learnMemory(t, book, memoryObservation(memoryUser, 7, "private fact"))
	one, err := book.Share(initial, memoryUser, memoryGroup, "first approved wording")
	if err != nil {
		t.Fatal(err)
	}
	two, err := book.Share(one, memoryUser, memoryOther, "second approved wording")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := book.Share(one, memoryUser, memoryOther, "stale approval"); !errors.Is(err, ErrMemoryConflict) {
		t.Fatalf("older approval after share change = %v", err)
	}
	if _, err := book.Share(two, memoryUser, memoryGroup, ""); err != nil {
		t.Fatal(err)
	}
	query := MemoryQuery{ChatID: memoryGroup, MaxItems: 5, MaxBytes: 120}
	if got := visibleMemory(t, book, query); len(got) != 0 {
		t.Fatalf("revoked group projection = %+v", got)
	}
	query.ChatID = memoryOther
	if got := visibleMemory(t, book, query); len(got) != 1 || got[0].Text != "second approved wording" {
		t.Fatalf("other group projection = %+v", got)
	}
}

func TestMemoryDivergedMigrationRollsBack(t *testing.T) {
	for _, fork := range []string{"evidence", "admin edits", "shares", "deletion"} {
		t.Run(fork, func(t *testing.T) {
			dir := t.TempDir()
			cfg := Config{ChatDBPath: filepath.Join(dir, "active.db"), StickerRulesPath: filepath.Join(dir, "stickers.json"),
				OpenAIKey: "test-key", OpenAIModel: "test-model"}
			db := openMemoryTestDB(t, cfg.ChatDBPath)
			b := &Bot{cfg: cfg, chatDB: db, store: NewHistoryStore(db), memory: NewMemoryBook(db)}
			t.Cleanup(func() { _ = b.closeChatDB() })
			b.store.Append(memoryUser, openai.ChatCompletionMessage{Role: "user", Content: "active history"}, 3)
			initial := learnMemory(t, b.memory, memoryObservation(memoryUser, 7, "common starting point"))

			next := cfg
			next.ChatDBPath = filepath.Join(dir, "target.db")
			target := openMemoryTestDB(t, next.ChatDBPath)
			if err := migrateChatDB(target, nil, NewMemoryBook(db)); err != nil {
				t.Fatal(err)
			}
			targetBook := NewMemoryBook(target)
			NewHistoryStore(target).Append(memoryUser, openai.ChatCompletionMessage{Role: "user", Content: "target history"}, 3)
			targetEvidence := memoryObservation(memoryUser, 8, "destination evidence")
			targetEvidence.Expect = initial
			activeEvidence := memoryObservation(memoryUser, 9, "active evidence")
			activeEvidence.Expect = initial
			followup := memoryObservation(memoryUser, 10, "active followup")

			revise := func(book *MemoryBook, ref MemoryRef, text string) MemoryRef {
				t.Helper()
				next, err := book.Revise(ref, MemoryEdit{Text: text})
				if err != nil {
					t.Fatal(err)
				}
				return next
			}
			share := func(book *MemoryBook, ref MemoryRef, text string) MemoryRef {
				t.Helper()
				next, err := book.Share(ref, memoryUser, memoryGroup, text)
				if err != nil {
					t.Fatal(err)
				}
				return next
			}
			switch fork {
			case "evidence", "deletion":
				learnMemory(t, targetBook, targetEvidence)
				followup.Expect = learnMemory(t, b.memory, activeEvidence)
				latest := learnMemory(t, b.memory, followup)
				if fork == "deletion" {
					if err := b.memory.Delete(latest); err != nil {
						t.Fatal(err)
					}
				}
			case "admin edits":
				revise(targetBook, revise(targetBook, initial, "destination decision"), "same current decision")
				active := revise(b.memory, initial, "active decision")
				active = revise(b.memory, active, "same current decision")
				revise(b.memory, active, "new active decision")
			case "shares":
				share(targetBook, initial, "destination approval")
				active := share(b.memory, initial, "active approval")
				share(b.memory, active, "new active approval")
			}

			wantTarget, err := targetBook.Get(initial.ID)
			if err != nil {
				t.Fatal(err)
			}
			sourceOnly := memoryObservation(memoryUser, 11, "source-only row")
			sourceOnlyRef := learnMemory(t, b.memory, sourceOnly)
			if err := target.Close(); err != nil {
				t.Fatal(err)
			}
			if err := b.applyRuntimeConfig(next); !errors.Is(err, ErrMemoryConflict) || b.snapshot().chatDB != db {
				t.Fatalf("switch with divergent revisions = %v, active path = %s", err, b.currentConfig().ChatDBPath)
			}
			if _, err := b.memory.Get(sourceOnlyRef.ID); err != nil {
				t.Fatalf("active memory unavailable after rollback: %v", err)
			}
			learnMemory(t, b.memory, memoryObservation(memoryUser, 12, "active write after rollback"))
			target = openMemoryTestDB(t, next.ChatDBPath)
			targetBook = NewMemoryBook(target)
			if got, err := targetBook.Get(initial.ID); err != nil || !reflect.DeepEqual(got, wantTarget) {
				t.Fatalf("target revision after rollback = %+v, %v; want %+v", got, err, wantTarget)
			}
			if got := target.LoadAllHistory()[memoryUser]; len(got) != 1 || got[0].Content != "target history" {
				t.Fatalf("target history after rollback = %+v", got)
			}
			if _, err := targetBook.Get(sourceOnlyRef.ID); !errors.Is(err, ErrMemoryNotFound) {
				t.Fatalf("source-only record was copied on failed migration: %v", err)
			}
			learnMemory(t, targetBook, sourceOnly)
			if fork == "evidence" || fork == "deletion" {
				if _, err := targetBook.Learn(targetEvidence); !errors.Is(err, ErrMemoryProcessed) {
					t.Fatalf("target evidence index after rollback = %v", err)
				}
				activeOnly := activeEvidence
				activeOnly.Expect = MemoryRef{}
				learnMemory(t, targetBook, activeOnly)
				if _, err := b.memory.Learn(activeEvidence); !errors.Is(err, ErrMemoryProcessed) {
					t.Fatalf("active evidence index after rollback = %v", err)
				}
			}
		})
	}
}

func TestMemoryMigrationKeepsCompatibleRevisionsAndSequence(t *testing.T) {
	dir := t.TempDir()
	activeDB := openMemoryTestDB(t, filepath.Join(dir, "active.db"))
	targetDB := openMemoryTestDB(t, filepath.Join(dir, "target.db"))
	book := NewMemoryBook(activeDB)
	first := memoryObservation(memoryUser, 7, "common starting point")
	initial := learnMemory(t, book, first)
	if err := migrateChatDB(targetDB, nil, NewMemoryBook(activeDB)); err != nil {
		t.Fatal(err)
	}
	targetBook := NewMemoryBook(targetDB)
	learnMemory(t, targetBook, memoryObservation(memoryUser, 8, "target-only row"))
	followup := memoryObservation(memoryUser, 9, "identical revision made in each database")
	followup.Expect = initial
	updated := learnMemory(t, book, followup)
	learnMemory(t, targetBook, followup)
	learnMemory(t, targetBook, memoryObservation(memoryUser, 10, "newest target-only row"))
	if err := migrateChatDB(targetDB, nil, book); err != nil {
		t.Fatalf("equal revisions with different ranking order: %v", err)
	}
	afterSwitch := learnMemory(t, book, memoryObservation(memoryUser, 11, "first write after migration"))
	query := MemoryQuery{ChatID: memoryUser, Participants: []int64{memoryUser}, MaxItems: 1, MaxBytes: 120}
	if got := visibleMemory(t, book, query); len(got) != 1 || got[0].Ref != afterSwitch {
		t.Fatalf("new write did not sort ahead of target history: %+v", got)
	}
	revoked, err := book.Revise(updated, MemoryEdit{Text: "admin revised"})
	if err != nil {
		t.Fatal(err)
	}
	if err := book.Delete(revoked); err != nil {
		t.Fatal(err)
	}
	if err := migrateChatDB(activeDB, nil, book); err != nil {
		t.Fatalf("known descendant tombstone could not overwrite its ancestor: %v", err)
	}
	if _, err := book.Get(initial.ID); !errors.Is(err, ErrMemoryNotFound) {
		t.Fatalf("ancestor was resurrected: %v", err)
	}
	for _, req := range []MemoryLearn{first, followup} {
		if _, err := book.Learn(req); !errors.Is(err, ErrMemoryProcessed) {
			t.Fatalf("event replay after migrating deletion = %v", err)
		}
	}
}

func TestMemoryPrivateCorrectionRetiresGroupInference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.db")
	db := openMemoryTestDB(t, path)
	book := NewMemoryBook(db)
	inference := memoryObservation(memoryGroup, 7, "他不重视约定")
	inference.Evidence.Kind = MemoryInference
	inference.Cue = MemoryReserved
	old := learnMemory(t, book, inference)
	unrelated := learnMemory(t, book, memoryObservation(memoryGroup, 8, "群里的下次约定"))
	query := MemoryQuery{ChatID: memoryGroup, Participants: []int64{memoryUser}, Query: "不重视约定 私密原因",
		MaxItems: 10, MaxBytes: 500}
	if got := visibleMemory(t, book, query); len(got) != 3 || got[0].Ref != old {
		t.Fatalf("group before clarification = %+v", got)
	}
	query.ChatID = memoryUser
	if got := visibleMemory(t, book, query); !reflect.DeepEqual(got, []MemoryProjection{{Cue: MemoryReserved, SubjectID: memoryUser}}) {
		t.Fatalf("private context available for interpreting the clarification = %+v", got)
	}
	query.ChatID = memoryGroup

	correction := memoryObservation(memoryUser, 9, "那次没赴约有私密原因")
	correction.Evidence.Kind = MemoryCorrection
	correction.Cue = MemoryFamiliar
	target, err := book.FindCorrectableInference(memoryUser, inference.Evidence.Source)
	if err != nil {
		t.Fatal(err)
	}
	edit := MemoryEdit{Text: correction.Text, Cue: correction.Cue}
	private, err := book.CorrectInference(target, correction.Evidence, edit)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	book = NewMemoryBook(openMemoryTestDB(t, path))

	cue := MemoryProjection{Cue: MemoryFamiliar, SubjectID: memoryUser}
	groupText := MemoryProjection{Ref: unrelated, Text: "群里的下次约定"}
	if got := visibleMemory(t, book, query); !reflect.DeepEqual(got, []MemoryProjection{groupText, cue}) {
		t.Fatalf("group after private clarification = %+v", got)
	}
	query.ChatID = memoryUser
	privateText := MemoryProjection{Ref: private, Text: correction.Text}
	if got := visibleMemory(t, book, query); !reflect.DeepEqual(got, []MemoryProjection{privateText, cue}) {
		t.Fatalf("private projections after clarification = %+v", got)
	}
	query.ChatID = memoryOther
	if got := visibleMemory(t, book, query); !reflect.DeepEqual(got, []MemoryProjection{cue}) {
		t.Fatalf("third-party group after clarification = %+v", got)
	}
	query.ChatID, query.Participants = 42, []int64{42, memoryUser}
	if got := visibleMemory(t, book, query); len(got) != 0 {
		t.Fatalf("third-party private projections after clarification = %+v", got)
	}
	oldEntry, err := book.Get(old.ID)
	if err != nil || !oldEntry.Retired || oldEntry.Ref.Version != old.Version+1 ||
		oldEntry.Text != inference.Text || !reflect.DeepEqual(oldEntry.Evidence, []MemoryEvidence{inference.Evidence}) {
		t.Fatalf("admin view of retired entry = %+v, %v", oldEntry, err)
	}
	if _, err := book.CorrectInference(target, correction.Evidence, edit); !errors.Is(err, ErrMemoryProcessed) {
		t.Fatalf("replaying dedicated correction = %v", err)
	}
	if _, err := book.Learn(correction); !errors.Is(err, ErrMemoryProcessed) {
		t.Fatalf("replaying correction via Learn = %v", err)
	}
	if _, err := book.Learn(inference); !errors.Is(err, ErrMemoryProcessed) {
		t.Fatalf("replaying old group event = %v", err)
	}
	stale := memoryObservation(memoryGroup, 10, "new attempt to use old inference")
	stale.Expect = old
	if _, err := book.Learn(stale); !errors.Is(err, ErrMemoryConflict) {
		t.Fatalf("stale group update after clarification = %v", err)
	}
	stale.Expect = oldEntry.Ref
	if _, err := book.Learn(stale); !errors.Is(err, ErrMemoryDenied) {
		t.Fatalf("automatic update of retired inference = %v", err)
	}
	if _, err := book.Share(private, memoryUser, memoryOther, "那次是有原因的"); err != nil {
		t.Fatal(err)
	}
	query.ChatID, query.Participants = memoryOther, nil
	if got := visibleMemory(t, book, query); len(got) != 1 || got[0].Text != "那次是有原因的" {
		t.Fatalf("third-party group with explicit approval = %+v", got)
	}
	query.ChatID = memoryGroup
	if got := visibleMemory(t, book, query); !reflect.DeepEqual(got, []MemoryProjection{groupText}) {
		t.Fatalf("approval for another group affected the original group: %+v", got)
	}
}

func TestMemoryCorrectionTargetsOnlyCurrentPersonalInference(t *testing.T) {
	book := NewMemoryBook(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
	inference := memoryObservation(memoryGroup, 7, "current inference")
	inference.Evidence.Kind = MemoryInference
	old := learnMemory(t, book, inference)
	legitimate := inference.Evidence.Source
	tampered := legitimate
	tampered.SenderID = 42
	if _, err := book.FindCorrectableInference(memoryUser, tampered); !errors.Is(err, ErrMemoryDenied) {
		t.Fatalf("mismatched group event sender = %v", err)
	}
	if _, err := book.FindCorrectableInference(42, legitimate); !errors.Is(err, ErrMemoryDenied) {
		t.Fatalf("different person targeting a personal inference = %v", err)
	}
	fact := memoryObservation(memoryGroup, 8, "group fact")
	learnMemory(t, book, fact)
	mixed := memoryObservation(memoryGroup, 9, "group inference backed by a connector grant")
	mixed.Evidence.Kind = MemoryInference
	mixed.Evidence.Source.Access = MemoryDataAccess{OwnerID: memoryUser, Key: "calendar"}
	learnMemory(t, book, mixed)
	multi := memoryObservation(memoryGroup, 10, "inference about multiple people")
	multi.Evidence.Kind = MemoryInference
	multi.Subjects = []int64{memoryUser, 42}
	learnMemory(t, book, multi)
	for _, observation := range []MemoryLearn{fact, mixed, multi} {
		if _, err := book.FindCorrectableInference(memoryUser, observation.Evidence.Source); !errors.Is(err, ErrMemoryDenied) {
			t.Fatalf("target outside the narrow correction scope %q = %v", observation.Text, err)
		}
	}
	target, err := book.FindCorrectableInference(memoryUser, legitimate)
	if err != nil {
		t.Fatal(err)
	}
	correction := memoryObservation(memoryUser, 11, "private reason").Evidence
	correction.Kind = MemoryCorrection
	bystander := correction
	bystander.Source.ChatID, bystander.Source.SenderID = 42, 42
	if _, err := book.CorrectInference(target, bystander, MemoryEdit{Text: "unsupported"}); !errors.Is(err, ErrMemoryDenied) {
		t.Fatalf("different person submitting the correction = %v", err)
	}
	if _, err := book.CorrectInference(target, correction, MemoryEdit{Text: "global habit", Cue: MemoryAskFirst}); !errors.Is(err, ErrMemoryInvalid) {
		t.Fatalf("correction carrying a global habit = %v", err)
	}
	next := memoryObservation(memoryGroup, 12, "updated group inference")
	next.Expect = old
	next.Evidence.Kind = MemoryInference
	recent := learnMemory(t, book, next)
	if _, err := book.FindCorrectableInference(memoryUser, legitimate); !errors.Is(err, ErrMemoryDenied) {
		t.Fatalf("finding a new version through old evidence = %v", err)
	}
	if _, err := book.CorrectInference(target, correction, MemoryEdit{Text: "private replacement"}); !errors.Is(err, ErrMemoryConflict) {
		t.Fatalf("correcting an out-of-date inference = %v", err)
	}
	target, err = book.FindCorrectableInference(memoryUser, next.Evidence.Source)
	if err != nil {
		t.Fatal(err)
	}
	revised, err := book.Revise(recent, MemoryEdit{Text: "admin decision"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := book.CorrectInference(target, correction, MemoryEdit{Text: "private replacement"}); !errors.Is(err, ErrMemoryConflict) {
		t.Fatalf("correcting a version before admin revision = %v", err)
	}
	if _, err := book.FindCorrectableInference(memoryUser, next.Evidence.Source); !errors.Is(err, ErrMemoryDenied) {
		t.Fatalf("targeting the admin version = %v", err)
	}
	afterAdmin := memoryObservation(memoryGroup, 13, "new inference after admin decision")
	afterAdmin.Expect = revised
	afterAdmin.Evidence.Kind = MemoryInference
	learnMemory(t, book, afterAdmin)
	if _, err := book.FindCorrectableInference(memoryUser, afterAdmin.Evidence.Source); !errors.Is(err, ErrMemoryDenied) {
		t.Fatalf("looking up a previously admin-revised entry after learning = %v", err)
	}
	query := MemoryQuery{ChatID: memoryUser, Participants: []int64{memoryUser}, MaxItems: 5, MaxBytes: 120}
	if got := visibleMemory(t, book, query); len(got) != 0 {
		t.Fatalf("failed correction left a private projection: %+v", got)
	}
	learnMemory(t, book, MemoryLearn{Subjects: []int64{memoryUser}, Evidence: correction, Text: "private fallback"})
}

func TestMemoryConcurrentCorrectionAndAdminDelete(t *testing.T) {
	book := NewMemoryBook(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
	for i := int64(1); i <= 16; i++ {
		inference := memoryObservation(memoryGroup, i, "group inference")
		inference.Evidence.Kind = MemoryInference
		ref := learnMemory(t, book, inference)
		target, err := book.FindCorrectableInference(memoryUser, inference.Evidence.Source)
		if err != nil {
			t.Fatal(err)
		}
		private := memoryObservation(memoryUser, i, "private reason")
		private.Evidence.Kind = MemoryCorrection
		start := make(chan struct{})
		deleted, corrected := make(chan error, 1), make(chan error, 1)
		go func() {
			<-start
			deleted <- book.Delete(ref)
		}()
		go func() {
			<-start
			_, err := book.CorrectInference(target, private.Evidence, MemoryEdit{Text: private.Text})
			corrected <- err
		}()
		close(start)
		deleteErr, correctionErr := <-deleted, <-corrected
		switch {
		case deleteErr == nil && errors.Is(correctionErr, ErrMemoryNotFound):
			learnMemory(t, book, private)
		case errors.Is(deleteErr, ErrMemoryConflict) && correctionErr == nil:
			entry, err := book.Get(ref.ID)
			if err != nil || !entry.Retired {
				t.Fatalf("entry after the correction won = %+v, %v", entry, err)
			}
			if err := book.Delete(entry.Ref); err != nil {
				t.Fatal(err)
			}
			if _, err := book.Learn(private); !errors.Is(err, ErrMemoryProcessed) {
				t.Fatalf("private event already committed = %v", err)
			}
		default:
			t.Fatalf("concurrent delete and correction = %v, %v", deleteErr, correctionErr)
		}
		if _, err := book.Learn(inference); !errors.Is(err, ErrMemoryProcessed) {
			t.Fatalf("old group event after delete = %v", err)
		}
	}
	query := MemoryQuery{ChatID: memoryGroup, MaxItems: 20, MaxBytes: 1000}
	if got := visibleMemory(t, book, query); len(got) != 0 {
		t.Fatalf("group projections after concurrent deletes = %+v", got)
	}
}

func TestMemoryUnchangedCorroborationKeepsApprovedShare(t *testing.T) {
	book := NewMemoryBook(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
	first := memoryObservation(memoryUser, 7, "同一项日程")
	ref := learnMemory(t, book, first)
	shared, err := book.Share(ref, memoryUser, memoryGroup, "已约好见面")
	if err != nil {
		t.Fatal(err)
	}
	access := MemoryDataAccess{OwnerID: memoryUser, Key: "current-calendar-grant"}
	corroboration := memoryObservation(memoryUser, 8, first.Text)
	corroboration.Expect = shared
	corroboration.Evidence.Source.Access = access
	confirmed := learnMemory(t, book, corroboration)

	query := MemoryQuery{ChatID: memoryGroup, MaxItems: 5, MaxBytes: 120}
	if got := visibleMemory(t, book, query); len(got) != 0 {
		t.Fatalf("without current grant = %+v", got)
	}
	query.DataAccess = map[MemoryDataAccess]bool{access: true}
	if got := visibleMemory(t, book, query); !reflect.DeepEqual(got, []MemoryProjection{{Ref: confirmed, Text: "已约好见面"}}) {
		t.Fatalf("approved wording after corroboration = %+v", got)
	}

	correction := memoryObservation(memoryUser, 9, first.Text)
	correction.Expect = confirmed
	correction.Evidence.Kind = MemoryCorrection
	learnMemory(t, book, correction)
	if got := visibleMemory(t, book, query); len(got) != 0 {
		t.Fatalf("correction kept the old approval: %+v", got)
	}
}

func TestMemorySubjectCanRevokeShareAfterThirdPartyCorroboration(t *testing.T) {
	book := NewMemoryBook(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
	first := memoryObservation(memoryUser, 7, "已有约定")
	ref := learnMemory(t, book, first)
	ref, err := book.Share(ref, memoryUser, memoryGroup, "已经约好")
	if err != nil {
		t.Fatal(err)
	}
	access := MemoryDataAccess{OwnerID: memoryUser + 1, Key: "shared-calendar"}
	evidence := memoryObservation(memoryUser, 8, first.Text)
	evidence.Expect = ref
	evidence.Evidence.Source.Access = access
	ref = learnMemory(t, book, evidence)
	query := MemoryQuery{ChatID: memoryGroup, DataAccess: map[MemoryDataAccess]bool{access: true}, MaxItems: 2, MaxBytes: 100}
	if got := visibleMemory(t, book, query); len(got) != 1 || got[0].Text != "已经约好" {
		t.Fatalf("existing share = %+v", got)
	}
	if _, err := book.Share(ref, access.OwnerID, memoryGroup, ""); !errors.Is(err, ErrMemoryDenied) {
		t.Fatalf("connector owner revoking subject's share: %v", err)
	}
	if _, err := book.Share(ref, memoryUser, memoryGroup, "更多信息"); !errors.Is(err, ErrMemoryDenied) {
		t.Fatalf("expanding the share with third-party data: %v", err)
	}
	if _, err := book.Share(ref, memoryUser, memoryGroup, ""); err != nil {
		t.Fatal(err)
	}
	if got := visibleMemory(t, book, query); len(got) != 0 {
		t.Fatalf("after revocation = %+v", got)
	}
}
