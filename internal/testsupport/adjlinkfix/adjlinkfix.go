//go:build integration

// Package adjlinkfix (integration builds ONLY) is the single, explicit owner-only fixture exemption from migration
// 0153's session-independent manual_adjustment link fence (MANUAL-ADJ-LINK-1, ADR 0100 section 21).
//
// Since 0153 the database refuses, for EVERY session (runtime role or table owner, with or without GUCs), a
// ledger_transactions row of type manual_adjustment - and its entries - unless it is the posting of a governed
// ledger_adjustment_requests row executing in the same transaction (MA050 / MA051). Pre-0153 test fixtures used a
// manual_adjustment as a generic "fund the wallet" or "arbitrary balanced posting" fact. Those fixtures do not test
// manual adjustments; they need money on a wallet, or a posting of an arbitrary shape, to test something else.
//
// The exemption is NOT a new bypass: it is the existing ADR 0110 section 9.4 owner residual - the table owner may
// ALTER TABLE ... DISABLE TRIGGER - used exactly like launchfix / staffsv / pitest already use it. There is no GUC,
// no role check and no environment switch in the production schema. Without disables EXACTLY the two 0153 triggers
// inside the caller's transaction, runs fn, and re-enables them in the SAME transaction, so no other session ever
// observes them disabled: ALTER TABLE is transactional and holds a SHARE ROW EXCLUSIVE lock on both tables until the
// caller's transaction ends (other writers of ledger_transactions/ledger_entries wait; nothing is weakened for them).
//
// Rules for callers (pinned by internal/adjustment's static inventory test):
//   - tx must belong to the TABLE OWNER (TEST_DATABASE_URL); on the runtime role the ALTER fails with 42501;
//   - use it only for fixture setup (or to demonstrate the owner residual), never in a test that asserts the 0153
//     fence, and never inside a concurrency race (the table lock would serialize the racers);
//   - every new caller file must be added to the inventory with a one-line justification.
package adjlinkfix

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The two 0153 triggers. Named here once; the inventory test pins these names against the migration.
const (
	TransactionTrigger = "ledger_transactions_manual_adjustment_link"
	EntryTrigger       = "ledger_entries_manual_adjustment_link"
)

// Without runs fn inside tx with the two 0153 link triggers disabled, then re-enables them in the same transaction.
//
// On a scratch schema migrated below 0153 the triggers do not exist and fn simply runs (there is nothing to exempt).
//
// ALTER TABLE refuses a table with pending (deferred) trigger events (55006), and every ledger posting queues some
// (ledger_entries_balanced, the withdrawal-hold commit checks). Before each ALTER, Without therefore fires exactly
// the two ledger tables' own deferrable constraints (never another table's: e.g. the MA041 "never commit executing"
// check must stay at commit) and puts them back to DEFERRED - every one of them is INITIALLY DEFERRED (pinned by
// TestAdjLinkFix_LedgerDeferrableConstraintsInitiallyDeferred), so this restores exactly the default mode. Those
// checks run early, never skipped.
//
// If fn fails the triggers are re-enabled best effort; if tx is already aborted that fails too and the caller's
// rollback undoes the disable. fn's error is returned.
func Without(ctx context.Context, tx pgx.Tx, fn func() error) error {
	var present int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_trigger
		WHERE (tgrelid = 'public.ledger_transactions'::regclass AND tgname = $1)
		   OR (tgrelid = 'public.ledger_entries'::regclass AND tgname = $2)`, TransactionTrigger, EntryTrigger).Scan(&present); err != nil {
		return err
	}
	switch present {
	case 0:
		return fn()
	case 2:
	default:
		return fmt.Errorf("adjlinkfix: expected both 0153 link triggers or neither, found %d", present)
	}
	var names string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(string_agg(quote_ident(conname), ', ' ORDER BY conname), '')
		FROM pg_catalog.pg_constraint
		WHERE conrelid IN ('public.ledger_transactions'::regclass, 'public.ledger_entries'::regclass) AND condeferrable`).Scan(&names); err != nil {
		return err
	}
	flush := func() error {
		if names == "" {
			return nil
		}
		if _, err := tx.Exec(ctx, `SET CONSTRAINTS `+names+` IMMEDIATE`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SET CONSTRAINTS `+names+` DEFERRED`)
		return err
	}
	alter := func(action string) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE ledger_transactions `+action+` TRIGGER `+TransactionTrigger); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `ALTER TABLE ledger_entries `+action+` TRIGGER `+EntryTrigger)
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	if err := alter("DISABLE"); err != nil {
		return err
	}
	if ferr := fn(); ferr != nil {
		_ = flush()
		_ = alter("ENABLE")
		return ferr
	}
	if err := flush(); err != nil {
		return err
	}
	return alter("ENABLE")
}
