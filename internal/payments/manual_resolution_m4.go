// PAY-PAYOUT-UNBOUND-RESOLVE-1 (ADR 0111 section 4; owner decisions ADR 0095
// section 44, decisions 9-12; migration 0125): the evidence-backed four-eyes
// resolution M4 of an UNBOUND payout park.
//
// Status: IMPLEMENTED against MOCK statement sources only. Decisions 9-12 are
// not broadened: nothing resolves automatically (only an executed four-eyes
// resolution in the final approval's own transaction posts); the park stays
// held until the database's deterministic verdict over SEALED statement imports
// is positive AND the approvers confirmed the line against the provider portal
// (evidence_ref_hash); the money moves only through withdrawal.Complete
// (m4_evidence_paid, keyed provider_id:R) or withdrawal.Fail
// (m4_evidence_not_paid). The evidence standard itself (A-13) needs the owner's
// acknowledgement before any non-MOCK M4 (D-7); the T10 launch flag stands.
//
// The authority is the database's (migration 0125): the scope, the DB-forced
// amount/asset/evidence columns, the recomputed verdict at request AND at
// `pending -> executing`, the platform_acting approver floor in the recount,
// MR041 per kind and the executing-only ledger fences. Go is the first check
// at execution (it ends a stale resolution refused_at_execution instead of
// failing the transaction) and the ONLY place the import seals are verified
// (the key never enters the database).
package payments

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// M4EvidenceNotPaidReason is the withdrawal.Fail reason code of an executed
// m4_evidence_not_paid (recorded on the withdrawal.failed audit row).
const M4EvidenceNotPaidReason = "m4_evidence_not_paid"

// ErrResolutionEvidenceUnsealed: an import the M4 verdict rests on carries no
// seal, or its seal does not verify over the stored lines, or this process has
// no import keys. Fail closed (force_resolve_evidence_unsealed).
var ErrResolutionEvidenceUnsealed = errors.New("payments: M4 evidence rests on an unsealed or tampered statement import")

// m4ResolutionColumns reads the five 0125 columns through to_jsonb of the row
// (NULL when absent), so the K3 service keeps working against a pre-0125
// schema (rolling deploys; the 0115-era K3 harnesses) while every M4 path
// requires 0125 (the kinds do not exist before it).
const m4ResolutionColumns = `(to_jsonb(payment_manual_resolutions) ->> 'evidence_line_id')::uuid,
	to_jsonb(payment_manual_resolutions) ->> 'evidence_reference',
	to_jsonb(payment_manual_resolutions) ->> 'evidence_verdict',
	CASE WHEN jsonb_typeof(to_jsonb(payment_manual_resolutions) -> 'evidence_import_ids') = 'array'
	     THEN ARRAY(SELECT j.x::uuid FROM jsonb_array_elements_text(to_jsonb(payment_manual_resolutions) -> 'evidence_import_ids') WITH ORDINALITY AS j(x, o) ORDER BY j.o)
	END,
	to_jsonb(payment_manual_resolutions) ->> 'provider_reference_at_submission'`

// ErrResolutionEvidenceNotEligible: the database's verdict is positive, but a Go-side
// D-7 eligibility check (ADR 0111 section 25, m4EligibilityRefusal) refuses it. It
// only ever refuses; it classifies as force_resolve_evidence_insufficient.
var ErrResolutionEvidenceNotEligible = errors.New("payments: M4 evidence is not eligible (D-7 Go-side refusal)")

// M4 verdicts (payout_m4_evidence).
const (
	M4VerdictPaid           = "paid"
	M4VerdictNotPaid        = "not_paid"
	M4VerdictInsufficient   = "insufficient"
	M4VerdictContradictory  = "contradictory"
	M4VerdictOverflow       = "evidence_overflow"
	m4SettlementWindowLabel = "24 hours" // migration 0125; pinned equal to DefaultSettlementWindow by a test
)

// IsM4 reports whether k is one of the two M4 kinds.
func (k ResolutionKind) IsM4() bool {
	return k == ResolutionM4EvidencePaid || k == ResolutionM4EvidenceNotPaid
}

