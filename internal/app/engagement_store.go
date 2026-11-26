package app

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	engagementBucket          = []byte("engagements")
	engagementRequestsBucket  = []byte("engagement_requests")
	engagementWorkBucket      = []byte("engagement_work")
	engagementRevisionsBucket = []byte("engagement_revisions") // obsolete ancestry index
)

const engagementMigrationAnchors = 8

// EngagementStore keeps one handle across successful ChatDB switches.
type EngagementStore struct {
	mu  sync.RWMutex
	db  *ChatDB
	now func() time.Time
}

func NewEngagementStore(db *ChatDB) *EngagementStore { return &EngagementStore{db: db, now: time.Now} }

type engagementRecord struct {
	Engagement
	LeaseToken       string
	LeaseUntil       time.Time
	Revision         string
	ScopedWorkKeys   bool
	MigrationAnchors []string `json:",omitempty"`
}

func (s *EngagementStore) transact(write bool, run func(*bolt.Tx) error) error {
	if s == nil {
		return bolt.ErrDatabaseNotOpen
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return bolt.ErrDatabaseNotOpen
	}
	if write {
		return s.db.db.Update(run)
	}
	return s.db.db.View(run)
}

func loadEngagement(tx *bolt.Tx, id string) (engagementRecord, error) {
	var record engagementRecord
	raw := tx.Bucket(engagementBucket).Get([]byte(id))
	if raw == nil {
		return record, ErrEngagementNotFound
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		return record, err
	}
	err := record.readLegacyWork(tx)
	return record, err
}

func (r *engagementRecord) readLegacyWork(tx *bolt.Tx) error {
	if r.ScopedWorkKeys {
		return nil
	}
	if r.OnceChangeKey != "" {
		r.OnceChangeKey = legacyEngagementChange(r.OnceChangeKey)
	}
	// Old versions also used completed for a verified end condition.
	if r.EndReason == EngagementCompleted && r.Current != nil {
		if r.Current.Once && r.OnceChangeKey != "" {
			work, err := loadEngagementWork(tx, r.Ref.ID, r.OnceChangeKey)
			if err != nil && err != ErrEngagementNotFound {
				return err
			}
			if err == nil && work.completed(*r.Current) {
				r.CompletedChangeKey = work.ChangeKey
			}
		}
		if r.CompletedChangeKey == "" {
			r.EndReason = EngagementConditionEnded
		}
	}
	return nil
}

func putEngagementJSON(bucket *bolt.Bucket, key []byte, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return bucket.Put(key, raw)
}

func saveEngagement(tx *bolt.Tx, record *engagementRecord) error {
	// A storage revision covers control, work and request-index mutations.
	record.Revision = rand.Text()
	record.ScopedWorkKeys = true
	return putEngagementJSON(tx.Bucket(engagementBucket), []byte(record.Ref.ID), record)
}

func pruneLegacyEngagementRevisions(tx *bolt.Tx) error {
	if key, _ := tx.Bucket(engagementRevisionsBucket).Cursor().First(); key == nil {
		return nil
	}
	// The old keys contain no ordering or evidence of which heads were
	// copied. Keep the current record revisions and discard the old index.
	if err := tx.DeleteBucket(engagementRevisionsBucket); err != nil {
		return err
	}
	_, err := tx.CreateBucket(engagementRevisionsBucket)
	return err
}

func (s *EngagementStore) Create(req EngagementCreate) (EngagementRef, error) {
	d := req.Decision
	if (req.Kind != EngagementTracking && req.Kind != EngagementDelegation) ||
		!req.Reason.valid() || !d.valid() || (d.Verdict == EngagementAccept && d.Change != EngagementAgree) {
		return EngagementRef{}, ErrEngagementInvalid
	}
	for _, person := range req.People {
		if person.ID <= 0 || person.ChatID == 0 {
			return EngagementRef{}, ErrEngagementInvalid
		}
	}
	for _, evidence := range req.Evidence {
		if !evidence.Source.valid() || !evidence.Kind.valid() || strings.TrimSpace(evidence.Text) == "" {
			return EngagementRef{}, ErrEngagementInvalid
		}
	}
	for _, chatID := range req.ManageChats {
		if chatID == 0 {
			return EngagementRef{}, ErrEngagementInvalid
		}
	}

	record := engagementRecord{Engagement: Engagement{
		Ref: EngagementRef{ID: rand.Text()}, Kind: req.Kind, Origin: d.Request.Source,
		Reason: req.Reason, People: req.People, Evidence: req.Evidence,
		ManageChats: req.ManageChats, InitiatorOnly: req.InitiatorOnly, Status: EngagementProposed,
	}}
	err := s.transact(true, func(tx *bolt.Tx) error {
		return record.decide(tx, d, d.Request.Source.eventKey())
	})
	if err != nil {
		return EngagementRef{}, err
	}
	return record.Ref, nil
}

func (s *EngagementStore) Decide(ref EngagementRef, d EngagementDecision) (EngagementRef, error) {
	if !d.valid() || ref.ID == "" || ref.Version == 0 {
		return EngagementRef{}, ErrEngagementInvalid
	}
	var next EngagementRef
	err := s.transact(true, func(tx *bolt.Tx) error {
		record, err := loadEngagement(tx, ref.ID)
		if err != nil {
			return err
		}
		view := EngagementViewer{ChatID: d.Request.Source.ChatID, ActorID: d.Request.Source.SenderID}
		if !record.visibleTo(view) {
			return ErrEngagementDenied
		}
		key := []byte(ref.ID + "/" + string(d.Request.Source.eventKey()))
		events := tx.Bucket(engagementRequestsBucket)
		// The original event is also already processed for this item.
		if events.Get(key) != nil || bytes.Equal(events.Get(d.Request.Source.eventKey()), []byte(ref.ID)) {
			return ErrEngagementProcessed
		}
		if record.Ref != ref {
			return ErrEngagementConflict
		}
		if err := record.decide(tx, d, key); err != nil {
			return err
		}
		next = record.Ref
		return nil
	})
	return next, err
}

