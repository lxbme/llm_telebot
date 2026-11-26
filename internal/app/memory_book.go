package app

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"

	bolt "go.etcd.io/bbolt"
)

var (
	memoryEntryBucket = []byte("memory_entries")
	memoryEventBucket = []byte("memory_events")

	ErrMemoryInvalid   = errors.New("invalid memory request")
	ErrMemoryDenied    = errors.New("memory audience or ownership mismatch")
	ErrMemoryProcessed = errors.New("memory event already processed")
	ErrMemoryConflict  = errors.New("memory version conflict")
	ErrMemoryNotFound  = errors.New("memory entry not found")
)

type MemoryEvidenceKind string

const (
	MemoryExperience MemoryEvidenceKind = "experience"
	MemoryStatement  MemoryEvidenceKind = "statement"
	MemoryInference  MemoryEvidenceKind = "inference"
	MemoryCorrection MemoryEvidenceKind = "correction"
)

type MemoryCue string

const (
	MemoryFamiliar MemoryCue = "familiar"
	MemoryReserved MemoryCue = "reserved"
	MemoryTrusted  MemoryCue = "trusted"
	MemoryAskFirst MemoryCue = "ask_first"
)

// MemoryDataAccess identifies a connector permission and its delegated scope.
// Key is a stable application identifier, separate from credentials.
type MemoryDataAccess struct {
	OwnerID int64
	Key     string
}

// MemorySource is supplied by trusted application code from a Telegram event.
// Access is zero for ordinary chat material.
type MemorySource struct {
	ChatID, MessageID, SenderID int64
	Access                      MemoryDataAccess
}

type MemoryEvidence struct {
	Source MemorySource
	Kind   MemoryEvidenceKind
	Text   string
}

type MemoryRef struct {
	ID      string
	Version uint64
}

// MemoryLearn creates an entry when Expect is zero. An update supplies the
// reference that the extractor read and may omit Subjects to keep them.
type MemoryLearn struct {
	Expect   MemoryRef
	Subjects []int64
	Evidence MemoryEvidence
	Text     string
	Cue      MemoryCue
}

type MemoryEdit struct {
	Text string
	Cue  MemoryCue
}

// MemoryCorrectionTarget ties a verified group event to the inference version
// found for its subject. Keep it in application state.
type MemoryCorrectionTarget struct {
	ref       MemoryRef
	subjectID int64
}

// MemoryEntry contains raw evidence and is for the admin private-chat path.
type MemoryEntry struct {
	Ref            MemoryRef
	AudienceChatID int64
	Subjects       []int64
	Evidence       []MemoryEvidence
	Text           string
	Cue            MemoryCue
	Shares         map[int64]string
	Retired        bool `json:",omitempty"`
}

// MemoryQuery describes the actual audience. Participants are verified
// Telegram user IDs. DataAccess contains current grants for this audience and
// use, checked by the caller. Both output limits must be positive.
type MemoryQuery struct {
	ChatID        int64
	Participants  []int64
	DataAccess    map[MemoryDataAccess]bool
	Query         string
	MaxItems      int
	MaxBytes      int
	learnFor      int64 // limit edit candidates to local, single-subject conclusions
	trackCues     bool  // include internal refs for in-flight role revalidation
	localCuesOnly bool  // background group turns use only cues from that chat
}

// MemoryProjection carries audience-approved text or a finite cue. Keep Ref
// in application state when constructing prompts. SubjectID is set for
// relationship cues involving a verified participant.
type MemoryProjection struct {
	Ref       MemoryRef
	Text      string
	Cue       MemoryCue
	SubjectID int64
}

// MemoryBook keeps one stable handle across ChatDB rebinds.
type MemoryBook struct {
	mu sync.RWMutex
	db *ChatDB
}

func NewMemoryBook(db *ChatDB) *MemoryBook { return &MemoryBook{db: db} }

type memoryRecord struct {
	MemoryEntry
	Deleted        bool
	Order          uint64
	PriorRevisions []string `json:",omitempty"`
}

