package aml

// ADR 0116 s4 (A7): the database side of screening. Every method is an officer call: it opens the actor's own session and
// reaches an OWNER-owned SECURITY DEFINER function of migration 0154 that re-authorises the actor in SQL. Recording a result
// and placing the hold it requires are ONE transaction (aml_screening_record), so a recorded match without its enforcing hold
// cannot exist.

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
)

// Proof operation names of migration 0154 (the closed set of proof.go gains these three).
const (
	OpProofScreeningRecord    = "aml_screening:record"
	OpProofScreeningRead      = "aml_screening:read"
	OpProofScreeningPolicySet = "aml_screening_policy:set"
)

// ScreeningTrigger is what caused a screening run.
type ScreeningTrigger string

const (
	TriggerManual          ScreeningTrigger = "manual"
	TriggerRegistration    ScreeningTrigger = "registration"
	TriggerKYCApproval     ScreeningTrigger = "kyc_approval"
	TriggerAttributeChange ScreeningTrigger = "attribute_change"
	TriggerScheduled       ScreeningTrigger = "scheduled"
)

func (t ScreeningTrigger) valid() bool {
	switch t {
	case TriggerManual, TriggerRegistration, TriggerKYCApproval, TriggerAttributeChange, TriggerScheduled:
		return true
	}
	return false
}

// RecordScreeningInput is one screening attempt to record. The database re-validates every shape rule.
type RecordScreeningInput struct {
	PersonID          uuid.UUID
	IdempotencyKey    string
	Trigger           ScreeningTrigger
	ProviderID        string
	ProviderReference string
	SubjectRef        string
	Lists             []ListType
	Outcome           ScreeningOutcome
	MatchLists        []ListType
	ErrorCode         ScreeningErrorCode
	ScreenedAt        time.Time
	Decision          HoldAction
	HoldReasonCode    string
}

// Validate mirrors the table CHECKs so a malformed record is refused before a proof is signed. The database remains the
// authority.
func (in RecordScreeningInput) Validate() error {
	req := ScreeningRequest{IdempotencyKey: in.IdempotencyKey, SubjectRef: in.SubjectRef, Lists: in.Lists}
	if err := req.Validate(); err != nil {
		return err
	}
	if in.PersonID == uuid.Nil || !in.Trigger.valid() || !providerIDRE.MatchString(in.ProviderID) || in.ScreenedAt.IsZero() {
		return fmt.Errorf("%w: person, trigger, provider id and screened time are required", ErrInvalidInput)
	}
	if in.ProviderReference != "" && !providerRefRE.MatchString(in.ProviderReference) {
		return fmt.Errorf("%w: provider reference", ErrInvalidInput)
	}
	switch in.Decision {
	case HoldNone, HoldWithdrawalPayout, HoldAllValueMovement:
	default:
		return fmt.Errorf("%w: decision %q", ErrInvalidInput, in.Decision)
	}
	if (in.Decision == HoldNone) != (in.HoldReasonCode == "") || (in.HoldReasonCode != "" && !ValidReasonCode(in.HoldReasonCode)) {
		return fmt.Errorf("%w: a hold decision needs exactly a reason code", ErrInvalidInput)
	}
	for _, l := range in.MatchLists {
		if !l.Valid() {
			return fmt.Errorf("%w: match list %q", ErrInvalidInput, l)
		}
	}
	hasSanctions := false
	for _, l := range in.MatchLists {
		hasSanctions = hasSanctions || l == ListSanctions
	}
	// SR7-2: all-value-movement is the decision of a confirmed outcome or a sanctions-list match and of nothing else; an
	// error or a PEP-only match can never be recorded with it (and so never reaches a sanctions hold class).
	if in.Decision == HoldAllValueMovement && in.Outcome != OutcomeConfirmed && !hasSanctions {
		return fmt.Errorf("%w: all-value-movement is the decision of a confirmed outcome or a sanctions-list match only", ErrInvalidInput)
	}
	switch in.Outcome {
	case OutcomeClear:
		if in.Decision != HoldNone || len(in.MatchLists) != 0 || in.ErrorCode != "" {
			return fmt.Errorf("%w: a clear carries no match, error or action", ErrInvalidInput)
		}
	case OutcomeError:
		if !in.ErrorCode.valid() || len(in.MatchLists) != 0 {
			return fmt.Errorf("%w: an error names its closed code and no match", ErrInvalidInput)
		}
	case OutcomePotentialMatch, OutcomeConfirmed:
		if len(in.MatchLists) == 0 || in.ErrorCode != "" || in.Decision == HoldNone {
			return fmt.Errorf("%w: a match names its list and is never recorded as no action", ErrInvalidInput)
		}
		if (in.Outcome == OutcomeConfirmed || hasSanctions) && in.Decision != HoldAllValueMovement {
			return fmt.Errorf("%w: a sanctions match gates all value movement", ErrInvalidInput)
		}
	default:
		return fmt.Errorf("%w: outcome %q", ErrInvalidInput, in.Outcome)
	}
	return nil
}

