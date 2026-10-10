// Package reconciliation implements the ledger-vs-projection
// reconciliation stream (docs/architecture/reconciliation-model.md §2.1)
// - the minimum reconciliation framework Stage 3B's approved scope
// requires. The other seven streams in that document (wallet<->PSP,
// wallet<->casino/sportsbook provider, provider payable, PSP clearing/
// reserve, crypto custodian) are explicitly out of scope this stage:
// they either depend on providers/domains not yet implemented (casino,
// sportsbook, crypto) or on a real PSP settlement feed this stage
// deliberately does not integrate (CLAUDE.md's Stage 3B scope gate). This
// package's Run/Mismatch model is designed so those streams slot into
// the same ReconciliationRun/ReconciliationMismatch tables later without
// a schema change - see docs/decisions/0019's enumerated table list.
// Stage 10 W1 adds the sportsbook_settlement stream (ADR 0088 §8,
// sportsbook_settlement.go) on exactly those tables.
//
// Per reconciliation-model.md §1: no financial tolerance is introduced
// where the Blueprint expects drift to be zero. Every mismatch found here
// is recorded, never silently corrected - "the projection is rebuilt
// from the ledger, never the reverse" (§2.1's own correction mechanism).
package reconciliation

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// Stream identifies which reconciliation stream a Run belongs to.
type Stream string

// StreamLedgerVsProjection is the stream Stage 3B implemented. Stage 10
// W1 adds StreamSportsbookSettlement (sportsbook_settlement.go).
const StreamLedgerVsProjection Stream = "ledger_vs_projection"

// Status is a ReconciliationRun's outcome.
type Status string

const (
	StatusClean             Status = "clean"
	StatusMismatchesFound   Status = "mismatches_found"
	investigationStatusOpen        = "open"
)

// MismatchKind classifies what kind of discrepancy a Mismatch records -
// Stage 3C directive item 4 requires "missing projection" be detected as
// its own condition, distinct from "unexpected projection balance" /
// "ledger/projection mismatch" (both represented here as
// MismatchKindBalanceMismatch, since both are "a projection row exists
// but its totals are wrong" from this stream's point of view).
type MismatchKind string

const (
	MismatchKindMissingProjection MismatchKind = "missing_projection"
	MismatchKindBalanceMismatch   MismatchKind = "balance_mismatch"
	// MismatchKindLedgerUnlinkedManualAdjustment (PRH-2 K2, ADR 0100 §12,
	// LF ruling 4) is the DETECTIVE control for governed manual
	// adjustments: a manual_adjustment ledger transaction created at or
	// after the ledger_adjustment classification's governed_since cutover
	// that no executed ledger_adjustment_requests row links to. Standing
	// and unwindowed (every run re-raises every such transaction,
	// whatever the run's period), severity P1 (routed via ADR 0102 once
	// I-wire lands). The PREVENTIVE rule is LEDGER-MANUAL-ADJ-LINK-1.
	MismatchKindLedgerUnlinkedManualAdjustment MismatchKind = "ledger_unlinked_manual_adjustment"
)

// Run is one execution of a reconciliation stream over a period.
type Run struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	Stream      Stream
	PeriodStart time.Time
	PeriodEnd   time.Time
	RunAt       time.Time
	Status      Status
}

// Mismatch is one discrepancy a Run found, per
// reconciliation-model.md §5. ReconciliationKey/ExpectedValue/ActualValue
// are rendered as text since different streams compare different kinds
// of values (a ledger_account_id here; a (provider_id, provider_tx_id)
// pair for a future PSP stream).
type Mismatch struct {
	ID                  uuid.UUID
	TenantID            uuid.UUID
	RunID               uuid.UUID
	ReconciliationKey   string
	ExpectedValue       string
	ActualValue         string
	MismatchKind        MismatchKind
	InvestigationStatus string
}

