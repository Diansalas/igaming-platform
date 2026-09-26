//go:build integration

// Package noeffect implements the QA binding test plan's shared six-point
// "no financial/account-state effect on rejection" checklist
// (docs/plans/stage-10.2-planning/03-review-qa-test-plan.md, "Six-point
// no-effect checklist"; docs/plans/stage-10.2-planning/01-webhook-trust-
// design.md §H) as ONE helper, so a rejection test asserts the checklist
// by calling AssertNoEffect once, instead of duplicating six ad hoc
// assertions per test (per the plan's own instruction).
//
// Every read runs inside its OWN fresh transaction (db.Pool.WithTenant),
// never inside - or reusing - the transaction the code under test ran in,
// so a snapshot genuinely observes committed-or-rolled-back state, not an
// in-flight one.
//
// KYC-WH-1 (Stage 10.2, ADR 0091) is this helper's first caller
// (internal/kyc, internal/httpserver). CAS-WH-TENANT-1's casino tests are
// a separate specialist's own concern (out of this change's scope) and may
// extend this same package with casino-specific fields (e.g. a tombstone
// count) without touching the KYC call sites below.
package noeffect

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// Snapshot is the six-point checklist's observable state for a set of
// tenants and a set of individually-tracked verification rows:
//
//  1. no new/changed kyc_verifications row       -> VerificationStatus/VerificationUpdatedAt
//  2. no ledger_entries/ledger_transactions       -> LedgerTxCount/LedgerEntryCount
//  3. no tombstones (either tenant)               -> out of scope here (casino-only; see package doc)
//  4. no audit_log rows                           -> AuditLogCount
//  5. SUM(debits) == SUM(credits) unchanged       -> DebitsEqualCredits
//  6. projections unchanged                       -> out of scope here (KYC has no projection)
type Snapshot struct {
	// VerificationStatus/VerificationUpdatedAt are keyed by verification
	// id, for every id passed to Capture via ids.
	VerificationStatus     map[uuid.UUID]string
	VerificationUpdatedAt  map[uuid.UUID]string
	AuditLogCount          map[uuid.UUID]int
	LedgerTransactionCount map[uuid.UUID]int
	LedgerEntryCount       map[uuid.UUID]int
	DebitsEqualCredits     map[uuid.UUID]bool
}

// Verification names one row to track by id, scoped to tenantID (the tx
// used to read it is WithTenant(tenantID, ...) - a cross-tenant id/tenant
// pairing simply reads zero rows, which Capture treats as "not found",
// exactly the no-effect expectation for a row that must never have leaked
// across tenants in the first place).
type Verification struct {
	TenantID uuid.UUID
	ID       uuid.UUID
}

