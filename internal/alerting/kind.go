// Package alerting implements ADR 0102 ("Durable Alerting and
// Provider-Neutral Delivery", PRH-2 workstream I-core, registry
// ALERT-DELIVERY-1): durable, deduplicated, tenant/platform-scoped alert
// rows, delivered asynchronously through a provider-neutral Sink. No
// database transaction ever performs alert I/O (Sink.Deliver). No
// recipient, person, email, phone number or rota is ever named or seeded
// by this package (HD-PRH2-4) - alert_routes ships and stays empty until
// a platform admin configures it.
//
// This file is the Go-side mirror of migration 0110's alert_kinds seed
// table (ADR §3.1/§9 item 1). The two must stay in lock-step: the
// database is the ultimate authority (every column here is independently
// enforced by a trigger or CHECK), but Go validates BEFORE any SQL runs
// (LF F1/AL-6a) so a bad Kind or payload never reaches, and never rolls
// back, a business transaction.
package alerting

import "fmt"

// Kind is ADR 0102's closed alert-Kind vocabulary. It is a plain string
// so it round-trips through the database's alert_kinds.kind column
// (TEXT PRIMARY KEY) without any custom marshalling, but callers should
// only ever use one of the named constants below - Kinds() is the closed
// set an AST/table test walks, mirroring db.PlatformService's own
// "closed vocabulary" pattern (internal/db/platform_service.go).
type Kind string

// Severity is ADR 0102 §3.1's severity vocabulary.
type Severity string

const (
	SeverityP1 Severity = "p1"
	SeverityP2 Severity = "p2"
	SeverityP3 Severity = "p3"
)

// Scope is ADR 0102 §3.1's Kind scope: platform-owned or tenant-owned.
// No tenant-scope Kind is seeded in PRH-2 (ADR §4.3); the value exists so
// the Go model matches the database column exactly.
type Scope string

const (
	ScopeKindPlatform Scope = "platform"
	ScopeKindTenant   Scope = "tenant"
)

// RaiseMode documents where a Kind is raised from (ADR §3.1/§7.1). It is
// not itself enforced by Go or the database - the ADR's own raise-site
// discipline and the table-driven LF-test-3 walk are the enforcement -
// but is recorded here so both live in one place.
type RaiseMode string

const (
	RaiseModeInTx       RaiseMode = "in_tx"
	RaiseModeDetached   RaiseMode = "detached"
	RaiseModePostCommit RaiseMode = "post_commit"
)

// KindDef is the Go-side mirror of one alert_kinds row.
type KindDef struct {
	Kind                 Kind
	Severity             Severity
	Scope                Scope
	Simulation           bool
	RequiresSubject      bool
	InTxRaisableByTenant bool
	AllowedKeys          map[string]struct{}
	RaiseMode            RaiseMode
}

// The three accepted meta-Kinds (ADR §6.3, security Q1/Part 1 ACCEPTED).
// Raised ONLY by the alert_dispatcher platform-service identity, never by
// business code - see fallback.go and dispatcher.go, and the
// static test that confines db.ServiceAlertDispatcher's use to this
// package.
const (
	KindAlertingUnrouted     Kind = "alerting.unrouted"
	KindAlertingDeliveryDead Kind = "alerting.delivery_dead"
	KindAlertingRaiseFailed  Kind = "alerting.raise_failed"
)

// Business Kinds (ADR §8).
const (
	KindPaymentMultipleSuccessForIntent     Kind = "payment.multiple_success_for_intent"
	KindPaymentDepositIntentIndexBackstop   Kind = "payment.deposit_intent_index_backstop_fired"
	KindReconciliationLedgerProjectionDrift Kind = "reconciliation.ledger_projection_drift"
	KindReconciliationSportsbookSettlement  Kind = "reconciliation.sportsbook_settlement_mismatch"
	KindReconciliationCasinoConsistency     Kind = "reconciliation.casino_consistency_mismatch"
	KindReconciliationCasinoStatement       Kind = "reconciliation.casino_statement_mismatch"
	KindReconciliationPaymentStatement      Kind = "reconciliation.payment_statement_mismatch"
	KindReconciliationRunFailed             Kind = "reconciliation.run_failed"
	KindPaymentKillSwitchEngaged            Kind = "payment.kill_switch_engaged"
	KindCasinoCallbackIntegrity             Kind = "casino.callback_integrity"
	KindPaymentWebhookIntegrity             Kind = "payment.webhook_integrity"
	KindSimulationPaymentPayloadMismatch    Kind = "simulation.payment.payload_mismatch"
	KindSimulationCasinoPlay                Kind = "simulation.casino_play"
	KindSimulationSportsbookSettlement      Kind = "simulation.sportsbook_settlement"
)

