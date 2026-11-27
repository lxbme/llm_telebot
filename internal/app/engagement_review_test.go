package app

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestEngagementReceiptAfterStopOrScopeChange(t *testing.T) {
	for _, change := range []string{"stop", "remove-action", "deadline", "revoked"} {
		t.Run(change, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "chat.db")
			db := openMemoryTestDB(t, path)
			store := NewEngagementStore(db)
			create := engagementFixture(engagementOwner)
			action := EngagementStep{Action: addEngagementAction(&create)}
			if change == "deadline" {
				create.Decision.Agreement.ExpiresAt = engagementNow.Add(time.Second)
			}
			ref := createEngagement(t, store, create)
			claim := engagementClaim(t, store, engagementNow)
			observed, _, err := store.Observe(claim, engagementNow, EngagementObservation{ChangeKey: "evt-9"})
			if err != nil {
				t.Fatal(err)
			}
			result, start, err := store.BeginStep(claim, engagementNow, observed.ChangeKey, action)
			if err != nil || !start {
				t.Fatalf("begin: %v %v", start, err)
			}

			received := make(chan struct{})
			respond := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(received)
				<-respond
				io.WriteString(w, "calendar confirmed event id 9")
			}))
			defer server.Close()
			defer func() {
				select {
				case <-respond:
				default:
					close(respond)
				}
			}()
			receiptCh := make(chan string, 1)
			go func() {
				req, _ := http.NewRequest(http.MethodPost, server.URL, nil)
				req.Header.Set("Idempotency-Key", result.Key)
				resp, err := server.Client().Do(req)
				if err != nil {
					receiptCh <- ""
					return
				}
				defer resp.Body.Close()
				body, _ := io.ReadAll(resp.Body)
				receiptCh <- string(body)
			}()
			<-received
			now := engagementNow.Add(time.Second)
			decision := EngagementDecision{At: now, Request: engagementRequest(engagementOwner, engagementOwner, 2, change), Verdict: EngagementAccept}
			switch change {
			case "stop":
				decision.Change = EngagementEnd
				ref = reviseEngagement(t, store, ref, decision)
			case "remove-action":
				agreement := *create.Decision.Agreement
				agreement.Actions = nil
				decision.Change, decision.Agreement, decision.Authorized = EngagementAgree, &agreement, create.Decision.Authorized
				ref = reviseEngagement(t, store, ref, decision)
			case "revoked":
				if err := store.PauseClaim(claim, now); err != nil {
					t.Fatal(err)
				}
			}
			destination := filepath.Join(t.TempDir(), "next.db")
			nextDB := openMemoryTestDB(t, destination)
			if err := migrateChatStores(nextDB, nil, nil, nil, store); err != nil {
				t.Fatal(err)
			}
			close(respond)
			receipt := <-receiptCh
			if receipt == "" {
				t.Fatal("no receipt from local calendar")
			}
			if err := store.RecordOutcome(claim, now, observed.ChangeKey, action, result.Attempt, EngagementSucceeded, receipt); !errors.Is(err, ErrEngagementLease) {
				t.Fatalf("old lease accepted: %v", err)
			}
			if change == "remove-action" {
				next := engagementClaim(t, store, now)
				if _, start, err := store.BeginStep(next, now, observed.ChangeKey, action); start || !errors.Is(err, ErrEngagementDenied) {
					t.Fatalf("removed action could start again: %v %v", start, err)
				}
			} else if claims, err := store.ClaimDue(now, time.Minute, 1); err != nil || len(claims) != 0 {
				t.Fatalf("stopped, expired or paused engagement claimed: %v %v", claims, err)
			}
			if err := store.ArchiveOutcome(result.AttemptRef, now, EngagementSucceeded, receipt); err != nil {
				t.Fatal(err)
			}
			if err := nextDB.Close(); err != nil {
				t.Fatal(err)
			}
			store = NewEngagementStore(openMemoryTestDB(t, destination))
			work, err := store.Work(ref.ID, "", 1)
			if err != nil || len(work) != 1 || len(work[0].Results) != 1 ||
				work[0].Results[0].State != EngagementSucceeded || work[0].Results[0].Receipt != receipt {
				t.Fatalf("archive after switch and reopen: %v %v", work, err)
			}
			if err := store.ArchiveOutcome(work[0].Results[0].AttemptRef, now, EngagementSucceeded, receipt); err != nil {
				t.Fatalf("exact receipt replay: %v", err)
			}
			if err := store.ArchiveOutcome(work[0].Results[0].AttemptRef, now, EngagementNoEffect, "conflicting result"); !errors.Is(err, ErrEngagementConflict) {
				t.Fatalf("settled receipt overwritten: %v", err)
			}
			item, err := store.Get(ref.ID)
			if err != nil || item.EndReason == EngagementCompleted || item.CompletedChangeKey != "" {
				t.Fatalf("archival changed fulfillment: %v %v", item, err)
			}
		})
	}
}