// HoldReasonClassFor is the hold class a screening record places, derived STRICTLY from the outcome and the list (SR7-2,
// SR7-8, SR7-10): ANY sanctions-list match is a four-eyes-lift class, ReasonConfirmedSanctions for a CONFIRMED outcome and
// ReasonSanctionsPotential otherwise; a PEP-only match and an error are ReasonManual. It mirrors aml_screening_record and the
// aml_screening_results CHECK (migration 0155); the database is the authority.
func HoldReasonClassFor(outcome ScreeningOutcome, matches []ListType) ReasonClass {
	for _, l := range matches {
		if l == ListSanctions {
			if outcome == OutcomeConfirmed {
				return ReasonConfirmedSanctions
			}
			return ReasonSanctionsPotential
		}
	}
	return ReasonManual
}

// ScreeningRecorded is the result of RecordScreening.
type ScreeningRecorded struct {
	ResultID uuid.UUID
	// HoldID is the hold that enforces the decision (nil when the decision was none).
	HoldID *uuid.UUID
	// Replayed: the idempotency key had already been recorded with the same content; nothing was placed again.
	Replayed bool
}

func listsStr(ls []ListType) string { return strings.Join(sortedLists(ls), ",") }

func listsArg(ls []ListType) []string {
	if len(ls) == 0 {
		return nil
	}
	return sortedLists(ls)
}

func strPtrIf(s string) *string { return actorproof.SP(s) }

// RecordScreening records one attempt and places the required hold in the same transaction.
func (s *Service) RecordScreening(ctx context.Context, a Actor, in RecordScreeningInput) (ScreeningRecorded, error) {
	if err := in.Validate(); err != nil {
		return ScreeningRecorded{}, err
	}
	return s.recordScreening(ctx, a, in)
}

// recordScreening is the database call behind RecordScreening. Split out so a test can present the database with an input that
// Validate would refuse and prove the function and the table CHECKs refuse it independently (export_test.go).
func (s *Service) recordScreening(ctx context.Context, a Actor, in RecordScreeningInput) (ScreeningRecorded, error) {
	var out ScreeningRecorded
	screened := in.ScreenedAt.UTC().Truncate(time.Microsecond)
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		var matchStr *string
		if len(in.MatchLists) > 0 {
			m := listsStr(in.MatchLists)
			matchStr = &m
		}
		hash := actorproof.Digest(tenantStr(a.TenantID), actorproof.S(in.PersonID.String()), actorproof.S(in.IdempotencyKey), actorproof.S(string(in.Trigger)),
			actorproof.S(in.ProviderID), strPtrIf(in.ProviderReference), actorproof.S(in.SubjectRef), actorproof.S(listsStr(in.Lists)), actorproof.S(string(in.Outcome)),
			matchStr, strPtrIf(string(in.ErrorCode)), actorproof.TS(&screened), actorproof.S(string(in.Decision)), strPtrIf(in.HoldReasonCode))
		if err := s.attachProof(ctx, tx, a, OpProofScreeningRecord, actorproof.TargetNew, hash); err != nil {
			return err
		}
		var hold *uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT o_result_id, o_hold_id, o_replayed FROM aml_screening_record($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			a.TenantID, in.PersonID, in.IdempotencyKey, string(in.Trigger), in.ProviderID, strPtrIf(in.ProviderReference), in.SubjectRef,
			sortedLists(in.Lists), string(in.Outcome), listsArg(in.MatchLists), strPtrIf(string(in.ErrorCode)), screened,
			string(in.Decision), strPtrIf(in.HoldReasonCode)).Scan(&out.ResultID, &hold, &out.Replayed); err != nil {
			return err
		}
		out.HoldID = hold
		return nil
	})
	return out, err
}

