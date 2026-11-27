package app

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

var (
	ErrEngagementInvalid    = errors.New("invalid engagement input")
	ErrEngagementNotFound   = errors.New("engagement or change not found")
	ErrEngagementDenied     = errors.New("engagement outside authorized scope")
	ErrEngagementConflict   = errors.New("engagement revision conflict")
	ErrEngagementProcessed  = errors.New("engagement request already processed")
	ErrEngagementTerminal   = errors.New("engagement has ended")
	ErrEngagementLease      = errors.New("engagement claim is no longer valid")
	ErrEngagementIncomplete = errors.New("engagement work is incomplete")
)

type EngagementKind string

const (
	EngagementTracking   EngagementKind = "tracking"
	EngagementDelegation EngagementKind = "delegation"
)

type EngagementStatus string

const (
	EngagementProposed EngagementStatus = "proposed"
	EngagementActive   EngagementStatus = "active"
	EngagementPaused   EngagementStatus = "paused"
	EngagementEnded    EngagementStatus = "ended"
)

type EngagementVerdict string

const (
	EngagementAccept  EngagementVerdict = "accept"
	EngagementReject  EngagementVerdict = "reject"
	EngagementClarify EngagementVerdict = "clarify"
)

type EngagementChange string

const (
	EngagementAgree    EngagementChange = "agree"
	EngagementReaccept EngagementChange = "reaccept"
	EngagementPause    EngagementChange = "pause"
	EngagementResume   EngagementChange = "resume"
	EngagementEnd      EngagementChange = "end"
	EngagementManage   EngagementChange = "manage"
	EngagementMute     EngagementChange = "mute"
	EngagementUnmute   EngagementChange = "unmute"
)

type EngagementPauseReason string

const (
	EngagementRouteUnverified EngagementPauseReason = "recipient_needs_clarification"
	EngagementAccessLost      EngagementPauseReason = "access_could_not_be_verified"
)

type EngagementEndReason string

const (
	EngagementStopped        EngagementEndReason = "stopped"
	EngagementCompleted      EngagementEndReason = "completed"
	EngagementExpired        EngagementEndReason = "expired"
	EngagementConditionEnded EngagementEndReason = "condition_ended"
)

type EngagementAudience struct {
	ChatID int64
	Access MemoryDataAccess
}

// EngagementText is material approved for a specific chat. Access, when set,
// must also be granted to the actual audience for the current use.
type EngagementText struct {
	ChatID int64
	Text   string
	Access MemoryDataAccess
}

type EngagementPerson struct {
	ID     int64
	ChatID int64
}

// EngagementCapability is the application's exact binding of a background
// operation and target. Access identifies the actual connector and account
// (owner 0 for built-in or global capabilities). It never contains credentials.
type EngagementCapability struct {
	Access     MemoryDataAccess
	Operation  string
	Target     string
	DataChatID int64
	Write      bool
}

type EngagementAgreement struct {
	Observe          EngagementCapability
	Trigger          EngagementText
	Actions          []EngagementCapability
	Recipients       []int64
	RecipientSources []EngagementRecipientSource `json:",omitempty"`
	Once             bool
	EndCondition     EngagementText
	ExpiresAt        time.Time
	NextCheck        time.Time // agreed first check after this acceptance
	// Bound by the store, independent of the candidate's labels.
	TriggerSource, EndConditionSource MemorySource
	TimingAudience                    *EngagementAudience // nil for legacy agreements, falls back to the trigger
}

type EngagementTerm string

const (
	EngagementTriggerTerm EngagementTerm = "trigger"
	EngagementEndTerm     EngagementTerm = "end_condition"
)

// EngagementTermGrant is application-verified authority to replace (or, with
// a zero Value, remove) this exact term. Source is the real evidence for the
// new term or the explicit consent to share existing material at its new scope.
type EngagementTermGrant struct {
	Term   EngagementTerm
	Value  EngagementText
	Source MemorySource
}