func (b *MemoryBook) transact(write bool, run func(*bolt.Tx) error) error {
	if b == nil {
		return bolt.ErrDatabaseNotOpen
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.db == nil {
		return bolt.ErrDatabaseNotOpen
	}
	if write {
		return b.db.db.Update(run)
	}
	return b.db.db.View(run)
}

func (s MemorySource) valid() bool {
	return s.ChatID != 0 && s.MessageID > 0 && s.SenderID > 0 &&
		(s.ChatID < 0 || s.ChatID == s.SenderID) &&
		(s.Access == (MemoryDataAccess{}) || (s.Access.OwnerID > 0 && s.Access.Key != ""))
}

func (s MemorySource) eventKey() []byte {
	return []byte(strconv.FormatInt(s.ChatID, 10) + "/" + strconv.FormatInt(s.MessageID, 10))
}

func (k MemoryEvidenceKind) valid() bool {
	switch k {
	case MemoryExperience, MemoryStatement, MemoryInference, MemoryCorrection:
		return true
	}
	return false
}

func (c MemoryCue) valid(subjects []int64) bool {
	switch c {
	case "", MemoryAskFirst:
		return true
	case MemoryFamiliar, MemoryReserved, MemoryTrusted:
		return len(subjects) == 1
	}
	return false
}

// Learn commits the evidence and its event index together. Each Telegram event
// can contribute to one entry, including a correction to an existing entry.
func (b *MemoryBook) Learn(req MemoryLearn) (MemoryRef, error) {
	source := req.Evidence.Source
	req.Text = strings.TrimSpace(req.Text)
	if !source.valid() || !req.Evidence.Kind.valid() || req.Text == "" ||
		(req.Expect.ID == "") != (req.Expect.Version == 0) {
		return MemoryRef{}, ErrMemoryInvalid
	}
	subjects := slices.Clone(req.Subjects)
	slices.Sort(subjects)
	subjects = slices.Compact(subjects)
	if len(subjects) > 0 && subjects[0] <= 0 {
		return MemoryRef{}, ErrMemoryInvalid
	}

	var ref MemoryRef
	err := b.transact(true, func(tx *bolt.Tx) error {
		events := tx.Bucket(memoryEventBucket)
		key := source.eventKey()
		if events.Get(key) != nil {
			return ErrMemoryProcessed
		}

		record := memoryRecord{MemoryEntry: MemoryEntry{
			Subjects: subjects, AudienceChatID: source.ChatID,
		}}
		if req.Expect.ID == "" {
			if len(subjects) == 0 {
				return ErrMemoryInvalid
			}
			record.Ref.ID = rand.Text()
		} else {
			var err error
			record, err = loadMemoryVersion(tx, req.Expect)
			if err != nil {
				return err
			}
			if source.ChatID != record.AudienceChatID ||
				record.Retired || (len(subjects) > 0 && !slices.Equal(subjects, record.Subjects)) {
				return ErrMemoryDenied
			}
		}
		if !req.Cue.valid(record.Subjects) {
			return ErrMemoryInvalid
		}
		if err := record.advanceRevision(); err != nil {
			return err
		}
		record.Evidence = append(record.Evidence, req.Evidence)
		// Keep the approved wording when new evidence confirms the conclusion.
		// Visible still checks the current grants for every source.
		if req.Text != record.Text || req.Evidence.Kind == MemoryCorrection {
			record.Shares = nil
		}
		if err := updateMemoryContent(tx, &record, MemoryEdit{Text: req.Text, Cue: req.Cue}); err != nil {
			return err
		}
		if err := events.Put(key, []byte(record.Ref.ID)); err != nil {
			return err
		}
		ref = record.Ref
		return nil
	})
	if err != nil {
		return MemoryRef{}, err
	}
	return ref, nil
}

// canRetryCorrection prevents an older message from overriding a newer
// independent statement on the same local record. The subsequent write still
// uses the version read by the reflection.
func (b *MemoryBook) canRetryCorrection(ref MemoryRef, source MemorySource) bool {
	return b.transact(false, func(tx *bolt.Tx) error {
		record, err := loadMemoryVersion(tx, ref)
		if err != nil {
			return err
		}
		for _, evidence := range record.Evidence {
			if evidence.Source.ChatID == source.ChatID && evidence.Source.MessageID > source.MessageID {
				return ErrMemoryConflict
			}
		}
		return nil
	}) == nil
}

// FindCorrectableInference resolves the exact current group evidence event.
// The caller supplies a verified subject and group source, never a model's
// proposed IDs. The result carries no group text or evidence.
func (b *MemoryBook) FindCorrectableInference(subjectID int64, groupSource MemorySource) (MemoryCorrectionTarget, error) {
	if subjectID <= 0 || groupSource.ChatID >= 0 || !groupSource.valid() {
		return MemoryCorrectionTarget{}, ErrMemoryInvalid
	}
	var target MemoryCorrectionTarget
	err := b.transact(false, func(tx *bolt.Tx) error {
		id := tx.Bucket(memoryEventBucket).Get(groupSource.eventKey())
		record, err := loadMemoryRecord(tx, string(id))
		if err != nil {
			return err
		}
		if !record.correctableBy(subjectID) || record.Evidence[len(record.Evidence)-1].Source != groupSource {
			return ErrMemoryDenied
		}
		target = MemoryCorrectionTarget{ref: record.Ref, subjectID: subjectID}
		return nil
	})
	return target, err
}

func (r memoryRecord) correctableBy(subjectID int64) bool {
	// Group entries cannot be shared. Each automatic learning revision adds an
	// evidence row, so a higher version denotes a manual edit or retirement.
	if r.Retired || r.AudienceChatID >= 0 || len(r.Subjects) != 1 || r.Subjects[0] != subjectID ||
		len(r.Evidence) == 0 || r.Ref.Version != uint64(len(r.Evidence)) {
		return false
	}
	return !slices.ContainsFunc(r.Evidence, func(evidence MemoryEvidence) bool {
		return evidence.Kind != MemoryInference || evidence.Source.Access != (MemoryDataAccess{})
	})
}

// CorrectInference retires the group inference and records the explanation in
// the subject's private chat. Both changes and the new event index commit
// together. Only a relationship cue may accompany the private explanation.
func (b *MemoryBook) CorrectInference(target MemoryCorrectionTarget, evidence MemoryEvidence, edit MemoryEdit) (MemoryRef, error) {
	source := evidence.Source
	subjects := []int64{target.subjectID}
	edit.Text = strings.TrimSpace(edit.Text)
	if target.subjectID <= 0 || !source.valid() || source.ChatID <= 0 ||
		source.Access != (MemoryDataAccess{}) || evidence.Kind != MemoryCorrection ||
		edit.Text == "" || edit.Cue == MemoryAskFirst || !edit.Cue.valid(subjects) {
		return MemoryRef{}, ErrMemoryInvalid
	}
	if source.SenderID != target.subjectID {
		return MemoryRef{}, ErrMemoryDenied
	}
	var ref MemoryRef
	err := b.transact(true, func(tx *bolt.Tx) error {
		events := tx.Bucket(memoryEventBucket)
		key := source.eventKey()
		if events.Get(key) != nil {
			return ErrMemoryProcessed
		}
		old, err := loadMemoryVersion(tx, target.ref)
		if err != nil {
			return err
		}
		if !old.correctableBy(target.subjectID) {
			return ErrMemoryDenied
		}
		if err := old.advanceRevision(); err != nil {
			return err
		}
		old.Retired = true
		if err := saveMemoryRecord(tx, old); err != nil {
			return err
		}

		private := memoryRecord{MemoryEntry: MemoryEntry{
			Ref:            MemoryRef{ID: rand.Text(), Version: 1},
			AudienceChatID: source.ChatID,
			Subjects:       subjects,
			Evidence:       []MemoryEvidence{evidence},
		}}
		if err := updateMemoryContent(tx, &private, edit); err != nil {
			return err
		}
		if err := events.Put(key, []byte(private.Ref.ID)); err != nil {
			return err
		}
		ref = private.Ref
		return nil
	})
	if err != nil {
		return MemoryRef{}, err
	}
	return ref, nil
}

// findOwnConclusion resolves the approved wording, including a unique excerpt, to a private
// entry without exposing raw rows or IDs to the user or the model.
func (b *MemoryBook) findOwnConclusion(ownerID int64, text string) (MemoryRef, error) {
	var ref, clause MemoryRef
	var ambiguous bool
	err := b.transact(false, func(tx *bolt.Tx) error {
		return tx.Bucket(memoryEntryBucket).ForEach(func(_, raw []byte) error {
			var record memoryRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return err
			}
			if !record.ownPrivateConclusion(ownerID) || !record.dataAllowed(nil) {
				return nil
			}
			if record.Text != text {
				if strings.Contains(record.Text, text) {
					ambiguous = clause.ID != ""
					clause = record.Ref
				}
				return nil
			}
			if ref.ID != "" {
				return ErrMemoryConflict
			}
			ref = record.Ref
			return nil
		})
	})
	if err == nil && ref.ID == "" {
		switch {
		case ambiguous:
			err = ErrMemoryConflict
		case clause.ID != "":
			ref = clause
		default:
			err = ErrMemoryNotFound
		}
	}
	return ref, err
}