func TestEngagementObservationIdentityAfterChangingSource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repository-a", "/repository-b":
			io.WriteString(w, "v1.0")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	observe := func(target string) EngagementObservation {
		t.Helper()
		resp, err := server.Client().Get(server.URL + target)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("source: %d %v", resp.StatusCode, err)
		}
		return EngagementObservation{ChangeKey: string(body), Evidence: EngagementText{ChatID: engagementOwner, Text: "verified release " + target}}
	}
	path := filepath.Join(t.TempDir(), "chat.db")
	db := openMemoryTestDB(t, path)
	store := NewEngagementStore(db)
	create := engagementFixture(engagementOwner)
	create.Decision.Agreement.Observe.Target = "/repository-a"
	create.Decision.Authorized.Capabilities = []EngagementCapability{create.Decision.Agreement.Observe}
	ref := createEngagement(t, store, create)
	claim := engagementClaim(t, store, engagementNow)
	first, fresh, err := store.Observe(claim, engagementNow, observe("/repository-a"))
	if err != nil || !fresh {
		t.Fatalf("source A: %v %v", fresh, err)
	}
	step := EngagementStep{Recipient: engagementOwner}
	sent, start, err := store.BeginStep(claim, engagementNow, first.ChangeKey, step)
	if err != nil || !start {
		t.Fatalf("source A reservation: %v %v", start, err)
	}
	if err := store.RecordOutcome(claim, engagementNow, first.ChangeKey, step, sent.Attempt, EngagementSucceeded, "message from repository A"); err != nil {
		t.Fatal(err)
	}
	now := engagementNow.Add(time.Second)
	newDecision := create.Decision
	newDecision.Change, newDecision.At = EngagementReaccept, now
	newDecision.Request = engagementRequest(engagementOwner, engagementOwner, 2, "explicitly reaccept a different repo")
	newDecision.Request.ExplicitReaccept = true
	agreement := *create.Decision.Agreement
	agreement.Observe.Target = "/repository-b"
	newDecision.Agreement = &agreement
	newDecision.Authorized.Capabilities = []EngagementCapability{agreement.Observe}
	reviseEngagement(t, store, ref, newDecision)
	claim = engagementClaim(t, store, now)
	second, fresh, err := store.Observe(claim, now, observe("/repository-b"))
	if err != nil || !fresh || second.ChangeKey == first.ChangeKey || second.Observe != agreement.Observe {
		t.Fatalf("source B: %+v %v %v", second, fresh, err)
	}
	if _, start, err := store.BeginStep(claim, now, first.ChangeKey, step); start || !errors.Is(err, ErrEngagementDenied) {
		t.Fatalf("source A work accepted under B: %v %v", start, err)
	}
	pending, start, err := store.BeginStep(claim, now, second.ChangeKey, step)
	if err != nil || !start || pending.Key == sent.Key {
		t.Fatalf("source B reservation: %+v %v %v", pending, start, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store = NewEngagementStore(openMemoryTestDB(t, path))
	now = now.Add(time.Minute)
	claim = engagementClaim(t, store, now)
	replay, fresh, err := store.Observe(claim, now, observe("/repository-b"))
	if err != nil || fresh || replay.ChangeKey != second.ChangeKey || replay.Results[0].Key != pending.Key {
		t.Fatalf("source B replay: %+v %v %v", replay, fresh, err)
	}
	item, err := store.ReadClaim(claim, now)
	if err != nil || item.OnceChangeKey != replay.ChangeKey {
		t.Fatalf("source B one-time binding: %+v %v", item, err)
	}
	if err := store.ArchiveOutcome(replay.Results[0].AttemptRef, now, EngagementSucceeded, "verified message from repository B"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishCheck(claim, now, EngagementCheck{CompletedChange: replay.ChangeKey}); err != nil {
		t.Fatal(err)
	}
	works, err := store.Work(ref.ID, "", 2)
	if err != nil || len(works) != 2 || works[0].Results[0].Receipt == works[1].Results[0].Receipt {
		t.Fatalf("source receipts merged: %+v %v", works, err)
	}
}

func TestEngagementPrivateTermsRequireTrustedProvenance(t *testing.T) {
	store := NewEngagementStore(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
	create := engagementFixture(engagementOwner)
	create.ManageChats = []int64{engagementGroup}
	access := MemoryDataAccess{OwnerID: engagementOwner, Key: "personal-calendar/instance-1"}
	original := create.Decision.Agreement
	original.Trigger = EngagementText{ChatID: engagementOwner, Access: access, Text: "A private calendar criterion"}
	original.EndCondition = EngagementText{ChatID: engagementOwner, Text: "when the personal project is archived"}
	original.ExpiresAt = engagementNow.Add(time.Hour)
	ref := createEngagement(t, store, create)

	viewer := EngagementViewer{ChatID: engagementGroup, ActorID: engagementPeer}
	decision := create.Decision
	decision.Request = engagementRequest(engagementGroup, engagementPeer, 2, "can this be followed from here?")
	agreement := *original
	decision.Agreement = &agreement
	agreement.Trigger.ChatID, agreement.Trigger.Access = engagementGroup, MemoryDataAccess{}
	agreement.TriggerSource = decision.Request.Source
	agreement.TimingAudience = &EngagementAudience{ChatID: engagementGroup}
	if _, err := store.Decide(ref, decision); !errors.Is(err, ErrEngagementDenied) {
		t.Fatalf("ordinary relabel exposed private terms: %v", err)
	}
	decision.Change, decision.Request.ExplicitReaccept = EngagementReaccept, true
	if _, err := store.Decide(ref, decision); !errors.Is(err, ErrEngagementDenied) {
		t.Fatalf("reaccept alone authorized disclosure: %v", err)
	}

	decision.Change = EngagementAgree
	decision.Request = engagementRequest(engagementGroup, engagementPeer, 3, "Please watch only the public changelog.")
	agreement.Trigger = EngagementText{ChatID: engagementGroup, Text: "only the public changelog"}
	agreement.EndCondition = EngagementText{}
	if _, err := store.Decide(ref, decision); !errors.Is(err, ErrEngagementDenied) {
		t.Fatalf("group removed a private end condition: %v", err)
	}
	agreement.EndCondition = original.EndCondition
	agreement.ExpiresAt = engagementNow.Add(30 * time.Minute)
	if _, err := store.Decide(ref, decision); !errors.Is(err, ErrEngagementDenied) {
		t.Fatalf("group changed a private expiry: %v", err)
	}
	agreement.ExpiresAt = original.ExpiresAt
	ref = reviseEngagement(t, store, ref, decision)
	after, err := store.ReadVisible(ref.ID, viewer)
	if err != nil || after.Agreement.Trigger != agreement.Trigger.Text || after.Agreement.EndCondition != "" ||
		after.Agreement.Once != nil || after.Agreement.ExpiresAt != nil {
		t.Fatalf("projection with independent group wording: %+v %v", after, err)
	}
	item, err := store.Get(ref.ID)
	if err != nil || item.Initial.Effective.Trigger != original.Trigger || item.Current.EndCondition != original.EndCondition ||
		item.Current.TriggerSource != decision.Request.Source || !item.Current.ExpiresAt.Equal(original.ExpiresAt) {
		t.Fatalf("source or effective terms lost: %+v %v", item, err)
	}

	// A's independently verified sharing consent can authorize the exact new scope.
	decision.Request.Source.MessageID++
	agreement.Trigger.Text = original.Trigger.Text
	share := engagementRequest(engagementOwner, engagementOwner, 20, "Share the original criterion with this group.")
	decision.Authorized.TermEdits = []EngagementTermGrant{{Term: EngagementTriggerTerm, Value: agreement.Trigger, Source: share.Source}}
	ref = reviseEngagement(t, store, ref, decision)
	item, err = store.Get(ref.ID)
	if err != nil || item.Current.TriggerSource != share.Source || item.Current.Trigger.ChatID != engagementGroup {
		t.Fatalf("verified sharing provenance: %+v %v", item, err)
	}
}

func TestEngagementTermEditsCannotCopyRestrictedTextAcrossFields(t *testing.T) {
	for _, tc := range []struct {
		name      string
		publicEnd string
		reverse   bool
	}{
		{name: "empty end condition"},
		{name: "existing public end condition", publicEnd: "after the final release"},
		{name: "swap private end and public trigger", reverse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewEngagementStore(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
			create := engagementFixture(engagementGroup)
			access := MemoryDataAccess{OwnerID: engagementOwner, Key: "private-criteria"}
			const privateText = "Confidential client name Natasha"
			private := EngagementText{ChatID: engagementGroup, Access: access, Text: privateText}
			original := create.Decision.Agreement
			if tc.reverse {
				original.EndCondition = private
			} else {
				original.Trigger = private
				if tc.publicEnd != "" {
					original.EndCondition = EngagementText{ChatID: engagementGroup, Text: tc.publicEnd}
				}
			}
			ref := createEngagement(t, store, create)
			decision := create.Decision
			decision.Request = engagementRequest(engagementGroup, engagementOwner, 2, "Revise the conditions for this group.")
			decision.Authorized.DataAccess = []MemoryDataAccess{access}
			editor := EngagementViewer{ChatID: engagementGroup, ActorID: engagementOwner, DataAccess: map[MemoryDataAccess]bool{access: true}}
			visible, err := store.ReadVisible(ref.ID, editor)
			if err != nil {
				t.Fatal(err)
			}
			next := *original
			decision.Agreement = &next
			if tc.reverse {
				if visible.Agreement.EndCondition != privateText {
					t.Fatal("editor cannot see the original end condition")
				}
				next.Trigger.Text = visible.Agreement.EndCondition
				next.EndCondition = EngagementText{}
			} else {
				if visible.Agreement.Trigger != privateText {
					t.Fatal("editor cannot see the original trigger")
				}
				next.EndCondition = EngagementText{ChatID: engagementGroup, Text: visible.Agreement.Trigger}
			}
			if _, err := store.Decide(ref, decision); !errors.Is(err, ErrEngagementDenied) {
				t.Fatalf("copied restricted text to a public field: %v", err)
			}
			viewer := EngagementViewer{ChatID: engagementGroup, ActorID: engagementPeer}
			projection, err := store.ReadVisible(ref.ID, viewer)
			if err != nil || projection.Ref != ref || projection.Agreement.EndCondition != tc.publicEnd ||
				projection.Agreement.Trigger == privateText {
				t.Fatalf("public terms after denied edit: %+v %v", projection, err)
			}

			// Independently stated text can enter the public field, while a
			// candidate placement under a different data grant needs authority.
			decision.Request.Text = "Set the conditions to after the next stable release."
			const publicText = "after the next stable release"
			next = *original
			next.Trigger = EngagementText{ChatID: engagementGroup, Text: publicText, Access: MemoryDataAccess{OwnerID: 99, Key: "ungranted"}}
			if _, err := store.Decide(ref, decision); !errors.Is(err, ErrEngagementDenied) {
				t.Fatalf("same-chat statement authorized an ungranted data label: %v", err)
			}
			next.Trigger.Access = MemoryDataAccess{}
			ref = reviseEngagement(t, store, ref, decision)
			projection, err = store.ReadVisible(ref.ID, viewer)
			if err != nil || projection.Agreement.Trigger != publicText {
				t.Fatalf("authenticated public replacement missing: %+v %v", projection, err)
			}
		})
	}
}

func TestEngagementProjectionPreservesAllowedEvidenceAttribution(t *testing.T) {
	store := NewEngagementStore(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
	var evidence [][]EngagementVisibleEvidence
	for i, actor := range []int64{engagementOwner, engagementPeer} {
		create := engagementFixture(engagementGroup)
		create.Decision.Request.Source.MessageID += int64(i)
		create.Evidence = []MemoryEvidence{{Kind: MemoryStatement, Source: engagementRequest(engagementGroup, actor, 11, "").Source, Text: "Only I should manage it."}}
		ref := createEngagement(t, store, create)
		p, err := store.ReadVisible(ref.ID, EngagementViewer{ChatID: engagementGroup, ActorID: engagementPeer})
		if err != nil {
			t.Fatal(err)
		}
		evidence = append(evidence, p.Evidence)
	}
	if reflect.DeepEqual(evidence[0], evidence[1]) {
		t.Errorf("A and B's explicit statements render identically: %+v", evidence[0])
	}
}

func TestEngagementEndConditionMustDistinguishIncompleteOnce(t *testing.T) {
	store := NewEngagementStore(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
	create := engagementFixture(engagementOwner)
	create.Decision.Agreement.EndCondition = EngagementText{ChatID: engagementOwner, Text: "when project closes"}
	ref := createEngagement(t, store, create)
	claim := engagementClaim(t, store, engagementNow)
	observed, _, err := store.Observe(claim, engagementNow, EngagementObservation{ChangeKey: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	step := EngagementStep{Recipient: engagementOwner}
	result, start, err := store.BeginStep(claim, engagementNow, observed.ChangeKey, step)
	if err != nil || !start {
		t.Fatalf("reserve: %v %v", start, err)
	}
	if err := store.FinishCheck(claim, engagementNow, EngagementCheck{CompletedChange: observed.ChangeKey}); !errors.Is(err, ErrEngagementIncomplete) {
		t.Fatalf("reserved delivery accepted as complete: %v", err)
	}
	if err := store.RecordOutcome(claim, engagementNow, observed.ChangeKey, step, result.Attempt, EngagementUnknown, "local delivery timed out"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishCheck(claim, engagementNow, EngagementCheck{CompletedChange: observed.ChangeKey, EndConditionMet: true}); !errors.Is(err, ErrEngagementIncomplete) {
		t.Fatalf("unknown delivery accepted as complete: %v", err)
	}
	if err := store.FinishCheck(claim, engagementNow, EngagementCheck{EndConditionMet: true}); err != nil {
		t.Fatal(err)
	}
	now := engagementNow.Add(time.Second)
	if err := store.ArchiveOutcome(result.AttemptRef, now, EngagementSucceeded, "delivery found later"); err != nil {
		t.Fatal(err)
	}
	item, err := store.Get(ref.ID)
	if err != nil || item.Status != EngagementEnded || item.EndReason != EngagementConditionEnded ||
		item.CompletedChangeKey != "" || !item.LastDeliveredAt.Equal(now) {
		t.Fatalf("termination and late delivery: %+v %v", item, err)
	}
}

func TestEngagementTightenedTriggerWithPendingOnce(t *testing.T) {
	store := NewEngagementStore(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
	create := engagementFixture(engagementGroup)
	create.Decision.Agreement.Trigger.Text = "any release"
	action := EngagementStep{Action: addEngagementAction(&create)}
	ref := createEngagement(t, store, create)
	claim := engagementClaim(t, store, engagementNow)
	old, _, err := store.Observe(claim, engagementNow, EngagementObservation{ChangeKey: "v1-rc1"})
	if err != nil {
		t.Fatal(err)
	}
	pending, start, err := store.BeginStep(claim, engagementNow, old.ChangeKey, action)
	if err != nil || !start {
		t.Fatalf("old calendar action: %+v %v %v", pending, start, err)
	}

	now := engagementNow.Add(time.Second)
	decision := create.Decision
	decision.At = now
	decision.Request = engagementRequest(engagementGroup, engagementOwner, 2, "No prerelease reports, even if pending. Notify for stable releases only.")
	agreement := *decision.Agreement
	agreement.Trigger.Text = "stable releases only"
	decision.Agreement = &agreement
	decision.InvalidateChanges = []string{old.ChangeKey, "missing"}
	if _, err := store.Decide(ref, decision); !errors.Is(err, ErrEngagementNotFound) {
		t.Fatalf("expected rollback on invalid work key: %v", err)
	}
	decision.InvalidateChanges = []string{old.ChangeKey}
	ref = reviseEngagement(t, store, ref, decision)
	claim = engagementClaim(t, store, now)
	if works, err := store.ClaimWork(claim, now, "", 10); err != nil || len(works) != 0 {
		t.Fatalf("old result in executable queue: %+v %v", works, err)
	}
	if err := store.ArchiveOutcome(pending.AttemptRef, now, EngagementSucceeded, "calendar event already created"); err != nil {
		t.Fatal(err)
	}
	works, err := store.Work(ref.ID, "", 10)
	if err != nil || len(works) != 1 || works[0].InvalidatedVersion != ref.Version ||
		works[0].Results[0].State != EngagementSucceeded {
		t.Fatalf("cancelled history and real outcome: %+v %v", works, err)
	}
	step := EngagementStep{Recipient: engagementGroup}
	if _, start, err := store.BeginStep(claim, now, old.ChangeKey, step); start || !errors.Is(err, ErrEngagementDenied) {
		t.Fatalf("cancelled prerelease still executable: %v %v", start, err)
	}
	stable, fresh, err := store.Observe(claim, now, EngagementObservation{ChangeKey: "v1.0"})
	if err != nil || !fresh {
		t.Fatalf("stable observation did not replace old match: %+v %v %v", stable, fresh, err)
	}
	if _, fresh, err := store.Observe(claim, now, EngagementObservation{ChangeKey: "v1-rc1"}); err != nil || fresh {
		t.Fatalf("cancelled observation dedupe: %v %v", fresh, err)
	}
	if works, err := store.ClaimWork(claim, now, "", 10); err != nil || len(works) != 1 || works[0].ChangeKey != stable.ChangeKey {
		t.Fatalf("stable result missing from executable queue: %+v %v", works, err)
	}
	for _, next := range []EngagementStep{action, step} {
		result, start, err := store.BeginStep(claim, now, stable.ChangeKey, next)
		if err != nil || !start {
			t.Fatalf("stable reservation: %+v %v %v", result, start, err)
		}
		if err := store.RecordOutcome(claim, now, stable.ChangeKey, next, result.Attempt, EngagementSucceeded, "confirmed stable result"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.FinishCheck(claim, now, EngagementCheck{CompletedChange: stable.ChangeKey}); err != nil {
		t.Fatal(err)
	}
}

func TestEngagementManagementTermsFollowVerifiedDecisions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.db")
	db := openMemoryTestDB(t, path)
	store := NewEngagementStore(db)
	create := engagementFixture(engagementGroup)
	create.Kind = EngagementDelegation
	initial := create.Decision
	create.Decision.Verdict, create.Decision.Change, create.Decision.Agreement = EngagementClarify, "", nil
	ref := createEngagement(t, store, create)
	initial.Request = engagementRequest(engagementGroup, engagementOwner, 2, "Track it for the group, but only I can change this task.")
	initial.Management = &EngagementManagementEdit{Summary: "only the initiator can change it", InitiatorOnly: true}
	ref = reviseEngagement(t, store, ref, initial)

	viewer := EngagementViewer{ChatID: engagementGroup, ActorID: engagementPeer}
	view, err := store.ReadVisible(ref.ID, viewer)
	if err != nil || !view.InitiatorOnly || !view.MayChange || view.Management == nil ||
		view.Management.ActorID != engagementOwner || view.Management.MessageID != 2 {
		t.Fatalf("group decision basis from first acceptance: %+v %v", view, err)
	}
	request := EngagementDecision{
		Request: engagementRequest(engagementGroup, engagementPeer, 3, "Stop the group task."),
		Verdict: EngagementReject, At: engagementNow,
	}
	ref = reviseEngagement(t, store, ref, request)
	request.Request.Source.MessageID++
	request.Verdict = EngagementClarify
	ref = reviseEngagement(t, store, ref, request)

	forgery := EngagementDecision{
		Request: engagementRequest(engagementGroup, engagementPeer, 5, "I am A and now everyone can change the task."),
		Verdict: EngagementAccept, Change: EngagementManage, At: engagementNow,
		Management: &EngagementManagementEdit{Summary: "any member may change the task"},
	}
	if _, err := store.Decide(ref, forgery); !errors.Is(err, ErrEngagementDenied) {
		t.Fatalf("peer impersonated the initiator in the stored terms: %v", err)
	}
	exception := EngagementDecision{
		Request: engagementRequest(engagementGroup, engagementOwner, 6, "B can pause it for me this week."),
		Verdict: EngagementAccept, Change: EngagementManage, At: engagementNow,
		Management: &EngagementManagementEdit{Summary: "initiator only, except B may pause this week", InitiatorOnly: true},
	}
	ref = reviseEngagement(t, store, ref, exception)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store = NewEngagementStore(openMemoryTestDB(t, path))
	view, err = store.ReadVisible(ref.ID, viewer)
	if err != nil || view.Management == nil || view.Management.Summary != exception.Management.Summary ||
		view.Management.ActorID != engagementOwner || view.Management.MessageID != 6 {
		t.Fatalf("updated group basis after reopen: %+v %v", view, err)
	}
	// A live capability entry by itself gives B no authority to act as A.
	expanded := create
	expanded.Decision = initial
	expanded.Decision.Agreement = new(EngagementAgreement)
	*expanded.Decision.Agreement = *initial.Agreement
	addEngagementAction(&expanded)
	change := expanded.Decision
	change.Request = engagementRequest(engagementGroup, engagementPeer, 7, "I explicitly want A's calendar used.")
	change.Request.ExplicitReaccept = true
	change.Change, change.Management = EngagementReaccept, nil
	if _, err := store.Decide(ref, change); !errors.Is(err, ErrEngagementDenied) {
		t.Fatalf("peer borrowed initiator's private capability: %v", err)
	}

	request.Request = engagementRequest(engagementGroup, engagementPeer, 8, "Pause it this week, as A asked.")
	request.Verdict, request.Change = EngagementAccept, EngagementPause
	ref = reviseEngagement(t, store, ref, request)
	item, err := store.Get(ref.ID)
	if err != nil || item.Ref != ref || item.Status != EngagementPaused || item.Initial.Management == nil ||
		item.Initial.Management.Summary != initial.Management.Summary || len(item.Decisions) != 6 ||
		item.Decisions[2].Verdict != EngagementReject || item.Decisions[3].Verdict != EngagementClarify ||
		item.Decisions[5].Management.Source != exception.Request.Source {
		t.Fatalf("effective and historical management decisions: %+v %v", item, err)
	}
}

func TestEngagementDelegateCanAmendPolicyWithCurrentInitiatorStatement(t *testing.T) {
	store := NewEngagementStore(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
	create := engagementFixture(engagementGroup)
	create.Decision.Management = &EngagementManagementEdit{Summary: "only the initiator can change it", InitiatorOnly: true}
	ref := createEngagement(t, store, create)
	delegation := EngagementDecision{
		Request: engagementRequest(engagementGroup, engagementOwner, 2, "B can administer this task and decide who can manage it."),
		Verdict: EngagementAccept, Change: EngagementManage, At: engagementNow,
		Management: &EngagementManagementEdit{Summary: "initiator only except B can amend this restriction", InitiatorOnly: true},
	}
	ref = reviseEngagement(t, store, ref, delegation)
	viewer := EngagementViewer{ChatID: engagementGroup, ActorID: engagementPeer}
	projection, err := store.ReadVisible(ref.ID, viewer)
	if err != nil || projection.Management == nil || projection.Management.ActorID != engagementOwner || projection.Management.MessageID != 2 {
		t.Fatalf("real delegation source missing from the role's input: %+v %v", projection, err)
	}
	amend := EngagementDecision{
		Request: engagementRequest(engagementGroup, engagementPeer, 3, "Under A's delegation, let everyone in this group manage it."),
		Verdict: EngagementAccept, Change: EngagementManage, At: engagementNow,
		Management: &EngagementManagementEdit{Summary: "any group member may manage"},
	}
	if _, err := store.Decide(ref, amend); !errors.Is(err, ErrEngagementDenied) {
		t.Fatalf("request alone replaced the initiating party's policy: %v", err)
	}
	stale := create.Decision.Request.Source
	amend.Authorized.ManagementBasis = &stale
	if _, err := store.Decide(ref, amend); !errors.Is(err, ErrEngagementDenied) {
		t.Fatalf("an inactive management statement authorized amendment: %v", err)
	}
	amend.Authorized.ManagementBasis = &delegation.Request.Source
	expansion := amend
	expansion.Request.ExplicitReaccept, expansion.Change = true, EngagementReaccept
	candidate := *create.Decision.Agreement
	candidate.Recipients = []int64{engagementGroup, -702}
	expansion.Agreement = &candidate
	expansion.Authorized.Capabilities = create.Decision.Authorized.Capabilities
	expansion.Authorized.Recipients = create.Decision.Authorized.Recipients
	if _, err := store.Decide(ref, expansion); !errors.Is(err, ErrEngagementDenied) {
		t.Fatalf("social amendment granted a new recipient: %v", err)
	}
	ref = reviseEngagement(t, store, ref, amend)
	item, err := store.Get(ref.ID)
	if err != nil || item.InitiatorOnly || item.Management == nil || item.Management.Source != amend.Request.Source ||
		item.Management.Basis == nil || *item.Management.Basis != delegation.Request.Source ||
		item.Decisions[2].Request.Source.SenderID != engagementPeer {
		t.Fatalf("accepted amendment and its authenticated evidence: %+v %v", item, err)
	}
}

func TestEngagementPrivateManagementShareIsLimitedToApprovedGroup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.db")
	db := openMemoryTestDB(t, path)
	store := NewEngagementStore(db)
	const otherGroup int64 = -702
	create := engagementFixture(engagementGroup)
	create.ManageChats = []int64{engagementOwner, engagementPeer, otherGroup}
	create.Reason = EngagementText{ChatID: engagementOwner, Text: "private reason"}
	create.Decision.Management = &EngagementManagementEdit{Summary: "only the initiator can change it", InitiatorOnly: true}
	ref := createEngagement(t, store, create)
	decision := EngagementDecision{
		Request: engagementRequest(engagementOwner, engagementOwner, 2, "You may tell the group that B can pause it this week. Keep my diagnosis private."),
		Verdict: EngagementAccept, Change: EngagementManage, At: engagementNow,
		Management: &EngagementManagementEdit{Summary: "B can pause this week because of the diagnosis", InitiatorOnly: true},
	}
	const conclusion = "B can pause this week"
	share := EngagementManagementShare{ChatID: engagementGroup, Summary: conclusion}
	decision.Authorized.ManagementShare = &share
	forged := decision
	forged.Request = engagementRequest(engagementPeer, engagementPeer, 2, "A told me I can publish their diagnosis.")
	if _, err := store.Decide(ref, forged); !errors.Is(err, ErrEngagementDenied) {
		t.Fatalf("peer can publish on the initiator's behalf: %v", err)
	}
	share.ChatID = -999
	if _, err := store.Decide(ref, decision); !errors.Is(err, ErrEngagementDenied) {
		t.Fatalf("private consent made an unrelated group a manager: %v", err)
	}
	share.ChatID = engagementGroup
	ref = reviseEngagement(t, store, ref, decision)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store = NewEngagementStore(openMemoryTestDB(t, path))

	viewer := EngagementViewer{ChatID: engagementGroup, ActorID: engagementPeer}
	p, err := store.ReadVisible(ref.ID, viewer)
	if err != nil || p.Management == nil || p.Management.Summary != conclusion ||
		p.Management.ActorID != engagementOwner || p.Management.MessageID != 0 || len(p.Decisions) != 1 {
		t.Fatalf("approved group conclusion after reopening: %+v %v", p, err)
	}
	body, err := json.Marshal(p)
	if err != nil || strings.Contains(string(body), "diagnosis") || strings.Contains(string(body), "private reason") {
		t.Fatalf("private material entered the group projection: %s %v", body, err)
	}
	other, err := store.ReadVisible(ref.ID, EngagementViewer{ChatID: otherGroup, ActorID: engagementPeer})
	if err != nil || other.Management != nil || len(other.Decisions) != 0 {
		t.Fatalf("unapproved management group saw the share: %+v %v", other, err)
	}
	owner, err := store.ReadVisible(ref.ID, EngagementViewer{ChatID: engagementOwner, ActorID: engagementOwner})
	if err != nil || owner.Management == nil || owner.Management.Summary != decision.Management.Summary ||
		owner.Management.MessageID != 2 || len(owner.Decisions) != 1 || owner.Decisions[0].Request != decision.Request.Text {
		t.Fatalf("private management audit: %+v %v", owner, err)
	}

	ref = reviseEngagement(t, store, ref, EngagementDecision{
		Request: engagementRequest(engagementGroup, engagementPeer, 3, "Pause it as A allowed this week."),
		Verdict: EngagementAccept, Change: EngagementPause, At: engagementNow,
	})
	decision.Request = engagementRequest(engagementOwner, engagementOwner, 4, "I withdraw the exception.")
	decision.Management = &EngagementManagementEdit{Summary: "No more exception", InitiatorOnly: true}
	decision.Authorized.ManagementShare = nil
	ref = reviseEngagement(t, store, ref, decision)
	p, err = store.ReadVisible(ref.ID, viewer)
	if err != nil || p.Management != nil || p.Status != EngagementPaused || !p.InitiatorOnly {
		t.Fatalf("old share outlived its management decision: %+v %v", p, err)
	}
}

func TestEngagementProjectionDistinguishesAccounts(t *testing.T) {
	store := NewEngagementStore(openMemoryTestDB(t, filepath.Join(t.TempDir(), "chat.db")))
	create := engagementFixture(engagementOwner)
	first := addEngagementAction(&create)
	second := first
	second.Access.Key = "personal-calendar/instance-2"
	create.Decision.Agreement.Actions = append(create.Decision.Agreement.Actions, second)
	create.Decision.Authorized.Capabilities = append(create.Decision.Authorized.Capabilities, second)
	ref := createEngagement(t, store, create)
	view := EngagementViewer{ChatID: engagementOwner, ActorID: engagementOwner, DataAccess: map[MemoryDataAccess]bool{first.Access: true, second.Access: true}}
	projection, err := store.ReadVisible(ref.ID, view)
	if err != nil || len(projection.Agreement.Actions) != 2 {
		t.Fatalf("authorized actions: %+v %v", projection, err)
	}
	if reflect.DeepEqual(projection.Agreement.Actions[0], projection.Agreement.Actions[1]) {
		t.Fatal("different verified connections render identically")
	}
	body, err := json.Marshal(projection)
	if err != nil || strings.Contains(string(body), "personal-calendar") {
		t.Fatalf("internal connection name in projection: %s %v", body, err)
	}
}

func TestEngagementLegacyWorkIdentityAndCompletion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ended  bool
		state  EngagementResultState
		reason EngagementEndReason
	}{
		{"active", false, EngagementReserved, ""},
		{"ended with all receipts", true, EngagementSucceeded, EngagementCompleted},
		{"ended with uncertain delivery", true, EngagementUnknown, EngagementConditionEnded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewEngagementStore(openMemoryTestDB(t, filepath.Join(t.TempDir(), "legacy.db")))
			create := engagementFixture(engagementOwner)
			create.Decision.Agreement.EndCondition = EngagementText{ChatID: engagementOwner, Text: "when the project closes"}
			ref := createEngagement(t, store, create)
			nativeID := "legacy:v1"
			step := EngagementStep{Recipient: engagementOwner}
			err := store.transact(true, func(tx *bolt.Tx) error {
				r, err := loadEngagement(tx, ref.ID)
				if err != nil {
					return err
				}
				r.ScopedWorkKeys, r.OnceChangeKey = false, nativeID
				if tc.ended {
					r.Status, r.EndReason = EngagementEnded, EngagementCompleted
				}
				work := EngagementWork{
					Observation: EngagementObservation{ChangeKey: nativeID, At: engagementNow},
					Observe:     r.Current.Observe, Trigger: r.Current.Trigger, Recipients: r.Current.Recipients,
					Results: []EngagementResult{{Step: step, Key: "legacy-idempotency-key", Attempt: 1, State: tc.state, At: engagementNow}},
				}
				if tc.state == EngagementSucceeded {
					work.Results[0].Receipt = "verified legacy delivery"
				}
				if err := putEngagementJSON(tx.Bucket(engagementWorkBucket), []byte(ref.ID+"/"+nativeID), work); err != nil {
					return err
				}
				return putEngagementJSON(tx.Bucket(engagementBucket), []byte(ref.ID), r)
			})
			if err != nil {
				t.Fatal(err)
			}
			works, err := store.Work(ref.ID, "", 1)
			if err != nil || len(works) != 1 || works[0].Observation.ChangeKey != nativeID {
				t.Fatalf("read legacy event: %+v %v", works, err)
			}
			old := works[0]
			item, err := store.Get(ref.ID)
			if err != nil || item.OnceChangeKey != old.ChangeKey || item.EndReason != tc.reason {
				t.Fatalf("legacy event and end reason: %+v %v", item, err)
			}
			if tc.ended {
				view := EngagementViewer{ChatID: engagementOwner, ActorID: engagementOwner}
				items, err := store.Visible(view, "", 1)
				if err != nil || len(items) != 1 || items[0].EndReason != tc.reason {
					t.Fatalf("projected legacy end reason: %+v %v", items, err)
				}
				return
			}
			claim := engagementClaim(t, store, engagementNow)
			observed, fresh, err := store.Observe(claim, engagementNow, old.Observation)
			if err != nil || fresh || observed.ChangeKey != old.ChangeKey ||
				observed.Results[0].Key != "legacy-idempotency-key" {
				t.Fatalf("legacy replay: %+v %v %v", observed, fresh, err)
			}
			if _, start, err := store.BeginStep(claim, engagementNow, observed.ChangeKey, step); err != nil || start {
				t.Fatalf("legacy reservation restarted: %v %v", start, err)
			}
			if err := store.ArchiveOutcome(old.Results[0].AttemptRef, engagementNow, EngagementSucceeded, "verified delivery"); err != nil {
				t.Fatal(err)
			}

			rebind := create.Decision
			rebind.Request = engagementRequest(engagementOwner, engagementOwner, 2, "Explicitly move tracking to the new repository.")
			rebind.Change, rebind.Request.ExplicitReaccept = EngagementReaccept, true
			rebind.Agreement.Observe.Target = "new/repo"
			rebind.Authorized.Capabilities = []EngagementCapability{rebind.Agreement.Observe}
			reviseEngagement(t, store, ref, rebind)
			claim = engagementClaim(t, store, engagementNow)
			observed, fresh, err = store.Observe(claim, engagementNow, old.Observation)
			if err != nil || !fresh || observed.ChangeKey == old.ChangeKey {
				t.Fatalf("legacy ID at a new source: %+v %v %v", observed, fresh, err)
			}
			works, err = store.Work(ref.ID, old.ChangeKey, 1)
			if err != nil || len(works) != 1 || works[0].ChangeKey != observed.ChangeKey {
				t.Fatalf("pagination from old to scoped work: %+v %v", works, err)
			}
		})
	}
}