// EngagementAuthorization comes from live application checks at acceptance.
// Each binding incorporates the operation, account, target and data audience.
type EngagementAuthorization struct {
	Capabilities      []EngagementCapability
	Recipients        []int64
	RecipientSources  []EngagementRecipientSource
	DelegatedAccounts []MemoryDataAccess // explicit grants to this request's actual sender
	DataAccess        []MemoryDataAccess // the sender's current grants in the request chat
	TermEdits         []EngagementTermGrant
	// The active initiator statement in this chat, verified by the application
	// as the role's basis for accepting a delegate's policy change.
	ManagementBasis *MemorySource
	// The exact conclusion the initiator explicitly approved for a group in
	// this private request. The application verifies the consent and audience.
	ManagementShare *EngagementManagementShare
}

type EngagementRef struct {
	ID      string
	Version uint64
}

type EngagementRequest struct {
	Source MemorySource
	Text   string
	// ExplicitReaccept is bound by the application to an explicit user request.
	ExplicitReaccept bool
	SentAt           time.Time // timestamp supplied by Telegram for the authorizing message
}

type EngagementDecision struct {
	Request           EngagementRequest `json:"-"`
	Verdict           EngagementVerdict
	Change            EngagementChange
	Reply             EngagementText
	Agreement         *EngagementAgreement
	Management        *EngagementManagementEdit
	InvalidateChanges []string                // durable work keys explicitly excluded by this revision
	Authorized        EngagementAuthorization `json:"-"`
	At                time.Time
	MuteUntil         time.Time
}

type EngagementManagementEdit struct {
	Summary       string
	InitiatorOnly bool // minimum shareable conclusion, used as context by the role
}

type EngagementManagementShare struct {
	ChatID  int64
	Summary string
}

type EngagementManagementTerm struct {
	EngagementManagementEdit
	Source MemorySource               // assigned from the authenticated management request
	Basis  *MemorySource              `json:",omitempty"`
	Share  *EngagementManagementShare `json:",omitempty"`
}

type EngagementCreate struct {
	Kind          EngagementKind
	Decision      EngagementDecision
	Reason        EngagementText
	People        []EngagementPerson
	Evidence      []MemoryEvidence // each source supplies its original audience and data grant
	ManageChats   []int64          // additional application-approved management audiences
	InitiatorOnly bool             // an explicit management term
}

type EngagementDecisionRecord struct {
	ManagementBasis    *MemorySource `json:",omitempty"`
	MuteUntil          time.Time
	Request            EngagementRequest
	Verdict            EngagementVerdict
	Change             EngagementChange
	Reply              EngagementText
	Effective          *EngagementAgreement
	Management         *EngagementManagementTerm // effective terms at this decision
	InvalidatedChanges []string
	Status             EngagementStatus
	At                 time.Time
}

// Engagement contains raw material for trusted application code.
type Engagement struct {
	Ref                EngagementRef
	Kind               EngagementKind
	Origin             MemorySource
	Reason             EngagementText
	People             []EngagementPerson
	Evidence           []MemoryEvidence
	ManageChats        []int64
	InitiatorOnly      bool
	Management         *EngagementManagementTerm
	Initial            *EngagementDecisionRecord
	Current            *EngagementAgreement
	Decisions          []EngagementDecisionRecord
	Status             EngagementStatus
	EndReason          EngagementEndReason
	PauseReason        EngagementPauseReason `json:",omitempty"`
	PauseNotices       []EngagementResult    `json:",omitempty"`
	NextCheck          time.Time
	OnceChangeKey      string // bound qualifying change for one-time work
	CompletedChangeKey string
	UncertainSteps     int `json:",omitempty"`
	LastObservation    *EngagementObservation
	LastObservedAt     time.Time
	LastDeliveredAt    time.Time
	DeliveredTo        map[int64]time.Time `json:",omitempty"`
	Muted              bool
	MuteUntil          time.Time
}

// EngagementViewer uses the actual chat and verified actor and data grants.
type EngagementViewer struct {
	ChatID     int64
	ActorID    int64
	DataAccess map[MemoryDataAccess]bool
}

type EngagementVisibleAgreement struct {
	Observe      *EngagementVisibleCapability
	Trigger      string
	Actions      []EngagementVisibleCapability
	DeliversHere bool
	Once         *bool `json:",omitempty"`
	EndCondition string
	ExpiresAt    *time.Time `json:",omitempty"`
}

