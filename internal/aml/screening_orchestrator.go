package aml

// The screening ORCHESTRATOR (ADR 0116 s4.2). LABEL: IMPLEMENTED (logic and database recording) over a MOCK provider; the
// automatic triggers (registration, KYC approval, scheduler) and their system actor are NOT WIRED (see
// docs/plans/a7-aml-record.md): today a screening is an OFFICER-initiated call, so the hold it places is attributed to a real,
// proof-bound officer.
//
// FAIL CLOSED, by construction:
//   - A provider error, a timeout, a panic, an unknown outcome and an inconsistent response are all an `error` outcome. None of
//     them can reach OutcomeClear; the only path to a clear is a COMPLETED, self-consistent provider `clear`.
//   - A provider that answers a match without naming a list is treated as a match on EVERY requested list.
//   - The decision is the pure Decide(); the result and the hold it requires are recorded in ONE database transaction. If the
//     record cannot be written, Screen returns *NotEnforcedError carrying the normalised outcome: the caller must retry with
//     the SAME idempotency key and must not treat the person as screened.
//   - No vendor call runs inside a database transaction (the provider is called first, outside any transaction).

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// SubjectKeyResolver returns the per-tenant key of the pseudonymous subject reference (C7). The real implementation reads the
// secret store; MockSubjectKeys is the MOCK.
type SubjectKeyResolver interface {
	Key(ctx context.Context, tenantID uuid.UUID) ([]byte, error)
}

// ScreeningStore is the database side the orchestrator needs (satisfied by *Service).
type ScreeningStore interface {
	EffectiveScreeningPolicy(ctx context.Context, a Actor, jurisdictionID uuid.UUID) (*Policy, error)
	RecordScreening(ctx context.Context, a Actor, in RecordScreeningInput) (ScreeningRecorded, error)
	OpenCase(ctx context.Context, a Actor, personID uuid.UUID, kind CaseKind, reasonCode string, alertIDs []uuid.UUID) (uuid.UUID, error)
}

var _ ScreeningStore = (*Service)(nil)

// OrchestratorConfig wires an Orchestrator. Every field is required.
type OrchestratorConfig struct {
	Provider ScreeningProvider
	Store    ScreeningStore
	Keys     SubjectKeyResolver
	// CallTimeout bounds one provider call (0 means 10s).
	CallTimeout time.Duration
	// Now is the clock (nil means time.Now).
	Now func() time.Time
}

// Orchestrator runs one screening end to end. It holds no per-request state.
type Orchestrator struct {
	cfg OrchestratorConfig
}

// NewOrchestrator builds an Orchestrator.
func NewOrchestrator(cfg OrchestratorConfig) (*Orchestrator, error) {
	if cfg.Provider == nil || cfg.Store == nil || cfg.Keys == nil {
		return nil, errors.New("aml: a screening provider, a store and a subject key resolver are required")
	}
	if !providerIDRE.MatchString(cfg.Provider.ID()) {
		return nil, errors.New("aml: the screening provider id is malformed")
	}
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = 10 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Orchestrator{cfg: cfg}, nil
}

// ScreenRequest is one screening to run.
type ScreenRequest struct {
	PersonID uuid.UUID
	// JurisdictionID is the tenant's licensing jurisdiction whose policy applies.
	JurisdictionID uuid.UUID
	// PersonRegisteredAt is when the person registered in the tenant (decides the C1 grace rule for a missing result).
	PersonRegisteredAt time.Time
	// VendorSubjectRef is the KYC-vendor subject reference (attributes are BLOCKED, Q6). Never a platform id.
	VendorSubjectRef string
	Trigger          ScreeningTrigger
	// IdempotencyKey identifies the screening: the SAME key must be reused on a retry, for the provider and for the record.
	IdempotencyKey string
	// Lists is used only when the tenant has no policy (an officer-initiated run); a policy's lists win.
	Lists []ListType
}