// Capture reads the checklist's state for every tenant in tenantIDs and
// every named verification in verifications, each inside its own fresh
// transaction.
func Capture(t *testing.T, pool *db.Pool, tenantIDs []uuid.UUID, verifications []Verification) Snapshot {
	t.Helper()
	snap := Snapshot{
		VerificationStatus:     map[uuid.UUID]string{},
		VerificationUpdatedAt:  map[uuid.UUID]string{},
		AuditLogCount:          map[uuid.UUID]int{},
		LedgerTransactionCount: map[uuid.UUID]int{},
		LedgerEntryCount:       map[uuid.UUID]int{},
		DebitsEqualCredits:     map[uuid.UUID]bool{},
	}

	for _, tenantID := range tenantIDs {
		var auditCount, ledgerTxCount, ledgerEntryCount int
		var debits, credits int64
		err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1`, tenantID).Scan(&auditCount); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenantID).Scan(&ledgerTxCount); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE tenant_id = $1`, tenantID).Scan(&ledgerEntryCount); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx,
				`SELECT COALESCE(SUM(amount) FILTER (WHERE direction = 'debit'), 0),
				        COALESCE(SUM(amount) FILTER (WHERE direction = 'credit'), 0)
				 FROM ledger_entries WHERE tenant_id = $1`, tenantID).Scan(&debits, &credits); err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			t.Fatalf("noeffect: capture tenant %s: %v", tenantID, err)
		}
		snap.AuditLogCount[tenantID] = auditCount
		snap.LedgerTransactionCount[tenantID] = ledgerTxCount
		snap.LedgerEntryCount[tenantID] = ledgerEntryCount
		snap.DebitsEqualCredits[tenantID] = debits == credits
	}

	for _, v := range verifications {
		var status, updatedAt string
		err := pool.WithTenant(context.Background(), v.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT status, updated_at::text FROM kyc_verifications WHERE id = $1`, v.ID).Scan(&status, &updatedAt)
		})
		if err == pgx.ErrNoRows {
			snap.VerificationStatus[v.ID] = "<absent>"
			snap.VerificationUpdatedAt[v.ID] = ""
			continue
		}
		if err != nil {
			t.Fatalf("noeffect: capture verification %s: %v", v.ID, err)
		}
		snap.VerificationStatus[v.ID] = status
		snap.VerificationUpdatedAt[v.ID] = updatedAt
	}

	return snap
}

// CasinoSnapshot is the casino-side six-point checklist's observable
// state, keyed per tenant (CAS-WH-TENANT-1, Stage 10.2). It fills in the
// two points KYC's own Snapshot leaves out-of-scope (see the package doc
// above): point 3 (tombstones) and point 6 (the wallet_balance_projection
// projection), plus the shared points 1/2/4/5 re-expressed over casino's
// own tables (ledger_transactions rather than kyc_verifications).
type CasinoSnapshot struct {
	LedgerTransactionCount map[uuid.UUID]int
	LedgerEntryCount       map[uuid.UUID]int
	// TombstoneCount counts ledger_transactions rows whose idempotency_key
	// carries the "tombstone:<provider>:<original>" shape ReceiveCallback's
	// postRollbackTombstone writes for a rollback of a never-seen original
	// (financial-transaction-flows.md §7) - point 3 of the checklist.
	TombstoneCount     map[uuid.UUID]int
	AuditLogCount      map[uuid.UUID]int
	DebitsEqualCredits map[uuid.UUID]bool
	// ProjectionDebitTotal/ProjectionCreditTotal sum
	// wallet_balance_projection's own debit_total/credit_total columns
	// across every ledger_account under the tenant - point 6. A rejection
	// that posts nothing changes neither sum, independent of whichever
	// individual account it would have touched had it posted.
	ProjectionDebitTotal  map[uuid.UUID]int64
	ProjectionCreditTotal map[uuid.UUID]int64
	// CasinoProviderRoundCount is casino_provider_rounds' own row count -
	// postBet's ONLY casino_* insert on the money path (BindProviderRound) -
	// so a rejection must never add one (security review SC-2's "no
	// casino_* rows").
	CasinoProviderRoundCount map[uuid.UUID]int
	// LaunchSessionStatusFingerprint is a per-tenant, deterministic
	// fingerprint of every casino_launch_sessions row's (id, status) pair -
	// catches a session being silently consumed/revoked by a rejected
	// callback even though inserting/deleting a launch session is not
	// itself part of ReceiveCallback (SC-2: a plain row COUNT would miss a
	// same-row status UPDATE, e.g. active -> consumed, so this checks the
	// full projection of id/status pairs instead of just count()).
	LaunchSessionStatusFingerprint map[uuid.UUID]string
}

// CaptureCasino reads the casino checklist's state for every tenant in
// tenantIDs, each inside its own fresh transaction (never the transaction
// the rejected callback ran in).
func CaptureCasino(t *testing.T, pool *db.Pool, tenantIDs []uuid.UUID) CasinoSnapshot {
	t.Helper()
	snap := CasinoSnapshot{
		LedgerTransactionCount:         map[uuid.UUID]int{},
		LedgerEntryCount:               map[uuid.UUID]int{},
		TombstoneCount:                 map[uuid.UUID]int{},
		AuditLogCount:                  map[uuid.UUID]int{},
		DebitsEqualCredits:             map[uuid.UUID]bool{},
		ProjectionDebitTotal:           map[uuid.UUID]int64{},
		ProjectionCreditTotal:          map[uuid.UUID]int64{},
		CasinoProviderRoundCount:       map[uuid.UUID]int{},
		LaunchSessionStatusFingerprint: map[uuid.UUID]string{},
	}

	for _, tenantID := range tenantIDs {
		var ledgerTxCount, ledgerEntryCount, tombstoneCount, auditCount, roundCount int
		var debits, credits, projDebit, projCredit int64
		var sessionFingerprint string
		err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenantID).Scan(&ledgerTxCount); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE tenant_id = $1`, tenantID).Scan(&ledgerEntryCount); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx,
				`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND idempotency_key LIKE 'tombstone:%'`, tenantID).Scan(&tombstoneCount); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1`, tenantID).Scan(&auditCount); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx,
				`SELECT COALESCE(SUM(amount) FILTER (WHERE direction = 'debit'), 0),
				        COALESCE(SUM(amount) FILTER (WHERE direction = 'credit'), 0)
				 FROM ledger_entries WHERE tenant_id = $1`, tenantID).Scan(&debits, &credits); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx,
				`SELECT COALESCE(SUM(p.debit_total), 0), COALESCE(SUM(p.credit_total), 0)
				   FROM wallet_balance_projection p
				   JOIN ledger_accounts la ON la.id = p.ledger_account_id
				  WHERE la.tenant_id = $1`, tenantID).Scan(&projDebit, &projCredit); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM casino_provider_rounds WHERE tenant_id = $1`, tenantID).Scan(&roundCount); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx,
				`SELECT COALESCE(string_agg(id::text || ':' || status, ',' ORDER BY id), '')
				   FROM casino_launch_sessions WHERE tenant_id = $1`, tenantID).Scan(&sessionFingerprint); err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			t.Fatalf("noeffect: capture casino tenant %s: %v", tenantID, err)
		}
		snap.LedgerTransactionCount[tenantID] = ledgerTxCount
		snap.LedgerEntryCount[tenantID] = ledgerEntryCount
		snap.TombstoneCount[tenantID] = tombstoneCount
		snap.AuditLogCount[tenantID] = auditCount
		snap.DebitsEqualCredits[tenantID] = debits == credits
		snap.ProjectionDebitTotal[tenantID] = projDebit
		snap.ProjectionCreditTotal[tenantID] = projCredit
		snap.CasinoProviderRoundCount[tenantID] = roundCount
		snap.LaunchSessionStatusFingerprint[tenantID] = sessionFingerprint
	}
	return snap
}