// M4ResolvableDispute is the Go restatement of migration 0125's
// payment_m4_in_scope (ADR 0111 4.1, A-12, C-6, M-10): a disputed payout whose
// reason is invalid_provider_reference, invalid_provider_reference:* or
// provider_reference_conflict and which holds NO provider reference (both
// kinds), or destination_mismatch (not-paid only). A parity test pins it to the
// database function.
func M4ResolvableDispute(kind ResolutionKind, state AttemptState, terminalReason, providerReference *string) bool {
	if !kind.IsM4() || state != AttemptDisputed || terminalReason == nil {
		return false
	}
	r := *terminalReason
	if providerReference == nil && (r == "invalid_provider_reference" || strings.HasPrefix(r, "invalid_provider_reference:") || r == "provider_reference_conflict") {
		return true
	}
	return kind == ResolutionM4EvidenceNotPaid && r == "destination_mismatch"
}

func (in ResolutionRequestInput) validateM4() error {
	if in.BasisCode != BasisProviderConfirmedOutOfBand {
		return fmt.Errorf("%w: an M4 needs basis_code provider_confirmed_out_of_band", ErrResolutionInvalidInput)
	}
	if in.FindingCode != "" || in.ContextCode != "" {
		return fmt.Errorf("%w: an M4 carries no finding or context code", ErrResolutionInvalidInput)
	}
	if in.EvidenceRefHash == "" {
		return fmt.Errorf("%w: evidence_ref_hash (the four-eyes provider-portal confirmation) is required for M4", ErrResolutionInvalidInput)
	}
	if in.EvidenceLineID == uuid.Nil {
		return fmt.Errorf("%w: evidence_line_id is required for M4", ErrResolutionInvalidInput)
	}
	return nil
}

// evidenceLineText is the actor-proof digest field (ADR 0111 L-1): the line id,
// or "" (encoded '~') for M1/M2.
func evidenceLineText(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}

// WithImportSealKeys sets the keys that verify statement-import seals (the B13
// key module, ADR 0111 4.3). nil refuses every M4.
func (s *ManualResolutionService) WithImportSealKeys(k *payoutinstrument.Keys) *ManualResolutionService {
	s.importKeys = k
	return s
}

// verifyImportSeals verifies every import id, in tx's session (fail closed).
//
// Review amendment L-2/sec (ADR 0111 §17.7): the database cannot verify a
// seal (the key never enters it; payout_m4_evidence only sees that a seal is
// PRESENT), so EVERY M4 path must call this in Go before it commits: the
// request (requestInTx, before the requested audit row) and the execution
// (m4EvidenceRefusal, after the re-evaluation and before postM4). A new M4
// path that skips it would accept a forged or stale import as positive
// evidence. TestL2_EveryM4PathVerifiesImportSeals (static, go/ast) pins the
// call sites and their order.
func (s *ManualResolutionService) verifyImportSeals(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, ids []uuid.UUID) error {
	if s == nil || s.importKeys == nil || len(ids) == 0 {
		return ErrResolutionEvidenceUnsealed
	}
	for _, id := range ids {
		if err := payoutinstrument.VerifyStatementImportInTx(ctx, tx, s.importKeys, tenantID, id); err != nil {
			if errors.Is(err, payoutinstrument.ErrImportSealInvalid) || errors.Is(err, payoutinstrument.ErrStatementImportUnsealed) ||
				errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: %v", ErrResolutionEvidenceUnsealed, err)
			}
			return err
		}
	}
	return nil
}