// KindKYCSubmissionFailedTerminal is the PRH-2 E1 (ADR 0106, HD-PRH2-10)
// KYC outbox Kind: a create/submit outbox row reached failed_terminal. It
// is raised in-tx from the tenant phase-C session (alerting.InTx +
// RaiseGuarded + Flush) by internal/kyc, never by the worker identity.
// Severity p2 is an engineering default (HQ-E1-2 OPEN). Alert
// NOTIFICATION for it is NOT IMPLEMENTED (no route or recipient,
// ALERT-DELIVERY-1 OPEN).
const KindKYCSubmissionFailedTerminal Kind = "kyc.submission_failed_terminal"

// KindLaunchSuspensionExhausted is the ADR 0112 slice 3 (security C4) P1 Kind: every bounded
// attempt to execute a SUSPENSION (the fail-closed direction of a launch status change) ended in
// a lock timeout, a deadlock or the C-3(b) commit check, so a subject that should have been
// suspended was not. Raised by internal/launchgov through RaiseDetached AFTER the business
// transaction rolled back (ADR 0102 7.4). Alert NOTIFICATION is NOT IMPLEMENTED (no route or
// recipient; ALERT-DELIVERY-1 OPEN): the row exists, nobody is paged.
const KindLaunchSuspensionExhausted Kind = "launch.suspension_exhausted"

// ADR 0119 slice S3 (migration 0138): the staff MFA and session security Kinds. Each ADR signal has a TENANT-staff
// Kind (requires a subject tenant; raised from the staff member's tenant session, so the tenant can read it) and a
// PLATFORM-staff Kind (no subject; raised from the platform staff member's own platform session). ADR 0102 requires a
// subject for every business Kind and a platform staff member has no tenant; migration 0138 widens alert_kinds'
// subject CHECK by exactly the five platform Kinds. All are raised post-commit (alerting.RaisePostCommit), after the
// business transaction that recorded the event committed. The ADR 0119 names map as staff_mfa_integrity ->
// staff.mfa_integrity / staff.platform_mfa_integrity, and likewise. Attributes are ids and closed codes only (never a
// code, token, secret or email). Notification routing is NOT IMPLEMENTED (ALERT-DELIVERY-1 OPEN).
const (
	// KindStaffMFAIntegrity (p1): a stored factor failed its AEAD check, or an account that once had an active factor
	// has none (residual R3, a factor deleted or revoked out of band), or the active factor vanished between the
	// challenge and its verification.
	KindStaffMFAIntegrity         Kind = "staff.mfa_integrity"
	KindStaffPlatformMFAIntegrity Kind = "staff.platform_mfa_integrity"
	// KindStaffSessionIntegrity (p1): a staff session's assurance seal is missing (after the 0138 cutover) or wrong.
	KindStaffSessionIntegrity         Kind = "staff.session_integrity"
	KindStaffPlatformSessionIntegrity Kind = "staff.platform_session_integrity"
	// KindStaffMFAHardLocked (p2): 10 second-factor failures in 24 hours after a correct password (a compromise
	// signal); only an MFA reset clears the lock.
	KindStaffMFAHardLocked         Kind = "staff.mfa_hard_locked"
	KindStaffPlatformMFAHardLocked Kind = "staff.platform_mfa_hard_locked"
	// KindStaffMFARecoveryUsed (p3, the ADR's "info"): a recovery code was used.
	KindStaffMFARecoveryUsed         Kind = "staff.mfa_recovery_used"
	KindStaffPlatformMFARecoveryUsed Kind = "staff.platform_mfa_recovery_used"
	// KindStaffMFAFailurePattern (p2, S3 entry gate (e)): second-factor failures after a correct password on 3 or
	// more distinct days within 7 days (a slow-rate guessing pattern below the lock thresholds).
	KindStaffMFAFailurePattern         Kind = "staff.mfa_failure_pattern"
	KindStaffPlatformMFAFailurePattern Kind = "staff.platform_mfa_failure_pattern"
	// KindStaffPlatformBreakGlassUsed (p1, ADR 0119 S6 section 11, migration 0139): cmd/seed-admin -break-glass
	// created a platform admin. Platform-only (subjectless), raised post-commit by the job.
	KindStaffPlatformBreakGlassUsed Kind = "staff.platform_break_glass_used"
)