type EngagementVisibleCapability struct {
	Operation string
	Target    string
	OwnerID   int64
	AccountID string // opaque per-engagement identifier for the granted connection
	Write     bool
}

type EngagementVisibleEvidence struct {
	Kind      MemoryEvidenceKind
	Text      string
	ActorID   int64
	MessageID int64
}

type EngagementVisibleManagement struct {
	Summary   string
	ActorID   int64
	MessageID int64 `json:",omitempty"`
}

type EngagementVisibleDecision struct {
	Request string
	ActorID int64
	Verdict EngagementVerdict
	Change  EngagementChange
	Reply   string
}

type EngagementProjection struct {
	Muted                     bool
	Ref                       EngagementRef `json:"-"` // application-side version for committing a decision
	Kind                      EngagementKind
	Status                    EngagementStatus
	EndReason                 EngagementEndReason   `json:",omitempty"`
	PauseReason               EngagementPauseReason `json:",omitempty"`
	OutcomeUncertain          bool                  `json:",omitempty"`
	InitiatorID               int64                 `json:",omitempty"`
	InitiatorOnly             bool
	RequesterIsInitiator      bool
	Management                *EngagementVisibleManagement
	MayChange                 bool // structural eligibility, the role decides the request
	Reason                    string
	People                    []int64
	Evidence                  []EngagementVisibleEvidence
	Decisions                 []EngagementVisibleDecision
	Agreement                 *EngagementVisibleAgreement
	managementOutsideAudience bool
}

// EngagementClaim fences worker writes. The token survives a hot switch and
// becomes invalid on lease expiry or an accepted lifecycle/agreement change.
type EngagementClaim struct {
	ID, Token string
}

type EngagementObservation struct {
	ChangeKey string // native event ID from the current observer; empty when nothing matched
	Evidence  EngagementText
	Event     *engagementEvent `json:",omitempty"`
	At        time.Time        // assigned by Observe
}

type EngagementCheck struct {
	NextCheck       time.Time
	CompletedChange string
	EndConditionMet bool
}

// Exactly one of Action and Recipient is set. Delivery records describe the
// outcome for this particular Telegram destination.
type EngagementStep struct {
	Action          EngagementCapability
	Recipient       int64
	UncertainNotice bool `json:",omitempty"`
}

type EngagementResultState string

const (
	EngagementReserved  EngagementResultState = "reserved"
	EngagementUnknown   EngagementResultState = "unknown"
	EngagementNoEffect  EngagementResultState = "no_effect"
	EngagementSucceeded EngagementResultState = "succeeded"
)

type EngagementResult struct {
	Step            EngagementStep
	Key             string // stable idempotency key for the same external request
	Attempt         uint64
	State           EngagementResultState
	Receipt         string
	At              time.Time
	CalendarRequest string               `json:",omitempty"` // typed arguments before adding the idempotency key
	CalendarReplay  uint64               `json:",omitempty"` // zero for the initial dispatch of a logical attempt
	AttemptRef      EngagementAttemptRef `json:"-"`
}

// EngagementAttemptRef identifies a specific reservation. Trusted executors or
// reconcilers obtain it with the result from BeginStep or Work.
type EngagementAttemptRef struct {
	id, change, key string
	step            EngagementStep
	attempt         uint64
	calendarReplay  uint64
}

type engagementCalendarDispatch struct {
	EngagementResult
	Observation EngagementObservation
}

// EngagementWork keeps the original qualifying observation and agreed steps.
// Changes to the current agreement can narrow old work. Newly added steps
// belong to later changes.
type EngagementWork struct {
	ChangeKey          string // opaque durable event key; use for execution, recovery and pagination
	Observation        EngagementObservation
	Observe            EngagementCapability
	Trigger            EngagementText
	Actions            []EngagementCapability
	Recipients         []int64
	Results            []EngagementResult
	CalendarDispatches []engagementCalendarDispatch `json:",omitempty"`
	InvalidatedVersion uint64
	Superseded         bool `json:",omitempty"` // a newer source snapshot displaced unsent one-shot work
}