// ScreenOutcome is what Screen returns on success.
type ScreenOutcome struct {
	Outcome    ScreeningOutcome
	MatchLists []ListType
	ErrorCode  ScreeningErrorCode
	Decision   Decision
	Recorded   ScreeningRecorded
	// CaseID is the screening case opened for a match (nil otherwise, or when opening failed: see CaseError).
	CaseID *uuid.UUID
	// CaseError is set when the case could not be opened. The hold stands; an officer opens the case.
	CaseError error
}

// NotEnforcedError: the provider answered, but the result and its hold could not be recorded. Outcome and Decision say what
// must still be enforced. Retry with the same idempotency key.
type NotEnforcedError struct {
	Outcome  ScreeningOutcome
	Decision Decision
	Err      error
}

func (e *NotEnforcedError) Error() string {
	return fmt.Sprintf("aml screening: outcome %s (action %s) was NOT recorded or enforced: %v", e.Outcome, e.Decision.Action, e.Err)
}
func (e *NotEnforcedError) Unwrap() error { return e.Err }

// SubjectRef is HMAC-SHA256(per-tenant key, "aml-screening-subject:v1:" || person id), lowercase hex (C7). The vendor never
// receives a platform id.
func SubjectRef(key []byte, personID uuid.UUID) (string, error) {
	if len(key) < 32 {
		return "", errors.New("aml: the subject key must be at least 32 bytes")
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte("aml-screening-subject:v1:" + personID.String()))
	return hex.EncodeToString(m.Sum(nil)), nil
}

// Screen runs one screening: policy -> pseudonymous reference -> provider (bounded, outside any transaction) -> normalisation
// -> Decide -> record + hold (one transaction) -> case (matches).
func (o *Orchestrator) Screen(ctx context.Context, a Actor, req ScreenRequest) (ScreenOutcome, error) {
	if req.PersonID == uuid.Nil || req.JurisdictionID == uuid.Nil || !req.Trigger.valid() || req.PersonRegisteredAt.IsZero() {
		return ScreenOutcome{}, fmt.Errorf("%w: person, jurisdiction, trigger and registration time are required", ErrInvalidInput)
	}
	pol, err := o.cfg.Store.EffectiveScreeningPolicy(ctx, a, req.JurisdictionID)
	if err != nil {
		// Without the policy the error rule cannot be applied: refuse to run rather than guess.
		return ScreenOutcome{}, fmt.Errorf("aml screening: policy unavailable: %w", err)
	}
	lists := req.Lists
	if pol != nil {
		lists = pol.Lists
	}
	preq := ScreeningRequest{IdempotencyKey: req.IdempotencyKey, Subject: ScreeningSubject{VendorSubjectRef: req.VendorSubjectRef}, Lists: lists}
	key, err := o.cfg.Keys.Key(ctx, a.TenantID)
	if err != nil {
		return ScreenOutcome{}, fmt.Errorf("aml screening: subject key unavailable: %w", err)
	}
	if preq.SubjectRef, err = SubjectRef(key, req.PersonID); err != nil {
		return ScreenOutcome{}, err
	}
	if err := preq.Validate(); err != nil {
		return ScreenOutcome{}, err
	}
	res, matches := o.callProvider(ctx, preq)

	now := o.cfg.Now()
	decision := Decide(now, pol, req.PersonRegisteredAt, res.Outcome, matches)
	rec := RecordScreeningInput{
		PersonID: req.PersonID, IdempotencyKey: req.IdempotencyKey, Trigger: req.Trigger, ProviderID: o.cfg.Provider.ID(),
		ProviderReference: validProviderRef(res.ProviderReference), SubjectRef: preq.SubjectRef, Lists: lists, Outcome: res.Outcome,
		MatchLists: matches, ErrorCode: res.ErrorCode, ScreenedAt: res.ScreenedAt, Decision: decision.Action, HoldReasonCode: decision.ReasonCode,
	}
	recorded, err := o.cfg.Store.RecordScreening(ctx, a, rec)
	if err != nil {
		return ScreenOutcome{}, &NotEnforcedError{Outcome: res.Outcome, Decision: decision, Err: err}
	}
	out := ScreenOutcome{Outcome: res.Outcome, MatchLists: matches, ErrorCode: res.ErrorCode, Decision: decision, Recorded: recorded}
	if decision.OpenCase && !recorded.Replayed {
		id, cerr := o.cfg.Store.OpenCase(ctx, a, req.PersonID, CaseKindManual, "screening_match", nil)
		if cerr != nil {
			out.CaseError = cerr
		} else {
			out.CaseID = &id
		}
	}
	return out, nil
}

