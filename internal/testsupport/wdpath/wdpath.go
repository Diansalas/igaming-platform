//go:build integration

// Package wdpath is the TEST-ONLY shared legal-path helper of ADR 0113 s5 (C6): since migration 0132
// a withdrawal request is born only as `requested` (WD025), moves only along the legal edges E1-E10
// (WD021/WD022/WD023), and must carry its exact ledger legs at COMMIT (A, B-i, B-ii). Fixtures that
// used to INSERT a row directly in a later state, or post a hand-built leg to a hold account, create
// the request here instead:
//
//   - Plant performs E0 + E0b in the caller's transaction exactly as withdrawal.RequestWithdrawal does
//     at the database level (INSERT `requested`, the withdrawal_requested hold leg keyed and correlated
//     by the request id, the hold link). It skips only the Go-layer gates that are not what these
//     fixtures test (KYC, funds sufficiency, the payout-instrument gate evaluation): the database
//     binding guard (0126) still runs unless the caller is an owner tamper plant inside
//     pitest.WithoutBindingGuard.
//   - Advance walks the REAL transitions through the Go writers in internal/withdrawal
//     (MoveToPendingReview, MarkSubmittedPending, AttachProviderReference, Complete, Fail, Cancel). E7
//     first records a fixture ALLOW of the WD-RG-1 dispatch gate (F4, migration 0169 WD053). E3
//     and E4 are the two exceptions, driven as the same legal database edge withdrawal.Approve /
//     withdrawal.Reject produce (an AUTOMATED approve / reject record, then the UPDATE and, for E4,
//     the `:rejected` release leg) because the Go writers resolve a four-eyes policy and a
//     person-linked staff approver that these fixtures do not exercise. Nothing is disabled.
//   - Seed runs Plant + Advance as the RUNTIME role (igaming_runtime, TEST_RUNTIME_DATABASE_URL's
//     credentials on the caller's database), never as the table owner.
//
// Integration build tag, under internal/testsupport: never compiled into an application binary.
package wdpath

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
	"github.com/Diansalas/igaming-platform/internal/withdrawalgate"
)

// Spec is one request to create. Zero ID / IdempotencyKey are generated. InstrumentID and Fingerprint
// are the payout binding (both or neither; neither = the legacy NULL/NULL shape, which only an owner
// plant inside pitest.WithoutBindingGuard can insert).
type Spec struct {
	ID              uuid.UUID
	TenantID        uuid.UUID
	BrandID         uuid.UUID
	PlayerAccountID uuid.UUID
	WalletID        uuid.UUID
	AssetCode       string
	Amount          int64
	IdempotencyKey  string
	InstrumentID    *uuid.UUID
	Fingerprint     *string

	// Used on E7 / E8 / E9 when the target needs them. Defaults: "wdpath-psp", no reference,
	// "wdpath-tx-<id>".
	ProviderID        string
	ProviderReference string
	ProviderTxID      string
}

func (s *Spec) defaults() {
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	if s.IdempotencyKey == "" {
		s.IdempotencyKey = "wdpath-" + s.ID.String()
	}
	if s.AssetCode == "" {
		s.AssetCode = "EUR"
	}
	if s.ProviderID == "" {
		s.ProviderID = "wdpath-psp"
	}
	if s.ProviderTxID == "" {
		s.ProviderTxID = "wdpath-tx-" + s.ID.String()
	}
}

// Result is what a fixture usually needs back.
type Result struct {
	ID                uuid.UUID
	HoldTransactionID uuid.UUID
	State             withdrawal.State
}