func engagementDataAccessValid(access MemoryDataAccess) bool {
	return access == (MemoryDataAccess{}) || (access.OwnerID >= 0 && strings.TrimSpace(access.Key) != "")
}

func (text EngagementText) valid() bool {
	return text == (EngagementText{}) ||
		(text.ChatID != 0 && strings.TrimSpace(text.Text) != "" && engagementDataAccessValid(text.Access))
}

func (c EngagementCapability) valid() bool {
	return c.Access != (MemoryDataAccess{}) && engagementDataAccessValid(c.Access) &&
		c.DataChatID != 0 && strings.TrimSpace(c.Operation) != "" && strings.TrimSpace(c.Target) != ""
}

func (d EngagementDecision) valid() bool {
	if !d.Request.Source.valid() || strings.TrimSpace(d.Request.Text) == "" ||
		d.At.IsZero() || !d.Reply.valid() || (d.Reply.ChatID != 0 && d.Reply.ChatID != d.Request.Source.ChatID) {
		return false
	}
	if d.Management != nil && (strings.TrimSpace(d.Management.Summary) == "" ||
		d.Verdict != EngagementAccept ||
		(d.Change != EngagementManage && d.Change != EngagementAgree && d.Change != EngagementReaccept)) {
		return false
	}
	if d.Authorized.ManagementShare != nil && d.Management == nil {
		return false
	}
	if len(d.InvalidateChanges) != 0 && (d.Verdict != EngagementAccept ||
		(d.Change != EngagementAgree && d.Change != EngagementReaccept)) {
		return false
	}
	switch d.Verdict {
	case EngagementReject, EngagementClarify:
		return d.Change == "" && d.Agreement == nil
	case EngagementAccept:
		switch d.Change {
		case EngagementAgree, EngagementReaccept:
			return d.Agreement != nil
		case EngagementPause, EngagementResume, EngagementEnd, EngagementMute, EngagementUnmute:
			return d.Agreement == nil
		case EngagementManage:
			return d.Agreement == nil && d.Management != nil
		}
	}
	return false
}

func (a EngagementAgreement) authorize(kind EngagementKind, d EngagementDecision) error {
	if !a.Observe.valid() || a.Observe.Write || !a.Trigger.valid() || a.Trigger.Text == "" ||
		!a.EndCondition.valid() || a.NextCheck.IsZero() || len(a.Recipients) == 0 ||
		(!a.ExpiresAt.IsZero() && (!a.ExpiresAt.After(d.At) || !a.ExpiresAt.After(a.NextCheck))) {
		return ErrEngagementInvalid
	}
	if a.Observe.Operation == engagementPrivateFollowup && !a.permitsPrivateFollowup(kind, d.Request.Source) {
		return ErrEngagementDenied
	}
	if !d.allowsCapability(a.Observe) {
		return ErrEngagementDenied
	}
	for i, action := range a.Actions {
		if !action.valid() || slices.Contains(a.Actions[:i], action) {
			return ErrEngagementInvalid
		}
		if (kind != EngagementDelegation && action.Write) || !d.allowsCapability(action) {
			return ErrEngagementDenied
		}
	}
	for i, recipient := range a.Recipients {
		if recipient == 0 || slices.Contains(a.Recipients[:i], recipient) {
			return ErrEngagementInvalid
		}
		if !slices.Contains(d.Authorized.Recipients, recipient) {
			return ErrEngagementDenied
		}
	}
	return nil
}

func (d EngagementDecision) allowsCapability(c EngagementCapability) bool {
	return slices.Contains(d.Authorized.Capabilities, c) &&
		(c.Access.OwnerID == 0 || c.Access.OwnerID == d.Request.Source.SenderID ||
			slices.Contains(d.Authorized.DelegatedAccounts, c.Access))
}

func (t EngagementText) audience() EngagementAudience {
	return EngagementAudience{ChatID: t.ChatID, Access: t.Access}
}

func (a EngagementAgreement) timingAudience() EngagementAudience {
	if a.TimingAudience != nil {
		return *a.TimingAudience
	}
	return a.Trigger.audience()
}