// AssertNoCasinoEffect re-captures the identical casino checklist and
// fails the test on any divergence from before - the single call every
// casino rejection test needs, covering every tenant the caller named
// (typically BOTH tenants in a cross-tenant scenario).
func AssertNoCasinoEffect(t *testing.T, pool *db.Pool, tenantIDs []uuid.UUID, before CasinoSnapshot) {
	t.Helper()
	after := CaptureCasino(t, pool, tenantIDs)

	for _, tenantID := range tenantIDs {
		if before.LedgerTransactionCount[tenantID] != after.LedgerTransactionCount[tenantID] {
			t.Errorf("noeffect: tenant %s: ledger_transactions count changed %d -> %d", tenantID, before.LedgerTransactionCount[tenantID], after.LedgerTransactionCount[tenantID])
		}
		if before.LedgerEntryCount[tenantID] != after.LedgerEntryCount[tenantID] {
			t.Errorf("noeffect: tenant %s: ledger_entries count changed %d -> %d", tenantID, before.LedgerEntryCount[tenantID], after.LedgerEntryCount[tenantID])
		}
		if before.TombstoneCount[tenantID] != after.TombstoneCount[tenantID] {
			t.Errorf("noeffect: tenant %s: tombstone count changed %d -> %d", tenantID, before.TombstoneCount[tenantID], after.TombstoneCount[tenantID])
		}
		if before.AuditLogCount[tenantID] != after.AuditLogCount[tenantID] {
			t.Errorf("noeffect: tenant %s: audit_log count changed %d -> %d", tenantID, before.AuditLogCount[tenantID], after.AuditLogCount[tenantID])
		}
		if !after.DebitsEqualCredits[tenantID] {
			t.Errorf("noeffect: tenant %s: SUM(debits) != SUM(credits) after the rejection", tenantID)
		}
		if before.ProjectionDebitTotal[tenantID] != after.ProjectionDebitTotal[tenantID] || before.ProjectionCreditTotal[tenantID] != after.ProjectionCreditTotal[tenantID] {
			t.Errorf("noeffect: tenant %s: wallet_balance_projection totals changed (debit %d -> %d, credit %d -> %d)",
				tenantID, before.ProjectionDebitTotal[tenantID], after.ProjectionDebitTotal[tenantID], before.ProjectionCreditTotal[tenantID], after.ProjectionCreditTotal[tenantID])
		}
		if before.CasinoProviderRoundCount[tenantID] != after.CasinoProviderRoundCount[tenantID] {
			t.Errorf("noeffect: tenant %s: casino_provider_rounds count changed %d -> %d", tenantID, before.CasinoProviderRoundCount[tenantID], after.CasinoProviderRoundCount[tenantID])
		}
		if before.LaunchSessionStatusFingerprint[tenantID] != after.LaunchSessionStatusFingerprint[tenantID] {
			t.Errorf("noeffect: tenant %s: casino_launch_sessions (id,status) set changed %q -> %q", tenantID, before.LaunchSessionStatusFingerprint[tenantID], after.LaunchSessionStatusFingerprint[tenantID])
		}
	}
}