func staffKind(k Kind, sev Severity, platform bool, allowed ...string) KindDef {
	return KindDef{Kind: k, Severity: sev, Scope: ScopeKindPlatform, RequiresSubject: !platform,
		InTxRaisableByTenant: !platform, RaiseMode: RaiseModePostCommit, AllowedKeys: keys(allowed...)}
}

// KindAMLHoldEvalFailed is the ADR 0116 s5.1 (migration 0141) Kind: the AML hold evaluator errored or timed out at a
// money-boundary gate, so the evaluated operation was denied fail-closed. The payload carries the operation name only:
// no case id, person id, account id or match category (ADR 0116 s5.2). Severity p2 is an ENGINEERING DEFAULT
// (ALERT-DELIVERY-1 OPEN: nobody is paged).
const KindAMLHoldEvalFailed Kind = "aml.hold_eval_failed"

// KindReconciliationSweepFailed is the A9 (migration 0156) platform, subjectless P1 Kind: the reconciliation
// scheduler could not LIST tenants (phase list_active or list_non_active; R3-LISTING-FAILURE-LOG-ONLY-1) or a
// whole sweep panicked (phase panic), so no tenant was (fully) reconciled in that sweep. Per-tenant failures
// are reconciliation.run_failed. It is raised ONLY by RaiseSweepFailed (fallback.go) under the
// alert_dispatcher platform-service identity, because the scheduler has no tenant and no platform-admin
// principal. Attributes: phase (closed set) and sqlstate_class. Never the error text.
const KindReconciliationSweepFailed Kind = "reconciliation.sweep_failed"

// KindWithdrawalPayoutGateDenied is the F4 (migration 0169) Kind: the WD-RG-1 withdrawal RG/risk gate, re-run at payout
// dispatch (T1p) or re-dispatch (the sweeper's T2 / T12), REFUSED a payout that was allowed at request time. No money
// moved: a T1p denial leaves the request `approved` with its hold, a T2 / T12 denial escalates the attempt (T16). Raised
// in-tx by internal/payments as the last statement of the denial transaction. Attributes are closed codes only (gate
// code, reason code, stage): never an amount, an account or a person. Severity p2 is an ENGINEERING DEFAULT
// (ALERT-DELIVERY-1 OPEN: nobody is paged).
const KindWithdrawalPayoutGateDenied Kind = "withdrawal.payout_gate_denied"

func keys(ks ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(ks))
	for _, k := range ks {
		m[k] = struct{}{}
	}
	return m
}