func (d EngagementDecision) sees(a EngagementAudience) bool {
	source := d.Request.Source
	return a.ChatID == source.ChatID && (a.Access == (MemoryDataAccess{}) ||
		a.Access == source.Access || slices.Contains(d.Authorized.DataAccess, a.Access))
}

func (d EngagementDecision) termSource(term EngagementTerm, next, prior EngagementText, priorSource MemorySource, initial bool) (MemorySource, error) {
	if next == prior && prior.Text != "" {
		return priorSource, nil
	}
	for _, grant := range d.Authorized.TermEdits {
		if grant.Term == term && grant.Value == next && grant.Source.valid() {
			return grant.Source, nil
		}
	}
	source := d.Request.Source
	if next.Text == "" {
		if prior.Text != "" && !d.sees(prior.audience()) {
			return MemorySource{}, ErrEngagementDenied
		}
		return MemorySource{}, nil
	}
	// Narrowing exactly the prior text retains its original provenance.
	if next.Text == prior.Text && next.ChatID == prior.ChatID &&
		prior.Access == (MemoryDataAccess{}) && d.sees(next.audience()) {
		return priorSource, nil
	}
	// First acceptance takes terms derived from application-filtered material.
	// On revision, even an empty slot or the same audience label can receive
	// text drawn from a different, restricted field in the role's context.
	sameSource := next.ChatID == source.ChatID &&
		(source.Access == (MemoryDataAccess{}) || source.Access == next.Access)
	if sameSource && (initial || (d.sees(next.audience()) && strings.Contains(d.Request.Text, next.Text))) {
		return source, nil
	}
	return MemorySource{}, ErrEngagementDenied
}

func (d EngagementDecision) bindAgreement(prior *EngagementAgreement) (EngagementAgreement, error) {
	a := *d.Agreement
	a.RecipientSources = slices.Clone(d.Authorized.RecipientSources)
	var old EngagementAgreement
	timing := a.Trigger.audience()
	if prior != nil {
		old = *prior
		timing = old.timingAudience()
		if (a.Once != old.Once || !a.ExpiresAt.Equal(old.ExpiresAt)) && !d.sees(timing) {
			return EngagementAgreement{}, ErrEngagementDenied
		}
		if timing == old.Trigger.audience() && a.Trigger.ChatID == timing.ChatID &&
			timing.Access == (MemoryDataAccess{}) && a.Trigger.Access != (MemoryDataAccess{}) {
			timing = a.Trigger.audience()
		}
	}
	a.TimingAudience = &timing
	trigger, err := d.termSource(EngagementTriggerTerm, a.Trigger, old.Trigger, old.TriggerSource, prior == nil)
	if err != nil {
		return EngagementAgreement{}, err
	}
	end, err := d.termSource(EngagementEndTerm, a.EndCondition, old.EndCondition, old.EndConditionSource, prior == nil)
	a.TriggerSource, a.EndConditionSource = trigger, end
	return a, err
}

func (a EngagementAgreement) within(prior EngagementAgreement) bool {
	if a.Observe != prior.Observe || (prior.Once && !a.Once) ||
		(!prior.ExpiresAt.IsZero() && (a.ExpiresAt.IsZero() || a.ExpiresAt.After(prior.ExpiresAt))) {
		return false
	}
	for _, action := range a.Actions {
		if !slices.Contains(prior.Actions, action) {
			return false
		}
	}
	for _, recipient := range a.Recipients {
		if !slices.Contains(prior.Recipients, recipient) {
			return false
		}
	}
	return true
}

func (v EngagementViewer) valid() bool {
	return v.ChatID != 0 && v.ActorID > 0 && (v.ChatID < 0 || v.ChatID == v.ActorID)
}

func (v EngagementViewer) sees(chatID int64, access MemoryDataAccess) bool {
	return v.ChatID == chatID && (access == (MemoryDataAccess{}) || v.DataAccess[access])
}

func (v EngagementViewer) text(text EngagementText) string {
	if v.sees(text.ChatID, text.Access) {
		return text.Text
	}
	return ""
}

func (e Engagement) visibleTo(v EngagementViewer) bool {
	return v.ChatID == e.Origin.ChatID || slices.Contains(e.ManageChats, v.ChatID)
}

