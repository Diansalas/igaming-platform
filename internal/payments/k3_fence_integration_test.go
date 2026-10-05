//go:build integration

package payments

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// k3Accounts looks up the ledger account ids the fence cases need.
type k3Accounts struct {
	hold, cash, otherHold, otherCash, houseGaming uuid.UUID
}

func (w *k3World) accounts(otherWallet uuid.UUID) k3Accounts {
	w.t.Helper()
	var a k3Accounts
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		ids, err := ledger.GetOrCreateAccounts(ctx, tx, w.f.tenantID,
			ledger.AccountSpec{WalletID: &w.f.walletID, AccountType: ledger.AccountPlayerWithdrawalHold, AssetCode: "EUR"},
			ledger.AccountSpec{WalletID: &w.f.walletID, AccountType: ledger.AccountPlayerCash, AssetCode: "EUR"},
			ledger.AccountSpec{WalletID: &otherWallet, AccountType: ledger.AccountPlayerWithdrawalHold, AssetCode: "EUR"},
			ledger.AccountSpec{WalletID: &otherWallet, AccountType: ledger.AccountPlayerCash, AssetCode: "EUR"},
			ledger.AccountSpec{AccountType: ledger.AccountHouseGaming, AssetCode: "EUR"})
		if err != nil {
			return err
		}
		a = k3Accounts{ids[0], ids[1], ids[2], ids[3], ids[4]}
		return nil
	})
	return a
}

// otherWalletOf creates a second player's wallet in the world's tenant.
func (w *k3World) otherWallet() uuid.UUID {
	w.t.Helper()
	var wal uuid.UUID
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		player, person := uuid.New(), uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			VALUES ($1, $2, $3, $4, $5, 'x', 'active')`, player, w.f.tenantID, w.f.brandID, person, player.String()+"@k3.invalid"); err != nil {
			return err
		}
		wal = uuid.New()
		_, err := tx.Exec(ctx, `INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1, $2, $3, $4, 'EUR')`,
			wal, w.f.tenantID, w.f.brandID, player)
		return err
	})
	return wal
}

func k3InsertTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, id uuid.UUID, typ, key string, providerID, providerTx *string, corr uuid.UUID) error {
	_, err := tx.Exec(ctx, `INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key, provider_id, provider_tx_id, correlation_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, id, tenantID, typ, key, providerID, providerTx, corr)
	return err
}

func k3InsertEntry(ctx context.Context, tx pgx.Tx, tenantID, txID, account uuid.UUID, direction string, amount int64) error {
	_, err := tx.Exec(ctx, `INSERT INTO ledger_entries (ledger_transaction_id, ledger_account_id, tenant_id, asset_code, direction, amount)
		VALUES ($1, $2, $3, (SELECT asset_code FROM ledger_accounts WHERE id = $2), $4, $5)`, txID, account, tenantID, direction, amount)
	return err
}

