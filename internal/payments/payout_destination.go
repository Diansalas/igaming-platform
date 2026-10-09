package payments

// B13-B (ADR 0111 section 18; ADR 0095 section 44 decisions 1-8): the payments side of the
// payout destination binding.
//
//	T1p          ClaimForDispatch: destination gate (EvaluateGate + CheckTier on the routed
//	             adapter) BEFORE the allow decision, the state change and the attempt insert,
//	             then the write-once snapshot in the same transaction.
//	phase B      DispatchWithdraw: re-reads the instrument, applies the gate and the tiering
//	             predicate on the EXACT adapter value about to be invoked, verifies the snapshot
//	             and builds WithdrawRequest.Destination from the snapshot plus the decrypted
//	             immutable detail. Any failure: no provider call (NotSent class).
//	T2 / T12     destinationGateAndEscalate (payout_sweep.go): the same gate before a re-claim or
//	             a resend; failure escalates (T16), never resends, never releases.
//	phase C /    payoutDestinationEvidence: snapshot integrity + destination echo comparison
//	poll /       before any evidence that could settle the attempt. A provider-reported
//	callback     destination can ONLY be compared: it never determines, replaces or changes the
//	             destination (decision 3), and a mismatch parks the attempt as `disputed`
//	             (destination_mismatch) with an audit row and a B12 P1, no payout progression
//	             (decision 5). Missing or ambiguous attribution fails closed (decision 4).
//
// Nothing here writes a payout_instrument* table except through payoutinstrument.Service
// (the only snapshot writer is Service.WriteSnapshot, called by T1p).

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
	"github.com/Diansalas/igaming-platform/internal/providercred"
	"github.com/Diansalas/igaming-platform/internal/providerref"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// Closed destination reasons. terminal_reason has only a length CHECK (migration 0101), so no
// migration is needed for these (ADR 0111 2.6).
const terminalSignalAuditAction = "payments.payout_destination_mismatch_terminal"

const (
	// TerminalReasonDestinationMismatch: a provider-reported destination differs from the
	// attempt's snapshot (parked, disputed). Not M2-admitted: the hold is kept.
	TerminalReasonDestinationMismatch = "destination_mismatch"
	// TerminalReasonDestinationIntegrityFailure: the attempt's snapshot of a bound withdrawal is
	// missing, unsealed or inconsistent when evidence tries to settle it (parked, disputed). It is
	// also the T16 escalation reason when a gate refuses for an integrity cause.
	TerminalReasonDestinationIntegrityFailure = "destination_integrity_failure"

	alertReasonPayoutDestinationNotUsable          = "destination_not_usable"
	alertReasonPayoutDestinationMismatchOnTerminal = "destination_mismatch_on_terminal_payout"
)

// ErrPayoutDestinationNotUsable is returned by ClaimForDispatch when the destination gate
// refuses at T1p. Only the denial audit was committed; the request stays `approved`.
// It wraps the *payoutinstrument.GateRefusal (closed reason, never a value).
var ErrPayoutDestinationNotUsable = errors.New("payments: payout destination is not usable")

// ErrPaymentMethodMismatch: the staff submit body named a payment method different from the
// bound instrument's rail (ADR 0111 A-10). The rail is the instrument's; the body can never
// influence the destination or the route.
var ErrPaymentMethodMismatch = errors.New("payments: payment_method differs from the bound payout instrument's rail")

// ErrDestinationServiceUnavailable: a bound withdrawal reached a payments path with no payout
// destination service configured. Fail closed.
var ErrDestinationServiceUnavailable = errors.New("payments: payout destination service is not configured")

// ---- options -------------------------------------------------------------

// PayoutOption configures the free payout phase functions (DispatchWithdraw, ApplyPayoutResult,
// the poll path) with the destination collaborators. Without WithDestinations a BOUND
// withdrawal's payout makes no provider call and cannot be settled (fail closed).
type PayoutOption func(*payoutEnv)

type payoutEnv struct {
	destinations *payoutinstrument.Service
	providers    func(providerID string) (PaymentProvider, bool)
}

// WithDestinations supplies the payout-instrument service.
func WithDestinations(svc *payoutinstrument.Service) PayoutOption {
	return func(e *payoutEnv) { e.destinations = svc }
}