func (r *engagementRecord) decide(tx *bolt.Tx, d EngagementDecision, key []byte) error {
	events := tx.Bucket(engagementRequestsBucket)
	if events.Get(key) != nil {
		return ErrEngagementProcessed
	}
	if r.expired(d.At) {
		if d.Verdict == EngagementAccept {
			return ErrEngagementTerminal
		}
		r.end(EngagementExpired)
	}
	if d.Verdict == EngagementAccept {
		if r.Status == EngagementEnded {
			return ErrEngagementTerminal
		}
		if basis := d.Authorized.ManagementBasis; basis != nil &&
			(r.Management == nil || r.Management.Source != *basis || basis.SenderID != r.Origin.SenderID ||
				!d.sees(EngagementAudience{ChatID: basis.ChatID, Access: basis.Access})) {
			return ErrEngagementDenied
		}
		if !r.mayChange(d.Request.Source.SenderID) {
			return ErrEngagementDenied
		}
		if err := r.accept(tx, d); err != nil {
			return err
		}
		if d.Change != EngagementMute && d.Change != EngagementUnmute {
			r.release()
		}
	}
	decision := EngagementDecisionRecord{
		Request: d.Request, Verdict: d.Verdict, Change: d.Change,
		Reply: d.Reply, Effective: r.Current, Management: r.Management,
		InvalidatedChanges: d.InvalidateChanges, Status: r.Status, At: d.At,
		ManagementBasis: d.Authorized.ManagementBasis, MuteUntil: d.MuteUntil,
	}
	if r.Initial == nil && r.Current != nil {
		r.Initial = &decision
	}
	r.Decisions = append(r.Decisions, decision)
	r.Ref.Version++
	if err := saveEngagement(tx, r); err != nil {
		return err
	}
	return events.Put(key, []byte(r.Ref.ID))
}

func (r *engagementRecord) accept(tx *bolt.Tx, d EngagementDecision) error {
	if d.Management != nil {
		if err := r.acceptManagement(d); err != nil {
			return err
		}
	}
	switch d.Change {
	case EngagementAgree, EngagementReaccept:
		expands := r.Current != nil && !d.Agreement.within(*r.Current)
		if (d.Change == EngagementReaccept && (r.Current == nil || !d.Request.ExplicitReaccept)) ||
			(d.Change == EngagementAgree && expands) {
			return ErrEngagementDenied
		}
		if err := d.Agreement.authorize(r.Kind, d); err != nil {
			return err
		}
		if err := r.authorizeRecipientSources(d); err != nil {
			return err
		}
		agreement, err := d.bindAgreement(r.Current)
		if err != nil {
			return err
		}
		if err := r.invalidateChanges(tx, d.InvalidateChanges); err != nil {
			return err
		}
		if r.Current == nil {
			r.Status = EngagementActive
		}
		if !d.Agreement.Once {
			r.OnceChangeKey = ""
		} else if expands && r.OnceChangeKey != "" {
			work, err := loadEngagementWork(tx, r.Ref.ID, r.OnceChangeKey)
			if err != nil {
				return err
			}
			if !work.covers(*d.Agreement) {
				r.OnceChangeKey = ""
			}
		}
		r.Current, r.NextCheck = &agreement, agreement.NextCheck
	case EngagementManage:
		if r.Current == nil {
			return ErrEngagementDenied
		}
	case EngagementPause:
		if r.Status != EngagementActive {
			return ErrEngagementDenied
		}
		r.Status, r.PauseReason = EngagementPaused, ""
	case EngagementResume:
		if r.Status != EngagementPaused {
			return ErrEngagementDenied
		}
		if err := r.Current.authorize(r.Kind, d); err != nil {
			return err
		}
		r.Status, r.PauseReason = EngagementActive, ""
	case EngagementEnd:
		r.end(EngagementStopped)
	case EngagementMute, EngagementUnmute:
		if r.Current == nil || (!d.MuteUntil.IsZero() && (d.Change != EngagementMute || !d.MuteUntil.After(d.At))) {
			return ErrEngagementDenied
		}
		r.Muted, r.MuteUntil = d.Change == EngagementMute, d.MuteUntil
	}
	return nil
}

func (r *engagementRecord) acceptManagement(d EngagementDecision) error {
	source := d.Request.Source
	basis, share := d.Authorized.ManagementBasis, d.Authorized.ManagementShare
	if basis != nil {
		if r.Management == nil || r.Management.Source != *basis ||
			basis.SenderID != r.Origin.SenderID ||
			!d.sees(EngagementAudience{ChatID: basis.ChatID, Access: basis.Access}) {
			return ErrEngagementDenied
		}
	} else if d.Management.InitiatorOnly != r.InitiatorOnly && source.SenderID != r.Origin.SenderID {
		return ErrEngagementDenied
	}
	if share != nil && (source.SenderID != r.Origin.SenderID || source.ChatID != source.SenderID ||
		source.Access != (MemoryDataAccess{}) || share.ChatID >= 0 || strings.TrimSpace(share.Summary) == "" ||
		!r.visibleTo(EngagementViewer{ChatID: share.ChatID})) {
		return ErrEngagementDenied
	}
	r.Management = &EngagementManagementTerm{
		EngagementManagementEdit: *d.Management, Source: source, Basis: basis, Share: share,
	}
	r.InitiatorOnly = d.Management.InitiatorOnly
	return nil
}

func (r *engagementRecord) invalidateChanges(tx *bolt.Tx, changes []string) error {
	for _, change := range changes {
		if r.Current == nil {
			return ErrEngagementDenied
		}
		work, err := loadEngagementWork(tx, r.Ref.ID, change)
		if err != nil {
			return err
		}
		if work.InvalidatedVersion == 0 {
			work.InvalidatedVersion = r.Ref.Version + 1
			if err := putEngagementJSON(tx.Bucket(engagementWorkBucket), engagementWorkKey(r.Ref.ID, change), work); err != nil {
				return err
			}
		}
		if r.OnceChangeKey == change {
			r.OnceChangeKey = ""
		}
	}
	return nil
}

func (r *engagementRecord) release() {
	r.LeaseToken = ""
	r.LeaseUntil = time.Time{}
}

