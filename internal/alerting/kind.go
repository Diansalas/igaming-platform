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