// T-2 (R-1, D-1, D-2; K2-C1/C2 analogs): an ACTING M2 "paid" and "not paid" post
// successfully - including the first-ever psp_clearing creation for a tenant and
// asset - and every deviation is refused by the entries fence or the widened
// ledger_accounts policy: wrong wallet, wrong amount, wrong asset, wrong
// account type, a third entry, a second leg in one direction, an entry appended
// after `executed` in the same transaction, a ledger_accounts INSERT of another
// type or another wallet, and the same inserts with no executing M2 in the txid.
func TestK3_T2_ActingFence(t *testing.T) {
	t.Run("paid_posts_including_first_psp_clearing_creation", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1, noClearing: true})
		wr, a := w.ambiguousPayout(250)
		if n := w.countRows(`SELECT count(*) FROM ledger_accounts WHERE tenant_id = $1 AND account_type = 'psp_clearing'`, w.f.tenantID); n != 0 {
			t.Fatalf("setup: the tenant already has %d psp_clearing account(s)", n)
		}
		res := w.executeM2Acting(a.ID, ResolutionM2DeclarePaid)
		if res.State != ResolutionExecuted {
			t.Fatalf("state %s", res.State)
		}
		if n := w.countRows(`SELECT count(*) FROM ledger_accounts WHERE tenant_id = $1 AND account_type = 'psp_clearing' AND wallet_id IS NULL`, w.f.tenantID); n != 1 {
			t.Fatalf("psp_clearing accounts after the acting M2 = %d, want 1", n)
		}
		if w.withdrawalOf(wr.ID).State != withdrawal.StateCompleted || w.pspClearing() != 250 {
			t.Fatal("acting paid outcome wrong")
		}
		w.assertInvariants()
	})
	t.Run("not_paid_posts", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		wr, a := w.ambiguousPayout(250)
		res := w.executeM2Acting(a.ID, ResolutionM2DeclareNotPaid)
		if res.State != ResolutionExecuted || w.withdrawalOf(wr.ID).State != withdrawal.StateFailed {
			t.Fatalf("acting not-paid outcome wrong: %+v", res)
		}
		w.assertInvariants()
	})

	for _, kind := range []ResolutionKind{ResolutionM2DeclarePaid, ResolutionM2DeclareNotPaid} {
		kind := kind
		t.Run("deviations_refused/"+string(kind), func(t *testing.T) {
			w := newK3World(t, k3Opts{base: 1})
			wr, a := w.ambiguousPayout(250)
			other := w.otherWallet()
			acc := w.accounts(other)
			// A psp_clearing account exists (seed deposit); its id for the house leg.
			var psp uuid.UUID
			w.tx(func(ctx context.Context, tx pgx.Tx) error {
				id, err := ledger.GetOrCreateAccount(ctx, tx, w.f.tenantID, nil, ledger.AccountPSPClearing, "EUR")
				psp = id
				return err
			})
			r := w.mustRequest(w.acting, w.m2In(a.ID, kind))
			typ, key := "withdrawal_completed", ""
			var provider, ptx *string
			houseDebitOrCash, houseDir := psp, "credit"
			if kind == ResolutionM2DeclarePaid {
				provider = &w.provider
				p := ReservedDeclaredTxID(r.ID)
				ptx = &p
				key = w.provider + ":" + p
			} else {
				typ, key = "withdrawal_failed", wr.ID.String()+":failed"
				houseDebitOrCash = acc.cash
			}
			err := w.inExecutingActing(r, w.acting2, func(ctx context.Context, tx pgx.Tx) error {
				newTx := func(ctx context.Context, tx pgx.Tx) (uuid.UUID, error) {
					id := uuid.New()
					return id, k3InsertTx(ctx, tx, w.f.tenantID, id, typ, key, provider, ptx, wr.ID)
				}
				expect := func(name, code string, fn func(ctx context.Context, tx pgx.Tx) error) {
					err := k3Try(ctx, tx, fn)
					if k3Code(err) != code {
						t.Errorf("%s: want SQLSTATE %s, got %v", name, code, err)
					}
				}
				// Header deviations.
				expect("wrong idempotency key", "CG030", func(ctx context.Context, tx pgx.Tx) error {
					return k3InsertTx(ctx, tx, w.f.tenantID, uuid.New(), typ, key+"x", provider, ptx, wr.ID)
				})
				expect("wrong correlation", "CG030", func(ctx context.Context, tx pgx.Tx) error {
					return k3InsertTx(ctx, tx, w.f.tenantID, uuid.New(), typ, key, provider, ptx, uuid.New())
				})
				expect("wrong transaction type", "CG030", func(ctx context.Context, tx pgx.Tx) error {
					return k3InsertTx(ctx, tx, w.f.tenantID, uuid.New(), "manual_adjustment", key, nil, nil, wr.ID)
				})
				// Entry deviations on a correct header.
				entry := func(name string, build func(ctx context.Context, tx pgx.Tx, id uuid.UUID) error) {
					expect(name, "CG030", func(ctx context.Context, tx pgx.Tx) error {
						id, err := newTx(ctx, tx)
						if err != nil {
							return err
						}
						return build(ctx, tx, id)
					})
				}
				entry("wrong wallet hold", func(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
					return k3InsertEntry(ctx, tx, w.f.tenantID, id, acc.otherHold, "debit", wr.Amount)
				})
				entry("wrong amount", func(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
					return k3InsertEntry(ctx, tx, w.f.tenantID, id, acc.hold, "debit", wr.Amount+1)
				})
				entry("wrong account type (player_cash debited)", func(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
					return k3InsertEntry(ctx, tx, w.f.tenantID, id, acc.cash, "debit", wr.Amount)
				})
				entry("house gaming leg", func(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
					return k3InsertEntry(ctx, tx, w.f.tenantID, id, acc.houseGaming, "credit", wr.Amount)
				})
				entry("wrong direction (hold credited)", func(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
					return k3InsertEntry(ctx, tx, w.f.tenantID, id, acc.hold, "credit", wr.Amount)
				})
				entry("other wallet's cash leg", func(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
					return k3InsertEntry(ctx, tx, w.f.tenantID, id, acc.otherCash, "credit", wr.Amount)
				})
				entry("second leg in one direction", func(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
					if err := k3InsertEntry(ctx, tx, w.f.tenantID, id, acc.hold, "debit", wr.Amount); err != nil {
						return fmt.Errorf("the first, valid leg was refused: %w", err)
					}
					return k3InsertEntry(ctx, tx, w.f.tenantID, id, acc.hold, "debit", wr.Amount)
				})
				entry("a third entry", func(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
					if err := k3InsertEntry(ctx, tx, w.f.tenantID, id, acc.hold, "debit", wr.Amount); err != nil {
						return fmt.Errorf("leg 1 refused: %w", err)
					}
					if err := k3InsertEntry(ctx, tx, w.f.tenantID, id, houseDebitOrCash, houseDir, wr.Amount); err != nil {
						return fmt.Errorf("leg 2 refused: %w", err)
					}
					return k3InsertEntry(ctx, tx, w.f.tenantID, id, acc.hold, "debit", wr.Amount)
				})
				// ledger_accounts INSERTs: another type, another wallet's hold, and (not-paid)
				// psp_clearing, which only "declare paid" admits.
				insertAccount := func(wallet *uuid.UUID, typ string) error {
					_, err := tx.Exec(ctx, `INSERT INTO ledger_accounts (id, tenant_id, wallet_id, account_type, asset_code) VALUES ($1, $2, $3, $4, 'EUR')
						ON CONFLICT DO NOTHING`, uuid.New(), w.f.tenantID, wallet, typ)
					return err
				}
				expect("ledger_accounts house_gaming", "42501", func(ctx context.Context, tx pgx.Tx) error { return insertAccount(nil, "house_gaming") })
				expect("ledger_accounts another wallet's hold", "42501", func(ctx context.Context, tx pgx.Tx) error {
					return insertAccount(&other, "player_withdrawal_hold")
				})
				if kind == ResolutionM2DeclareNotPaid {
					expect("ledger_accounts psp_clearing under not-paid", "42501", func(ctx context.Context, tx pgx.Tx) error { return insertAccount(nil, "psp_clearing") })
				}

				// A correct execution still works in this very transaction.
				if kind == ResolutionM2DeclarePaid {
					if err := applyOperatorResolution(ctx, tx, a, AttemptSucceeded); err != nil {
						return err
					}
					if err := withdrawal.Complete(ctx, tx, wr.ID, w.provider, *r.ReservedProviderTxID); err != nil {
						return fmt.Errorf("the governed posting itself was refused: %w", err)
					}
				} else {
					if err := applyOperatorResolution(ctx, tx, a, AttemptDeclined); err != nil {
						return err
					}
					if err := withdrawal.Fail(ctx, tx, wr.ID, DeclaredNotPaidReason); err != nil {
						return fmt.Errorf("the governed posting itself was refused: %w", err)
					}
				}
				// An entry appended after `executed` in the same transaction.
				var linked uuid.UUID
				if err := tx.QueryRow(ctx, `SELECT release_ledger_transaction_id FROM withdrawal_requests WHERE id = $1`, wr.ID).Scan(&linked); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE payment_manual_resolutions SET state = 'executed', ledger_transaction_id = $2 WHERE id = $1`, r.ID, linked); err != nil {
					return fmt.Errorf("executed transition: %w", err)
				}
				expect("an entry after executed", "CG030", func(ctx context.Context, tx pgx.Tx) error {
					return k3InsertEntry(ctx, tx, w.f.tenantID, linked, acc.hold, "debit", wr.Amount)
				})
				return nil
			})
			k3RequireNoErr(t, err, "acting fence deviations")
		})
	}

	t.Run("no_executing_resolution_in_the_txid", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		wr, a := w.ambiguousPayout(250)
		other := w.otherWallet()
		acc := w.accounts(other)
		// An acting session with a valid grant but NO executing M2 resolution.
		err := w.pool.WithPlatformActingInTenant(context.Background(), w.acting.ID, w.f.tenantID, uuid.Nil, OperationKindForceResolve, func(ctx context.Context, tx pgx.Tx) error {
			for name, fn := range map[string]func() error{
				"ledger_transactions": func() error {
					return k3InsertTx(ctx, tx, w.f.tenantID, uuid.New(), "withdrawal_failed", wr.ID.String()+":failed", nil, nil, wr.ID)
				},
				"ledger_accounts hold": func() error {
					fresh := uuid.New()
					_, err := tx.Exec(ctx, `INSERT INTO ledger_accounts (id, tenant_id, wallet_id, account_type, asset_code) VALUES ($1, $2, $3, 'player_withdrawal_hold', 'EUR') ON CONFLICT DO NOTHING`, uuid.New(), w.f.tenantID, fresh)
					return err
				},
				"withdrawal.Fail": func() error { return withdrawal.Fail(ctx, tx, wr.ID, "x") },
				"withdrawal.Complete": func() error {
					return withdrawal.Complete(ctx, tx, wr.ID, w.provider, ReservedDeclaredTxID(uuid.New()))
				},
				"attempt UPDATE": func() error {
					_, err := tx.Exec(ctx, `UPDATE payment_attempts SET state = 'declined', last_evidence_kind = 'operator', resolved_at = now(), next_action_at = NULL WHERE id = $1`, a.ID)
					return err
				},
				"withdrawal UPDATE": func() error {
					_, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'failed' WHERE id = $1`, wr.ID)
					return err
				},
				"entry on an existing tx": func() error {
					return k3InsertEntry(ctx, tx, w.f.tenantID, uuid.New(), acc.hold, "debit", 1)
				},
			} {
				if err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error { return fn() }); err == nil {
					t.Errorf("%s was admitted with no executing M2 resolution in this transaction", name)
				}
			}
			return nil
		})
		k3RequireNoErr(t, err, "no-executing probes")
		if w.withdrawalOf(wr.ID).State != withdrawal.StateSubmitted {
			t.Fatal("withdrawal moved")
		}
	})
}