func validProviderRef(s string) string {
	if providerRefRE.MatchString(s) {
		return s
	}
	return ""
}

// callProvider calls the provider under a deadline and normalises EVERYTHING it can produce into a self-consistent result.
// The returned outcome is clear only when the provider completed with a clear that carries no match. It never panics.
func (o *Orchestrator) callProvider(ctx context.Context, in ScreeningRequest) (res ScreeningResult, matches []ListType) {
	now := o.cfg.Now().UTC()
	fail := func(code ScreeningErrorCode) (ScreeningResult, []ListType) {
		return ScreeningResult{Outcome: OutcomeError, ErrorCode: code, ScreenedAt: now}, nil
	}
	cctx, cancel := context.WithTimeout(ctx, o.cfg.CallTimeout)
	defer cancel()
	var raw ScreeningResult
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				raw, err = ScreeningResult{}, errUnknownProvider
			}
		}()
		raw, err = o.cfg.Provider.Screen(cctx, in)
	}()
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, ErrProviderTimeout):
			return fail(ErrCodeTimeout)
		case errors.Is(err, ErrProviderRateLimited):
			return fail(ErrCodeRateLimited)
		case errors.Is(err, ErrProviderUnavailable), errors.Is(err, context.Canceled):
			return fail(ErrCodeUnavailable)
		default:
			return fail(ErrCodeUnknown)
		}
	}
	if raw.ScreenedAt.IsZero() {
		raw.ScreenedAt = now
	}
	requested := map[ListType]bool{}
	for _, l := range in.Lists {
		requested[l] = true
	}
	var named []ListType
	seen := map[ListType]bool{}
	for _, m := range raw.Matches {
		if !requested[m.List] {
			// A match on a list that was not requested is not a conforming answer.
			return fail(ErrCodeInvalidResponse)
		}
		if !seen[m.List] {
			seen[m.List] = true
			named = append(named, m.List)
		}
	}
	switch raw.Outcome {
	case OutcomeClear:
		if len(raw.Matches) != 0 || raw.ErrorCode != "" {
			return fail(ErrCodeInvalidResponse) // a clear with a match is inconsistent: never a clear
		}
		return ScreeningResult{Outcome: OutcomeClear, ProviderReference: raw.ProviderReference, ScreenedAt: raw.ScreenedAt}, nil
	case OutcomePotentialMatch, OutcomeConfirmed:
		if len(named) == 0 {
			// Ambiguous match: widen to every requested list (the conservative reading).
			named = append(named, in.Lists...)
		}
		if raw.Outcome == OutcomeConfirmed {
			// A confirmed outcome is a sanctions determination: it must have been asked for, and it is recorded against that list.
			if !requested[ListSanctions] {
				return fail(ErrCodeInvalidResponse)
			}
			if !seen[ListSanctions] {
				named = append(named, ListSanctions)
			}
		}
		return ScreeningResult{Outcome: raw.Outcome, ProviderReference: raw.ProviderReference, ScreenedAt: raw.ScreenedAt}, dedupe(named)
	case OutcomeError:
		code := raw.ErrorCode
		if !code.valid() {
			code = ErrCodeUnknown
		}
		r, _ := fail(code)
		r.ProviderReference = raw.ProviderReference
		return r, nil
	}
	return fail(ErrCodeInvalidResponse) // an unknown outcome value
}

var errUnknownProvider = errors.New("aml screening: provider panicked")

func dedupe(ls []ListType) []ListType {
	seen := map[ListType]bool{}
	var out []ListType
	for _, l := range ls {
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	return out
}