// The closed reasons of m4EligibilityRefusal (ADR 0111 section 25).
const (
	// m4EligNonMock: the evidence rests on an import of a non-MOCK source. Non-MOCK M4 is
	// BLOCKED (ADR 0111 10.3: T10 / S-3 / LF C-3 / HD-R15-5; owner decision D-7): the
	// statement-line model carries no destination echo and no real source has passed the
	// section 25 checklist (m4_source_contract.go), so this refusal is unconditional and is
	// lifted only by a reviewed code change.
	m4EligNonMock = "non_mock_m4_blocked"
	// m4EligIncomplete: a paid resolution without its pinned R or line (the database forces both).
	m4EligIncomplete = "paid_evidence_incomplete"
	// m4EligPlatformRef: the evidenced R is a platform-issued merchant reference. The
	// merchant reference is what attributes a line to the attempt; if R is the same string
	// one provider field is both the causal link and the provider transaction reference.
	m4EligPlatformRef = "reference_is_platform_issued"
	// m4EligPredates: the succeeded line is dated before the attempt existed, so it cannot
	// be the payout of this attempt (the not-paid verdict already requires occurred_at >=
	// last_sent_at; the paid verdict reads no timestamp).
	m4EligPredates = "line_predates_attempt"
	// m4EligAfterCoverage: the succeeded line is dated after the coverage end of the import
	// that carries it, i.e. after the window the source vouches for.
	m4EligAfterCoverage = "line_after_import_coverage"
)

// m4EligibilityRefusal is the Go-side, refusal-only half of the D-7 standard
// that payout_m4_evidence does not enforce (ADR 0111 section 25; migration 0125 is not
// edited). "" when the evidence is eligible. It runs at the request (after the seals)
// and at the execution (after the seals), on exactly the pinned evidence.
func m4EligibilityRefusal(ctx context.Context, tx pgx.Tx, res ManualResolution, att PaymentAttempt) (string, error) {
	var nonMock bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM payment_statement_imports
		WHERE tenant_id = $1 AND id = ANY ($2) AND NOT is_mock)`, res.TenantID, res.EvidenceImportIDs).Scan(&nonMock); err != nil {
		return "", err
	}
	if nonMock {
		return m4EligNonMock, nil
	}
	if res.Kind != ResolutionM4EvidencePaid {
		return "", nil
	}
	if res.EvidenceReference == nil || res.EvidenceLineID == nil {
		return m4EligIncomplete, nil
	}
	var platformRef bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM payment_attempts WHERE tenant_id = $1 AND merchant_reference = $2)`,
		res.TenantID, *res.EvidenceReference).Scan(&platformRef); err != nil {
		return "", err
	}
	if platformRef {
		return m4EligPlatformRef, nil
	}
	var occurred, coverageEnd time.Time
	if err := tx.QueryRow(ctx, `SELECT l.occurred_at, i.coverage_end FROM payment_statement_lines l
		JOIN payment_statement_imports i ON i.id = l.import_id AND i.tenant_id = l.tenant_id
		WHERE l.id = $1 AND l.tenant_id = $2`, *res.EvidenceLineID, res.TenantID).Scan(&occurred, &coverageEnd); err != nil {
		return "", err
	}
	if occurred.Before(att.CreatedAt) {
		return m4EligPredates, nil
	}
	if occurred.After(coverageEnd) {
		return m4EligAfterCoverage, nil
	}
	return "", nil
}

// M4Evidence is one payout_m4_evidence result.
type M4Evidence struct {
	Verdict   string
	LineID    *uuid.UUID
	Reference *string
	ImportIDs []uuid.UUID
}

// EvaluateM4Evidence runs the database's deterministic evidence function in
// tx's session. A session that cannot see the whole scope gets MR060 (an
// error, never a verdict).
func EvaluateM4Evidence(ctx context.Context, tx pgx.Tx, tenantID, attemptID uuid.UUID) (M4Evidence, error) {
	var e M4Evidence
	err := tx.QueryRow(ctx, `SELECT verdict, line_id, reference, import_ids FROM payout_m4_evidence($1, $2)`, tenantID, attemptID).
		Scan(&e.Verdict, &e.LineID, &e.Reference, &e.ImportIDs)
	return e, err
}