// Plant performs E0 + E0b in tx (the session must be the tenant scope of spec.TenantID). It returns the
// request id and the hold transaction id. Funds sufficiency is NOT checked (player_cash may go negative;
// a fixture that reads balances accounts for the hold leg).
func Plant(ctx context.Context, tx pgx.Tx, spec Spec) (Result, error) {
	spec.defaults()
	if spec.Amount <= 0 {
		return Result{}, errors.New("wdpath: amount must be positive")
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO withdrawal_requests
			(id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, idempotency_key,
			 payout_instrument_id, payout_instrument_fingerprint)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		spec.ID, spec.TenantID, spec.BrandID, spec.PlayerAccountID, spec.WalletID, spec.AssetCode,
		spec.Amount, spec.IdempotencyKey, spec.InstrumentID, spec.Fingerprint,
	); err != nil {
		return Result{}, fmt.Errorf("wdpath: E0 insert: %w", err)
	}
	accounts, err := ledger.GetOrCreateAccounts(ctx, tx, spec.TenantID,
		ledger.AccountSpec{WalletID: &spec.WalletID, AccountType: ledger.AccountPlayerCash, AssetCode: spec.AssetCode},
		ledger.AccountSpec{WalletID: &spec.WalletID, AccountType: ledger.AccountPlayerWithdrawalHold, AssetCode: spec.AssetCode},
	)
	if err != nil {
		return Result{}, fmt.Errorf("wdpath: resolve hold accounts: %w", err)
	}
	post, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID:        spec.TenantID,
		TransactionType: ledger.TxWithdrawalRequested,
		IdempotencyKey:  spec.ID.String(),
		CorrelationID:   spec.ID,
		Entries: []ledger.EntryInput{
			{LedgerAccountID: accounts[0], Direction: ledger.Debit, Amount: spec.Amount},
			{LedgerAccountID: accounts[1], Direction: ledger.Credit, Amount: spec.Amount},
		},
	})
	if err != nil {
		return Result{}, fmt.Errorf("wdpath: E0b hold post: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE withdrawal_requests SET hold_ledger_transaction_id = $1, updated_at = now() WHERE id = $2`,
		post.TransactionID, spec.ID); err != nil {
		return Result{}, fmt.Errorf("wdpath: E0b hold link: %w", err)
	}
	return Result{ID: spec.ID, HoldTransactionID: post.TransactionID, State: withdrawal.StateRequested}, nil
}

// Advance walks request id from its current state to target along the legal edges, in tx (the tenant
// scope of the request). Paths: requested -> pending_review -> approved -> submitted -> completed |
// failed; requested -> cancelled; pending_review -> rejected. spec supplies the provider values.
func Advance(ctx context.Context, tx pgx.Tx, id uuid.UUID, target withdrawal.State, spec Spec) error {
	spec.ID = id
	spec.defaults()
	for i := 0; i < 8; i++ {
		wr, err := withdrawal.GetByID(ctx, tx, id)
		if err != nil {
			return fmt.Errorf("wdpath: read request: %w", err)
		}
		if wr.State == target {
			return nil
		}
		var step error
		switch {
		case wr.State == withdrawal.StateRequested && target == withdrawal.StateCancelled:
			step = withdrawal.Cancel(ctx, tx, id)
		case wr.State == withdrawal.StateRequested:
			step = withdrawal.MoveToPendingReview(ctx, tx, id)
		case wr.State == withdrawal.StatePendingReview && target == withdrawal.StateRejected:
			step = rejectE4(ctx, tx, wr)
		case wr.State == withdrawal.StatePendingReview:
			step = approveE3(ctx, tx, wr)
		case wr.State == withdrawal.StateApproved:
			// F4 (migration 0169): E7 needs an allowing WD-RG-1 dispatch decision of the same transaction. The
			// fixture RECORDS one (like Plant, it skips the Go-layer gate evaluation these fixtures do not
			// test); the database still re-checks the RG half (WD052: account and wallet active, no negative
			// balance) at the transition.
			// (A migration test on a scratch schema that predates 0169 has no table and no backstop.)
			var present bool
			if step = tx.QueryRow(ctx, `SELECT to_regclass('public.withdrawal_gate_decisions') IS NOT NULL`).Scan(&present); step == nil && present {
				step = withdrawal.RecordDispatchGateDecision(ctx, tx, wr, withdrawal.DispatchStageDispatch, nil, withdrawalgate.Decision{Allowed: true, Outcome: "allow"})
			}
			if step == nil {
				step = withdrawal.MarkSubmittedPending(ctx, tx, id, spec.ProviderID)
			}
			if step == nil && spec.ProviderReference != "" {
				step = withdrawal.AttachProviderReference(ctx, tx, id, spec.ProviderReference)
			}
		case wr.State == withdrawal.StateSubmitted && target == withdrawal.StateCompleted:
			step = withdrawal.Complete(ctx, tx, id, spec.ProviderID, spec.ProviderTxID)
		case wr.State == withdrawal.StateSubmitted && target == withdrawal.StateFailed:
			step = withdrawal.Fail(ctx, tx, id, "wdpath_fixture_failed")
		default:
			return fmt.Errorf("wdpath: no legal path from %s to %s", wr.State, target)
		}
		if step != nil {
			return fmt.Errorf("wdpath: %s -> %s: %w", wr.State, target, step)
		}
	}
	return fmt.Errorf("wdpath: did not reach %s", target)
}