func (e Engagement) mayChange(actorID int64) bool {
	return e.Status != EngagementEnded && (e.Initial != nil || actorID == e.Origin.SenderID)
}

func (e Engagement) project(v EngagementViewer) EngagementProjection {
	p := EngagementProjection{
		Ref: e.Ref, Muted: e.Muted, Kind: e.Kind, Status: e.Status, EndReason: e.EndReason, InitiatorOnly: e.InitiatorOnly,
		RequesterIsInitiator: v.ActorID == e.Origin.SenderID, MayChange: e.mayChange(v.ActorID), Reason: v.text(e.Reason),
	}
	if a := e.Current; a != nil && slices.Contains(a.Recipients, v.ChatID) {
		p.PauseReason = e.PauseReason
	}
	if v.sees(e.Origin.ChatID, e.Origin.Access) {
		p.InitiatorID = e.Origin.SenderID
	}
	if m := e.Management; m != nil {
		if v.sees(m.Source.ChatID, m.Source.Access) {
			p.Management = &EngagementVisibleManagement{Summary: m.Summary, ActorID: m.Source.SenderID, MessageID: m.Source.MessageID}
		} else if m.Share != nil && m.Share.ChatID == v.ChatID {
			p.Management = &EngagementVisibleManagement{Summary: m.Share.Summary, ActorID: m.Source.SenderID}
		} else {
			p.managementOutsideAudience = true
		}
	} else if !v.sees(e.Origin.ChatID, e.Origin.Access) {
		p.managementOutsideAudience = true
	}
	for _, person := range e.People {
		if person.ChatID == v.ChatID {
			p.People = append(p.People, person.ID)
		}
	}
	for _, evidence := range e.Evidence {
		if v.sees(evidence.Source.ChatID, evidence.Source.Access) {
			p.Evidence = append(p.Evidence, EngagementVisibleEvidence{
				Kind: evidence.Kind, Text: evidence.Text, ActorID: evidence.Source.SenderID, MessageID: evidence.Source.MessageID,
			})
		}
	}
	for _, decision := range e.Decisions {
		source := decision.Request.Source
		if v.sees(source.ChatID, source.Access) {
			p.Decisions = append(p.Decisions, EngagementVisibleDecision{
				Request: decision.Request.Text, ActorID: source.SenderID,
				Verdict: decision.Verdict, Change: decision.Change, Reply: v.text(decision.Reply),
			})
		}
	}
	if a := e.Current; a != nil {
		p.Agreement = &EngagementVisibleAgreement{
			Trigger: v.text(a.Trigger), EndCondition: v.text(a.EndCondition),
			DeliversHere: slices.Contains(a.Recipients, v.ChatID),
		}
		timing := a.timingAudience()
		if v.sees(timing.ChatID, timing.Access) {
			p.Agreement.Once = &a.Once
			if !a.ExpiresAt.IsZero() {
				p.Agreement.ExpiresAt = &a.ExpiresAt
			}
		}
		if v.sees(a.Observe.DataChatID, a.Observe.Access) {
			observe := a.Observe.project(e.Ref.ID)
			p.Agreement.Observe = &observe
		}
		for _, action := range a.Actions {
			if v.sees(action.DataChatID, action.Access) {
				p.Agreement.Actions = append(p.Agreement.Actions, action.project(e.Ref.ID))
			}
		}
	}
	return p
}

func (c EngagementCapability) project(id string) EngagementVisibleCapability {
	access, _ := json.Marshal(c.Access)
	account := sha256.Sum256(append([]byte(id+"/"), access...))
	return EngagementVisibleCapability{
		Operation: c.Operation, Target: c.Target, OwnerID: c.Access.OwnerID, AccountID: fmt.Sprintf("%x", account), Write: c.Write,
	}
}

// NotificationsMutedAt controls delivery for this item. Observations and
// external actions continue while a notification mute is in effect.
func (e Engagement) NotificationsMutedAt(now time.Time) bool {
	return e.Muted && (e.MuteUntil.IsZero() || now.Before(e.MuteUntil))
}