func (r *engagementRecord) end(reason EngagementEndReason) {
	r.Status, r.EndReason, r.NextCheck = EngagementEnded, reason, time.Time{}
	r.release()
}

func (r engagementRecord) expired(now time.Time) bool {
	return r.Status != EngagementEnded && r.Current != nil && !r.Current.ExpiresAt.IsZero() && !now.Before(r.Current.ExpiresAt)
}

// Get exposes the raw record for trusted application code. Model input uses
// ReadVisible or Visible.
func (s *EngagementStore) Get(id string) (Engagement, error) {
	var item Engagement
	err := s.transact(false, func(tx *bolt.Tx) error {
		record, err := loadEngagement(tx, id)
		item = record.Engagement
		return err
	})
	return item, err
}

func (s *EngagementStore) ReadVisible(id string, viewer EngagementViewer) (EngagementProjection, error) {
	if !viewer.valid() {
		return EngagementProjection{}, ErrEngagementInvalid
	}
	var projection EngagementProjection
	err := s.transact(false, func(tx *bolt.Tx) error {
		item, err := loadEngagement(tx, id)
		if err != nil {
			return err
		}
		if !item.visibleTo(viewer) {
			return ErrEngagementDenied
		}
		projection, err = item.projectVisible(tx, viewer)
		return err
	})
	return projection, err
}

func (s *EngagementStore) Visible(viewer EngagementViewer, afterID string, limit int) ([]EngagementProjection, error) {
	if !viewer.valid() || limit <= 0 {
		return nil, ErrEngagementInvalid
	}
	var items []EngagementProjection
	err := s.transact(false, func(tx *bolt.Tx) error {
		cursor := tx.Bucket(engagementBucket).Cursor()
		key, raw := cursor.Seek([]byte(afterID))
		if key != nil && string(key) == afterID {
			key, raw = cursor.Next()
		}
		for ; key != nil && len(items) < limit; key, raw = cursor.Next() {
			var record engagementRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return err
			}
			if record.visibleTo(viewer) {
				if err := record.readLegacyWork(tx); err != nil {
					return err
				}
				projection, err := record.projectVisible(tx, viewer)
				if err != nil {
					return err
				}
				items = append(items, projection)
			}
		}
		return nil
	})
	return items, err
}

func (r engagementRecord) projectVisible(tx *bolt.Tx, viewer EngagementViewer) (EngagementProjection, error) {
	projection := r.project(viewer)
	if projection.Agreement == nil || !projection.Agreement.DeliversHere && len(projection.Agreement.Actions) == 0 {
		return projection, nil
	}
	// Read exact attempts in the same snapshot as the agreement. Work keeps
	// its audience even after a stop, under both legacy and scoped keys.
	matches, err := listEngagementWork(tx, r.Ref.ID, "", 1, func(work EngagementWork) bool {
		return slices.ContainsFunc(work.Results, func(result EngagementResult) bool {
			step := result.Step
			if step.UncertainNotice || !work.outcomeUncertain(result) {
				return false
			}
			if step.Recipient != 0 {
				// A source change retires work for execution, but the
				// retained recipient can still see its own delivery status.
				return step.Recipient == viewer.ChatID && work.includesRecipient(*r.Current, step)
			}
			access := step.Action.Access
			return work.permits(*r.Current, step) && (access.OwnerID == 0 || access.OwnerID == viewer.ActorID) &&
				viewer.sees(step.Action.DataChatID, access)
		})
	})
	if err != nil {
		return EngagementProjection{}, err
	}
	projection.OutcomeUncertain = len(matches) != 0
	return projection, nil
}