// WithProviderLookup supplies the adapter registry, used to read the manifest's
// EchoesDestinationFingerprint declaration.
func WithProviderLookup(f func(providerID string) (PaymentProvider, bool)) PayoutOption {
	return func(e *payoutEnv) { e.providers = f }
}

func buildPayoutEnv(opts []PayoutOption) payoutEnv {
	var e payoutEnv
	for _, o := range opts {
		if o != nil {
			o(&e)
		}
	}
	return e
}

// WithPayoutDestinations sets the payout-instrument service this orchestrator's payout paths
// (T1p, the sweeper's T2/T12, the receipt path) use. Returns o for chaining. cmd/platform-api
// wires it; there is no default (nil refuses every bound payout).
func (o *Orchestrator) WithPayoutDestinations(svc *payoutinstrument.Service) *Orchestrator {
	o.destinations = svc
	return o
}

// PayoutOptions returns the options the production call sites (HTTP submit, sweeper) pass to
// DispatchWithdraw / ApplyPayoutResult / the poll path. A static test pins that every
// non-test call site passes them.
func (o *Orchestrator) PayoutOptions() []PayoutOption {
	if o == nil {
		return nil
	}
	return []PayoutOption{WithDestinations(o.destinations), WithProviderLookup(o.Provider)}
}

func (o *Orchestrator) payoutEnv() payoutEnv {
	return buildPayoutEnv(o.PayoutOptions())
}

func (e payoutEnv) echoDeclared(providerID *string) bool {
	if providerID == nil || e.providers == nil {
		return false
	}
	p, ok := e.providers(*providerID)
	if !ok {
		return false
	}
	return p.Capabilities().Manifest.EchoesDestinationFingerprint
}

// ---- the gate ------------------------------------------------------------

// gatePayoutDestination applies the ADR 0111 gate rule and the tiering predicate for a payout
// of wr on the exact adapter value. A legacy (unbound) withdrawal passes only the tiering
// predicate with Bound=false (Synthetic adapters only). lock takes the instrument FOR SHARE
// (T1p); wantDetail returns the decrypted detail (phase B).
func gatePayoutDestination(ctx context.Context, tx pgx.Tx, svc *payoutinstrument.Service, wr withdrawal.WithdrawalRequest, adapter any, lock, wantDetail bool) (payoutinstrument.GateResult, error) {
	// Security L-1: this function is ONLY for the dispatch gates (T1p, phase B, T2/T12). EvaluateGate reads a nil
	// adapter as "request creation: no adapter involved" and skips the tiering predicate; that meaning must not
	// leak here. A dispatch gate with no adapter value (an unregistered provider id, a nil registry entry) is refused.
	if adapter == nil {
		return payoutinstrument.GateResult{}, payoutinstrument.RefuseNotUsable(payoutinstrument.ReasonTierRefused)
	}
	if !wr.Bound() {
		if err := payoutinstrument.CheckTier(adapter, payoutinstrument.TierInput{Bound: false}); err != nil {
			return payoutinstrument.GateResult{}, payoutinstrument.RefuseNotUsable(payoutinstrument.ReasonTierRefused)
		}
		return payoutinstrument.GateResult{}, nil
	}
	if svc == nil {
		return payoutinstrument.GateResult{}, payoutinstrument.RefuseNotUsable(payoutinstrument.ReasonNoGate)
	}
	if wr.PayoutInstrumentFingerprint == nil {
		return payoutinstrument.GateResult{}, payoutinstrument.RefuseIntegrity(payoutinstrument.ReasonBindingMismatch)
	}
	account, err := identity.GetPlayerAccountByID(ctx, tx, wr.PlayerAccountID)
	if err != nil {
		return payoutinstrument.GateResult{}, fmt.Errorf("payments: load player account for the destination gate: %w", err)
	}
	g, err := svc.EvaluateGate(ctx, tx, payoutinstrument.GateParams{
		TenantID: wr.TenantID, BrandID: wr.BrandID, PlayerAccountID: wr.PlayerAccountID, PersonID: account.PersonID,
		InstrumentID: *wr.PayoutInstrumentID, AssetCode: wr.AssetCode, Adapter: adapter, Lock: lock, WantDetail: wantDetail,
	})
	if err != nil {
		return payoutinstrument.GateResult{}, err
	}
	if g.Instrument.Fingerprint != *wr.PayoutInstrumentFingerprint {
		return payoutinstrument.GateResult{}, payoutinstrument.RefuseIntegrity(payoutinstrument.ReasonBindingMismatch)
	}
	return g, nil
}