// RunLedgerVsProjection recomputes every ledger_account's balance
// directly from ledger_entries (the authoritative source,
// ledger.RebuildBalance) and compares it against the materialized
// wallet_balance_projection row for the same account
// (ledger.GetProjectedBalance), for every ledger_account belonging to
// tenantID. It records exactly one ReconciliationRun and zero or more
// ReconciliationMismatch rows, atomically, inside tx - the caller opens
// tx via db.Pool.WithTenant(ctx, tenantID, ...), matching every other
// tenant-scoped write in this codebase.
//
// Per the Blueprint's own explicit example of a P1-triggering drift
// (reconciliation-model.md §2.1), ANY non-zero difference is a mismatch -
// there is no tolerance band. In the design this stage ships (the
// projection trigger runs in the SAME transaction as the ledger_entries
// insert - see migration 0023), a real mismatch here can only mean the
// trigger was bypassed, disabled, or a manual/administrative write
// touched wallet_balance_projection directly - i.e. exactly the class of
// bug this reconciliation stream exists to catch structurally, not
// speculatively.
func RunLedgerVsProjection(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, periodStart, periodEnd time.Time) (Run, []Mismatch, error) {
	run := Run{
		ID:          uuid.New(),
		TenantID:    tenantID,
		Stream:      StreamLedgerVsProjection,
		PeriodStart: periodStart,
		PeriodEnd:   periodEnd,
		RunAt:       time.Now().UTC(),
	}

	accountIDs, err := ledgerAccountIDsForTenant(ctx, tx, tenantID)
	if err != nil {
		return Run{}, nil, fmt.Errorf("reconciliation: list ledger accounts: %w", err)
	}

	var mismatches []Mismatch
	for _, accountID := range accountIDs {
		rebuilt, err := ledger.RebuildBalance(ctx, tx, accountID)
		if err != nil {
			return Run{}, nil, fmt.Errorf("reconciliation: rebuild balance for account %s: %w", accountID, err)
		}
		projected, err := ledger.GetProjectedBalance(ctx, tx, accountID)
		if err != nil {
			return Run{}, nil, fmt.Errorf("reconciliation: get projected balance for account %s: %w", accountID, err)
		}

		switch {
		case !projected.Found && rebuilt.DebitTotal == 0 && rebuilt.CreditTotal == 0:
			// A never-posted account: no ledger_entries (amount > 0 is a
			// CHECK, so zero totals mean no entries) and no projection row.
			// Ordinary, not a mismatch (see the comment below); for example
			// a sportsbook settlement event resolves house_gaming before any
			// posting, and a void of an open bet never posts to it. Before
			// this case the comparison below reported it as a
			// balance_mismatch (empty projected asset/type) - a false
			// positive (ADR 0112 slice 2 ledger-finance condition).
			continue
		case !projected.Found && (rebuilt.DebitTotal != 0 || rebuilt.CreditTotal != 0):
			// Directive item 4's first detection case: no
			// wallet_balance_projection row exists at all, yet
			// ledger_entries show real activity for this account. An
			// account that has genuinely never been posted to also has
			// no projection row (the projection trigger only ever fires
			// alongside the first ledger_entries insert - migration
			// 0023) - that case is ordinary and not a mismatch. Only a
			// non-zero rebuild with a missing row means the trigger was
			// bypassed, disabled, or the row was deleted after the fact.
			mismatches = append(mismatches, Mismatch{
				ID:                  uuid.New(),
				TenantID:            tenantID,
				ReconciliationKey:   accountID.String(),
				ExpectedValue:       fmt.Sprintf("debit=%d credit=%d", rebuilt.DebitTotal, rebuilt.CreditTotal),
				ActualValue:         "no wallet_balance_projection row",
				MismatchKind:        MismatchKindMissingProjection,
				InvestigationStatus: investigationStatusOpen,
			})
		case rebuilt.DebitTotal != projected.DebitTotal || rebuilt.CreditTotal != projected.CreditTotal ||
			rebuilt.AssetCode != projected.AssetCode || rebuilt.AccountType != projected.AccountType:
			// Specialist review (ledger-finance): the comparison
			// originally covered only the two totals. AssetCode/
			// AccountType are part of the SAME projection row and can
			// drift independently of the totals (e.g. a direct
			// administrative UPDATE touching the wrong column) - a
			// corrupted account_type specifically would previously have
			// gone undetected here while still feeding wallet.GetSummary
			// elsewhere, mis-reporting what kind of balance a player's
			// funds actually represent (e.g. a withdrawal hold reported
			// as spendable cash).
			mismatches = append(mismatches, Mismatch{
				ID:                uuid.New(),
				TenantID:          tenantID,
				ReconciliationKey: accountID.String(),
				ExpectedValue: fmt.Sprintf("asset=%s type=%s debit=%d credit=%d",
					rebuilt.AssetCode, rebuilt.AccountType, rebuilt.DebitTotal, rebuilt.CreditTotal),
				ActualValue: fmt.Sprintf("asset=%s type=%s debit=%d credit=%d",
					projected.AssetCode, projected.AccountType, projected.DebitTotal, projected.CreditTotal),
				MismatchKind:        MismatchKindBalanceMismatch,
				InvestigationStatus: investigationStatusOpen,
			})
		}
	}

	unlinked, err := unlinkedManualAdjustments(ctx, tx, tenantID)
	if err != nil {
		return Run{}, nil, err
	}
	mismatches = append(mismatches, unlinked...)

	if len(mismatches) > 0 {
		run.Status = StatusMismatchesFound
	} else {
		run.Status = StatusClean
	}

	if err := persistRun(ctx, tx, run, mismatches); err != nil {
		return Run{}, nil, err
	}
	return run, mismatches, nil
}

