package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	bolt "go.etcd.io/bbolt"
)

const (
	engagementOwner = int64(71)
	engagementPeer  = int64(72)
	engagementGroup = int64(-701)
)

var engagementNow = time.Date(2026, 11, 26, 3, 0, 0, 0, time.UTC)

func engagementRequest(chat, user, message int64, text string) EngagementRequest {
	return EngagementRequest{Source: MemorySource{ChatID: chat, SenderID: user, MessageID: message}, Text: text}
}

func engagementFixture(chatID int64) EngagementCreate {
	read := EngagementCapability{Access: MemoryDataAccess{Key: "public-releases"}, Operation: "read", Target: "upstream/releases", DataChatID: chatID}
	agreement := EngagementAgreement{
		Observe: read, Trigger: EngagementText{ChatID: chatID, Text: "stable releases"},
		Recipients: []int64{chatID}, NextCheck: engagementNow, Once: true,
	}
	return EngagementCreate{
		Kind: EngagementTracking,
		Decision: EngagementDecision{
			Request: engagementRequest(chatID, engagementOwner, 1, "tell me about upstream releases"),
			Verdict: EngagementAccept, Change: EngagementAgree, Agreement: &agreement,
			Authorized: EngagementAuthorization{Capabilities: []EngagementCapability{read}, Recipients: []int64{chatID}},
			At:         engagementNow,
		},
	}
}

func addEngagementAction(create *EngagementCreate) EngagementCapability {
	write := EngagementCapability{
		Access:    MemoryDataAccess{OwnerID: engagementOwner, Key: "personal-calendar/instance-1"},
		Operation: "calendar.insert", Target: "calendar/work", DataChatID: engagementOwner, Write: true,
	}
	create.Kind = EngagementDelegation
	create.Decision.Agreement.Actions = []EngagementCapability{write}
	create.Decision.Authorized.Capabilities = append(create.Decision.Authorized.Capabilities, write)
	return write
}