// m4ExecutionRefusal is the Go restatement of the M4 preconditions at
// execution (ADR 0111 4.5 step 7; the database re-runs the same checks at
// `-> executing`). "" when they hold.
func (s *ManualResolutionService) m4ExecutionRefusal(res ManualResolution, att PaymentAttempt, wr withdrawal.WithdrawalRequest) string {
	if att.Operation != AttemptOperationPayout || att.ProviderID == nil || res.ProviderID == nil || *att.ProviderID != *res.ProviderID {
		return resolutionRefusedPrecond
	}
	if !M4ResolvableDispute(res.Kind, att.State, att.TerminalReason, att.ProviderReference) {
		return resolutionRefusedNotAllowed
	}
	// C-6 / R-3: the reference pinned at submission (NULL for the unbound reasons).
	if !equalOptString(att.ProviderReference, res.ProviderReferenceAtSubmission) {
		return resolutionRefusedAttempt
	}
	if wr.State != withdrawal.StateSubmitted {
		return resolutionRefusedPrecond
	}
	// R-4: the withdrawal, the attempt and the resolution agree on amount and asset.
	if wr.Amount != att.Amount || wr.AssetCode != att.AssetCode || res.Amount != att.Amount || res.AssetCode != att.AssetCode {
		return resolutionRefusedPrecond
	}
	if !s.statementSourceRegistered(att.ProviderID) {
		return resolutionRefusedNoSource
	}
	return ""
}

// m4EvidenceRefusal re-evaluates the evidence (S-2: FIRST), then verifies the
// seals of exactly the imports the re-evaluation returned, which must be the
// pinned set. "" when the evidence still stands.
func (s *ManualResolutionService) m4EvidenceRefusal(ctx context.Context, tx pgx.Tx, res ManualResolution) (string, error) {
	ev, err := EvaluateM4Evidence(ctx, tx, res.TenantID, res.AttemptID)
	if err != nil {
		return "", err
	}
	if res.EvidenceVerdict == nil || ev.Verdict != *res.EvidenceVerdict ||
		!equalOptUUID(ev.LineID, res.EvidenceLineID) || !equalOptString(ev.Reference, res.EvidenceReference) ||
		!slices.Equal(ev.ImportIDs, res.EvidenceImportIDs) {
		return resolutionRefusedEvidence, nil
	}
	if err := s.verifyImportSeals(ctx, tx, res.TenantID, ev.ImportIDs); err != nil {
		if errors.Is(err, ErrResolutionEvidenceUnsealed) {
			return resolutionRefusedUnsealed, nil
		}
		return "", err
	}
	// D-7 (ADR 0111 section 25): the Go-side, refusal-only eligibility of the pinned evidence.
	att, err := GetAttemptByID(ctx, tx, res.AttemptID)
	if err != nil {
		return "", err
	}
	if reason, err := m4EligibilityRefusal(ctx, tx, res, att); err != nil {
		return "", err
	} else if reason != "" {
		return resolutionRefusedEvidence, nil
	}
	return "", nil
}

// postM4 posts the executed M4 through the existing withdrawal writers and
// returns the release transaction: m4_evidence_paid completes the attempt's
// own withdrawal keyed provider_id:R (LF F6), m4_evidence_not_paid fails it
// (hold -> player_cash, keyed wr.id:failed).
func postM4(ctx context.Context, tx pgx.Tx, res ManualResolution, wr withdrawal.WithdrawalRequest) (*uuid.UUID, error) {
	switch res.Kind {
	case ResolutionM4EvidencePaid:
		if res.ProviderID == nil || res.EvidenceReference == nil || *res.EvidenceReference == "" {
			return nil, fmt.Errorf("%w: m4_evidence_paid has no provider or evidenced reference", ErrResolutionIntegrity)
		}
		if err := withdrawal.Complete(ctx, tx, wr.ID, *res.ProviderID, *res.EvidenceReference); err != nil {
			return nil, fmt.Errorf("payments: M4 evidence paid: %w", err)
		}
	case ResolutionM4EvidenceNotPaid:
		if err := withdrawal.Fail(ctx, tx, wr.ID, M4EvidenceNotPaidReason); err != nil {
			return nil, fmt.Errorf("payments: M4 evidence not paid: %w", err)
		}
	default:
		return nil, fmt.Errorf("%w: %s is not an M4 kind", ErrResolutionIntegrity, res.Kind)
	}
	after, err := withdrawal.GetByID(ctx, tx, wr.ID)
	if err != nil {
		return nil, err
	}
	if after.ReleaseLedgerTransactionID == nil {
		return nil, fmt.Errorf("%w: withdrawal %s has no release transaction after the M4 posting", ErrResolutionIntegrity, wr.ID)
	}
	return after.ReleaseLedgerTransactionID, nil
}

func equalOptUUID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