// executeM2Acting runs the governed path with PLATFORM_ACTING requester and
// approver (acting, acting2).
func (w *k3World) executeM2Acting(attemptID uuid.UUID, kind ResolutionKind) ManualResolution {
	w.t.Helper()
	r, err := w.request(w.acting, w.m2In(attemptID, kind))
	if err != nil {
		w.t.Fatalf("acting request: %v", err)
	}
	out, err := w.decide(w.acting2, r, ResolutionApprove)
	if err != nil || !out.Executed {
		w.t.Fatalf("acting approval: %v %+v", err, out)
	}
	return out.Resolution
}

// T-10 / K3-S1 (R-7): an operator-evidence UPDATE (or ANY acting-session UPDATE,
// state change or not) that also writes a column outside {state,
// last_evidence_kind, resolved_at, next_action_at, updated_at} is refused (MR040)
// in tenant AND acting sessions - including a same-state acting UPDATE that sets
// provider_reference or ledger_transaction_id from NULL.
func TestK3_T10_ColumnDiscipline(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(100)
	cols := []struct{ name, set string }{
		{"escalated_at", `escalated_at = now()`},
		{"poll_count", `poll_count = poll_count + 1`},
		{"decline_reason", `decline_reason = 'x'`},
		{"cascadable", `cascadable = true`},
		{"lease_owner", `lease_owner = 'x'`},
		{"claim_token", `claim_token = gen_random_uuid()`},
		{"terminal_reason", `terminal_reason = 'provider_reference_mismatch'`},
		{"submit_count", `submit_count = submit_count + 1`},
		{"last_sent_at", `last_sent_at = now()`},
	}
	for _, kind := range []ResolutionKind{ResolutionM2DeclareNotPaid} {
		for _, mode := range []string{"tenant", "acting"} {
			mode := mode
			r := w.mustRequest(w.f1, w.m2In(a.ID, kind))
			run := w.inExecuting
			approver := w.f2
			if mode == "acting" {
				run, approver = w.inExecutingActing, w.acting2
			}
			err := run(r, approver, func(ctx context.Context, tx pgx.Tx) error {
				for _, c := range cols {
					err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
						_, err := tx.Exec(ctx, `UPDATE payment_attempts SET state = 'declined', last_evidence_kind = 'operator', resolved_at = now(),
							next_action_at = NULL, `+c.set+` WHERE id = $1`, a.ID)
						return err
					})
					if k3Code(err) != "MR040" {
						t.Errorf("%s/%s: want MR040, got %v", mode, c.name, err)
					}
				}
				// The legal column set passes (control: the trigger does not over-fire).
				return k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
					_, err := tx.Exec(ctx, `UPDATE payment_attempts SET state = 'declined', last_evidence_kind = 'operator', resolved_at = now(),
						next_action_at = NULL, updated_at = now() WHERE id = $1`, a.ID)
					if err != nil {
						t.Errorf("%s: the allowed column set was refused: %v", mode, err)
					}
					return errK3Rollback
				})
			})
			k3RequireNoErr(t, err, mode+" T-10")
			// cancel so the next iteration can request again.
			if _, err := w.svc.Cancel(k3Ctx(w.f1), w.target(w.f1), r.ID, ResolutionMeta{}); err != nil {
				t.Fatal(err)
			}
		}
	}

	// K3-S1: a same-state ACTING update that sets provider_reference or
	// ledger_transaction_id from NULL (a reference-less never-sent attempt).
	_, ns := w.neverSentPayout(100)
	r := w.mustRequest(w.acting, w.m2In(ns.ID, ResolutionM2DeclareNotPaid))
	var anyTx uuid.UUID
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM ledger_transactions WHERE tenant_id = $1 LIMIT 1`, w.f.tenantID).Scan(&anyTx)
	})
	err := w.inExecutingActing(r, w.acting2, func(ctx context.Context, tx pgx.Tx) error {
		for name, set := range map[string]string{
			"provider_reference":    `provider_reference = 'k3-injected-ref'`,
			"ledger_transaction_id": fmt.Sprintf(`ledger_transaction_id = '%s'`, anyTx),
		} {
			err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE payment_attempts SET last_evidence_kind = 'operator', `+set+` WHERE id = $1`, ns.ID)
				return err
			})
			if k3Code(err) != "MR040" {
				t.Errorf("same-state acting UPDATE setting %s: want MR040, got %v", name, err)
			}
		}
		return nil
	})
	k3RequireNoErr(t, err, "K3-S1")
}