func createEngagement(t *testing.T, store *EngagementStore, create EngagementCreate) EngagementRef {
	t.Helper()
	ref, err := store.Create(create)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func reviseEngagement(t *testing.T, store *EngagementStore, ref EngagementRef, decision EngagementDecision) EngagementRef {
	t.Helper()
	next, err := store.Decide(ref, decision)
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func engagementClaim(t *testing.T, store *EngagementStore, now time.Time) EngagementClaim {
	t.Helper()
	claims, err := store.ClaimDue(now, time.Minute, 10)
	if err != nil || len(claims) != 1 {
		t.Fatalf("ClaimDue = %+v, %v", claims, err)
	}
	return claims[0]
}

func TestEngagementRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.db")
	db := openMemoryTestDB(t, path)
	create := engagementFixture(engagementOwner)
	write := addEngagementAction(&create)
	create.Reason = EngagementText{ChatID: engagementOwner, Text: "private health reason"}
	create.People = []EngagementPerson{{ID: engagementPeer, ChatID: engagementOwner}}
	create.Evidence = []MemoryEvidence{
		{Kind: MemoryStatement, Source: create.Decision.Request.Source, Text: "only I may change this"},
		{Kind: MemoryInference, Source: create.Decision.Request.Source, Text: "expects the work calendar"},
	}
	create.InitiatorOnly = true
	create.Decision.Agreement.EndCondition = EngagementText{ChatID: engagementOwner, Text: "when the project is archived"}
	create.Decision.Agreement.ExpiresAt = engagementNow.AddDate(0, 1, 0)
	b := &Bot{chatDB: db, store: NewHistoryStore(db), summaries: NewSummaryStore(db), engagements: NewEngagementStore(db)}
	NewTaskStore(db).Upsert(engagementOwner, ScheduledTask{ID: "legacy-cron", Prompt: "original schedule", CronExpr: "0 8 * * *"})
	NewReminderStore(db).Insert(Reminder{ID: "legacy-reminder", UserID: engagementOwner, ChatID: engagementOwner, Prompt: "original reminder"})
	ref := createEngagement(t, b.snapshot().engagements, create)
	d := EngagementDecision{Request: engagementRequest(engagementOwner, engagementOwner, 2, "did that include reminders?"), Verdict: EngagementClarify,
		Reply: EngagementText{ChatID: engagementOwner, Text: "which reminders?"}, At: engagementNow.Add(time.Minute)}
	ref = reviseEngagement(t, b.engagements, ref, d)
	b.store.Append(engagementOwner, openai.ChatCompletionMessage{Role: "user", Content: "ephemeral"}, 10)
	if cleared, err := b.clearRoleChat(engagementOwner, func() error { return nil }); err != nil || !cleared {
		t.Fatalf("clear = %v, %v", cleared, err)
	}
	if err := b.closeChatDB(); err != nil {
		t.Fatal(err)
	}
	db = openMemoryTestDB(t, path)
	if len(db.LoadAllHistory()[engagementOwner]) != 0 {
		t.Fatal("cleared history returned")
	}
	if tasks := NewTaskStore(db).List(engagementOwner); len(tasks) != 1 || tasks[0].Prompt != "original schedule" {
		t.Fatalf("legacy cron after clear and reopen = %+v", tasks)
	}
	if reminder, ok := NewReminderStore(db).Get(engagementOwner, "legacy-reminder"); !ok || reminder.Prompt != "original reminder" {
		t.Fatalf("legacy reminder after clear and reopen = %+v, %v", reminder, ok)
	}
	store := NewEngagementStore(db)
	item, err := store.Get(ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	if item.Ref != ref || item.Origin != create.Decision.Request.Source || item.Kind != EngagementDelegation ||
		item.Reason != create.Reason || len(item.People) != 1 || item.People[0].ID != engagementPeer ||
		!item.InitiatorOnly || len(item.Evidence) != 2 ||
		item.Evidence[0].Text != "only I may change this" || item.Evidence[1].Kind != MemoryInference {
		t.Fatalf("reopened agreement basis = %+v", item)
	}
	if item.Initial == nil || item.Initial.Effective.Actions[0] != write ||
		item.Initial.Effective.Recipients[0] != engagementOwner ||
		item.Initial.Effective.EndCondition.Text != "when the project is archived" ||
		!item.Initial.Effective.ExpiresAt.Equal(engagementNow.AddDate(0, 1, 0)) ||
		len(item.Decisions) != 2 || item.Decisions[0].Request.Text != "tell me about upstream releases" ||
		item.Decisions[1].Verdict != EngagementClarify || item.Decisions[1].Effective.Actions[0] != write {
		t.Fatalf("reopened initial agreement and decisions = %+v", item)
	}
	if _, err := store.Create(create); !errors.Is(err, ErrEngagementProcessed) {
		t.Fatalf("replayed creation = %v", err)
	}
	view := EngagementViewer{ChatID: engagementOwner, ActorID: engagementOwner, DataAccess: map[MemoryDataAccess]bool{write.Access: true}}
	projected, err := store.ReadVisible(ref.ID, view)
	if err != nil || projected.Reason != "private health reason" || len(projected.Evidence) != 2 ||
		len(projected.Agreement.Actions) != 1 || projected.Agreement.Actions[0].OwnerID != engagementOwner {
		t.Fatalf("authorized private projection = %+v, %v", projected, err)
	}
}

func TestEngagementPrivateTermsInSharedManagement(t *testing.T) {
	store := NewEngagementStore(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
	create := engagementFixture(engagementOwner)
	create.Decision.Management = &EngagementManagementEdit{Summary: "private management explanation", InitiatorOnly: true}
	create.ManageChats = []int64{engagementGroup}
	create.Decision.Agreement.ExpiresAt = engagementNow.AddDate(0, 2, 0)
	create.Decision.Agreement.EndCondition = EngagementText{ChatID: engagementOwner, Text: "private deadline basis"}
	ref := createEngagement(t, store, create)
	view := EngagementViewer{ChatID: engagementGroup, ActorID: engagementPeer}
	projected, err := store.ReadVisible(ref.ID, view)
	if err != nil || !projected.InitiatorOnly || !projected.MayChange || projected.InitiatorID != 0 ||
		projected.RequesterIsInitiator || projected.Management != nil || len(projected.Decisions) != 0 {
		t.Fatalf("shared management conclusion = %+v, %v", projected, err)
	}
	view.ActorID = engagementOwner
	if own, err := store.ReadVisible(ref.ID, view); err != nil || !own.RequesterIsInitiator || own.InitiatorID != 0 {
		t.Fatalf("authenticated self-check with private origin = %+v, %v", own, err)
	}
	body, err := json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{`"Once"`, `"ExpiresAt"`, "private deadline basis", "upstream releases"} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("management-only group knows private term %q: %s", secret, body)
		}
	}
}

func TestEngagementVisibility(t *testing.T) {
	store := NewEngagementStore(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
	create := engagementFixture(engagementGroup)
	write := addEngagementAction(&create)
	private := engagementRequest(engagementOwner, engagementOwner, 97, "private original")
	create.InitiatorOnly = true
	create.Reason = EngagementText{ChatID: engagementOwner, Text: "private reason"}
	create.People = []EngagementPerson{{ID: 777, ChatID: engagementOwner}}
	create.Evidence = []MemoryEvidence{
		{Kind: MemoryStatement, Source: private.Source, Text: private.Text},
		{Kind: MemoryInference, Source: create.Decision.Request.Source, Text: "group release work"},
	}
	create.Decision.Agreement.EndCondition = EngagementText{ChatID: engagementGroup, Text: "internal connector signal", Access: write.Access}
	ref := createEngagement(t, store, create)

	audience := EngagementViewer{ChatID: engagementGroup, ActorID: engagementPeer, DataAccess: map[MemoryDataAccess]bool{write.Access: true}}
	projections, err := store.Visible(audience, "", 5)
	if err != nil || len(projections) != 1 {
		t.Fatalf("group projection = %+v, %v", projections, err)
	}
	got, _ := json.Marshal(projections[0])
	for _, secret := range []string{"private reason", "private original", "777", "calendar/", "personal-calendar", ref.ID} {
		if strings.Contains(string(got), secret) {
			t.Fatalf("group projection contains %q: %s", secret, got)
		}
	}
	if !projections[0].MayChange || !projections[0].InitiatorOnly || !strings.Contains(string(got), "group release work") {
		t.Fatalf("group management conclusion: %s", got)
	}
	audience.DataAccess = nil
	projected, err := store.ReadVisible(ref.ID, audience)
	if err != nil || projected.Agreement.EndCondition != "" {
		t.Fatalf("connector-gated group text = %+v, %v", projected, err)
	}
	if _, err := store.ReadVisible(ref.ID, EngagementViewer{ChatID: -702, ActorID: engagementOwner}); !errors.Is(err, ErrEngagementDenied) {
		t.Fatalf("unrelated group lookup = %v", err)
	}

	decision := EngagementDecision{Request: engagementRequest(engagementGroup, engagementPeer, 2, "end this"), Verdict: EngagementReject, At: engagementNow}
	next := reviseEngagement(t, store, ref, decision)
	item, err := store.Get(ref.ID)
	if err != nil || item.Ref != next || item.Status != EngagementActive || item.Decisions[1].Request.Source.SenderID != engagementPeer ||
		item.Decisions[1].Verdict != EngagementReject || item.Decisions[1].Effective.Actions[0] != write {
		t.Fatalf("rejected request audit = %+v, %v", item, err)
	}
}

func TestEngagementRevision(t *testing.T) {
	store := NewEngagementStore(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
	ref := createEngagement(t, store, engagementFixture(engagementGroup))
	start := make(chan struct{})
	results := make(chan error, 2)
	for i, change := range []EngagementChange{EngagementPause, EngagementEnd} {
		go func() {
			<-start
			_, err := store.Decide(ref, EngagementDecision{
				Request: engagementRequest(engagementGroup, engagementPeer, int64(i+2), "manage the group task"),
				Verdict: EngagementAccept, Change: change, At: engagementNow,
			})
			results <- err
		}()
	}
	close(start)
	one, two := <-results, <-results
	if one != nil && two == nil {
		one, two = two, one
	}
	if one != nil || !errors.Is(two, ErrEngagementConflict) {
		t.Fatalf("concurrent decisions = %v, %v", one, two)
	}
	current, err := store.Get(ref.ID)
	if err != nil || current.Ref.Version != 2 {
		t.Fatalf("current revision = %+v, %v", current, err)
	}
	if current.Status == EngagementPaused {
		current.Ref = reviseEngagement(t, store, current.Ref, EngagementDecision{
			Request: engagementRequest(engagementGroup, engagementPeer, 4, "end it"),
			Verdict: EngagementAccept, Change: EngagementEnd, At: engagementNow,
		})
	}
	resume := EngagementDecision{Request: engagementRequest(engagementGroup, engagementPeer, 5, "resume it"),
		Verdict: EngagementAccept, Change: EngagementResume, At: engagementNow}
	if _, err := store.Decide(ref, resume); !errors.Is(err, ErrEngagementConflict) {
		t.Fatalf("stale decision after end = %v", err)
	}
	if _, err := store.Decide(current.Ref, resume); !errors.Is(err, ErrEngagementTerminal) {
		t.Fatalf("fresh resurrection = %v", err)
	}
}

func TestEngagementScopeAndReacceptance(t *testing.T) {
	store := NewEngagementStore(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
	create := engagementFixture(engagementOwner)
	create.Kind = EngagementDelegation
	initial := create.Decision
	create.Decision.Verdict, create.Decision.Change, create.Decision.Agreement = EngagementReject, "", nil
	ref := createEngagement(t, store, create)
	if claims, err := store.ClaimDue(engagementNow, time.Minute, 10); err != nil || len(claims) != 0 {
		t.Fatalf("declined proposal is due: %+v, %v", claims, err)
	}
	initial.Request = engagementRequest(engagementOwner, engagementOwner, 2, "just notify me then")
	ref = reviseEngagement(t, store, ref, initial)

	create.Decision = engagementFixture(engagementOwner).Decision
	write := addEngagementAction(&create)
	expansion := create.Decision
	expansion.Request = engagementRequest(engagementOwner, engagementOwner, 3, "also add it to my calendar")
	if _, err := store.Decide(ref, expansion); !errors.Is(err, ErrEngagementDenied) {
		t.Fatalf("ordinary expansion with newly available tool = %v", err)
	}
	expansion.Request.ExplicitReaccept = true
	expansion.Change = EngagementReaccept
	allowed := expansion.Authorized
	noConsent := expansion
	noConsent.Request.ExplicitReaccept = false
	if _, err := store.Decide(ref, noConsent); !errors.Is(err, ErrEngagementDenied) {
		t.Fatalf("model labeled an ordinary request as reacceptance: %v", err)
	}
	expansion.Authorized.Capabilities = allowed.Capabilities[:1]
	if _, err := store.Decide(ref, expansion); !errors.Is(err, ErrEngagementDenied) {
		t.Fatalf("explicit expansion without verified capability = %v", err)
	}
	expansion.Authorized = allowed
	ref = reviseEngagement(t, store, ref, expansion)
	item, err := store.Get(ref.ID)
	if err != nil || len(item.Initial.Effective.Actions) != 0 || item.Initial.Request.Source.MessageID != 2 ||
		len(item.Decisions) != 3 || item.Current.Actions[0] != write ||
		item.Decisions[2].Change != EngagementReaccept {
		t.Fatalf("initial and current scope = %+v, %v", item, err)
	}

	for _, test := range []struct {
		name   string
		change func(*EngagementAgreement)
	}{
		{"account", func(a *EngagementAgreement) { a.Actions[0].Access.OwnerID = 88 }},
		{"connector", func(a *EngagementAgreement) { a.Actions[0].Access.Key = "same-tool/new-instance" }},
		{"target", func(a *EngagementAgreement) { a.Actions[0].Target = "calendar/other" }},
		{"data audience", func(a *EngagementAgreement) { a.Actions[0].DataChatID = engagementGroup }},
		{"recipient", func(a *EngagementAgreement) { a.Recipients = []int64{-750} }},
		{"recurrence", func(a *EngagementAgreement) { a.Once = false }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := expansion
			changed.Request = engagementRequest(engagementOwner, engagementOwner, 4, "adjust the existing task")
			changed.Change = EngagementAgree
			agreement := *changed.Agreement
			agreement.Actions = slices.Clone(agreement.Actions)
			test.change(&agreement)
			changed.Agreement = &agreement
			changed.Authorized = EngagementAuthorization{
				Capabilities: append([]EngagementCapability{agreement.Observe}, agreement.Actions...),
				Recipients:   agreement.Recipients,
			}
			if _, err := store.Decide(ref, changed); !errors.Is(err, ErrEngagementDenied) {
				t.Fatalf("ordinary scope change = %v", err)
			}
		})
	}
	tracking := engagementFixture(engagementOwner)
	addEngagementAction(&tracking)
	tracking.Kind = EngagementTracking
	tracking.Decision.Request.Source.MessageID = 5
	if _, err := store.Create(tracking); !errors.Is(err, ErrEngagementDenied) {
		t.Fatalf("autonomous tracking granted external write: %v", err)
	}
}

func TestEngagementGroupEdit(t *testing.T) {
	store := NewEngagementStore(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
	create := engagementFixture(engagementGroup)
	create.Decision.Agreement.Trigger.Text = "any release"
	ref := createEngagement(t, store, create)
	changed := create.Decision
	changed.Request = engagementRequest(engagementGroup, engagementPeer, 2, "only stable versions please")
	changed.Agreement.Trigger.Text = "stable releases"
	changed.Authorized.TermEdits = []EngagementTermGrant{{
		Term: EngagementTriggerTerm, Value: changed.Agreement.Trigger, Source: changed.Request.Source,
	}}
	next := reviseEngagement(t, store, ref, changed)
	p, err := store.ReadVisible(ref.ID, EngagementViewer{ChatID: engagementGroup, ActorID: engagementPeer})
	if err != nil || p.Ref != next || p.InitiatorID != engagementOwner || !p.MayChange || p.Agreement.Trigger != "stable releases" {
		t.Fatalf("group scope after authorized change = %+v, %v", p, err)
	}
	item, err := store.Get(ref.ID)
	if err != nil || item.Initial.Effective.Trigger.Text != "any release" ||
		item.Decisions[1].Request.Source.SenderID != engagementPeer || item.Decisions[1].Effective.Trigger.Text != "stable releases" ||
		item.Current.TriggerSource != changed.Request.Source {
		t.Fatalf("historical agreement after group edit = %+v, %v", item, err)
	}
	if _, err := store.Decide(next, changed); !errors.Is(err, ErrEngagementProcessed) {
		t.Fatalf("same management request was accepted twice: %v", err)
	}
}

func TestEngagementRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.db")
	db := openMemoryTestDB(t, path)
	store := NewEngagementStore(db)
	create := engagementFixture(engagementOwner)
	write := addEngagementAction(&create)
	create.Decision.Agreement.Recipients = []int64{engagementOwner, engagementGroup}
	create.Decision.Authorized.Recipients = []int64{engagementOwner, engagementGroup}
	ref := createEngagement(t, store, create)
	claim := engagementClaim(t, store, engagementNow)
	if others, err := store.ClaimDue(engagementNow, time.Minute, 10); err != nil || len(others) != 0 {
		t.Fatalf("same due item claimed twice: %+v, %v", others, err)
	}

	observation := EngagementObservation{ChangeKey: "v2.0", Evidence: EngagementText{ChatID: engagementOwner, Text: "verified stable release"}}
	observed, fresh, err := store.Observe(claim, engagementNow, observation)
	if err != nil || !fresh {
		t.Fatalf("record observation = %v, %v", fresh, err)
	}
	action := EngagementStep{Action: write}
	actionResult, execute, err := store.BeginStep(claim, engagementNow, observed.ChangeKey, action)
	if err != nil || !execute {
		t.Fatalf("reserve action = %+v, %v, %v", actionResult, execute, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openMemoryTestDB(t, path)
	store = NewEngagementStore(db)
	now := engagementNow.Add(time.Minute)
	recovered := engagementClaim(t, store, now)
	if _, _, err := store.BeginStep(claim, now, observed.ChangeKey, action); !errors.Is(err, ErrEngagementLease) {
		t.Fatalf("expired worker can attempt action: %v", err)
	}
	replayed, execute, err := store.BeginStep(recovered, now, observed.ChangeKey, action)
	if err != nil || execute || replayed.State != EngagementReserved || replayed.Attempt != actionResult.Attempt || replayed.Key != actionResult.Key {
		t.Fatalf("restart lost uncertain call reservation: %+v, %v, %v", replayed, execute, err)
	}
	if err := store.RecordOutcome(recovered, now, observed.ChangeKey, action, replayed.Attempt, EngagementSucceeded, "verified event ID 9"); err != nil {
		t.Fatal(err)
	}
	for _, chat := range []int64{engagementOwner, engagementGroup} {
		step := EngagementStep{Recipient: chat}
		reservation, send, err := store.BeginStep(recovered, now, observed.ChangeKey, step)
		if err != nil || !send {
			t.Fatalf("reserve delivery to %d: %+v, %v, %v", chat, reservation, send, err)
		}
		state, receipt := EngagementSucceeded, "private Telegram message 10"
		if chat == engagementGroup {
			state, receipt = EngagementNoEffect, "Telegram rejected request"
		}
		if err := store.RecordOutcome(recovered, now, observed.ChangeKey, step, reservation.Attempt, state, receipt); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.FinishCheck(recovered, now, EngagementCheck{CompletedChange: observed.ChangeKey}); !errors.Is(err, ErrEngagementIncomplete) {
		t.Fatalf("one-time work finished with an undelivered recipient: %v", err)
	}
	pause := EngagementDecision{Request: engagementRequest(engagementOwner, engagementOwner, 2, "pause it"),
		Verdict: EngagementAccept, Change: EngagementPause, At: now}
	ref = reviseEngagement(t, store, ref, pause)
	if claims, err := store.ClaimDue(now.Add(time.Minute), time.Minute, 10); err != nil || len(claims) != 0 {
		t.Fatalf("paused work was claimed: %+v, %v", claims, err)
	}
	resume := pause
	resume.Request.Source.MessageID = 3
	resume.Change = EngagementResume
	resume.At = now.Add(time.Minute)
	if _, err := store.Decide(ref, resume); !errors.Is(err, ErrEngagementDenied) {
		t.Fatalf("resume without current authorization = %v", err)
	}
	resume.Authorized = create.Decision.Authorized
	reviseEngagement(t, store, ref, resume)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openMemoryTestDB(t, path)
	store = NewEngagementStore(db)
	now = resume.At
	claim = engagementClaim(t, store, now)
	item, err := store.ReadClaim(claim, now)
	if err != nil || !item.LastObservedAt.Equal(engagementNow) || item.LastDeliveredAt.Equal(item.LastObservedAt) ||
		!item.LastDeliveredAt.Equal(engagementNow.Add(time.Minute)) {
		t.Fatalf("observation and delivery timestamps = %+v, %v", item, err)
	}
	work, err := store.Work(ref.ID, "", 10)
	if err != nil || len(work) != 1 || work[0].Observation.Evidence.Text != "verified stable release" || len(work[0].Results) != 3 {
		t.Fatalf("persisted observation and work = %+v, %v", work, err)
	}
	if _, fresh, err := store.Observe(claim, now, observation); err != nil || fresh {
		t.Fatalf("same change was recorded again: %v, %v", fresh, err)
	}
	if _, _, err := store.Observe(claim, now, EngagementObservation{ChangeKey: "v2.1"}); !errors.Is(err, ErrEngagementDenied) {
		t.Fatalf("one-time work took a second change: %v", err)
	}
	for _, step := range []EngagementStep{action, {Recipient: engagementOwner}} {
		if result, start, err := store.BeginStep(claim, now, observed.ChangeKey, step); err != nil || start || result.State != EngagementSucceeded {
			t.Fatalf("successful effect was replayed: %+v, %v, %v", result, start, err)
		}
	}
	group := EngagementStep{Recipient: engagementGroup}
	result, start, err := store.BeginStep(claim, now, observed.ChangeKey, group)
	if err != nil || !start || result.Attempt != 2 {
		t.Fatalf("known failed recipient could not retry: %+v, %v, %v", result, start, err)
	}
	if err := store.RecordOutcome(claim, now, observed.ChangeKey, group, 1, EngagementSucceeded, "late old attempt"); !errors.Is(err, ErrEngagementConflict) {
		t.Fatalf("stale attempt overwrote pending retry: %v", err)
	}
	if err := store.RecordOutcome(claim, now, observed.ChangeKey, group, result.Attempt, EngagementSucceeded, "group Telegram message 11"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishCheck(claim, now, EngagementCheck{CompletedChange: observed.ChangeKey}); err != nil {
		t.Fatal(err)
	}
	if claims, err := store.ClaimDue(now.Add(time.Hour), time.Minute, 10); err != nil || len(claims) != 0 {
		t.Fatalf("completed work was reclaimed: %+v, %v", claims, err)
	}
}

func TestEngagementConcurrentClaimAndReservation(t *testing.T) {
	store := NewEngagementStore(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
	create := engagementFixture(engagementOwner)
	action := EngagementStep{Action: addEngagementAction(&create)}
	createEngagement(t, store, create)
	const workers = 16
	type response struct {
		claims []EngagementClaim
		err    error
	}
	claims := make(chan response, workers)
	start := make(chan struct{})
	for range workers {
		go func() {
			<-start
			items, err := store.ClaimDue(engagementNow, time.Minute, 1)
			claims <- response{items, err}
		}()
	}
	close(start)
	var winners []EngagementClaim
	for range workers {
		got := <-claims
		if got.err != nil {
			t.Fatal(got.err)
		}
		winners = append(winners, got.claims...)
	}
	if len(winners) != 1 {
		t.Fatalf("duplicate lease count = %d", len(winners))
	}
	claim := winners[0]
	observed, _, err := store.Observe(claim, engagementNow, EngagementObservation{ChangeKey: "v4"})
	if err != nil {
		t.Fatal(err)
	}
	type reservation struct {
		result EngagementResult
		start  bool
		err    error
	}
	results := make(chan reservation, workers)
	begin := make(chan struct{})
	for range workers {
		go func() {
			<-begin
			result, execute, err := store.BeginStep(claim, engagementNow, observed.ChangeKey, action)
			results <- reservation{result, execute, err}
		}()
	}
	close(begin)
	var starts int
	var key string
	for range workers {
		got := <-results
		if key == "" {
			key = got.result.Key
		}
		if got.err != nil || got.result.Key == "" || got.result.Key != key || got.result.Attempt != 1 {
			t.Fatalf("inconsistent reservation = %+v", got)
		}
		if got.start {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("step started %d times", starts)
	}
}

func TestEngagementRecurringVerificationAndUnknownResult(t *testing.T) {
	store := NewEngagementStore(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
	create := engagementFixture(engagementOwner)
	create.Decision.Agreement.Once = false
	create.Decision.Agreement.EndCondition = EngagementText{ChatID: engagementOwner, Text: "when project is archived"}
	ref := createEngagement(t, store, create)
	claim := engagementClaim(t, store, engagementNow)
	if _, fresh, err := store.Observe(claim, engagementNow, EngagementObservation{}); err != nil || fresh {
		t.Fatalf("nonmatching verification = %v, %v", fresh, err)
	}
	if item, err := store.ReadClaim(claim, engagementNow); err != nil || !item.LastObservedAt.Equal(engagementNow) || !item.LastDeliveredAt.IsZero() {
		t.Fatalf("nonmatching check claimed delivery = %+v, %v", item, err)
	}
	now := engagementNow.Add(time.Hour)
	if err := store.FinishCheck(claim, engagementNow, EngagementCheck{NextCheck: now}); err != nil {
		t.Fatal(err)
	}
	if items, err := store.ClaimDue(now.Add(-time.Second), time.Minute, 1); err != nil || len(items) != 0 {
		t.Fatalf("recurring work claimed early = %+v, %v", items, err)
	}
	claim = engagementClaim(t, store, now)
	delivery := EngagementStep{Recipient: engagementOwner}
	observed, _, err := store.Observe(claim, now, EngagementObservation{ChangeKey: "v4"})
	if err != nil {
		t.Fatal(err)
	}
	got, execute, err := store.BeginStep(claim, now, observed.ChangeKey, delivery)
	if err != nil || !execute {
		t.Fatalf("reserve delivery = %+v, %v, %v", got, execute, err)
	}
	if err := store.RecordOutcome(claim, now, observed.ChangeKey, delivery, got.Attempt, EngagementUnknown, "Telegram timed out"); err != nil {
		t.Fatal(err)
	}
	if seen, start, err := store.BeginStep(claim, now, observed.ChangeKey, delivery); err != nil || start || seen.State != EngagementUnknown {
		t.Fatalf("uncertain delivery retried = %+v, %v, %v", seen, start, err)
	}
	if err := store.RecordOutcome(claim, now, observed.ChangeKey, delivery, got.Attempt, EngagementSucceeded, "confirmed message 101"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishCheck(claim, now, EngagementCheck{CompletedChange: observed.ChangeKey, NextCheck: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	claim = engagementClaim(t, store, now)
	if err := store.FinishCheck(claim, now, EngagementCheck{EndConditionMet: true}); err != nil {
		t.Fatal(err)
	}
	if item, err := store.Get(ref.ID); err != nil || item.Status != EngagementEnded || item.EndReason != EngagementConditionEnded {
		t.Fatalf("agreed end condition = %+v, %v", item, err)
	}
}

func TestEngagementRecurringWorkSettlesAgainstOriginalAndCurrentSteps(t *testing.T) {
	store := NewEngagementStore(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
	create := engagementFixture(engagementGroup)
	original := create.Decision.Agreement
	original.Once = false
	const removedGroup int64 = -702
	original.Recipients = []int64{engagementGroup, removedGroup}
	create.Decision.Authorized.Recipients = original.Recipients
	ref := createEngagement(t, store, create)
	now := engagementNow
	claim := engagementClaim(t, store, now)
	old, fresh, err := store.Observe(claim, now, EngagementObservation{ChangeKey: "v1"})
	if err != nil || !fresh {
		t.Fatalf("first event: %+v %v %v", old, fresh, err)
	}

	decision := create.Decision
	decision.Change = EngagementReaccept
	decision.Request = engagementRequest(engagementGroup, engagementOwner, 2, "From now on send to me instead of the secondary group.")
	decision.Request.ExplicitReaccept = true
	next := *original
	next.Recipients = []int64{engagementGroup, engagementOwner}
	decision.Agreement = &next
	decision.Authorized.Recipients = next.Recipients
	ref = reviseEngagement(t, store, ref, decision)
	claim = engagementClaim(t, store, now)
	first := EngagementStep{Recipient: engagementGroup}
	added := EngagementStep{Recipient: engagementOwner}
	removed := EngagementStep{Recipient: removedGroup}
	for _, step := range []EngagementStep{added, removed} {
		if _, start, err := store.BeginStep(claim, now, old.ChangeKey, step); start || !errors.Is(err, ErrEngagementDenied) {
			t.Fatalf("old event could begin in destination %d: %v %v", step.Recipient, start, err)
		}
	}
	schedule := now.Add(time.Minute)
	check := EngagementCheck{CompletedChange: old.ChangeKey, NextCheck: schedule}
	if err := store.FinishCheck(claim, now, check); !errors.Is(err, ErrEngagementIncomplete) {
		t.Fatalf("old event completed without existing recipient's receipt: %v", err)
	}
	result, start, err := store.BeginStep(claim, now, old.ChangeKey, first)
	if err != nil || !start {
		t.Fatalf("old event lost existing destination: %+v %v %v", result, start, err)
	}
	if err := store.RecordOutcome(claim, now, old.ChangeKey, first, result.Attempt, EngagementUnknown, "delivery pending verification"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishCheck(claim, now, check); !errors.Is(err, ErrEngagementIncomplete) {
		t.Fatalf("old event counted unknown delivery as success: %v", err)
	}
	if err := store.RecordOutcome(claim, now, old.ChangeKey, first, result.Attempt, EngagementSucceeded, "group message 101"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishCheck(claim, now, check); err != nil {
		t.Fatalf("old event did not settle with the still-required receipt: %v", err)
	}
	if item, err := store.Get(ref.ID); err != nil || item.Status != EngagementActive || !item.NextCheck.Equal(schedule) {
		t.Fatalf("recurrence did not advance: %+v %v", item, err)
	}

	now = schedule
	claim = engagementClaim(t, store, now)
	replay, fresh, err := store.Observe(claim, now, EngagementObservation{ChangeKey: "v1"})
	if err != nil || fresh || replay.ChangeKey != old.ChangeKey {
		t.Fatalf("old event replay: %+v %v %v", replay, fresh, err)
	}
	if seen, start, err := store.BeginStep(claim, now, replay.ChangeKey, first); err != nil || start || seen.Receipt != "group message 101" {
		t.Fatalf("old event repeated delivery: %+v %v %v", seen, start, err)
	}
	if _, start, err := store.BeginStep(claim, now, replay.ChangeKey, added); start || !errors.Is(err, ErrEngagementDenied) {
		t.Fatalf("replay gained the future recipient: %v %v", start, err)
	}
	future, fresh, err := store.Observe(claim, now, EngagementObservation{ChangeKey: "v2"})
	if err != nil || !fresh {
		t.Fatalf("future event: %+v %v %v", future, fresh, err)
	}
	check = EngagementCheck{CompletedChange: future.ChangeKey, NextCheck: now.Add(time.Minute)}
	for _, step := range []EngagementStep{first, added} {
		if err := store.FinishCheck(claim, now, check); !errors.Is(err, ErrEngagementIncomplete) {
			t.Fatalf("new event completed before delivery to %d: %v", step.Recipient, err)
		}
		result, start, err := store.BeginStep(claim, now, future.ChangeKey, step)
		if err != nil || !start {
			t.Fatalf("new event skipped destination %d: %+v %v %v", step.Recipient, result, start, err)
		}
		if err := store.RecordOutcome(claim, now, future.ChangeKey, step, result.Attempt, EngagementSucceeded, "confirmed future delivery"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.FinishCheck(claim, now, check); err != nil {
		t.Fatal(err)
	}
}

func TestEngagementExtendingDeadlineKeepsOnceChange(t *testing.T) {
	store := NewEngagementStore(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
	create := engagementFixture(engagementOwner)
	addEngagementAction(&create)
	create.Decision.Agreement.ExpiresAt = engagementNow.Add(time.Hour)
	ref := createEngagement(t, store, create)
	claim := engagementClaim(t, store, engagementNow)
	if _, _, err := store.Observe(claim, engagementNow, EngagementObservation{ChangeKey: "v2"}); err != nil {
		t.Fatal(err)
	}
	now := engagementNow.Add(time.Minute)
	extend := create.Decision
	extend.Agreement.ExpiresAt = engagementNow.Add(2 * time.Hour)
	extend.Request = engagementRequest(engagementOwner, engagementOwner, 2, "same agreement, allow an extra hour")
	extend.Request.ExplicitReaccept = true
	extend.At, extend.Change = now, EngagementReaccept
	reviseEngagement(t, store, ref, extend)
	claim = engagementClaim(t, store, now)
	if _, _, err := store.Observe(claim, now, EngagementObservation{ChangeKey: "v3"}); !errors.Is(err, ErrEngagementDenied) {
		t.Fatalf("deadline extension reset one-time change: %v", err)
	}
}

func TestEngagementExpiredPauseIsTerminal(t *testing.T) {
	store := NewEngagementStore(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
	create := engagementFixture(engagementOwner)
	create.Decision.Agreement.ExpiresAt = engagementNow.Add(time.Minute)
	ref := createEngagement(t, store, create)
	claim := engagementClaim(t, store, engagementNow)
	if err := store.PauseClaim(claim, engagementNow); err != nil {
		t.Fatal(err)
	}
	now := create.Decision.Agreement.ExpiresAt
	if claims, err := store.ClaimDue(now, time.Minute, 1); err != nil || len(claims) != 0 {
		t.Fatalf("expired pause was claimed: %+v, %v", claims, err)
	}
	item, err := store.Get(ref.ID)
	if err != nil || item.Status != EngagementEnded || item.EndReason != EngagementExpired || item.Ref.Version != ref.Version+2 {
		t.Fatalf("expired agreement = %+v, %v", item, err)
	}
}

func TestEngagementRuntimeDBSwitchAndRollback(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{ChatDBPath: filepath.Join(dir, "a.db"), StickerRulesPath: filepath.Join(dir, "stickers.json"),
		OpenAIKey: "test-key", OpenAIModel: "test-model"}
	db := openMemoryTestDB(t, cfg.ChatDBPath)
	b := &Bot{cfg: cfg, chatDB: db, store: NewHistoryStore(db), summaries: NewSummaryStore(db), engagements: NewEngagementStore(db)}
	t.Cleanup(func() { _ = b.closeChatDB() })
	store := b.snapshot().engagements
	create := engagementFixture(engagementOwner)
	ref := createEngagement(t, store, create)
	b.store.Append(engagementOwner, openai.ChatCompletionMessage{Role: "user", Content: "active history"}, 3)
	next := cfg
	next.ChatDBPath = filepath.Join(dir, "b.db")
	if err := b.applyRuntimeConfig(next); err != nil {
		t.Fatal(err)
	}
	if b.snapshot().engagements != store {
		t.Fatal("captured store was replaced")
	}
	claim := engagementClaim(t, store, engagementNow)
	observed, _, err := store.Observe(claim, engagementNow, EngagementObservation{ChangeKey: "v3"})
	if err != nil {
		t.Fatal(err)
	}
	recipient := EngagementStep{Recipient: engagementOwner}
	result, start, err := store.BeginStep(claim, engagementNow, observed.ChangeKey, recipient)
	if err != nil || !start {
		t.Fatalf("begin delivery = %+v, %v, %v", result, start, err)
	}
	if err := store.RecordOutcome(claim, engagementNow, observed.ChangeKey, recipient, result.Attempt, EngagementSucceeded, "Telegram 15"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishCheck(claim, engagementNow, EngagementCheck{CompletedChange: observed.ChangeKey}); err != nil {
		t.Fatal(err)
	}
	// Fail a switch to a divergent branch that advanced the old copy.
	old := openMemoryTestDB(t, cfg.ChatDBPath)
	oldStore := NewEngagementStore(old)
	reviseEngagement(t, oldStore, ref, EngagementDecision{
		Request: engagementRequest(engagementOwner, engagementOwner, 9, "pause old copy"),
		Verdict: EngagementAccept, Change: EngagementPause, At: engagementNow,
	})
	NewHistoryStore(old).Append(engagementOwner, openai.ChatCompletionMessage{Role: "assistant", Content: "destination history"}, 3)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.applyRuntimeConfig(cfg); !errors.Is(err, ErrEngagementConflict) || b.currentConfig().ChatDBPath != next.ChatDBPath {
		t.Fatalf("divergent database switch = %v", err)
	}
	item, err := store.Get(ref.ID)
	if err != nil || item.Status != EngagementEnded {
		t.Fatalf("source after rollback = %+v, %v", item, err)
	}
	old = openMemoryTestDB(t, cfg.ChatDBPath)
	if history := old.LoadAllHistory()[engagementOwner]; len(history) != 2 || history[1].Content != "destination history" {
		t.Fatalf("destination history was overwritten: %+v", history)
	}
	third := next
	third.ChatDBPath = filepath.Join(dir, "fresh.db")
	if err := b.applyRuntimeConfig(third); err != nil {
		t.Fatal(err)
	}
	now := engagementNow.Add(time.Hour)
	if claims, err := store.ClaimDue(now, time.Minute, 10); err != nil || len(claims) != 0 {
		t.Fatalf("ended engagement was revived on switch: %+v, %v", claims, err)
	}
	works, err := store.Work(ref.ID, "", 10)
	if err != nil || len(works) != 1 || len(works[0].Results) != 1 || works[0].Results[0].Receipt != "Telegram 15" {
		t.Fatalf("receipt after migration = %+v, %v", works, err)
	}
	if err := b.applyRuntimeConfig(next); err != nil {
		t.Fatalf("switch to unchanged ancestor = %v", err)
	}
}

func TestEngagementClaimAndProgressSurviveHotSwitch(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{ChatDBPath: filepath.Join(dir, "a.db"), StickerRulesPath: filepath.Join(dir, "stickers.json"),
		OpenAIKey: "test-key", OpenAIModel: "test-model"}
	db := openMemoryTestDB(t, cfg.ChatDBPath)
	b := &Bot{cfg: cfg, chatDB: db, store: NewHistoryStore(db), engagements: NewEngagementStore(db)}
	t.Cleanup(func() { _ = b.closeChatDB() })
	store := b.snapshot().engagements
	ref := createEngagement(t, store, engagementFixture(engagementOwner))
	claim := engagementClaim(t, store, engagementNow)
	observed, _, err := store.Observe(claim, engagementNow, EngagementObservation{ChangeKey: "v5"})
	if err != nil {
		t.Fatal(err)
	}
	step := EngagementStep{Recipient: engagementOwner}
	reservation, send, err := store.BeginStep(claim, engagementNow, observed.ChangeKey, step)
	if err != nil || !send {
		t.Fatalf("delivery reserve = %+v, %v, %v", reservation, send, err)
	}
	next := cfg
	next.ChatDBPath = filepath.Join(dir, "b.db")
	destination := openMemoryTestDB(t, next.ChatDBPath)
	targetOnly := createEngagement(t, NewEngagementStore(destination), engagementFixture(engagementGroup))
	if err := destination.Close(); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		result <- store.RecordOutcome(claim, engagementNow, observed.ChangeKey, step, reservation.Attempt, EngagementSucceeded, "telegram msg 181")
	}()
	switchErr := b.applyRuntimeConfig(next)
	if resultErr := <-result; switchErr != nil || resultErr != nil {
		t.Fatalf("switch and in-flight receipt = %v, %v", switchErr, resultErr)
	}
	if err := store.FinishCheck(claim, engagementNow, EngagementCheck{CompletedChange: observed.ChangeKey}); err != nil {
		t.Fatal(err)
	}
	if err := b.applyRuntimeConfig(cfg); err != nil {
		t.Fatalf("switch to known older ancestor = %v", err)
	}
	if _, err := store.Get(targetOnly.ID); err != nil {
		t.Fatalf("lost destination-only item = %v", err)
	}
	if err := b.closeChatDB(); err != nil {
		t.Fatal(err)
	}
	reopened := NewEngagementStore(openMemoryTestDB(t, cfg.ChatDBPath))
	item, err := reopened.Get(ref.ID)
	if err != nil || item.Status != EngagementEnded || !item.LastDeliveredAt.Equal(engagementNow) {
		t.Fatalf("terminal and delivery progress after re-open = %+v, %v", item, err)
	}
	work, err := reopened.Work(ref.ID, "", 10)
	if err != nil || len(work) != 1 || work[0].Results[0].Receipt != "telegram msg 181" {
		t.Fatalf("destination receipt after re-open = %+v, %v", work, err)
	}
}

func TestEngagementMigrationPrunesLegacyAndPreservesWork(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")
	db := openMemoryTestDB(t, path)
	active := NewEngagementStore(db)
	create := engagementFixture(engagementOwner)
	action := EngagementStep{Action: addEngagementAction(&create)}
	ref := createEngagement(t, active, create)
	management := EngagementDecision{
		Request: engagementRequest(engagementOwner, engagementOwner, 2, "Only I can manage this"),
		Verdict: EngagementAccept, Change: EngagementManage, At: engagementNow,
		Management: &EngagementManagementEdit{Summary: "Only the initiator may manage this", InitiatorOnly: true},
	}
	ref = reviseEngagement(t, active, ref, management)
	claim := engagementClaim(t, active, engagementNow)
	work, _, err := active.Observe(claim, engagementNow, EngagementObservation{ChangeKey: "release"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, started, err := active.BeginStep(claim, engagementNow, work.ChangeKey, action)
	if err != nil || !started {
		t.Fatalf("calendar reservation: %v %v", started, err)
	}
	count := func(db *ChatDB) (counts [4]int) {
		t.Helper()
		err := db.db.View(func(tx *bolt.Tx) error {
			for i, name := range [][]byte{engagementBucket, engagementWorkBucket, engagementRequestsBucket, engagementRevisionsBucket} {
				counts[i] = tx.Bucket(name).Stats().KeyN
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return counts
	}
	// Simulate keys written by the prior format alongside real work and requests.
	if err := db.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(engagementRevisionsBucket)
		for i := range 525 {
			if err := bucket.Put([]byte(fmt.Sprintf("%s/legacy-%03d", ref.ID, i)), []byte{1}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := count(db)
	if before[3] < 525 {
		t.Fatalf("missing legacy keys: %v", before)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openMemoryTestDB(t, path)
	if after := count(db); after != [4]int{1, 1, 2, 0} {
		t.Fatalf("upgrade counts (records, work, requests, revisions): %v → %v", before, after)
	}
	active = NewEngagementStore(db)
	other := openMemoryTestDB(t, filepath.Join(dir, "other.db"))
	independent := engagementFixture(engagementGroup)
	independent.Decision.Agreement.Once = false
	otherRef := createEngagement(t, NewEngagementStore(other), independent)
	if err := migrateChatStores(other, nil, nil, nil, active); err != nil {
		t.Fatal(err)
	}
	if err := migrateChatStores(db, nil, nil, nil, active); err != nil {
		t.Fatalf("upgraded legacy head should match its unmodified copy: %v", err)
	}
	if err := migrateChatStores(other, nil, nil, nil, active); err != nil {
		t.Fatal(err)
	}
	if err := active.ArchiveOutcome(attempt.AttemptRef, engagementNow, EngagementSucceeded, "calendar receipt"); err != nil {
		t.Fatal(err)
	}
	notice := EngagementStep{Recipient: engagementOwner}
	send, started, err := active.BeginStep(claim, engagementNow, work.ChangeKey, notice)
	if err != nil || !started {
		t.Fatalf("notice reservation after switch: %v %v", started, err)
	}
	if err := active.ArchiveOutcome(send.AttemptRef, engagementNow, EngagementSucceeded, "Telegram receipt"); err != nil {
		t.Fatal(err)
	}
	if err := active.FinishCheck(claim, engagementNow, EngagementCheck{CompletedChange: work.ChangeKey}); err != nil {
		t.Fatal(err)
	}
	for n := range 120 {
		now := engagementNow.Add(time.Duration(n) * time.Minute)
		poll := engagementClaim(t, active, now)
		if _, _, err := active.Observe(poll, now, EngagementObservation{}); err != nil {
			t.Fatal(err)
		}
		if err := active.FinishCheck(poll, now, EngagementCheck{NextCheck: now.Add(time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateChatStores(db, nil, nil, nil, active); err != nil {
		t.Fatal(err)
	}
	for _, copy := range []*ChatDB{db, other} {
		if got := count(copy); got != [4]int{2, 1, 3, 0} {
			t.Fatalf("switch with 120 empty polls changed row counts: %v", got)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openMemoryTestDB(t, path)
	active = NewEngagementStore(db)
	if got := count(db); got != [4]int{2, 1, 3, 0} {
		t.Fatalf("reopen counts: %v", got)
	}
	item, err := active.Get(ref.ID)
	if err != nil || item.EndReason != EngagementCompleted || item.CompletedChangeKey != work.ChangeKey ||
		item.LastObservation.ChangeKey != "release" || item.Management.Summary != management.Management.Summary || len(item.Decisions) != 2 {
		t.Fatalf("record or management history was lost: %+v, %v", item, err)
	}
	works, err := active.Work(ref.ID, "", 10)
	if err != nil || len(works) != 1 || len(works[0].Results) != 2 || works[0].Results[0].Receipt != "calendar receipt" ||
		works[0].Results[0].AttemptRef != attempt.AttemptRef || works[0].Results[1].Receipt != "Telegram receipt" {
		t.Fatalf("lost results: %+v, %v", works, err)
	}
	if _, err := active.Get(otherRef.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := active.Create(create); !errors.Is(err, ErrEngagementProcessed) {
		t.Fatalf("original request deduplication: %v", err)
	}
	if _, err := active.Decide(ref, management); !errors.Is(err, ErrEngagementProcessed) {
		t.Fatalf("management request deduplication: %v", err)
	}
	// Advance both copies of the independent item, then fail a switch while
	// the active database also holds a newly accepted request.
	fork := NewEngagementStore(other)
	pause := EngagementDecision{
		Request: engagementRequest(engagementGroup, engagementOwner, 3, "Pause in the old database"),
		Verdict: EngagementAccept, Change: EngagementPause, At: engagementNow.Add(120 * time.Minute),
	}
	reviseEngagement(t, fork, otherRef, pause)
	clarify := pause
	clarify.Request = engagementRequest(engagementGroup, engagementOwner, 4, "Review in the active database")
	clarify.Verdict, clarify.Change = EngagementClarify, ""
	reviseEngagement(t, active, otherRef, clarify)
	newItem := engagementFixture(engagementOwner)
	newItem.Decision.Request.Source.MessageID = 6
	newRef := createEngagement(t, active, newItem)
	if err := migrateChatStores(other, nil, nil, nil, active); !errors.Is(err, ErrEngagementConflict) || active.db != db {
		t.Fatalf("fork was accepted or active handle changed: %v", err)
	}
	if got := count(other); got != [4]int{2, 1, 4, 0} {
		t.Fatalf("destination was modified by failed switch: %v", got)
	}
	if _, err := fork.Get(newRef.ID); !errors.Is(err, ErrEngagementNotFound) {
		t.Fatalf("destination acquired source-only record: %v", err)
	}
	if _, err := active.Get(newRef.ID); err != nil {
		t.Fatalf("source lost its independent record: %v", err)
	}
	t.Logf("legacy %v → after upgrade [1 1 2 0] → both copies and reopen [2 1 3 0] after 120 empty polls", before)
}

func TestEngagementMigrationAnchorLimit(t *testing.T) {
	dir := t.TempDir()
	old := openMemoryTestDB(t, filepath.Join(dir, "old.db"))
	active := NewEngagementStore(old)
	ref := createEngagement(t, active, engagementFixture(engagementOwner))
	var previous, current *ChatDB
	for n := range 11 {
		previous, current = current, openMemoryTestDB(t, filepath.Join(dir, fmt.Sprintf("copy-%d.db", n)))
		if err := migrateChatStores(current, nil, nil, nil, active); err != nil {
			t.Fatal(err)
		}
		now := engagementNow.Add(time.Duration(n) * time.Minute)
		claim := engagementClaim(t, active, now)
		if err := active.FinishCheck(claim, now, EngagementCheck{NextCheck: now.Add(time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	var record struct{ MigrationAnchors []string }
	if err := current.db.View(func(tx *bolt.Tx) error {
		return json.Unmarshal(tx.Bucket(engagementBucket).Get([]byte(ref.ID)), &record)
	}); err != nil || len(record.MigrationAnchors) != 8 {
		t.Fatalf("after 11 switches, got %d durable migration anchors: %v", len(record.MigrationAnchors), err)
	}
	if err := migrateChatStores(old, nil, nil, nil, active); !errors.Is(err, ErrEngagementConflict) || active.db != current {
		t.Fatalf("ancient copy accepted or handle changed: %v", err)
	}
	if err := migrateChatStores(previous, nil, nil, nil, active); err != nil || active.db != previous {
		t.Fatalf("recent copied head rejected: %v", err)
	}
}