// destinationEscalationReason maps a gate error to the closed T16 escalation reason. A gate
// error that is not a GateRefusal is not mapped (the caller returns it).
func destinationEscalationReason(err error) (reason, closed string, ok bool) {
	g, isRef := payoutinstrument.IsGateRefusal(err)
	if !isRef {
		return "", "", false
	}
	if g.Integrity() {
		return TerminalReasonDestinationIntegrityFailure, g.Reason, true
	}
	return alertReasonPayoutDestinationNotUsable, g.Reason, true
}

// destinationGateAndEscalate is the T2/T12 re-check (ADR 0111 2.4): the gate rule and the
// tiering predicate on the exact adapter, applied in the claim/resend transaction BEFORE the
// KYC gate and the claim CAS. Failure escalates (T16) with a closed reason, writes the audit
// row and raises the B12 P1 as the LAST statement, and returns allowed=false: no resend, no
// release, no state change beyond escalated_at. An already-escalated attempt only reschedules.
// The destination is never re-resolved: phase B rebuilds it from the snapshot.
func (s *Sweeper) destinationGateAndEscalate(ctx context.Context, tx pgx.Tx, wr withdrawal.WithdrawalRequest, attempt PaymentAttempt, nextActionAt time.Time) (allowed bool, err error) {
	var adapter any
	if attempt.ProviderID != nil {
		if p, ok := s.Orchestrator.Provider(*attempt.ProviderID); ok {
			adapter = p
		}
	}
	_, gerr := gatePayoutDestination(ctx, tx, s.Orchestrator.destinations, wr, adapter, false, false)
	if gerr == nil && wr.Bound() {
		// The snapshot must still be intact: a missing snapshot on a bound withdrawal is a
		// destination_integrity_failure (decision 4).
		_, gerr = s.Orchestrator.destinations.CheckSnapshot(ctx, tx, snapshotExpectFor(wr, attempt))
	}
	if gerr == nil {
		return true, nil
	}
	reason, closed, ok := destinationEscalationReason(gerr)
	if !ok {
		return false, gerr
	}
	if attempt.EscalatedAt != nil {
		return false, RescheduleNonTerminal(ctx, tx, attempt.ID, nextActionAt)
	}
	if err := Escalate(ctx, tx, attempt.ID, nextActionAt); err != nil {
		return false, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: wr.TenantID, ActorType: audit.ActorSystem, Action: "payments.payout_destination_gate_denied",
		TargetType: "payment_attempt", TargetID: attempt.ID.String(), Outcome: audit.OutcomeDenied,
		Metadata: map[string]any{"withdrawal_request_id": wr.ID.String(), "reason": reason, "gate_reason": closed},
	}); err != nil {
		return false, err
	}
	// B12: the raise is the LAST statement of the escalation.
	return false, raisePayoutDisputeAlert(ctx, tx, attempt, reason)
}

func snapshotExpectFor(wr withdrawal.WithdrawalRequest, attempt PaymentAttempt) payoutinstrument.SnapshotExpect {
	e := payoutinstrument.SnapshotExpect{
		TenantID: wr.TenantID, AttemptID: attempt.ID, WithdrawalRequestID: wr.ID,
		Fingerprint: "", Amount: attempt.Amount, AssetCode: attempt.AssetCode,
	}
	if wr.PayoutInstrumentID != nil {
		e.InstrumentID = *wr.PayoutInstrumentID
	}
	if wr.PayoutInstrumentFingerprint != nil {
		e.Fingerprint = *wr.PayoutInstrumentFingerprint
	}
	return e
}

// ---- phase B -------------------------------------------------------------

// resolvedDestination is what phase B hands to payoutAdapterCall.
type resolvedDestination struct {
	paymentMethod string
	destination   payoutinstrument.PayoutDestination
}