// AssertNoEffect re-captures the identical checklist and fails the test on
// any divergence from before - the single call every rejection test needs,
// covering every tenant/verification the caller named (typically BOTH
// tenants in a cross-tenant scenario, even though only one is the
// "target").
func AssertNoEffect(t *testing.T, pool *db.Pool, tenantIDs []uuid.UUID, verifications []Verification, before Snapshot) {
	t.Helper()
	after := Capture(t, pool, tenantIDs, verifications)

	for _, tenantID := range tenantIDs {
		if before.AuditLogCount[tenantID] != after.AuditLogCount[tenantID] {
			t.Errorf("noeffect: tenant %s: audit_log count changed %d -> %d", tenantID, before.AuditLogCount[tenantID], after.AuditLogCount[tenantID])
		}
		if before.LedgerTransactionCount[tenantID] != after.LedgerTransactionCount[tenantID] {
			t.Errorf("noeffect: tenant %s: ledger_transactions count changed %d -> %d", tenantID, before.LedgerTransactionCount[tenantID], after.LedgerTransactionCount[tenantID])
		}
		if before.LedgerEntryCount[tenantID] != after.LedgerEntryCount[tenantID] {
			t.Errorf("noeffect: tenant %s: ledger_entries count changed %d -> %d", tenantID, before.LedgerEntryCount[tenantID], after.LedgerEntryCount[tenantID])
		}
		if !after.DebitsEqualCredits[tenantID] {
			t.Errorf("noeffect: tenant %s: SUM(debits) != SUM(credits) after the rejection", tenantID)
		}
	}
	for id, wantStatus := range before.VerificationStatus {
		if got := after.VerificationStatus[id]; got != wantStatus {
			t.Errorf("noeffect: verification %s: status changed %q -> %q", id, wantStatus, got)
		}
		if got := after.VerificationUpdatedAt[id]; got != before.VerificationUpdatedAt[id] {
			t.Errorf("noeffect: verification %s: updated_at changed %q -> %q", id, before.VerificationUpdatedAt[id], got)
		}
	}
}