// kindDefs is the closed registry, keyed by Kind. Every field here MUST
// match the corresponding migration 0110 alert_kinds seed row exactly -
// LF test 3 (a table-driven walk, kinds_migration_parity_integration_test.go)
// asserts this at the database.
var kindDefs = map[Kind]KindDef{
	KindPaymentMultipleSuccessForIntent: {
		Kind: KindPaymentMultipleSuccessForIntent, Severity: SeverityP1, Scope: ScopeKindPlatform,
		RequiresSubject: true, InTxRaisableByTenant: true, RaiseMode: RaiseModeInTx,
		AllowedKeys: keys("attempt_id", "evidence_kind", "provider_id", "request_id"),
	},
	KindPaymentDepositIntentIndexBackstop: {
		Kind: KindPaymentDepositIntentIndexBackstop, Severity: SeverityP1, Scope: ScopeKindPlatform,
		RequiresSubject: true, InTxRaisableByTenant: true, RaiseMode: RaiseModeInTx,
		AllowedKeys: keys("attempt_id", "request_id"),
	},
	KindReconciliationLedgerProjectionDrift: {
		Kind: KindReconciliationLedgerProjectionDrift, Severity: SeverityP1, Scope: ScopeKindPlatform,
		RequiresSubject: true, InTxRaisableByTenant: true, RaiseMode: RaiseModeInTx,
		AllowedKeys: keys("run_id", "mismatch_count", "request_id"),
	},
	KindReconciliationSportsbookSettlement: {
		Kind: KindReconciliationSportsbookSettlement, Severity: SeverityP1, Scope: ScopeKindPlatform,
		RequiresSubject: true, InTxRaisableByTenant: true, RaiseMode: RaiseModeInTx,
		AllowedKeys: keys("run_id", "mismatch_count", "statement_source", "request_id"),
	},
	KindReconciliationCasinoConsistency: {
		Kind: KindReconciliationCasinoConsistency, Severity: SeverityP1, Scope: ScopeKindPlatform,
		RequiresSubject: true, InTxRaisableByTenant: true, RaiseMode: RaiseModeInTx,
		AllowedKeys: keys("run_id", "mismatch_count", "request_id"),
	},
	KindReconciliationCasinoStatement: {
		Kind: KindReconciliationCasinoStatement, Severity: SeverityP1, Scope: ScopeKindPlatform,
		RequiresSubject: true, InTxRaisableByTenant: true, RaiseMode: RaiseModePostCommit,
		AllowedKeys: keys("run_id", "mismatch_count", "statement_source", "request_id"),
	},
	KindReconciliationPaymentStatement: {
		Kind: KindReconciliationPaymentStatement, Severity: SeverityP1, Scope: ScopeKindPlatform,
		RequiresSubject: true, InTxRaisableByTenant: true, RaiseMode: RaiseModePostCommit,
		AllowedKeys: keys("run_id", "mismatch_count", "statement_source", "import_id", "request_id"),
	},
	KindReconciliationRunFailed: {
		Kind: KindReconciliationRunFailed, Severity: SeverityP1, Scope: ScopeKindPlatform,
		RequiresSubject: true, InTxRaisableByTenant: true, RaiseMode: RaiseModeDetached,
		AllowedKeys: keys("stream", "phase", "sqlstate_class", "request_id"),
	},
	KindPaymentKillSwitchEngaged: {
		Kind: KindPaymentKillSwitchEngaged, Severity: SeverityP2, Scope: ScopeKindPlatform,
		RequiresSubject: true, InTxRaisableByTenant: true, RaiseMode: RaiseModePostCommit,
		AllowedKeys: keys("provider_scope", "operation_scope", "reason_code", "changed_by_scope", "is_platform_takeover", "request_id"),
	},
	KindCasinoCallbackIntegrity: {
		Kind: KindCasinoCallbackIntegrity, Severity: SeverityP1, Scope: ScopeKindPlatform,
		RequiresSubject: true, InTxRaisableByTenant: true, RaiseMode: RaiseModeDetached,
		AllowedKeys: keys("provider_id", "request_id"),
	},
	KindPaymentWebhookIntegrity: {
		Kind: KindPaymentWebhookIntegrity, Severity: SeverityP1, Scope: ScopeKindPlatform,
		RequiresSubject: true, InTxRaisableByTenant: true, RaiseMode: RaiseModeDetached,
		AllowedKeys: keys("provider_id", "request_id"),
	},
	KindSimulationPaymentPayloadMismatch: {
		Kind: KindSimulationPaymentPayloadMismatch, Severity: SeverityP3, Scope: ScopeKindPlatform, Simulation: true,
		RequiresSubject: true, InTxRaisableByTenant: true, RaiseMode: RaiseModeDetached,
		AllowedKeys: keys("provider_id", "request_id"),
	},
	KindSimulationCasinoPlay: {
		Kind: KindSimulationCasinoPlay, Severity: SeverityP3, Scope: ScopeKindPlatform, Simulation: true,
		RequiresSubject: true, InTxRaisableByTenant: true, RaiseMode: RaiseModeDetached,
		AllowedKeys: keys("action", "request_id"),
	},
	KindSimulationSportsbookSettlement: {
		Kind: KindSimulationSportsbookSettlement, Severity: SeverityP3, Scope: ScopeKindPlatform, Simulation: true,
		RequiresSubject: true, InTxRaisableByTenant: true, RaiseMode: RaiseModeDetached,
		AllowedKeys: keys("reason", "event_type", "generation", "bet_status", "request_id"),
	},
	KindKYCSubmissionFailedTerminal: {
		Kind: KindKYCSubmissionFailedTerminal, Severity: SeverityP2, Scope: ScopeKindPlatform,
		RequiresSubject: true, InTxRaisableByTenant: true, RaiseMode: RaiseModeInTx,
		AllowedKeys: keys("operation", "provider_id", "last_error_class", "outbox_id"),
	},
	KindLaunchSuspensionExhausted: {
		Kind: KindLaunchSuspensionExhausted, Severity: SeverityP1, Scope: ScopeKindPlatform,
		RequiresSubject: true, InTxRaisableByTenant: true, RaiseMode: RaiseModeDetached,
		AllowedKeys: keys("subject_kind", "attempts", "request_id"),
	},
	KindStaffMFAIntegrity:              staffKind(KindStaffMFAIntegrity, SeverityP1, false, "staff_id", "cause", "request_id"),
	KindStaffPlatformMFAIntegrity:      staffKind(KindStaffPlatformMFAIntegrity, SeverityP1, true, "staff_id", "cause", "request_id"),
	KindStaffSessionIntegrity:          staffKind(KindStaffSessionIntegrity, SeverityP1, false, "staff_id", "session_id", "cause", "request_id"),
	KindStaffPlatformSessionIntegrity:  staffKind(KindStaffPlatformSessionIntegrity, SeverityP1, true, "staff_id", "session_id", "cause", "request_id"),
	KindStaffMFAHardLocked:             staffKind(KindStaffMFAHardLocked, SeverityP2, false, "staff_id", "factor_id", "request_id"),
	KindStaffPlatformMFAHardLocked:     staffKind(KindStaffPlatformMFAHardLocked, SeverityP2, true, "staff_id", "factor_id", "request_id"),
	KindStaffMFARecoveryUsed:           staffKind(KindStaffMFARecoveryUsed, SeverityP3, false, "staff_id", "request_id"),
	KindStaffPlatformMFARecoveryUsed:   staffKind(KindStaffPlatformMFARecoveryUsed, SeverityP3, true, "staff_id", "request_id"),
	KindStaffMFAFailurePattern:         staffKind(KindStaffMFAFailurePattern, SeverityP2, false, "staff_id", "distinct_days", "request_id"),
	KindStaffPlatformMFAFailurePattern: staffKind(KindStaffPlatformMFAFailurePattern, SeverityP2, true, "staff_id", "distinct_days", "request_id"),
	KindStaffPlatformBreakGlassUsed:    staffKind(KindStaffPlatformBreakGlassUsed, SeverityP1, true, "staff_id", "operator", "reason", "force_additional"),

	KindAMLHoldEvalFailed: {
		Kind: KindAMLHoldEvalFailed, Severity: SeverityP2, Scope: ScopeKindPlatform,
		RequiresSubject: true, InTxRaisableByTenant: true, RaiseMode: RaiseModeInTx,
		AllowedKeys: keys("operation"),
	},
	KindWithdrawalPayoutGateDenied: {
		Kind: KindWithdrawalPayoutGateDenied, Severity: SeverityP2, Scope: ScopeKindPlatform,
		RequiresSubject: true, InTxRaisableByTenant: true, RaiseMode: RaiseModeInTx,
		AllowedKeys: keys("gate_code", "reason_code", "stage"),
	},
	KindReconciliationSweepFailed: {
		Kind: KindReconciliationSweepFailed, Severity: SeverityP1, Scope: ScopeKindPlatform,
		RequiresSubject: false, InTxRaisableByTenant: false, RaiseMode: RaiseModeDetached,
		AllowedKeys: keys("phase", "sqlstate_class"),
	},
	KindAlertingUnrouted: {
		Kind: KindAlertingUnrouted, Severity: SeverityP2, Scope: ScopeKindPlatform,
		RequiresSubject: false, InTxRaisableByTenant: false, RaiseMode: RaiseModeDetached,
		AllowedKeys: keys(),
	},
	KindAlertingDeliveryDead: {
		Kind: KindAlertingDeliveryDead, Severity: SeverityP2, Scope: ScopeKindPlatform,
		RequiresSubject: false, InTxRaisableByTenant: false, RaiseMode: RaiseModeDetached,
		AllowedKeys: keys(),
	},
	KindAlertingRaiseFailed: {
		Kind: KindAlertingRaiseFailed, Severity: SeverityP1, Scope: ScopeKindPlatform,
		RequiresSubject: false, InTxRaisableByTenant: false, RaiseMode: RaiseModeDetached,
		AllowedKeys: keys("kind", "sqlstate_class"),
	},
}

// Def returns the KindDef for k, and whether k is a known Kind.
func Def(k Kind) (KindDef, bool) {
	d, ok := kindDefs[k]
	return d, ok
}

// MustDef panics if k is unknown - for use only at Alert-construction
// call sites where the Kind is a compile-time constant from this file,
// never for a Kind derived from external input.
func MustDef(k Kind) KindDef {
	d, ok := kindDefs[k]
	if !ok {
		panic(fmt.Sprintf("alerting: unknown kind %q", k))
	}
	return d
}

// Kinds returns every known Kind, for table-driven tests that walk the
// whole vocabulary (LF test 3).
func Kinds() []Kind {
	out := make([]Kind, 0, len(kindDefs))
	for k := range kindDefs {
		out = append(out, k)
	}
	return out
}

// IsMetaKind reports whether k is one of the three dispatcher-only
// meta-Kinds (ADR §6.3).
func IsMetaKind(k Kind) bool {
	switch k {
	case KindAlertingUnrouted, KindAlertingDeliveryDead, KindAlertingRaiseFailed:
		return true
	default:
		return false
	}
}