// ClaimDue leases at most limit active records, oldest due first. Lease expiry
// makes a record claimable again without resetting any work or reservations.
func (s *EngagementStore) ClaimDue(now time.Time, duration time.Duration, limit int) ([]EngagementClaim, error) {
	if now.IsZero() || duration <= 0 || limit <= 0 {
		return nil, ErrEngagementInvalid
	}
	type dueItem struct {
		id string
		at time.Time
	}
	var claims []EngagementClaim
	err := s.transact(true, func(tx *bolt.Tx) error {
		var due []dueItem
		var expired []string
		if err := tx.Bucket(engagementBucket).ForEach(func(key, raw []byte) error {
			var record engagementRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return err
			}
			if record.expired(now) {
				expired = append(expired, string(key))
			} else if record.Status == EngagementActive && !record.NextCheck.After(now) && !record.LeaseUntil.After(now) {
				due = append(due, dueItem{id: string(key), at: record.NextCheck})
			}
			return nil
		}); err != nil {
			return err
		}
		for _, id := range expired {
			record, err := loadEngagement(tx, id)
			if err != nil {
				return err
			}
			record.end(EngagementExpired)
			record.Ref.Version++
			if err := saveEngagement(tx, &record); err != nil {
				return err
			}
		}
		slices.SortStableFunc(due, func(a, b dueItem) int { return a.at.Compare(b.at) })
		for _, item := range due[:min(len(due), limit)] {
			record, err := loadEngagement(tx, item.id)
			if err != nil {
				return err
			}
			record.LeaseToken, record.LeaseUntil = rand.Text(), now.Add(duration)
			if err := saveEngagement(tx, &record); err != nil {
				return err
			}
			claims = append(claims, EngagementClaim{ID: item.id, Token: record.LeaseToken})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claims, nil
}

func (s *EngagementStore) withClaim(claim EngagementClaim, now time.Time, write bool, run func(*bolt.Tx, *engagementRecord) error) error {
	if claim.ID == "" || claim.Token == "" || now.IsZero() {
		return ErrEngagementInvalid
	}
	return s.transact(write, func(tx *bolt.Tx) error {
		record, err := loadEngagement(tx, claim.ID)
		if err != nil {
			return err
		}
		if record.Status != EngagementActive || record.LeaseToken != claim.Token || !now.Before(record.LeaseUntil) || record.expired(now) {
			return ErrEngagementLease
		}
		return run(tx, &record)
	})
}

func (s *EngagementStore) ReadClaim(claim EngagementClaim, now time.Time) (Engagement, error) {
	var item Engagement
	err := s.withClaim(claim, now, false, func(_ *bolt.Tx, record *engagementRecord) error {
		item = record.Engagement
		return nil
	})
	return item, err
}

// PauseClaim suspends work when a worker finds that execution is blocked,
// for example by a revoked connector grant. Management resumes it explicitly.
func (s *EngagementStore) PauseClaim(claim EngagementClaim, now time.Time) error {
	return s.pauseClaim(claim, now, "")
}

func (s *EngagementStore) pauseClaim(claim EngagementClaim, now time.Time, reason EngagementPauseReason) error {
	return s.withClaim(claim, now, true, func(tx *bolt.Tx, record *engagementRecord) error {
		record.Status, record.PauseReason = EngagementPaused, reason
		record.Ref.Version++
		record.release()
		return saveEngagement(tx, record)
	})
}

// Pause notifications are separate from fulfilling a work item. Reserve one
// attempt per recipient and pause revision. A crash leaves a visible
// reservation, and a later resume cannot accidentally repeat the old notice.
func (s *EngagementStore) beginPauseNotice(ref EngagementRef, now time.Time, recipient int64) (EngagementResult, error) {
	var result EngagementResult
	err := s.transact(true, func(tx *bolt.Tx) error {
		row, err := loadEngagement(tx, ref.ID)
		if err != nil {
			return err
		}
		if row.Ref != ref || row.Status != EngagementPaused || row.PauseReason != EngagementAccessLost ||
			row.expired(now) || row.Current == nil || !slices.Contains(row.Current.Recipients, recipient) {
			return ErrEngagementDenied
		}
		if slices.ContainsFunc(row.PauseNotices, func(r EngagementResult) bool {
			return r.Attempt == ref.Version && r.Step.Recipient == recipient
		}) {
			return ErrEngagementConflict
		}
		result = EngagementResult{
			Step: EngagementStep{Recipient: recipient}, Key: rand.Text(),
			Attempt: ref.Version, State: EngagementReserved, At: now,
		}
		row.PauseNotices = append(row.PauseNotices, result)
		return saveEngagement(tx, &row)
	})
	return result, err
}

func (s *EngagementStore) archivePauseNotice(id string, attempt EngagementResult, now time.Time, state EngagementResultState, receipt string) error {
	return s.transact(true, func(tx *bolt.Tx) error {
		row, err := loadEngagement(tx, id)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(row.PauseNotices, func(saved EngagementResult) bool {
			return saved.Key == attempt.Key && saved.Attempt == attempt.Attempt &&
				saved.Step == attempt.Step && saved.State == EngagementReserved
		})
		if i < 0 || attempt.State != EngagementReserved {
			return ErrEngagementConflict
		}
		row.PauseNotices[i].State, row.PauseNotices[i].Receipt, row.PauseNotices[i].At = state, receipt, now
		return saveEngagement(tx, &row)
	})
}

const legacyEngagementPrefix = "legacy:"

func legacyEngagementChange(nativeID string) string { return legacyEngagementPrefix + nativeID }

func scopedEngagementChange(source EngagementCapability, nativeID string) string {
	// The audience can change without making the upstream event a new one.
	identity, _ := json.Marshal(struct {
		Access            MemoryDataAccess
		Operation, Target string
		NativeID          string
	}{source.Access, source.Operation, source.Target, nativeID})
	return fmt.Sprintf("event:%x", sha256.Sum256(identity))
}

// Old native IDs can start with any string, including our new key format.
// Keep those rows under id/ and scoped rows under the adjacent id0 prefix.
func engagementWorkKey(id, change string) []byte {
	if native, legacy := strings.CutPrefix(change, legacyEngagementPrefix); legacy {
		return []byte(id + "/" + native)
	}
	return []byte(id + "0" + change)
}

func (w *EngagementWork) bind(id, change string) {
	w.ChangeKey = change
	for i := range w.Results {
		w.Results[i] = w.Results[i].bind(id, change)
	}
	for i := range w.CalendarDispatches {
		dispatch := &w.CalendarDispatches[i]
		dispatch.EngagementResult = dispatch.EngagementResult.bind(id, change)
	}
}

func (r EngagementResult) bind(id, change string) EngagementResult {
	r.AttemptRef = EngagementAttemptRef{id: id, change: change, key: r.Key, step: r.Step, attempt: r.Attempt, calendarReplay: r.CalendarReplay}
	return r
}

func loadEngagementWork(tx *bolt.Tx, id, change string) (EngagementWork, error) {
	var work EngagementWork
	raw := tx.Bucket(engagementWorkBucket).Get(engagementWorkKey(id, change))
	if raw == nil {
		return work, ErrEngagementNotFound
	}
	err := json.Unmarshal(raw, &work)
	work.bind(id, change)
	return work, err
}

// Work pages raw, durable observations and outcomes by ChangeKey. Execution
// still needs a current claim and live authorization.
func (s *EngagementStore) Work(id, afterChange string, limit int) ([]EngagementWork, error) {
	if id == "" || limit <= 0 {
		return nil, ErrEngagementInvalid
	}
	var works []EngagementWork
	err := s.transact(false, func(tx *bolt.Tx) error {
		if tx.Bucket(engagementBucket).Get([]byte(id)) == nil {
			return ErrEngagementNotFound
		}
		var err error
		works, err = listEngagementWork(tx, id, afterChange, limit, nil)
		return err
	})
	return works, err
}

// ClaimWork is the recovery queue for a live claim. Work also retains
// historical, cancelled and out-of-scope observations for reconciliation.
func (s *EngagementStore) ClaimWork(claim EngagementClaim, now time.Time, afterChange string, limit int) ([]EngagementWork, error) {
	if limit <= 0 {
		return nil, ErrEngagementInvalid
	}
	var works []EngagementWork
	err := s.withClaim(claim, now, false, func(tx *bolt.Tx, r *engagementRecord) error {
		var err error
		works, err = listEngagementWork(tx, claim.ID, afterChange, limit, r.canWork)
		return err
	})
	return works, err
}

func listEngagementWork(tx *bolt.Tx, id, afterChange string, limit int, include func(EngagementWork) bool) ([]EngagementWork, error) {
	oldPrefix, newPrefix := []byte(id+"/"), []byte(id+"0")
	start := oldPrefix
	if afterChange != "" {
		start = engagementWorkKey(id, afterChange)
	}
	var works []EngagementWork
	cursor := tx.Bucket(engagementWorkBucket).Cursor()
	key, raw := cursor.Seek(start)
	if afterChange != "" && bytes.Equal(key, start) {
		key, raw = cursor.Next()
	}
	for ; key != nil && len(works) < limit; key, raw = cursor.Next() {
		var change string
		switch {
		case bytes.HasPrefix(key, oldPrefix):
			change = legacyEngagementChange(string(key[len(oldPrefix):]))
		case bytes.HasPrefix(key, newPrefix):
			change = string(key[len(newPrefix):])
		default:
			return works, nil
		}
		var work EngagementWork
		if err := json.Unmarshal(raw, &work); err != nil {
			return nil, err
		}
		work.bind(id, change)
		if include == nil || include(work) {
			works = append(works, work)
		}
	}
	return works, nil
}

// Observe saves the latest verification independently of delivery. A non-empty
// native ID records a qualifying event at most once for this engagement and
// observer. Use the returned work's ChangeKey for the remaining store calls.
func (s *EngagementStore) Observe(claim EngagementClaim, now time.Time, observed EngagementObservation) (EngagementWork, bool, error) {
	return s.observe(claim, now, observed, false)
}

func (s *EngagementStore) observe(claim EngagementClaim, now time.Time, observed EngagementObservation, revalidateOnce bool) (EngagementWork, bool, error) {
	if !observed.Evidence.valid() || observed.Event != nil &&
		(!observed.Event.valid() || observed.Event.ID != observed.ChangeKey) {
		return EngagementWork{}, false, ErrEngagementInvalid
	}
	var work EngagementWork
	var fresh bool
	err := s.withClaim(claim, now, true, func(tx *bolt.Tx, r *engagementRecord) error {
		calendar := revalidateOnce && len(r.Current.Actions) != 0
		if calendar && now.Before(r.LastObservedAt) {
			return ErrEngagementConflict
		}
		observed.At = now
		if revalidateOnce && r.OnceChangeKey != "" {
			bound, err := loadEngagementWork(tx, claim.ID, r.OnceChangeKey)
			if err != nil {
				return err
			}
			if observed.ChangeKey != bound.Observation.ChangeKey {
				if bound.mayHaveTakenEffect() {
					// A delivered or uncertain one-shot result still owns
					// the slot. Keep its receipts even if it vanishes.
					observed.ChangeKey, observed.Event = "", nil
				} else {
					bound.Superseded = true
					if err := putEngagementJSON(tx.Bucket(engagementWorkBucket), engagementWorkKey(claim.ID, bound.ChangeKey), bound); err != nil {
						return err
					}
					r.OnceChangeKey = ""
				}
			}
		}
		if observed.ChangeKey != "" {
			change := scopedEngagementChange(r.Current.Observe, observed.ChangeKey)
			existing, err := loadEngagementWork(tx, claim.ID, legacyEngagementChange(observed.ChangeKey))
			if err == ErrEngagementNotFound || (err == nil && scopedEngagementChange(existing.Observe, existing.Observation.ChangeKey) != change) {
				existing, err = loadEngagementWork(tx, claim.ID, change)
			}
			if err != nil && err != ErrEngagementNotFound {
				return err
			}
			if err == nil {
				change = existing.ChangeKey
			}
			if existing.InvalidatedVersion == 0 && r.OnceChangeKey != "" && r.OnceChangeKey != change {
				return ErrEngagementDenied
			}
			switch err {
			case nil:
				if calendar && now.Before(existing.Observation.At) {
					return ErrEngagementConflict
				}
				work = existing
				if revalidateOnce || !r.Current.Once {
					work.Superseded = false
				}
				if (revalidateOnce || !r.Current.Once) && r.canWork(work) && !now.Before(work.Observation.At) {
					refresh := !revalidateOnce || !work.mayHaveTakenEffect()
					if refresh && calendar {
						// Preserve old request evidence before replacing the observation.
						for i, result := range work.Results {
							if result.Step.Action.Operation == engagementCalendarCreate {
								dispatch := work.calendarDispatch(result)
								work.Results[i].CalendarRequest = dispatch.CalendarRequest
							}
						}
					}
					if refresh || !calendar {
						// Read-only catch-up can use current evidence while keeping prior receipts intact.
						work.Observation = observed
					}
					if refresh {
						if err := putEngagementJSON(tx.Bucket(engagementWorkBucket), engagementWorkKey(claim.ID, change), work); err != nil {
							return err
						}
					}
				}
			case ErrEngagementNotFound:
				work = EngagementWork{
					ChangeKey: change, Observation: observed, Observe: r.Current.Observe, Trigger: r.Current.Trigger,
					Actions: r.Current.Actions, Recipients: r.Current.Recipients,
				}
				key := engagementWorkKey(claim.ID, change)
				if err := putEngagementJSON(tx.Bucket(engagementWorkBucket), key, work); err != nil {
					return err
				}
				fresh = true
			}
			if r.Current.Once && r.canWork(work) {
				r.OnceChangeKey = work.ChangeKey
			} else if revalidateOnce {
				work = EngagementWork{}
			}
		}
		if !now.Before(r.LastObservedAt) {
			r.LastObservedAt, r.LastObservation = now, &observed
		}
		return saveEngagement(tx, r)
	})
	if err != nil {
		return EngagementWork{}, false, err
	}
	return work, fresh, nil
}

func (w EngagementWork) covers(a EngagementAgreement) bool {
	if w.Observe != a.Observe {
		return false
	}
	for _, action := range a.Actions {
		if !slices.Contains(w.Actions, action) {
			return false
		}
	}
	for _, recipient := range a.Recipients {
		if !slices.Contains(w.Recipients, recipient) {
			return false
		}
	}
	return true
}

func (w EngagementWork) permits(a EngagementAgreement, step EngagementStep) bool {
	if w.Observe != a.Observe {
		return false
	}
	if step.Recipient != 0 {
		return w.includesRecipient(a, step)
	}
	return !step.UncertainNotice && step.Action.valid() && slices.Contains(w.Actions, step.Action) && slices.Contains(a.Actions, step.Action)
}

func (w EngagementWork) includesRecipient(a EngagementAgreement, step EngagementStep) bool {
	return step.Action == (EngagementCapability{}) &&
		slices.Contains(w.Recipients, step.Recipient) && slices.Contains(a.Recipients, step.Recipient)
}

func (w EngagementWork) resultIndex(step EngagementStep) int {
	return slices.IndexFunc(w.Results, func(result EngagementResult) bool { return result.Step == step })
}

func (w EngagementWork) mayHaveTakenEffect() bool {
	return slices.ContainsFunc(w.Results, func(result EngagementResult) bool {
		// A status notice has its own delivery receipt. Only a contracted step can pin the event.
		return !result.Step.UncertainNotice && result.State != EngagementNoEffect
	})
}

func (w EngagementWork) outcomeUncertain(result EngagementResult) bool {
	if result.State != EngagementReserved || result.Step.Action.Operation != engagementCalendarCreate {
		return result.State == EngagementUnknown
	}
	// A rejected replay cannot resolve an earlier call whose reply never arrived.
	return slices.ContainsFunc(w.CalendarDispatches, func(d engagementCalendarDispatch) bool {
		return d.Step == result.Step && d.Key == result.Key && d.Attempt == result.Attempt &&
			d.CalendarReplay > result.CalendarReplay && d.State == EngagementNoEffect
	})
}

// Older rows have one result per step. Capture that dispatch and its original
// evidence before replaying, settling, or refreshing the source observation.
func (w *EngagementWork) calendarDispatch(result EngagementResult) *engagementCalendarDispatch {
	for i := range w.CalendarDispatches {
		dispatch := &w.CalendarDispatches[i]
		if dispatch.AttemptRef == result.AttemptRef {
			return dispatch
		}
	}
	if result.CalendarRequest == "" && w.Observation.Event != nil {
		result.CalendarRequest, _ = calendarCreateArgs(result.Step.Action.Target, w.Observation.Event.calendarEvent())
	}
	w.CalendarDispatches = append(w.CalendarDispatches, engagementCalendarDispatch{result, w.Observation})
	return &w.CalendarDispatches[len(w.CalendarDispatches)-1]
}

func (r engagementRecord) canWork(w EngagementWork) bool {
	return w.InvalidatedVersion == 0 && !w.Superseded && w.Observe == r.Current.Observe &&
		(!r.Current.Once || (w.covers(*r.Current) && (r.OnceChangeKey == "" || r.OnceChangeKey == w.ChangeKey)))
}

func saveEngagementWork(tx *bolt.Tx, r *engagementRecord, work EngagementWork) error {
	if err := putEngagementJSON(tx.Bucket(engagementWorkBucket), engagementWorkKey(r.Ref.ID, work.ChangeKey), work); err != nil {
		return err
	}
	return saveEngagement(tx, r)
}

// BeginStep persists a reservation before any external call. A false start
// means an existing receipt or an attempt that needs reconciliation.
func (s *EngagementStore) BeginStep(claim EngagementClaim, now time.Time, change string, step EngagementStep) (EngagementResult, bool, error) {
	return s.beginStep(claim, now, change, step, false, false)
}

func (s *EngagementStore) beginStep(claim EngagementClaim, now time.Time, change string, step EngagementStep, reconcileOnly, allowCalendarReplay bool) (EngagementResult, bool, error) {
	var result EngagementResult
	var start bool
	err := s.withClaim(claim, now, true, func(tx *bolt.Tx, r *engagementRecord) error {
		var err error
		result, start, err = beginEngagementStep(tx, r, now, change, step, reconcileOnly, allowCalendarReplay)
		return err
	})
	return result, start, err
}

func beginEngagementStep(tx *bolt.Tx, r *engagementRecord, now time.Time, change string, step EngagementStep, reconcileOnly, allowCalendarReplay bool) (EngagementResult, bool, error) {
	work, err := loadEngagementWork(tx, r.Ref.ID, change)
	if err != nil {
		return EngagementResult{}, false, err
	}
	if !r.canWork(work) || !work.permits(*r.Current, step) {
		return EngagementResult{}, false, ErrEngagementDenied
	}
	calendar := step.Action.Operation == engagementCalendarCreate
	i := work.resultIndex(step)
	var result EngagementResult
	if i >= 0 {
		result = work.Results[i]
		if result.State != EngagementNoEffect {
			if !calendar || !allowCalendarReplay || result.State == EngagementSucceeded {
				return result, false, nil
			}
			// Give the replay a separate receipt without changing the key,
			// account, or arguments of the unresolved logical attempt.
			prior := *work.calendarDispatch(result)
			result = prior.EngagementResult
			result.CalendarReplay = uint64(len(work.CalendarDispatches))
			result.State, result.At, result.Receipt = EngagementReserved, now, ""
			result = result.bind(r.Ref.ID, change)
			work.CalendarDispatches = append(work.CalendarDispatches, engagementCalendarDispatch{result, prior.Observation})
			return result, true, saveEngagementWork(tx, r, work)
		}
	} else {
		result = EngagementResult{Step: step}
		i = len(work.Results)
		work.Results = append(work.Results, result)
	}
	if reconcileOnly {
		// A late negative receipt must send the next claim back through
		// source verification before another attempt can be reserved.
		return EngagementResult{}, false, ErrEngagementConflict
	}
	if calendar && result.Attempt != 0 {
		result.CalendarRequest = work.calendarDispatch(result).CalendarRequest
	}
	if calendar && work.Observation.Event != nil {
		request, err := calendarCreateArgs(step.Action.Target, work.Observation.Event.calendarEvent())
		if err != nil {
			return EngagementResult{}, false, err
		}
		if result.CalendarRequest != request {
			result.Key = ""
		}
		result.CalendarRequest = request
	}
	if result.Key == "" {
		result.Key = rand.Text()
	}
	result.Attempt++
	result.CalendarReplay = 0
	result.State, result.At, result.Receipt = EngagementReserved, now, ""
	result = result.bind(r.Ref.ID, change)
	if calendar {
		work.CalendarDispatches = append(work.CalendarDispatches, engagementCalendarDispatch{result, work.Observation})
	}
	work.Results[i] = result
	if r.Current.Once {
		r.OnceChangeKey = change
	}
	if err := saveEngagementWork(tx, r, work); err != nil {
		return EngagementResult{}, false, err
	}
	return result, true, nil
}

// Reserve only the entries that will share the same Telegram message.
func (s *EngagementStore) beginCombinedNotice(claim EngagementClaim, now time.Time, changes []string, step EngagementStep) ([]EngagementAttemptRef, error) {
	var attempts []EngagementAttemptRef
	err := s.withClaim(claim, now, true, func(tx *bolt.Tx, r *engagementRecord) error {
		for _, change := range changes {
			result, start, err := beginEngagementStep(tx, r, now, change, step, false, false)
			if err != nil {
				return err
			}
			if !start {
				return ErrEngagementConflict
			}
			attempts = append(attempts, result.AttemptRef)
		}
		return nil
	})
	return attempts, err
}

// RecordOutcome settles a reservation by its logical attempt number. For
// calendar attempts with replays, use ArchiveOutcome with the dispatch reference.
func (s *EngagementStore) RecordOutcome(claim EngagementClaim, now time.Time, change string, step EngagementStep, attempt uint64, state EngagementResultState, receipt string) error {
	if (state != EngagementSucceeded && state != EngagementNoEffect && state != EngagementUnknown) ||
		(state != EngagementUnknown && strings.TrimSpace(receipt) == "") || attempt == 0 {
		return ErrEngagementInvalid
	}
	return s.withClaim(claim, now, true, func(tx *bolt.Tx, r *engagementRecord) error {
		work, err := loadEngagementWork(tx, claim.ID, change)
		if err != nil {
			return err
		}
		if !r.canWork(work) || !work.permits(*r.Current, step) {
			return ErrEngagementDenied
		}
		i := work.resultIndex(step)
		if i < 0 || work.Results[i].Attempt != attempt {
			return ErrEngagementConflict
		}
		if slices.ContainsFunc(work.CalendarDispatches, func(d engagementCalendarDispatch) bool {
			return d.Step == step && d.Attempt == attempt && d.CalendarReplay != 0
		}) {
			return ErrEngagementConflict
		}
		_, err = settleEngagementAttempt(tx, r, work, i, work.Results[i].AttemptRef, now, state, receipt)
		return err
	})
}

// ArchiveOutcome records a late result for a persisted reservation.
// Use the initiating executor's attempt reference or the one obtained from
// Work when independently reconciling this exact step and external key.
// It does not require or issue a claim and never schedules work.
func (s *EngagementStore) ArchiveOutcome(ref EngagementAttemptRef, now time.Time, state EngagementResultState, receipt string) error {
	_, err := s.archiveOutcome(ref, now, state, receipt)
	return err
}

func (s *EngagementStore) archiveOutcome(ref EngagementAttemptRef, now time.Time, state EngagementResultState, receipt string) (EngagementResultState, error) {
	return s.archiveOutcomes([]EngagementAttemptRef{ref}, now, state, receipt)
}

func (s *EngagementStore) archiveOutcomes(refs []EngagementAttemptRef, now time.Time, state EngagementResultState, receipt string) (EngagementResultState, error) {
	if len(refs) == 0 || now.IsZero() || (state != EngagementSucceeded && state != EngagementNoEffect && state != EngagementUnknown) ||
		strings.TrimSpace(receipt) == "" {
		return "", ErrEngagementInvalid
	}
	for _, ref := range refs {
		if ref.id == "" || ref.id != refs[0].id || ref.change == "" || ref.key == "" || ref.attempt == 0 {
			return "", ErrEngagementInvalid
		}
	}
	var settled EngagementResultState
	err := s.transact(true, func(tx *bolt.Tx) error {
		r, err := loadEngagement(tx, refs[0].id)
		if err != nil {
			return err
		}
		for _, ref := range refs {
			work, err := loadEngagementWork(tx, ref.id, ref.change)
			if err != nil {
				return err
			}
			i := work.resultIndex(ref.step)
			if i < 0 {
				return ErrEngagementConflict
			}
			settled, err = settleEngagementAttempt(tx, &r, work, i, ref, now, state, receipt)
			if err != nil {
				return err
			}
		}
		return nil
	})
	return settled, err
}

func settleEngagementAttempt(tx *bolt.Tx, r *engagementRecord, work EngagementWork, index int, ref EngagementAttemptRef, now time.Time, state EngagementResultState, receipt string) (EngagementResultState, error) {
	result := &work.Results[index]
	previous := result.State
	if result.Step.Action.Operation == engagementCalendarCreate {
		if changed, err := work.settleCalendarDispatch(result, ref, now, state, receipt); !changed {
			return previous, err
		}
	} else {
		if result.AttemptRef != ref {
			return "", ErrEngagementConflict
		}
		if changed, err := result.settle(now, state, receipt); !changed {
			return previous, err
		}
	}
	state = result.State
	if !result.Step.UncertainNotice {
		if previous != EngagementUnknown && state == EngagementUnknown {
			r.UncertainSteps++
		} else if previous == EngagementUnknown && state != EngagementUnknown && r.UncertainSteps > 0 {
			r.UncertainSteps--
		}
	}
	if id := result.Step.Recipient; id != 0 && !result.Step.UncertainNotice && state == EngagementSucceeded {
		if now.After(r.LastDeliveredAt) {
			r.LastDeliveredAt = now
		}
		if r.DeliveredTo == nil {
			r.DeliveredTo = make(map[int64]time.Time)
		}
		if now.After(r.DeliveredTo[id]) {
			r.DeliveredTo[id] = now
		}
	}
	return state, saveEngagementWork(tx, r, work)
}

func (w *EngagementWork) settleCalendarDispatch(result *EngagementResult, ref EngagementAttemptRef, now time.Time, state EngagementResultState, receipt string) (bool, error) {
	w.calendarDispatch(*result)
	i := slices.IndexFunc(w.CalendarDispatches, func(d engagementCalendarDispatch) bool { return d.AttemptRef == ref })
	if i < 0 {
		return false, ErrEngagementConflict
	}
	dispatch := &w.CalendarDispatches[i].EngagementResult
	changed, err := dispatch.settle(now, state, receipt)
	if !changed || result.Attempt != ref.attempt || result.Key != ref.key || result.State == EngagementSucceeded {
		return changed, err
	}
	// A definitive negative only settles its dispatch. Any other possible
	// effect keeps the logical action pinned to this request.
	*result = *dispatch
	for _, candidate := range w.CalendarDispatches {
		if candidate.Step == ref.step && candidate.Attempt == ref.attempt && candidate.Key == ref.key &&
			calendarResultPriority(candidate.State) > calendarResultPriority(result.State) {
			*result = candidate.EngagementResult
		}
	}
	if result.State == EngagementReserved && w.outcomeUncertain(*result) {
		result.State, result.Receipt, result.At = EngagementUnknown, "calendar outcome could not be verified", now
	}
	return true, nil
}

func (r *EngagementResult) settle(now time.Time, state EngagementResultState, receipt string) (bool, error) {
	if r.State == state && (state == EngagementUnknown || r.Receipt == receipt) {
		return false, nil
	}
	if r.State != EngagementReserved && r.State != EngagementUnknown {
		return false, ErrEngagementConflict
	}
	r.State, r.Receipt, r.At = state, receipt, now
	return true, nil
}

func calendarResultPriority(state EngagementResultState) int {
	switch state {
	case EngagementSucceeded:
		return 3
	case EngagementUnknown:
		return 2
	case EngagementReserved:
		return 1
	default:
		return 0
	}
}

func (w EngagementWork) completed(a EngagementAgreement) bool {
	if w.InvalidatedVersion != 0 || w.Superseded || w.Observe != a.Observe || (a.Once && !w.covers(a)) {
		return false
	}
	// Old recurring work only owes steps that were originally committed and
	// still allowed. One-time work must cover the entire current agreement.
	settled := func(step EngagementStep) bool {
		if !w.permits(a, step) {
			return true
		}
		i := w.resultIndex(step)
		return i >= 0 && w.Results[i].State == EngagementSucceeded && strings.TrimSpace(w.Results[i].Receipt) != ""
	}
	for _, action := range a.Actions {
		if !settled(EngagementStep{Action: action}) {
			return false
		}
	}
	for _, recipient := range a.Recipients {
		if !settled(EngagementStep{Recipient: recipient}) {
			return false
		}
	}
	return true
}

// FinishCheck releases a claim with a next check, or ends the agreement once
// the named change is complete or the worker verifies its agreed end condition.
func (s *EngagementStore) FinishCheck(claim EngagementClaim, now time.Time, check EngagementCheck) error {
	return s.withClaim(claim, now, true, func(tx *bolt.Tx, r *engagementRecord) error {
		if check.CompletedChange != "" {
			work, err := loadEngagementWork(tx, claim.ID, check.CompletedChange)
			if err != nil {
				return err
			}
			if (r.Current.Once && r.OnceChangeKey != check.CompletedChange) || !work.completed(*r.Current) {
				return ErrEngagementIncomplete
			}
		}
		if check.EndConditionMet && r.Current.EndCondition.Text == "" {
			return ErrEngagementInvalid
		}
		if r.Current.Once && check.CompletedChange != "" {
			r.end(EngagementCompleted)
			r.CompletedChangeKey = check.CompletedChange
			r.Ref.Version++
		} else if check.EndConditionMet {
			r.end(EngagementConditionEnded)
			r.Ref.Version++
		} else {
			if !check.NextCheck.After(now) {
				return ErrEngagementInvalid
			}
			r.NextCheck = check.NextCheck
			r.release()
		}
		return saveEngagement(tx, r)
	})
}

func copyEngagements(source, target *bolt.Tx) error {
	// Work and request indexes share the metadata record's lineage. Each
	// mutation of work also advances that record in the same transaction.
	toRecords := target.Bucket(engagementBucket)
	if err := source.Bucket(engagementBucket).ForEach(func(id, raw []byte) error {
		var record engagementRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		if current := toRecords.Get(id); current != nil && !bytes.Equal(current, raw) {
			var prior engagementRecord
			if err := json.Unmarshal(current, &prior); err != nil {
				return err
			}
			if prior.Revision == "" || prior.Revision != record.Revision && !slices.Contains(record.MigrationAnchors, prior.Revision) {
				return fmt.Errorf("%w: incompatible engagement revision in destination", ErrEngagementConflict)
			}
		}
		// The source's current head stays behind. Remember that exact head
		// on the destination, even if it has never been migrated before.
		if record.Revision != "" && !slices.Contains(record.MigrationAnchors, record.Revision) {
			record.MigrationAnchors = append(record.MigrationAnchors, record.Revision)
			record.MigrationAnchors = record.MigrationAnchors[max(0, len(record.MigrationAnchors)-engagementMigrationAnchors):]
			return putEngagementJSON(toRecords, id, record)
		}
		return toRecords.Put(id, raw)
	}); err != nil {
		return err
	}
	for _, name := range [][]byte{engagementRequestsBucket, engagementWorkBucket} {
		destination := target.Bucket(name)
		if err := source.Bucket(name).ForEach(func(key, value []byte) error {
			if current := destination.Get(key); bytes.Equal(name, engagementRequestsBucket) && current != nil && !bytes.Equal(current, value) {
				return fmt.Errorf("%w: destination already processed an engagement request", ErrEngagementConflict)
			}
			return destination.Put(key, value)
		}); err != nil {
			return err
		}
	}
	return nil
}