// resolvePhaseBDestination re-reads the withdrawal, the instrument and the snapshot and applies
// the gate rule and the tiering predicate to the EXACT provider value about to be invoked. It
// runs in its own short read transaction and holds no lock across the provider call. Any
// refusal returns an error and the caller makes NO provider call.
func resolvePhaseBDestination(ctx context.Context, pool providercred.TenantTxRunner, env payoutEnv, provider PaymentProvider, attempt PaymentAttempt) (resolvedDestination, error) {
	var out resolvedDestination
	if pool == nil {
		return out, fmt.Errorf("%w: no transaction runner for the phase B destination gate", ErrDestinationServiceUnavailable)
	}
	if attempt.WithdrawalRequestID == nil {
		return out, payoutinstrument.RefuseIntegrity(payoutinstrument.ReasonBindingMismatch)
	}
	err := pool.WithTenant(ctx, attempt.TenantID, func(actx context.Context, tx pgx.Tx) error {
		wr, err := withdrawal.GetByID(actx, tx, *attempt.WithdrawalRequestID)
		if err != nil {
			return err
		}
		g, err := gatePayoutDestination(actx, tx, env.destinations, wr, provider, false, true)
		if err != nil {
			return err
		}
		if !wr.Bound() {
			// Legacy NULL-binding withdrawal: only a Synthetic adapter got past the tier check.
			out = resolvedDestination{paymentMethod: attempt.PaymentMethod}
			return nil
		}
		snap, err := env.destinations.CheckSnapshot(actx, tx, snapshotExpectFor(wr, attempt))
		if err != nil {
			return err
		}
		if snap.Rail != attempt.PaymentMethod {
			return payoutinstrument.RefuseIntegrity(payoutinstrument.ReasonSnapshotMismatch)
		}
		out = resolvedDestination{paymentMethod: snap.Rail, destination: g.Destination(snap.FingerprintKID)}
		return nil
	})
	return out, err
}

// ---- evidence (phase C / poll / callback) ------------------------------------

// parkPayoutDestination parks a non-terminal payout attempt as `disputed` with a closed
// destination reason: the CAS, then the durable park-evidence row (what the provider reported,
// GOV-R32), then the audit row, then the B12 P1 as the LAST statement. No Complete, no release,
// no ledger posting: the hold is kept.
func parkPayoutDestination(ctx context.Context, tx pgx.Tx, attempt PaymentAttempt, requestID uuid.UUID, evidence EvidenceKind, class ErrorClass, reason string, meta map[string]any, reference string) error {
	// LF H-2: bind the provider's (validated, unconflicted) reference BEFORE the park. A foreign-held reference is
	// parked by the existing B10 guard as provider_reference_conflict instead (hold kept, its own audit + P1).
	if parked, err := bindPayoutReferenceForPark(ctx, tx, attempt, requestID, reference, evidence); err != nil || parked {
		return err
	}
	if err := ApplyDisputeFromNonTerminal(ctx, tx, attempt.ID, evidence, reason); err != nil {
		return payoutHandleContradiction(ctx, tx, attempt, evidence, ErrorClassSucceeded, err)
	}
	// GOV-R32 (ledger-finance HIGH): the provider's reported outcome is recorded durably in the park's own
	// transaction, so a success-triggered park can never later be resolved "not paid" (payout_m4_evidence reads it).
	if err := recordDestinationParkEvidence(ctx, tx, attempt.TenantID, attempt.ID, reason, destinationParkOutcome(class), evidence); err != nil {
		return err
	}
	m := map[string]any{"withdrawal_request_id": requestID.String(), "reason": reason, "provider_id": providerIDOrEmpty(attempt)}
	for k, v := range meta {
		m[k] = v
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: attempt.TenantID, ActorType: audit.ActorSystem, Action: "payments.payout_parked_destination",
		TargetType: "payment_attempt", TargetID: attempt.ID.String(), Outcome: audit.OutcomeDenied, Metadata: m,
	}); err != nil {
		return err
	}
	return raisePayoutDisputeAlert(ctx, tx, attempt, reason)
}

func fpPrefix(s string) string {
	if len(s) >= 8 {
		return s[:8]
	}
	return s
}

// destinationEvidenceResult is payoutDestinationEvidence's verdict.
type destinationEvidenceResult struct {
	// Stop: the evidence was fully handled here (parked, or a signal raised on a terminal
	// attempt); the caller returns without applying it.
	Stop bool
	// AmbiguousSuccess: a success whose echo is absent although the manifest declares one. The
	// caller must treat the evidence as AMBIGUOUS (the poll decides), never as a success.
	AmbiguousSuccess bool
}