// ScreeningRow is one stored result as ListScreenings returns it. No vendor text and no vendor match reference is stored.
type ScreeningRow struct {
	ID         uuid.UUID
	OwnerScope string
	Trigger    ScreeningTrigger
	ProviderID string
	Lists      []ListType
	Outcome    ScreeningOutcome
	MatchLists []ListType
	ErrorCode  ScreeningErrorCode
	ScreenedAt time.Time
	Decision   HoldAction
	HoldID     *uuid.UUID
	RecordedAt time.Time
}

func toLists(ss []string) []ListType {
	var out []ListType
	for _, s := range ss {
		out = append(out, ListType(s))
	}
	return out
}

// ListScreenings returns the screening results of one person that the actor's owner scope may read.
func (s *Service) ListScreenings(ctx context.Context, a Actor, personID uuid.UUID) ([]ScreeningRow, error) {
	if personID == uuid.Nil {
		return nil, fmt.Errorf("%w: person id is required", ErrInvalidInput)
	}
	var out []ScreeningRow
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		hash := actorproof.Digest(tenantStr(a.TenantID), actorproof.S(personID.String()))
		if err := s.attachProof(ctx, tx, a, OpProofScreeningRead, personID.String(), hash); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT o_result_id, o_owner_scope, o_trigger, o_provider_id, o_lists, o_outcome, o_match_lists, o_error_code,
			o_screened_at, o_decision, o_hold_id, o_recorded_at FROM aml_screening_list($1, $2)`, a.TenantID, personID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r ScreeningRow
			var trig, outcome, decision string
			var lists, matches []string
			var errCode *string
			if err := rows.Scan(&r.ID, &r.OwnerScope, &trig, &r.ProviderID, &lists, &outcome, &matches, &errCode, &r.ScreenedAt, &decision, &r.HoldID, &r.RecordedAt); err != nil {
				return err
			}
			r.Trigger, r.Outcome, r.Decision = ScreeningTrigger(trig), ScreeningOutcome(outcome), HoldAction(decision)
			r.Lists, r.MatchLists = toLists(lists), toLists(matches)
			if errCode != nil {
				r.ErrorCode = ScreeningErrorCode(*errCode)
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// EffectiveScreeningPolicy returns the policy in force now for the tenant and licensing jurisdiction, or nil when none is
// configured. Nil means UNCONFIGURED: the caller must treat it as not-ready, never as clear. Proof-less (session-tenant bound).
func (s *Service) EffectiveScreeningPolicy(ctx context.Context, a Actor, jurisdictionID uuid.UUID) (*Policy, error) {
	if jurisdictionID == uuid.Nil {
		return nil, fmt.Errorf("%w: jurisdiction id is required", ErrInvalidInput)
	}
	var p *Policy
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		var lists []string
		var rescreen, grace int64
		var from time.Time
		var id uuid.UUID
		err := tx.QueryRow(ctx, `SELECT policy_id, lists, rescreen_interval_seconds, backfill_grace_seconds, effective_from
			FROM aml_screening_policy_effective($1, $2)`, a.TenantID, jurisdictionID).Scan(&id, &lists, &rescreen, &grace, &from)
		if err == pgx.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		p = &Policy{Lists: toLists(lists), RescreenInterval: time.Duration(rescreen) * time.Second, BackfillGrace: time.Duration(grace) * time.Second, EffectiveFrom: from}
		return nil
	})
	return p, err
}

// ScreeningPolicyInput is the name the HTTP layer uses for SetScreeningPolicyInput (the HTTP layer must not name the loosening-capable
// setter, SR7-3, and the static pin searches for that identifier).
type ScreeningPolicyInput = SetScreeningPolicyInput

// SetScreeningPolicyInput is the next VERSION of a tenant's screening policy for one licensing jurisdiction. Every value is a
// human compliance decision: there is no default, and the legal review reference is required.
type SetScreeningPolicyInput struct {
	JurisdictionID       uuid.UUID
	Lists                []ListType
	RescreenInterval     time.Duration
	BackfillGrace        time.Duration
	EffectiveFrom        time.Time
	LegalReviewReference string
	ReasonCode           string
}

func (in SetScreeningPolicyInput) validate() error {
	if in.JurisdictionID == uuid.Nil || len(in.Lists) < 1 || len(in.Lists) > 2 || in.EffectiveFrom.IsZero() || !ValidReasonCode(in.ReasonCode) {
		return fmt.Errorf("%w: jurisdiction, lists, effective time and reason code are required", ErrInvalidInput)
	}
	seen := map[ListType]bool{}
	for _, l := range in.Lists {
		if !l.Valid() || seen[l] {
			return fmt.Errorf("%w: list %q", ErrInvalidInput, l)
		}
		seen[l] = true
	}
	if in.RescreenInterval < time.Second || in.RescreenInterval%time.Second != 0 || in.BackfillGrace < 0 || in.BackfillGrace%time.Second != 0 {
		return fmt.Errorf("%w: intervals are whole seconds, the re-screen interval at least one", ErrInvalidInput)
	}
	ref := strings.TrimSpace(in.LegalReviewReference)
	if ref == "" || len(in.LegalReviewReference) > 200 || ref != in.LegalReviewReference {
		return fmt.Errorf("%w: a legal review reference is required", ErrInvalidInput)
	}
	for _, r := range in.LegalReviewReference {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("%w: legal review reference", ErrInvalidInput)
		}
	}
	return nil
}

// SetScreeningPolicy writes the next policy version. Platform officer (platform_acting) only, and only with the owner-scope
// officer quorum met; the database refuses anything else. NOT routed (SR7-3): it can loosen screening.
func (s *Service) SetScreeningPolicy(ctx context.Context, a Actor, in SetScreeningPolicyInput) (uuid.UUID, error) {
	return s.writeScreeningPolicy(ctx, a, in, "aml_screening_policy_set")
}

// AuthorScreeningPolicy (lic29) is what the HTTP route calls: the same authorisation and row as SetScreeningPolicy through the
// database function aml_screening_policy_author (migration 0189), which additionally refuses a version that is LESS STRICT than the
// latest one (a list removed, a longer re-screen interval or grace, an earlier effective time): loosening has no four-eyes path yet.
func (s *Service) AuthorScreeningPolicy(ctx context.Context, a Actor, in SetScreeningPolicyInput) (uuid.UUID, error) {
	return s.writeScreeningPolicy(ctx, a, in, "aml_screening_policy_author")
}

func (s *Service) writeScreeningPolicy(ctx context.Context, a Actor, in SetScreeningPolicyInput, fn string) (uuid.UUID, error) {
	if err := in.validate(); err != nil {
		return uuid.Nil, err
	}
	var id uuid.UUID
	from := in.EffectiveFrom.UTC().Truncate(time.Microsecond)
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		rescreen, grace := int64(in.RescreenInterval/time.Second), int64(in.BackfillGrace/time.Second)
		hash := actorproof.Digest(tenantStr(a.TenantID), actorproof.S(in.JurisdictionID.String()), actorproof.S(listsStr(in.Lists)),
			actorproof.S(fmt.Sprint(rescreen)), actorproof.S(fmt.Sprint(grace)), actorproof.TS(&from), actorproof.S(in.LegalReviewReference), actorproof.S(in.ReasonCode))
		if err := s.attachProof(ctx, tx, a, OpProofScreeningPolicySet, actorproof.TargetNew, hash); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT `+fn+`($1, $2, $3, $4, $5, $6, $7, $8)`,
			a.TenantID, in.JurisdictionID, sortedLists(in.Lists), rescreen, grace, from, in.LegalReviewReference, in.ReasonCode).Scan(&id)
	})
	return id, err
}