// approveE3 is the legal E3 edge as withdrawal.Approve's automated path writes it: an automated
// `approve` record (0034 admits an automated decision with no staff row), then the conditional UPDATE.
func approveE3(ctx context.Context, tx pgx.Tx, wr withdrawal.WithdrawalRequest) error {
	if _, err := tx.Exec(ctx,
		`INSERT INTO withdrawal_approvals
			(tenant_id, withdrawal_request_id, approver_principal_id, is_automated_approval, decision,
			 threshold_amount_at_decision, request_amount_at_decision)
		 VALUES ($1, $2, $3, true, 'approve', $4, $4)`,
		wr.TenantID, wr.ID, uuid.New(), wr.Amount); err != nil {
		return fmt.Errorf("approve record: %w", err)
	}
	tag, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'approved', updated_at = now() WHERE id = $1 AND state = 'pending_review'`, wr.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return withdrawal.ErrStateConflict
	}
	return nil
}

// rejectE4 is the legal E4 edge as withdrawal.Reject writes it (automated record instead of a
// person-linked staff approver): the `reject` record, the `:rejected` reversal leg, the UPDATE.
func rejectE4(ctx context.Context, tx pgx.Tx, wr withdrawal.WithdrawalRequest) error {
	if _, err := tx.Exec(ctx,
		`INSERT INTO withdrawal_approvals
			(tenant_id, withdrawal_request_id, approver_principal_id, is_automated_approval, decision, reason_code,
			 threshold_amount_at_decision, request_amount_at_decision)
		 VALUES ($1, $2, $3, true, 'reject', 'wdpath_fixture_rejected', $4, $4)`,
		wr.TenantID, wr.ID, uuid.New(), wr.Amount); err != nil {
		return fmt.Errorf("reject record: %w", err)
	}
	accounts, err := ledger.GetOrCreateAccounts(ctx, tx, wr.TenantID,
		ledger.AccountSpec{WalletID: &wr.WalletID, AccountType: ledger.AccountPlayerWithdrawalHold, AssetCode: wr.AssetCode},
		ledger.AccountSpec{WalletID: &wr.WalletID, AccountType: ledger.AccountPlayerCash, AssetCode: wr.AssetCode},
	)
	if err != nil {
		return err
	}
	post, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID: wr.TenantID, TransactionType: ledger.TxWithdrawalRejected,
		IdempotencyKey: wr.ID.String() + ":rejected", CorrelationID: wr.ID, ReversesTransactionID: wr.HoldLedgerTransactionID,
		Entries: []ledger.EntryInput{
			{LedgerAccountID: accounts[0], Direction: ledger.Debit, Amount: wr.Amount},
			{LedgerAccountID: accounts[1], Direction: ledger.Credit, Amount: wr.Amount},
		},
	})
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'rejected', release_ledger_transaction_id = $2, updated_at = now()
		WHERE id = $1 AND state = 'pending_review'`, wr.ID, post.TransactionID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return withdrawal.ErrStateConflict
	}
	return nil
}

// Seed creates the request and walks it to target in ONE tenant transaction on the RUNTIME role of the
// database p is connected to.
func Seed(ctx context.Context, tb testing.TB, p *db.Pool, spec Spec, target withdrawal.State) Result {
	tb.Helper()
	spec.defaults()
	var res Result
	if err := RuntimeFor(tb, p).WithTenant(ctx, spec.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if res, err = Plant(ctx, tx, spec); err != nil {
			return err
		}
		return Advance(ctx, tx, res.ID, target, spec)
	}); err != nil {
		tb.Fatalf("wdpath: seed %s request: %v", target, err)
	}
	res.State = target
	return res
}

