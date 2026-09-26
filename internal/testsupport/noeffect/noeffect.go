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