func (r memoryRecord) ownPrivateConclusion(ownerID int64) bool {
	return !r.Deleted && !r.Retired && r.AudienceChatID == ownerID && len(r.Subjects) == 1 && r.Subjects[0] == ownerID
}

// withdrawOwnConclusion removes every approval of the exact wording by
// this owner to this group in one transaction.
func (b *MemoryBook) withdrawOwnConclusion(ownerID, groupID int64, text string) error {
	if ownerID <= 0 || groupID >= 0 || strings.TrimSpace(text) == "" {
		return ErrMemoryInvalid
	}
	return b.transact(true, func(tx *bolt.Tx) error {
		var matches []memoryRecord
		err := tx.Bucket(memoryEntryBucket).ForEach(func(_, raw []byte) error {
			var record memoryRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return err
			}
			if record.ownPrivateConclusion(ownerID) && record.Shares[groupID] == text {
				matches = append(matches, record)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if len(matches) == 0 {
			return ErrMemoryNotFound
		}
		for _, record := range matches {
			if err := record.advanceRevision(); err != nil {
				return err
			}
			delete(record.Shares, groupID)
			if err := saveMemoryRecord(tx, record); err != nil {
				return err
			}
		}
		return nil
	})
}

// Share stores the exact wording approved by the sole subject of a private
// entry. An empty text revokes the grant for this group. The caller verifies the
// Telegram owner, destination group and explicit approval before calling.
func (b *MemoryBook) Share(ref MemoryRef, ownerID, groupID int64, text string) (MemoryRef, error) {
	if ownerID <= 0 || groupID >= 0 || (text != "" && strings.TrimSpace(text) == "") {
		return MemoryRef{}, ErrMemoryInvalid
	}
	return b.change(ref, func(tx *bolt.Tx, record *memoryRecord) error {
		if record.AudienceChatID != ownerID || len(record.Subjects) != 1 || record.Subjects[0] != ownerID {
			return ErrMemoryDenied
		}
		if text != "" {
			for _, evidence := range record.Evidence {
				if access := evidence.Source.Access; access.OwnerID != 0 && access.OwnerID != ownerID {
					return ErrMemoryDenied
				}
			}
		}
		if err := record.advanceRevision(); err != nil {
			return err
		}
		if text == "" {
			delete(record.Shares, groupID)
		} else {
			if record.Shares == nil {
				record.Shares = make(map[int64]string)
			}
			record.Shares[groupID] = text
		}
		return saveMemoryRecord(tx, *record)
	})
}

// Revise and Delete are for authorized admin callers. Revise preserves the
// evidence and invalidates shares when it replaces the current conclusion.
// An explicit admin revision can replace a retired inference.
func (b *MemoryBook) Revise(ref MemoryRef, edit MemoryEdit) (MemoryRef, error) {
	edit.Text = strings.TrimSpace(edit.Text)
	if edit.Text == "" {
		return MemoryRef{}, ErrMemoryInvalid
	}
	return b.change(ref, func(tx *bolt.Tx, record *memoryRecord) error {
		if !edit.Cue.valid(record.Subjects) {
			return ErrMemoryInvalid
		}
		if err := record.advanceRevision(); err != nil {
			return err
		}
		record.Retired = false
		record.Shares = nil
		return updateMemoryContent(tx, record, edit)
	})
}

// Delete keeps the tombstone and event index while removing the raw material.
func (b *MemoryBook) Delete(ref MemoryRef) error {
	_, err := b.change(ref, func(tx *bolt.Tx, record *memoryRecord) error {
		if err := record.advanceRevision(); err != nil {
			return err
		}
		*record = memoryRecord{
			MemoryEntry:    MemoryEntry{Ref: record.Ref},
			Deleted:        true,
			PriorRevisions: record.PriorRevisions,
		}
		return saveMemoryRecord(tx, *record)
	})
	return err
}

func (b *MemoryBook) change(ref MemoryRef, apply func(*bolt.Tx, *memoryRecord) error) (MemoryRef, error) {
	var next MemoryRef
	err := b.transact(true, func(tx *bolt.Tx) error {
		record, err := loadMemoryVersion(tx, ref)
		if err != nil {
			return err
		}
		if err := apply(tx, &record); err != nil {
			return err
		}
		next = record.Ref
		return nil
	})
	if err != nil {
		return MemoryRef{}, err
	}
	return next, nil
}

func loadMemoryVersion(tx *bolt.Tx, ref MemoryRef) (memoryRecord, error) {
	if ref.ID == "" || ref.Version == 0 {
		return memoryRecord{}, ErrMemoryInvalid
	}
	record, err := loadMemoryRecord(tx, ref.ID)
	if err == nil && record.Ref.Version != ref.Version {
		return memoryRecord{}, ErrMemoryConflict
	}
	return record, err
}

func loadMemoryRecord(tx *bolt.Tx, id string) (memoryRecord, error) {
	raw := tx.Bucket(memoryEntryBucket).Get([]byte(id))
	if raw == nil {
		return memoryRecord{}, ErrMemoryNotFound
	}
	var record memoryRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return memoryRecord{}, err
	}
	if record.Deleted {
		return memoryRecord{}, ErrMemoryNotFound
	}
	return record, nil
}

func updateMemoryContent(tx *bolt.Tx, record *memoryRecord, edit MemoryEdit) error {
	order, err := tx.Bucket(memoryEntryBucket).NextSequence()
	if err != nil {
		return err
	}
	record.Order = order
	record.Text, record.Cue = edit.Text, edit.Cue
	return saveMemoryRecord(tx, *record)
}

// revision excludes the destination-local ranking sequence. Including the
// parent fingerprint distinguishes histories that reach the same current state.
func (r memoryRecord) revision() (string, error) {
	r.Order = 0
	if n := len(r.PriorRevisions); n > 0 {
		r.PriorRevisions = r.PriorRevisions[n-1:]
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// Advance before editing so the first write to an older database anchors its
// existing revision even when previous revisions have no recorded fingerprints.
func (r *memoryRecord) advanceRevision() error {
	if r.Ref.Version > 0 {
		parent, err := r.revision()
		if err != nil {
			return err
		}
		r.PriorRevisions = append(r.PriorRevisions, parent)
	}
	r.Ref.Version++
	return nil
}

func saveMemoryRecord(tx *bolt.Tx, record memoryRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return tx.Bucket(memoryEntryBucket).Put([]byte(record.Ref.ID), raw)
}

// Get and List expose raw material to the authorized administration layer.
func (b *MemoryBook) Get(id string) (MemoryEntry, error) {
	var entry MemoryEntry
	err := b.transact(false, func(tx *bolt.Tx) error {
		record, err := loadMemoryRecord(tx, id)
		entry = record.MemoryEntry
		return err
	})
	return entry, err
}

// List pages through live entries in opaque ID order. For the first page, pass
// an empty afterID. For subsequent pages, pass the last returned ID.
func (b *MemoryBook) List(afterID string, limit int) ([]MemoryEntry, error) {
	if limit <= 0 {
		return nil, nil
	}
	var entries []MemoryEntry
	err := b.transact(false, func(tx *bolt.Tx) error {
		cursor := tx.Bucket(memoryEntryBucket).Cursor()
		key, raw := cursor.First()
		if afterID != "" {
			key, raw = cursor.Seek([]byte(afterID))
			if string(key) == afterID {
				key, raw = cursor.Next()
			}
		}
		for ; key != nil && len(entries) < limit; key, raw = cursor.Next() {
			var record memoryRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return err
			}
			if !record.Deleted {
				entries = append(entries, record.MemoryEntry)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

type memoryCandidate struct {
	MemoryProjection
	id    string
	order uint64
	score int
}

func (c memoryCandidate) newer(other memoryCandidate) bool {
	if c.order != other.order {
		return c.order > other.order
	}
	return c.id < other.id
}

// Visible applies audience and current data access before ranking candidates.
// MaxBytes counts UTF-8 bytes of emitted text and cue values. Whole candidates
// are selected within both limits.
func (b *MemoryBook) Visible(query MemoryQuery) ([]MemoryProjection, error) {
	if query.ChatID == 0 || query.MaxItems <= 0 || query.MaxBytes <= 0 {
		return nil, nil
	}
	participants := make(map[int64]bool, len(query.Participants))
	for _, id := range query.Participants {
		if id > 0 && (query.ChatID < 0 || id == query.ChatID) {
			participants[id] = true
		}
	}
	if query.ChatID > 0 && !participants[query.ChatID] {
		return nil, nil
	}

	var candidates []memoryCandidate
	cues := make(map[int64]memoryCandidate)
	err := b.transact(false, func(tx *bolt.Tx) error {
		return tx.Bucket(memoryEntryBucket).ForEach(func(_, raw []byte) error {
			var record memoryRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return err
			}
			if record.Deleted || record.Retired || !record.dataAllowed(query.DataAccess) {
				return nil
			}

			if query.learnFor != 0 && (record.AudienceChatID != query.ChatID ||
				len(record.Subjects) != 1 || record.Subjects[0] != query.learnFor) {
				return nil
			}

			text := record.Shares[query.ChatID]
			if record.AudienceChatID == query.ChatID {
				text = record.Text
			}
			if text != "" {
				candidates = append(candidates, memoryCandidate{
					MemoryProjection: MemoryProjection{Ref: record.Ref, Text: text},
					id:               record.Ref.ID,
					order:            record.Order,
				})
			}

			if query.learnFor != 0 || record.Cue == "" || !record.Cue.valid(record.Subjects) ||
				query.localCuesOnly && record.AudienceChatID != query.ChatID {
				return nil
			}
			var subjectID int64
			if record.Cue != MemoryAskFirst {
				subjectID = record.Subjects[0]
				if !participants[subjectID] {
					return nil
				}
			}
			cue := memoryCandidate{
				MemoryProjection: MemoryProjection{Cue: record.Cue, SubjectID: subjectID},
				id:               record.Ref.ID,
				order:            record.Order,
			}
			if query.trackCues {
				cue.Ref = record.Ref
			}
			if prev, exists := cues[subjectID]; !exists || cue.newer(prev) {
				cues[subjectID] = cue
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	for _, cue := range cues {
		candidates = append(candidates, cue)
	}

	terms := memoryQueryTerms(query.Query)
	for i := range candidates {
		text := strings.ToLower(candidates[i].Text)
		for _, term := range terms {
			if strings.Contains(text, term) {
				candidates[i].score++
			}
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].newer(candidates[j])
	})
	var projections []MemoryProjection
	remaining := query.MaxBytes
	for _, candidate := range candidates {
		size := len(candidate.Text) + len(candidate.Cue)
		if size > remaining {
			continue
		}
		projections = append(projections, candidate.MemoryProjection)
		remaining -= size
		if len(projections) == query.MaxItems || remaining == 0 {
			break
		}
	}
	return projections, nil
}

// projectionsValid checks the specific values this request used. Related
// writes and new evidence that confirms the same value do not cancel it.
func (b *MemoryBook) projectionsValid(query MemoryQuery, projections []MemoryProjection) bool {
	return b.transact(false, func(tx *bolt.Tx) error {
		for _, projection := range projections {
			record, err := loadMemoryRecord(tx, projection.Ref.ID)
			if err != nil || record.Retired || !record.dataAllowed(query.DataAccess) {
				return ErrMemoryDenied
			}
			if projection.Text != "" {
				text := record.Shares[query.ChatID]
				if record.AudienceChatID == query.ChatID {
					text = record.Text
				}
				if projection.Text != text {
					return ErrMemoryDenied
				}
			}
			if projection.Cue != "" && (record.Cue != projection.Cue ||
				(projection.SubjectID != 0 && (!slices.Contains(record.Subjects, projection.SubjectID) ||
					!slices.Contains(query.Participants, projection.SubjectID)))) {
				return ErrMemoryDenied
			}
		}
		return nil
	}) == nil
}

func memoryQueryTerms(query string) []string {
	query = strings.ToLower(query)
	terms := strings.FieldsFunc(query, func(r rune) bool {
		return unicode.Is(unicode.Han, r) || !(unicode.IsLetter(r) || unicode.IsNumber(r))
	})
	// Match phrases in unsegmented Chinese text by adjacent Han characters.
	var prev rune
	inPair := false
	for _, r := range query {
		if unicode.Is(unicode.Han, r) {
			inPair = prev != 0
			if inPair {
				terms = append(terms, string([]rune{prev, r}))
			}
			prev = r
		} else {
			if prev != 0 && !inPair {
				terms = append(terms, string(prev))
			}
			prev = 0
		}
	}
	if prev != 0 && !inPair {
		terms = append(terms, string(prev))
	}
	slices.Sort(terms)
	return slices.Compact(terms)
}

func (r memoryRecord) dataAllowed(grants map[MemoryDataAccess]bool) bool {
	for _, evidence := range r.Evidence {
		if access := evidence.Source.Access; access != (MemoryDataAccess{}) && !grants[access] {
			return false
		}
	}
	return true
}

// copyMemoryBook overlays the active book, retaining destination-only rows.
// Overlapping revisions must match or the destination must be a known ancestor
// of the active record. This also applies to tombstones.
func copyMemoryBook(source, target *bolt.Tx) error {
	fromEvents, toEvents := source.Bucket(memoryEventBucket), target.Bucket(memoryEventBucket)
	if err := fromEvents.ForEach(func(key, entryID []byte) error {
		if current := toEvents.Get(key); current != nil && !bytes.Equal(current, entryID) {
			return fmt.Errorf("%w: destination already processed a source event", ErrMemoryConflict)
		}
		return toEvents.Put(key, entryID)
	}); err != nil {
		return err
	}

	fromEntries, toEntries := source.Bucket(memoryEntryBucket), target.Bucket(memoryEntryBucket)
	if err := fromEntries.ForEach(func(id, raw []byte) error {
		if current := toEntries.Get(id); current != nil {
			if bytes.Equal(current, raw) {
				return nil
			}
			var active, existing memoryRecord
			if err := json.Unmarshal(raw, &active); err != nil {
				return err
			}
			if err := json.Unmarshal(current, &existing); err != nil {
				return err
			}
			existingRevision, err := existing.revision()
			if err != nil {
				return err
			}
			if active.Ref.Version == existing.Ref.Version {
				activeRevision, err := active.revision()
				if err != nil {
					return err
				}
				if activeRevision == existingRevision {
					return nil
				}
			}
			n := uint64(len(active.PriorRevisions))
			if active.Ref.Version > existing.Ref.Version {
				distance := active.Ref.Version - existing.Ref.Version
				if distance <= n && active.PriorRevisions[n-distance] == existingRevision {
					return toEntries.Put(id, raw)
				}
			}
			return fmt.Errorf("%w: incompatible memory revision in destination", ErrMemoryConflict)
		}
		return toEntries.Put(id, raw)
	}); err != nil {
		return err
	}
	return toEntries.SetSequence(max(fromEntries.Sequence(), toEntries.Sequence()))
}