// payoutDestinationEvidence is the single destination check every payout evidence path runs
// before it can settle or advance an attempt (sync phase C, the QueryStatus poll, the
// callback/receipt cell). The caller holds the withdrawal row lock (LockForPayoutEvidence).
//
// It only COMPARES; nothing here can set, replace or change a destination.
//
//   - Evidence that is neither a success nor carries an echo needs no destination check.
//   - A legacy (unbound) withdrawal has nothing to compare.
//   - Non-terminal attempt: the snapshot must exist, verify and equal the withdrawal/attempt/
//     instrument (else park destination_integrity_failure); an echo that differs (or names an
//     unknown kid) parks destination_mismatch; an absent echo on a success from an adapter whose
//     manifest declares EchoesDestinationFingerprint is ambiguous, never a success.
//   - succeeded / declined attempt: a differing echo changes nothing but writes the audit row
//     and raises the P1 signal destination_mismatch_on_terminal_payout.
//   - disputed / created / rejected: unchanged existing cells apply.
//
// reference is the provider reference the evidence carries ("" when none). A park binds it first (LF H-2) so that
// reconciliation and RESOLVE-1 see the reference the provider reported.
func payoutDestinationEvidence(ctx context.Context, tx pgx.Tx, env payoutEnv, attempt PaymentAttempt, requestID uuid.UUID, class ErrorClass, echo *payoutinstrument.DestinationEcho, evidence EvidenceKind, reference string) (destinationEvidenceResult, error) {
	var res destinationEvidenceResult
	if class != ErrorClassSucceeded && echo == nil {
		return res, nil
	}
	// ADR 0082 A7: withdrawal FIRST (a harmless re-lock when the caller already holds it).
	wr, err := withdrawal.LockForPayoutEvidence(ctx, tx, requestID)
	if err != nil {
		return res, err
	}
	if !wr.Bound() {
		return res, nil
	}
	fresh, err := GetAttemptByID(ctx, tx, attempt.ID)
	if err != nil {
		return res, fmt.Errorf("payments: payout destination evidence: re-read attempt: %w", err)
	}
	switch fresh.State {
	case AttemptSubmitting, AttemptPending, AttemptAmbiguous:
		// checked below
	case AttemptSucceeded, AttemptDeclined:
		if echo == nil {
			return res, nil
		}
		return payoutTerminalDestinationSignal(ctx, tx, env, fresh, wr, echo)
	default:
		return res, nil
	}
	if env.destinations == nil {
		res.Stop = true
		return res, parkPayoutDestination(ctx, tx, fresh, requestID, evidence, class, TerminalReasonDestinationIntegrityFailure,
			map[string]any{"gate_reason": payoutinstrument.ReasonNoGate}, reference)
	}
	snap, err := env.destinations.CheckSnapshot(ctx, tx, snapshotExpectFor(wr, fresh))
	if err != nil {
		g, ok := payoutinstrument.IsGateRefusal(err)
		if !ok {
			return res, err
		}
		res.Stop = true
		return res, parkPayoutDestination(ctx, tx, fresh, requestID, evidence, class, TerminalReasonDestinationIntegrityFailure,
			map[string]any{"gate_reason": g.Reason}, reference)
	}
	switch payoutinstrument.CompareEcho(snap, echo) {
	case payoutinstrument.EchoMismatch:
		res.Stop = true
		meta := map[string]any{"snapshot_fingerprint_prefix": fpPrefix(snap.Fingerprint), "echo_kid": echo.Kid, "echo_fingerprint_prefix": fpPrefix(echo.Fingerprint)}
		return res, parkPayoutDestination(ctx, tx, fresh, requestID, evidence, class, TerminalReasonDestinationMismatch, meta, reference)
	case payoutinstrument.EchoAbsent:
		if class == ErrorClassSucceeded && env.echoDeclared(fresh.ProviderID) {
			res.AmbiguousSuccess = true
		}
	}
	return res, nil
}

