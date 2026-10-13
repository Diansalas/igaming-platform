//go:build integration

// ADR 0115 S3 test support: hand-built ledgers (explicit created_at), intents and sessions seeded as
// the table owner into the S1 scratch world, and measurements run as the RUNTIME role in tenant
// staff scope (the scope of the bet path). Requires the same environment as the S1 world.
package rg

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// s3T is the fixed measurement instant of the hand-computed scenarios (UTC).
var s3T = time.Date(2025, 3, 12, 12, 0, 0, 0, time.UTC)

type s3Leg struct {
	acct uuid.UUID // uuid.Nil = the tenant's house account
	typ  ledger.AccountType
	dir  string // debit | credit
	amt  string // NUMERIC(38,0) text
}

type s3Tx struct {
	typ      string
	at       time.Time // zero = the database default (now())
	corr     uuid.UUID // zero = a fresh id
	reverses *uuid.UUID
	asset    string
	legs     []s3Leg
}

func cashDr(acct uuid.UUID, amt string) s3Leg {
	return s3Leg{acct, ledger.AccountPlayerCash, "debit", amt}
}
func cashCr(acct uuid.UUID, amt string) s3Leg {
	return s3Leg{acct, ledger.AccountPlayerCash, "credit", amt}
}
func houseDr(amt string) s3Leg { return s3Leg{uuid.Nil, ledger.AccountHouseGaming, "debit", amt} }
func houseCr(amt string) s3Leg { return s3Leg{uuid.Nil, ledger.AccountHouseGaming, "credit", amt} }

// s3Post inserts one balanced ledger transaction as the table owner with explicit created_at.
func (w *s1World) s3Post(t *testing.T, tenant uuid.UUID, p s3Tx) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if p.corr == uuid.Nil {
		p.corr = uuid.New()
	}
	var at *time.Time
	if !p.at.IsZero() {
		at = &p.at
	}
	err := w.owner.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key, correlation_id, reverses_transaction_id, created_at, posted_at)
			VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7::timestamptz, now()), COALESCE($7::timestamptz, now()))`,
			id, tenant, p.typ, "s3-"+id.String(), p.corr, p.reverses, at); err != nil {
			return err
		}
		for _, l := range p.legs {
			var wallet *uuid.UUID
			if l.acct != uuid.Nil {
				wid, err := s3Wallet(ctx, tx, tenant, l.acct, p.asset)
				if err != nil {
					return err
				}
				wallet = &wid
			}
			acc, err := ledger.GetOrCreateAccount(ctx, tx, tenant, wallet, l.typ, p.asset)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries (ledger_transaction_id, ledger_account_id, tenant_id, asset_code, direction, amount, created_at)
				VALUES ($1, $2, $3, $4, $5, $6::numeric, COALESCE($7::timestamptz, now()))`,
				id, acc, tenant, p.asset, l.dir, l.amt, at); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed ledger transaction %s: %v", p.typ, err)
	}
	return id
}

func s3Wallet(ctx context.Context, tx pgx.Tx, tenant, acct uuid.UUID, asset string) (uuid.UUID, error) {
	var brand uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT brand_id FROM player_accounts WHERE id = $1 AND tenant_id = $2`, acct, tenant).Scan(&brand); err != nil {
		return uuid.Nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO wallets (tenant_id, brand_id, player_account_id, asset_code) VALUES ($1, $2, $3, $4)
		ON CONFLICT (player_account_id, asset_code) DO NOTHING`, tenant, brand, acct, asset); err != nil {
		return uuid.Nil, err
	}
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM wallets WHERE player_account_id = $1 AND asset_code = $2`, acct, asset).Scan(&id)
	return id, err
}

// s3Intent seeds one deposit intent (explicit created_at) and returns its id.
func (w *s1World) s3Intent(t *testing.T, tenant, acct uuid.UUID, asset, status, amount string, at time.Time, ledgerTx *uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := w.owner.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		wid, err := s3Wallet(ctx, tx, tenant, acct, asset)
		if err != nil {
			return err
		}
		var brand uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT brand_id FROM player_accounts WHERE id = $1`, acct).Scan(&brand); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO deposit_intents (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, payment_method,
				idempotency_key, status, ledger_transaction_id, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, 'card', $8, $9, $10, $11, $11)`,
			id, tenant, brand, acct, wid, asset, amount, "s3-"+id.String(), status, ledgerTx, at)
		return err
	})
	if err != nil {
		t.Fatalf("seed deposit intent: %v", err)
	}
	return id
}

// s3Deposit seeds a ledger 'deposit' posting (Dr psp_clearing / Cr player_cash).
func (w *s1World) s3Deposit(t *testing.T, tenant, acct uuid.UUID, asset, amount string, at time.Time) uuid.UUID {
	t.Helper()
	return w.s3Post(t, tenant, s3Tx{typ: "deposit", at: at, asset: asset, legs: []s3Leg{
		{uuid.Nil, ledger.AccountPSPClearing, "debit", amount}, cashCr(acct, amount)}})
}

// s3Session seeds one sessions row (explicit created_at).
func (w *s1World) s3Session(t *testing.T, tenant, acct, id uuid.UUID, created time.Time, replacedBy *uuid.UUID, revoked *time.Time, expires time.Time) {
	t.Helper()
	err := w.owner.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO sessions (id, principal_type, principal_id, tenant_id, refresh_token_hash, created_at, expires_at, revoked_at, replaced_by_session_id)
			VALUES ($1, 'player', $2, $3, $4, $5, $6, $7, $8)`, id, acct, tenant, "s3-"+id.String(), created, expires, revoked, replacedBy)
		return err
	})
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

// measureStake runs MeasureStakeUsage as the runtime role in tenant staff scope.
func (w *s1World) measureStake(t *testing.T, f s1Fix, tenant uuid.UUID, req UsageRequest) StakeUsage {
	t.Helper()
	var out StakeUsage
	staff := f.staffA
	if tenant == f.tenantB {
		staff = f.staffB
	}
	wantOK(t, "measure stake usage", do(w.rt, staffSess(tenant, staff), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = MeasureStakeUsage(ctx, tx, req)
		return err
	}))
	return out
}

func (w *s1World) measureDeposit(t *testing.T, f s1Fix, tenant uuid.UUID, req UsageRequest) DepositUsage {
	t.Helper()
	var out DepositUsage
	staff := f.staffA
	if tenant == f.tenantB {
		staff = f.staffB
	}
	wantOK(t, "measure deposit usage", do(w.rt, staffSess(tenant, staff), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = MeasureDepositUsage(ctx, tx, req)
		return err
	}))
	return out
}

// amt renders a big.Int.
func amtStr(b *big.Int) string {
	if b == nil {
		return "<nil>"
	}
	return b.String()
}

// wantAmt fails unless got equals the decimal string want.
func wantAmt(t *testing.T, what string, got *big.Int, want string) {
	t.Helper()
	if got == nil || got.String() != want {
		t.Fatalf("%s: want %s, got %s", what, want, amtStr(got))
	}
}

// s3Windows is the standard window set of the hand-computed scenarios.
func s3Windows(tz string) []WindowSpec {
	return []WindowSpec{
		{Period: PeriodRolling24h}, {Period: PeriodRolling7d}, {Period: PeriodRolling30d},
		{Period: PeriodCalendarDay, CalendarTZ: tz},
	}
}

func ago(d time.Duration) time.Time { return s3T.Add(-d) }

const (
	hr  = time.Hour
	day = 24 * time.Hour
)
