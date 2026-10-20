package aml

// lic10 (migration 0181, audit G1): the governed configuration of WHICH events trigger an automatic screening, and the officer
// reads of the automatic screening queue. LABEL: IMPLEMENTED against the local database; the only provider is the MOCK. The
// configuration is authored exactly like the screening policy (SetScreeningPolicy): a platform_acting aml_officer, the officer
// quorum, a signed actor proof bound to every argument, a legal review reference. There is no default and no seed: an
// unconfigured tenant triggers nothing and its gates are unchanged.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
)

// Proof operation names of migration 0181 (aml_authorize requires ^aml_[a-z_]+:[a-z_]+$).
const (
	OpProofScreeningTriggerPolicySet = "aml_screening_trigger_policy:set"
	OpProofScreeningRequestRead      = "aml_screening_request:read"
)

// TriggerEvent is an event that can trigger an automatic screening. The vocabulary is closed (the table CHECK is the authority).
// first_deposit and withdrawal_request are deliberately NOT here: see migration 0181 (NOT BUILT).
type TriggerEvent string

const (
	EventRegistration TriggerEvent = "registration"
	EventKYCApproval  TriggerEvent = "kyc_approval"
	EventScheduled    TriggerEvent = "scheduled"
)

// Valid reports whether e is one of the closed events.
func (e TriggerEvent) Valid() bool {
	return e == EventRegistration || e == EventKYCApproval || e == EventScheduled
}

// TriggerPolicy is the effective per-tenant, per-jurisdiction trigger policy (a configuration row; no default, no seed).
type TriggerPolicy struct {
	Events         []TriggerEvent
	GateWithdrawal bool
	EffectiveFrom  time.Time
}

// SetTriggerPolicyInput is the next VERSION of a tenant's trigger policy for the jurisdiction of its licence. Every value is a
// human compliance decision: the legal review reference is required, nothing is defaulted.
type SetTriggerPolicyInput struct {
	JurisdictionID uuid.UUID
	Events         []TriggerEvent
	// GateWithdrawal: withdrawal and payout are HELD (the indistinguishable account_review) while a required screening is
	// outstanding: no fresh completed clear, or a system-found match no officer has recorded. Needs at least one event.
	GateWithdrawal       bool
	EffectiveFrom        time.Time
	LegalReviewReference string
	ReasonCode           string
}

func sortedEvents(es []TriggerEvent) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, e := range es {
		if !e.Valid() {
			return nil, fmt.Errorf("%w: trigger event %q", ErrInvalidInput, e)
		}
		if !seen[string(e)] {
			seen[string(e)] = true
			out = append(out, string(e))
		}
	}
	sort.Strings(out)
	return out, nil
}

func (in SetTriggerPolicyInput) validate() ([]string, error) {
	if in.JurisdictionID == uuid.Nil || in.EffectiveFrom.IsZero() || !ValidReasonCode(in.ReasonCode) {
		return nil, fmt.Errorf("%w: jurisdiction, effective time and reason code are required", ErrInvalidInput)
	}
	events, err := sortedEvents(in.Events)
	if err != nil {
		return nil, err
	}
	if in.GateWithdrawal && len(events) == 0 {
		return nil, fmt.Errorf("%w: a withdrawal gate needs at least one trigger event (nothing else would ever produce a result)", ErrInvalidInput)
	}
	ref := strings.TrimSpace(in.LegalReviewReference)
	if ref == "" || len(in.LegalReviewReference) > 200 || ref != in.LegalReviewReference {
		return nil, fmt.Errorf("%w: a legal review reference is required", ErrInvalidInput)
	}
	for _, r := range in.LegalReviewReference {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return nil, fmt.Errorf("%w: legal review reference", ErrInvalidInput)
		}
	}
	return events, nil
}