// unlinkedManualAdjustments is the ADR 0100 §12 detective check: every
// manual_adjustment transaction of tenantID created at or after the
// ledger_adjustment governed_since cutover (migration 0113) that no
// EXECUTED ledger_adjustment_requests row links to. It deliberately
// ignores the run's period (standing, unwindowed). It reads requests
// through migration 0113's tenant_system_read_executed policy - the only
// ledger_adjustment_requests rows this system-tenant session can see are
// executed ones, which is exactly the set the check needs.
func unlinkedManualAdjustments(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]Mismatch, error) {
	rows, err := tx.Query(ctx, `
		SELECT t.id, t.created_at, COALESCE(t.reason_code, '')
		  FROM ledger_transactions t
		 WHERE t.tenant_id = $1
		   AND t.transaction_type = 'manual_adjustment'
		   AND t.created_at >= (SELECT c.governed_since FROM financial_control_classifications c
		                         WHERE c.operation_kind = 'ledger_adjustment')
		   AND NOT EXISTS (
		       SELECT 1 FROM ledger_adjustment_requests r
		        WHERE r.tenant_id = t.tenant_id AND r.state = 'executed' AND r.ledger_transaction_id = t.id)
		 ORDER BY t.created_at, t.id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("reconciliation: unlinked manual adjustment check: %w", err)
	}
	defer rows.Close()
	var out []Mismatch
	for rows.Next() {
		var id uuid.UUID
		var createdAt time.Time
		var reason string
		if err := rows.Scan(&id, &createdAt, &reason); err != nil {
			return nil, err
		}
		out = append(out, Mismatch{
			ID:                  uuid.New(),
			TenantID:            tenantID,
			ReconciliationKey:   id.String(),
			ExpectedValue:       "an executed ledger_adjustment_requests row linked to this manual_adjustment transaction",
			ActualValue:         fmt.Sprintf("no linked executed request (reason_code=%s created_at=%s)", reason, createdAt.UTC().Format(time.RFC3339Nano)),
			MismatchKind:        MismatchKindLedgerUnlinkedManualAdjustment,
			InvestigationStatus: investigationStatusOpen,
		})
	}
	return out, rows.Err()
}

// persistRun inserts run and its mismatches (setting each mismatch's
// RunID) inside tx - the only writes any reconciliation stream makes.
func persistRun(ctx context.Context, tx pgx.Tx, run Run, mismatches []Mismatch) error {
	if _, err := tx.Exec(ctx,
		`INSERT INTO reconciliation_runs (id, tenant_id, stream, period_start, period_end, run_at, status)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		run.ID, run.TenantID, run.Stream, run.PeriodStart, run.PeriodEnd, run.RunAt, run.Status,
	); err != nil {
		return fmt.Errorf("reconciliation: insert run: %w", err)
	}

	for i := range mismatches {
		m := &mismatches[i]
		m.RunID = run.ID
		if _, err := tx.Exec(ctx,
			`INSERT INTO reconciliation_mismatches
				(id, tenant_id, reconciliation_run_id, reconciliation_key, expected_value, actual_value, mismatch_kind, investigation_status)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			m.ID, m.TenantID, m.RunID, m.ReconciliationKey, m.ExpectedValue, m.ActualValue, m.MismatchKind, m.InvestigationStatus,
		); err != nil {
			return fmt.Errorf("reconciliation: insert mismatch: %w", err)
		}
	}
	return nil
}

func ledgerAccountIDsForTenant(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `SELECT id FROM ledger_accounts WHERE tenant_id = $1`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ResolveMismatch marks a mismatch investigated/resolved with a note and
// resolver, per reconciliation-model.md §5 - it never itself mutates
// ledger data; a resolution that requires a ledger correction does so via
// a separate, ordinary compensating ledger.Post call (a manual_adjustment
// transaction), whose id the caller then records via
// correctionLedgerTransactionID.
func ResolveMismatch(ctx context.Context, tx pgx.Tx, mismatchID uuid.UUID, resolvedBy uuid.UUID, note string, correctionLedgerTransactionID *uuid.UUID) error {
	tag, err := tx.Exec(ctx,
		`UPDATE reconciliation_mismatches
		 SET investigation_status = 'resolved', resolution_note = $2, resolved_by = $3, resolved_at = now(),
		     correction_ledger_transaction_id = $4
		 WHERE id = $1`,
		mismatchID, note, resolvedBy, correctionLedgerTransactionID,
	)
	if err != nil {
		return fmt.Errorf("reconciliation: resolve mismatch: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("reconciliation: mismatch %s not found", mismatchID)
	}
	return nil
}
