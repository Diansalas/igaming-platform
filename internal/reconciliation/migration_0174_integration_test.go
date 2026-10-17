//go:build integration

// C3 / F4-R2-NEGBAL-SQL detection, migration 0174 (the player_wallet_negative_balance and
// payout_dispatched_while_wallet_negative mismatch kinds) on a THROWAWAY scratch database: the new CHECK is a strict
// superset of 0167's, a whole-schema up/down/up snapshot (the down restores 0167's CHECK), the check runs on that
// schema and stores its findings, and the down fails loud (WD099) while a finding of a new kind exists.
package reconciliation

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/negbal"
)

const migration0174Version = 174

func m0174KindCheck(t *testing.T, pool *db.Pool) string {
	t.Helper()
	var def string
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'reconciliation_mismatches_mismatch_kind_check'`).Scan(&def)
	}); err != nil {
		t.Fatal(err)
	}
	return def
}

func m0174Kinds(def string) map[string]bool {
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`'([a-z0-9_]+)'`).FindAllStringSubmatch(def, -1) {
		out[m[1]] = true
	}
	return out
}

func TestMigration0174_StrictSuperset_UpDownUp_DownFailsLoudOnFindings(t *testing.T) {
	ctx := context.Background()
	pool, dir := payScratchThrough(t, migration0174Version)
	snap174 := m0134Snapshot(t, pool)
	upKinds := m0174Kinds(m0174KindCheck(t, pool))
	for _, k := range []MismatchKind{MismatchKindPlayerWalletNegativeBalance, MismatchKindPayoutDispatchedWhileWalletNegative} {
		if !upKinds[string(k)] {
			t.Fatalf("0174 CHECK must admit %s", k)
		}
	}
	if rolled, err := pool.MigrateDown(ctx, dir, 1); err != nil || len(rolled) != 1 || rolled[0] != migration0174Version {
		t.Fatalf("down: %v %v", rolled, err)
	}
	downKinds := m0174Kinds(m0174KindCheck(t, pool))
	if !downKinds[string(MismatchKindPayPayoutReversalTombstoneButPaid)] || !downKinds[string(MismatchKindWithdrawalHoldOpenMismatch)] {
		t.Fatal("the down must restore 0167's list")
	}
	for k := range downKinds {
		if !upKinds[k] {
			t.Fatalf("0174 must be a strict superset of 0167: %s dropped", k)
		}
	}
	if len(upKinds) != len(downKinds)+2 || downKinds[string(MismatchKindPlayerWalletNegativeBalance)] || downKinds[string(MismatchKindPayoutDispatchedWhileWalletNegative)] {
		t.Fatalf("up adds exactly the two new kinds: up=%d down=%d", len(upKinds), len(downKinds))
	}
	if snap := m0134Snapshot(t, pool); snap == snap174 || strings.Contains(snap, string(MismatchKindPlayerWalletNegativeBalance)) {
		t.Fatal("down must remove exactly the new kinds")
	}
	if _, err := pool.MigrateUp(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if got := m0134Snapshot(t, pool); got != snap174 {
		t.Fatal("up after down differs from the first up")
	}

	// The check runs on this schema and its finding is storable.
	f := seedFixture(t, pool)
	if err := pool.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := negbal.Chargeback(ctx, tx, f.tenantID, f.walletID, "EUR", 1_001)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var ms []Mismatch
	if err := pool.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		_, ms, err = RunLedgerVsProjection(ctx, tx, f.tenantID, time.Now().Add(-time.Hour), time.Now())
		return err
	}); err != nil || len(ms) != 1 || ms[0].MismatchKind != MismatchKindPlayerWalletNegativeBalance {
		t.Fatalf("the new kind must be found and stored: %v %+v", err, ms)
	}
	_, err := pool.MigrateDown(ctx, dir, 1)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "WD099" {
		t.Fatalf("down with a finding of a new kind must refuse WD099, got %v", err)
	}
	if got := m0134Snapshot(t, pool); got != snap174 {
		t.Fatal("a refused down must change nothing")
	}
}