// AdvanceAsRuntime walks an existing request to target in one tenant transaction on the runtime role.
func AdvanceAsRuntime(ctx context.Context, tb testing.TB, p *db.Pool, tenantID, id uuid.UUID, target withdrawal.State, spec Spec) {
	tb.Helper()
	if err := RuntimeFor(tb, p).WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return Advance(ctx, tx, id, target, spec)
	}); err != nil {
		tb.Fatalf("wdpath: advance %s to %s: %v", id, target, err)
	}
}

// poolIdentity is (server address, server port, database, current user) as the SERVER reports them for a
// connection of p; (*db.Pool).Raw is reserved to internal/db (raw_guard_test), so it is read by query.
func poolIdentity(tb testing.TB, p *db.Pool) (addr string, port int, dbName, user string) {
	tb.Helper()
	if err := p.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(host(inet_server_addr()), 'local'), coalesce(inet_server_port(), 0), current_database()::text, current_user::text`).
			Scan(&addr, &port, &dbName, &user)
	}); err != nil {
		tb.Fatalf("wdpath: inspect pool: %v", err)
	}
	return addr, port, dbName, user
}

// sharedTestDB reports whether p is connected to the SHARED test database: the same server address,
// port and database name as a connection opened from TEST_DATABASE_URL (both reported by the server, so
// "localhost" and "127.0.0.1" compare correctly). It FAILS the test (closed) when TEST_DATABASE_URL is
// unset or unreachable: without it no pool can be classified, and a guess could run fixtures on the
// wrong role.
func sharedTestDB(tb testing.TB, p *db.Pool) bool {
	tb.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		tb.Fatal("wdpath: TEST_DATABASE_URL is required to classify the pool (shared test database or scratch)")
	}
	ctx := context.Background()
	c, err := pgx.Connect(ctx, raw)
	if err != nil {
		tb.Fatalf("wdpath: connect TEST_DATABASE_URL: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()
	var sAddr, sDB string
	var sPort int
	if err := c.QueryRow(ctx, `SELECT coalesce(host(inet_server_addr()), 'local'), coalesce(inet_server_port(), 0), current_database()::text`).
		Scan(&sAddr, &sPort, &sDB); err != nil {
		tb.Fatalf("wdpath: inspect TEST_DATABASE_URL: %v", err)
	}
	addr, port, dbName, _ := poolIdentity(tb, p)
	return addr == sAddr && port == sPort && dbName == sDB
}

// AdvanceOnScratch walks an existing request to target on p's OWN role. Only for a throwaway
// scratchdb database, where the runtime role is granted nothing (scratchdb creates the database with
// no default privileges for it): the database rules exercised are role-independent (no role is exempt
// from the 0132 guards), but the caller must say so explicitly. It refuses the shared test database.
func AdvanceOnScratch(ctx context.Context, tb testing.TB, p *db.Pool, tenantID, id uuid.UUID, target withdrawal.State, spec Spec) {
	tb.Helper()
	if sharedTestDB(tb, p) {
		tb.Fatal("wdpath.AdvanceOnScratch on the shared test database: use AdvanceAsRuntime")
	}
	if err := p.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return Advance(ctx, tx, id, target, spec)
	}); err != nil {
		tb.Fatalf("wdpath: advance %s to %s (scratch): %v", id, target, err)
	}
}

// AdvanceForFixture is AdvanceAsRuntime on the shared test database and AdvanceOnScratch on a
// throwaway scratch database (where the runtime role has no grants), for fixture helpers whose caller
// may hold either kind of pool.
func AdvanceForFixture(ctx context.Context, tb testing.TB, p *db.Pool, tenantID, id uuid.UUID, target withdrawal.State, spec Spec) {
	tb.Helper()
	if sharedTestDB(tb, p) {
		AdvanceAsRuntime(ctx, tb, p, tenantID, id, target, spec)
		return
	}
	AdvanceOnScratch(ctx, tb, p, tenantID, id, target, spec)
}

// RuntimeFor returns a pool on the RUNTIME role (TEST_RUNTIME_DATABASE_URL's credentials) for the
// SHARED test database p is connected to; p itself if it already is the runtime role. A runtime pool
// on a scratch database is refused (no grants there, and its connections would block the scratch
// cleanup). The pool is opened for the calling test and closed by its cleanup.
func RuntimeFor(tb testing.TB, p *db.Pool) *db.Pool {
	tb.Helper()
	base := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if base == "" {
		tb.Fatal("wdpath: TEST_RUNTIME_DATABASE_URL is required (the legal-path helper runs as igaming_runtime)")
	}
	u, err := url.Parse(base)
	if err != nil {
		tb.Fatalf("wdpath: parse TEST_RUNTIME_DATABASE_URL: %v", err)
	}
	_, _, dbName, user := poolIdentity(tb, p)
	if user == u.User.Username() {
		return p
	}
	if !sharedTestDB(tb, p) {
		tb.Fatalf("wdpath.RuntimeFor on %q, which is not the shared test database: use AdvanceForFixture / AdvanceOnScratch", dbName)
	}
	u.Path = "/" + dbName
	rp, err := db.Connect(context.Background(), u.String(), 4, 5*time.Second)
	if err != nil {
		tb.Fatalf("wdpath: connect runtime pool: %v", err)
	}
	tb.Cleanup(rp.Close)
	return rp
}

// PersistentDatabases are the long-lived databases of this repository's tooling (Makefile /
// docker-compose dev database; the local shared CI database of the agent harness). Fixtures that leave
// PERMANENT invariant-violating rows (owner tamper plants of history the database now refuses: the
// ledger is append-only, so such rows can never be cleaned up) must not run there.
// ADR0113_PERSISTENT_DBS (comma-separated) extends the list.
var PersistentDatabases = []string{"igaming_platform_dev", "igaming_platform_ci_local"}

// CIDatabases are the CI service databases (.github/workflows/ci.yml, ci-selfhosted.yml). The
// tamper-plant tests are the only evidence for several money-path controls (ADR 0113 slices 1-3: LA025
// stranded holds, check D orphan legs and relabels), so they must never be skipped there: a skip
// decision on a CI database (by name, or any database while CI=true) FAILS the test instead
// (ledger-finance review C2), so a CI run cannot go green with that evidence silently missing.
var CIDatabases = []string{"igaming_platform_test"}

// persistentDecision is OnPersistentDatabase's pure decision: skip on a persistent database, with a
// non-empty failure reason when that database is (or the run is) CI.
func persistentDecision(name, extra string, ci bool) (skip bool, fail string) {
	list := append([]string(nil), PersistentDatabases...)
	for _, n := range strings.Split(extra, ",") {
		if n = strings.TrimSpace(n); n != "" {
			list = append(list, n)
		}
	}
	for _, n := range list {
		if n == name {
			skip = true
		}
	}
	if !skip {
		return false, ""
	}
	for _, n := range CIDatabases {
		if n == name {
			return true, "database " + name + " is a CI database but is listed as persistent: the ADR 0113 tamper-plant evidence would be skipped"
		}
	}
	if ci {
		return true, "CI=true but database " + name + " is listed as persistent: the ADR 0113 tamper-plant evidence would be skipped in CI"
	}
	return true, ""
}

// OnPersistentDatabase reports whether p is connected to one of PersistentDatabases (or
// ADR0113_PERSISTENT_DBS). It FAILS the test when the answer would skip tamper-plant evidence in CI.
func OnPersistentDatabase(tb testing.TB, p *db.Pool) bool {
	tb.Helper()
	_, _, name, _ := poolIdentity(tb, p)
	skip, fail := persistentDecision(name, os.Getenv("ADR0113_PERSISTENT_DBS"), os.Getenv("CI") == "true")
	if fail != "" {
		tb.Fatalf("wdpath: %s", fail)
	}
	return skip
}

// SkipOnPersistentDatabase skips a test whose fixture leaves permanent tamper rows when p is a
// persistent shared database (run it on a private or CI service database).
func SkipOnPersistentDatabase(tb testing.TB, p *db.Pool) {
	tb.Helper()
	if OnPersistentDatabase(tb, p) {
		tb.Skip("owner tamper plant leaves permanent append-only rows: not run on a persistent database (ADR 0113 slice 1 review)")
	}
}