// SetScreeningTriggerPolicy writes the next trigger policy version. Platform officer (platform_acting) only, with the owner-scope
// officer quorum, an existing screening policy for the jurisdiction (the lists, the re-screen interval and the backfill grace are
// that policy's values) and a jurisdiction that is the one of the tenant's licence; the database refuses anything else.
func (s *Service) SetScreeningTriggerPolicy(ctx context.Context, a Actor, in SetTriggerPolicyInput) (uuid.UUID, error) {
	events, err := in.validate()
	if err != nil {
		return uuid.Nil, err
	}
	if events == nil {
		events = []string{} // an empty set is a non-NULL empty array (nil would be sent as SQL NULL and refused)
	}
	var id uuid.UUID
	from := in.EffectiveFrom.UTC().Truncate(time.Microsecond)
	err = s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		hash := actorproof.Digest(tenantStr(a.TenantID), actorproof.S(in.JurisdictionID.String()), actorproof.S(strings.Join(events, ",")),
			actorproof.S(fmt.Sprint(in.GateWithdrawal)), actorproof.TS(&from), actorproof.S(in.LegalReviewReference), actorproof.S(in.ReasonCode))
		if err := s.attachProof(ctx, tx, a, OpProofScreeningTriggerPolicySet, actorproof.TargetNew, hash); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT aml_screening_trigger_policy_set($1, $2, $3::text[], $4, $5, $6, $7)`,
			a.TenantID, in.JurisdictionID, events, in.GateWithdrawal, from, in.LegalReviewReference, in.ReasonCode).Scan(&id)
	})
	return id, err
}

// EffectiveScreeningTriggerPolicy returns the trigger policy in force now for the tenant and jurisdiction, or nil when none is
// configured (nothing triggers; no gate). Proof-less, session-tenant bound.
func (s *Service) EffectiveScreeningTriggerPolicy(ctx context.Context, a Actor, jurisdictionID uuid.UUID) (*TriggerPolicy, error) {
	if jurisdictionID == uuid.Nil {
		return nil, fmt.Errorf("%w: jurisdiction id is required", ErrInvalidInput)
	}
	var p *TriggerPolicy
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		p, err = ReadEffectiveTriggerPolicy(ctx, tx, a.TenantID, jurisdictionID)
		return err
	})
	return p, err
}

// ReadEffectiveTriggerPolicy reads the policy in force inside the caller's tenant-bound transaction (the worker's and the
// service's shared read). nil means unconfigured.
func ReadEffectiveTriggerPolicy(ctx context.Context, tx pgx.Tx, tenantID, jurisdictionID uuid.UUID) (*TriggerPolicy, error) {
	var id uuid.UUID
	var events []string
	var gate bool
	var from time.Time
	err := tx.QueryRow(ctx, `SELECT policy_id, events, gate_withdrawal, effective_from FROM aml_screening_trigger_policy_effective($1, $2)`,
		tenantID, jurisdictionID).Scan(&id, &events, &gate, &from)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, classify(err)
	}
	p := &TriggerPolicy{GateWithdrawal: gate, EffectiveFrom: from}
	for _, e := range events {
		p.Events = append(p.Events, TriggerEvent(e))
	}
	return p, nil
}

// RequestStatus is the lifecycle state of an automatic screening request.
type RequestStatus string

const (
	RequestPending        RequestStatus = "pending"
	RequestClaimed        RequestStatus = "claimed"
	RequestDone           RequestStatus = "done"
	RequestNeedsOfficer   RequestStatus = "needs_officer"
	RequestFailedTerminal RequestStatus = "failed_terminal"
)

// ScreeningRequestRow is one automatic screening request as an officer reads it. A needs_officer request is RESOLVED when an
// officer has recorded a screening result under its IdempotencyKey (RecordScreening with that key). No vendor text is stored.
type ScreeningRequestRow struct {
	ID             uuid.UUID
	Trigger        ScreeningTrigger
	IdempotencyKey string
	Status         RequestStatus
	Attempts       int
	Outcome        ScreeningOutcome
	MatchLists     []ListType
	LastErrorCode  ScreeningErrorCode
	CompletedAt    *time.Time
	Resolved       bool
	CreatedAt      time.Time
}

// ListScreeningRequests returns the automatic screening requests of one person that the actor's owner scope may read (officer,
// proof-bound). It is how an officer finds the idempotency key a needs_officer request must be resolved under.
func (s *Service) ListScreeningRequests(ctx context.Context, a Actor, personID uuid.UUID) ([]ScreeningRequestRow, error) {
	if personID == uuid.Nil {
		return nil, fmt.Errorf("%w: person id is required", ErrInvalidInput)
	}
	var out []ScreeningRequestRow
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		hash := actorproof.Digest(tenantStr(a.TenantID), actorproof.S(personID.String()))
		if err := s.attachProof(ctx, tx, a, OpProofScreeningRequestRead, personID.String(), hash); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT o_request_id, o_trigger, o_idempotency_key, o_status, o_attempts, o_outcome, o_match_lists,
			o_last_error_code, o_completed_at, o_resolved, o_created_at FROM aml_screening_request_list($1, $2)`, a.TenantID, personID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r ScreeningRequestRow
			var trig, status string
			var outcome, errCode *string
			var matches []string
			if err := rows.Scan(&r.ID, &trig, &r.IdempotencyKey, &status, &r.Attempts, &outcome, &matches, &errCode, &r.CompletedAt, &r.Resolved, &r.CreatedAt); err != nil {
				return err
			}
			r.Trigger, r.Status, r.MatchLists = ScreeningTrigger(trig), RequestStatus(status), toLists(matches)
			if outcome != nil {
				r.Outcome = ScreeningOutcome(*outcome)
			}
			if errCode != nil {
				r.LastErrorCode = ScreeningErrorCode(*errCode)
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}