// payoutTerminalDestinationSignal handles a differing echo on an already succeeded/declined
// payout: no state change, no posting; the audit row and the raise-only P1 signal.
func payoutTerminalDestinationSignal(ctx context.Context, tx pgx.Tx, env payoutEnv, attempt PaymentAttempt, wr withdrawal.WithdrawalRequest, echo *payoutinstrument.DestinationEcho) (destinationEvidenceResult, error) {
	// LF H-1: the signal is ADDITIVE. It never stops the evidence: the existing cells (T14 success after a decline, the
	// foreign-reference and amount signals on a succeeded attempt, late evidence) still run after it.
	res := destinationEvidenceResult{}
	var snapFP, kid string
	if env.destinations != nil {
		if snap, err := env.destinations.CheckSnapshot(ctx, tx, snapshotExpectFor(wr, attempt)); err == nil {
			if payoutinstrument.CompareEcho(snap, echo) != payoutinstrument.EchoMismatch {
				return res, nil
			}
			snapFP, kid = snap.Fingerprint, snap.FingerprintKID
		}
	}
	// A broken snapshot on a terminal attempt cannot prove equality: signal it as well. The audit row is written once per
	// distinct (attempt, echo) - NOT keyed on the receipt's alreadyApplied flag (LF L-2): the receipt of a bad-echo
	// redelivery dedupes against the earlier GOOD delivery of the same event (the echo is not in the fingerprint), so
	// alreadyApplied is true for the very first bad one. The check runs under the withdrawal row lock the caller holds, so
	// concurrent redeliveries serialise. The raise stays unconditional (the alert dedupes into one open alert).
	var seen bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit_log WHERE tenant_id = $1 AND action = $2 AND target_id = $3
		AND created_at >= $6 AND metadata->>'echo_kid' = $4 AND metadata->>'echo_fingerprint_prefix' = $5)`,
		attempt.TenantID, terminalSignalAuditAction, attempt.ID.String(), echo.Kid, fpPrefix(echo.Fingerprint), attempt.CreatedAt).Scan(&seen); err != nil {
		return res, fmt.Errorf("payments: terminal destination signal: audit lookup: %w", err)
	}
	if !seen {
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: attempt.TenantID, ActorType: audit.ActorSystem, Action: terminalSignalAuditAction,
			TargetType: "payment_attempt", TargetID: attempt.ID.String(), Outcome: audit.OutcomeDenied,
			Metadata: map[string]any{
				"withdrawal_request_id": wr.ID.String(), "provider_id": providerIDOrEmpty(attempt), "state": string(attempt.State),
				"snapshot_fingerprint_prefix": fpPrefix(snapFP), "snapshot_kid": kid, "echo_kid": echo.Kid, "echo_fingerprint_prefix": fpPrefix(echo.Fingerprint),
			},
		}); err != nil {
			return res, err
		}
	}
	return res, raisePayoutDisputeAlert(ctx, tx, attempt, alertReasonPayoutDestinationMismatchOnTerminal)
}

// amountText is the canonical decimal minor-unit text the snapshot stores.
func amountText(v int64) string { return strconv.FormatInt(v, 10) }

// ---- security alert for integrity refusals (security L-2) -----------------------------------------

// integrityAlertReasons is the closed set of gate/snapshot integrity reasons that may appear in a discriminator.
var integrityAlertReasons = map[string]struct{}{
	payoutinstrument.ReasonSealInvalid: {}, payoutinstrument.ReasonFingerprintMismatch: {}, payoutinstrument.ReasonDetailUnavailable: {},
	payoutinstrument.ReasonRelationMismatch: {}, payoutinstrument.ReasonVerificationMissing: {}, payoutinstrument.ReasonVerificationNotLast: {},
	payoutinstrument.ReasonSnapshotMissing: {}, payoutinstrument.ReasonSnapshotMismatch: {}, payoutinstrument.ReasonBindingMismatch: {},
}

// DestinationIntegrityAlert builds the P1 (existing Kind payment.webhook_integrity, no new Kind) for a destination
// integrity refusal where NO attempt exists to key the discriminator on: at request time (subject
// "payout_instrument:<id>") and at T1p (subject "withdrawal:<id>"). The reason is from the closed set above; anything
// else becomes "unclassified" and is never echoed. The caller raises it as the LAST statement of its site
// (alerting.RaiseGuarded inside an alerting.InTx closure) or detached after a rollback (alerting.RaiseDetached).
func DestinationIntegrityAlert(tenantID uuid.UUID, subject, reason string) alerting.Alert {
	if _, ok := integrityAlertReasons[reason]; !ok {
		reason = alertReasonUnclassified
	}
	return alerting.Alert{
		Kind: alerting.KindPaymentWebhookIntegrity, SubjectTenantID: tenantID,
		Discriminator: subject + ":reason:destination_integrity:" + reason, Attributes: map[string]alerting.AttrValue{},
	}
}

// bindPayoutReferenceForPark binds a provider reference the evidence reported before a destination park, through the
// same path every other payout binding takes: validate, the B10 foreign-reference guard (which itself parks on a
// conflict and then returns parked=true), then MarkAccepted (submitting|ambiguous -> pending, provider_reference set)
// and AttachProviderReference on the withdrawal. An attempt that already holds its reference, an empty reference and
// an invalid reference bind nothing (the park stays reference-less, exactly as before).
func bindPayoutReferenceForPark(ctx context.Context, tx pgx.Tx, attempt PaymentAttempt, requestID uuid.UUID, reference string, evidence EvidenceKind) (parkedByGuard bool, err error) {
	if reference == "" || (attempt.ProviderReference != nil && *attempt.ProviderReference != "") {
		return false, nil
	}
	if attempt.State != AttemptSubmitting && attempt.State != AttemptAmbiguous {
		return false, nil
	}
	if verr := providerref.ValidatePaymentReference("provider_reference", reference); verr != nil {
		return false, nil
	}
	if parked, err := payoutGuardReferenceBinding(ctx, tx, attempt, requestID, reference, evidence, ErrorClassPending); err != nil || parked {
		return parked, err
	}
	if err := MarkAccepted(ctx, tx, attempt.ID, evidence, reference, time.Now().Add(payoutNextPollInterval)); err != nil {
		return false, payoutHandleContradiction(ctx, tx, attempt, evidence, ErrorClassPending, err)
	}
	return false, withdrawal.AttachProviderReference(ctx, tx, requestID, reference)
}

// ---- durable park evidence (GOV-R32, migration 0127; ADR 0111 23.6) ---------------------------

// destinationParkOutcome is the closed outcome vocabulary of payout_destination_park_evidence.
func destinationParkOutcome(class ErrorClass) string {
	switch class {
	case ErrorClassSucceeded:
		return "succeeded"
	case ErrorClassDefiniteDecline:
		return "declined"
	case ErrorClassPending:
		return "pending"
	}
	return "ambiguous"
}

// isDestinationParkReason reports the two destination park reasons.
func isDestinationParkReason(r *string) bool {
	return r != nil && (*r == TerminalReasonDestinationMismatch || *r == TerminalReasonDestinationIntegrityFailure)
}

// recordDestinationParkEvidence appends one row to payout_destination_park_evidence (system shape only,
// append-only; the database guard requires the attempt to be a payout parked on that destination reason).
// A repeat of the same (attempt, outcome, source) adds nothing.
func recordDestinationParkEvidence(ctx context.Context, tx pgx.Tx, tenantID, attemptID uuid.UUID, reason, outcome string, evidence EvidenceKind) error {
	if _, err := tx.Exec(ctx, `INSERT INTO payout_destination_park_evidence (tenant_id, attempt_id, terminal_reason, reported_outcome, evidence_kind)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (tenant_id, attempt_id, reported_outcome, evidence_kind) DO NOTHING`,
		tenantID, attemptID, reason, outcome, string(evidence)); err != nil {
		return fmt.Errorf("payments: record destination park evidence: %w", err)
	}
	return nil
}

// recordSuccessOnDestinationPark records a provider SUCCESS that reaches an attempt ALREADY parked on a
// destination reason (a callback, a poll, or a late sync/poll result that lost the CAS to the park). It
// re-reads the attempt in the caller's transaction (the caller holds the withdrawal lock) and is a no-op
// for anything else. It never changes state, posts, releases or raises: it only makes the success durable
// so that M4 not-paid is refused (payout_m4_evidence, migration 0127).
func recordSuccessOnDestinationPark(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID, evidence EvidenceKind) error {
	a, err := GetAttemptByID(ctx, tx, attemptID)
	if err != nil {
		return fmt.Errorf("payments: destination park success: re-read attempt: %w", err)
	}
	if a.Operation != AttemptOperationPayout || a.State != AttemptDisputed || !isDestinationParkReason(a.TerminalReason) {
		return nil
	}
	return recordDestinationParkEvidence(ctx, tx, a.TenantID, a.ID, *a.TerminalReason, "succeeded", evidence)
}