// C-14b (RLS, C-101-1): an acting session UPDATEs payment_attempts with callback
// evidence -> refused; an attempt or withdrawal UPDATE without an executing
// resolution -> refused; a deposit_intents UPDATE -> refused; the governed M2
// path -> succeeds.
func TestK3_C14b_ActingUpdatePolicies(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	wr, a := w.ambiguousPayout(100)
	dep := w.disputedDeposit(5000)
	r := w.mustRequest(w.acting, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
	err := w.inExecutingActing(r, w.acting2, func(ctx context.Context, tx pgx.Tx) error {
		for name, sql := range map[string]string{
			"callback evidence":   `UPDATE payment_attempts SET state = 'declined', last_evidence_kind = 'callback', resolved_at = now(), next_action_at = NULL WHERE id = $1`,
			"sync evidence":       `UPDATE payment_attempts SET state = 'succeeded', last_evidence_kind = 'sync', resolved_at = now(), next_action_at = NULL WHERE id = $1`,
			"query_status":        `UPDATE payment_attempts SET state = 'declined', last_evidence_kind = 'query_status', resolved_at = now(), next_action_at = NULL WHERE id = $1`,
			"same-state callback": `UPDATE payment_attempts SET last_evidence_kind = 'callback' WHERE id = $1`,
		} {
			sql := sql
			if err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, sql, a.ID)
				return err
			}); err == nil {
				t.Errorf("acting %s UPDATE on an attempt was admitted", name)
			}
		}
		// The deposit attempt (a different attempt, no resolution for it): refused.
		if err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE payment_attempts SET last_evidence_kind = 'operator' WHERE id = $1`, dep.ID)
			return err
		}); err == nil {
			t.Error("acting operator UPDATE of an attempt with no executing resolution was admitted")
		}
		if err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE deposit_intents SET updated_at = now() WHERE id = $1`, dep.DepositIntentID)
			return err
		}); err == nil {
			t.Error("an acting deposit_intents UPDATE was admitted (WITH CHECK false)")
		}
		// A withdrawal UPDATE for ANOTHER withdrawal (no resolution): refused.
		wr2, _ := w.payout(100)
		if err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET updated_at = now() WHERE id = $1`, wr2.ID)
			return err
		}); err == nil {
			t.Error("an acting withdrawal UPDATE without a resolution for it was admitted")
		}
		return nil
	})
	k3RequireNoErr(t, err, "C-14b")
	// Outside any executing resolution: refused too.
	err = w.pool.WithPlatformActingInTenant(context.Background(), w.acting.ID, w.f.tenantID, uuid.Nil, OperationKindForceResolve, func(ctx context.Context, tx pgx.Tx) error {
		if err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET updated_at = now() WHERE id = $1`, wr.ID)
			return err
		}); err == nil {
			t.Error("an acting withdrawal UPDATE with no executing resolution was admitted")
		}
		return nil
	})
	k3RequireNoErr(t, err, "C-14b outside")
	// The governed acting path works.
	if out, err := w.decide(w.acting2, r, ResolutionApprove); err != nil || !out.Executed {
		t.Fatalf("governed acting execution: %v %+v", err, out)
	}
	w.assertInvariants()
}
