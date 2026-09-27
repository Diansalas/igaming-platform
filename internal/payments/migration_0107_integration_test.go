//go:build integration

// ADR 0095 §28.12: migration 0107 tests QA's test-first matrix
// (inv_dep1_matrix_integration_test.go) could not write, because they
// require migration-boundary control (a scratch DB pinned to a specific
// version, an up/down/up round trip, and pre-flight-refusal fixtures) that
// only this package's existing migration-test harness
// (migration_0101_integration_test.go's pattern) provides.
package payments

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

const migration0107Filename = "0107_deposit_intent_double_credit_backstop"

// migration0107Version mirrors migration0101Version's own derivation
// technique - read the real on-disk filename rather than hard-coding the
// integer literal, so a rename or renumbering fails loudly here instead of
// silently testing the wrong file.
func migration0107Version(t *testing.T) int64 {
	t.Helper()
	dir := realMigrationsDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), migration0107Filename+".up.sql") {
			return 107
		}
	}
	t.Fatalf("no %s.up.sql found under %s", migration0107Filename, dir)
	return 0
}

// --- up/down/up round trip on a CLEAN scratch DB ---------------------------

func TestMigration0107_UpDownUpRoundTrip_CleanDB(t *testing.T) {
	v := migration0107Version(t)
	pool, dir := migration0101Scratch(t, "m0107rt_", v)

	assertIndexExists := func(want bool, label string) {
		var n int
		if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE indexname IN
				('payment_attempts_one_succeeded_deposit_per_intent','ledger_transactions_one_deposit_per_intent')`).Scan(&n)
		}); err != nil {
			t.Fatalf("%s: query indexes: %v", label, err)
		}
		if want && n != 2 {
			t.Fatalf("%s: expected both 0107 indexes present, found %d", label, n)
		}
		if !want && n != 0 {
			t.Fatalf("%s: expected both 0107 indexes absent, found %d", label, n)
		}
	}
	assertIndexExists(true, "after up")

	down, err := pool.MigrateDown(context.Background(), dir, 1)
	if err != nil || len(down) != 1 || down[0] != v {
		t.Fatalf("down must roll back exactly %d: %v %v", v, down, err)
	}
	assertIndexExists(false, "after down")

	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("re-up after down: %v", err)
	}
	assertIndexExists(true, "after re-up")
}

// --- up on EXISTING VALID data (one succeeded attempt, one deposit
// posting per intent) must succeed -----------------------------------------

func TestMigration0107_Up_ExistingValidData_Succeeds(t *testing.T) {
	v := migration0107Version(t)
	pool, dir := migration0101ScratchApplyThroughPrev(t, "m0107valid_", v)
	f := seedM0101Fixture(t, pool)

	intentID := uuid.New()
	attemptID := uuid.New()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO deposit_intents (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, payment_method, status, idempotency_key)
			VALUES ($1,$2,$3,$4,$5,'EUR',5000,'card','succeeded','m0107valid')`,
			intentID, f.tenantID, f.brandID, f.playerID, f.walletID); err != nil {
			return err
		}
		accounts, err := ledger.GetOrCreateAccounts(ctx, tx, f.tenantID,
			ledger.AccountSpec{WalletID: &f.walletID, AccountType: ledger.AccountPlayerCash, AssetCode: "EUR"},
			ledger.AccountSpec{AccountType: ledger.AccountPSPClearing, AssetCode: "EUR"},
		)
		if err != nil {
			return err
		}
		res, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxDeposit, IdempotencyKey: "m0107valid-provider:ref",
			ProviderID: strPtr("m0107valid-provider"), ProviderTxID: strPtr("ref"), CorrelationID: intentID,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: accounts[1], Direction: ledger.Debit, Amount: 5000},
				{LedgerAccountID: accounts[0], Direction: ledger.Credit, Amount: 5000},
			},
		})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE deposit_intents SET ledger_transaction_id = $1 WHERE id = $2`, res.TransactionID, intentID); err != nil {
			return err
		}
		// The guard trigger (0101) only allows an INSERT in state
		// 'created' or 'submitting' - insert submitting, then a SEPARATE
		// UPDATE performs the T7 transition to 'succeeded' (the trigger's
		// own allowed state-pair), exactly as the real application code
		// path does in two steps (T1p commit, then T7).
		if _, err := tx.Exec(ctx, `INSERT INTO payment_attempts (id, tenant_id, operation, deposit_intent_id, attempt_no, payment_method, asset_code, amount,
			state, last_evidence_kind, ever_possibly_sent, interactive, provider_id, merchant_reference, external_idempotency_key)
			VALUES ($1,$2,'deposit',$3,1,'card','EUR',5000,'submitting','platform',false,false,'m0107valid-provider',$4,$4)`,
			attemptID, f.tenantID, intentID, attemptID.String()); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE payment_attempts SET state = 'succeeded', last_evidence_kind = 'callback',
			ever_possibly_sent = true, provider_id = 'm0107valid-provider', provider_reference = 'ref',
			ledger_transaction_id = $1, resolved_at = now(), next_action_at = NULL WHERE id = $2`,
			res.TransactionID, attemptID)
		return err
	}); err != nil {
		t.Fatalf("seed pre-0107 valid data: %v", err)
	}

	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migration 0107 must succeed on valid (non-duplicate) existing data: %v", err)
	}
}

// --- up on SEEDED DUPLICATE succeeded-attempt data: refused by the
// attempts pre-flight --------------------------------------------------

func TestMigration0107_Up_DuplicateSucceededAttempts_RefusedByAttemptsPreflight(t *testing.T) {
	v := migration0107Version(t)
	pool, dir := migration0101ScratchApplyThroughPrev(t, "m0107dupattempt_", v)
	f := seedM0101Fixture(t, pool)

	intentID := uuid.New()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO deposit_intents (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, payment_method, status, idempotency_key)
			VALUES ($1,$2,$3,$4,$5,'EUR',5000,'card','succeeded','m0107dupattempt')`,
			intentID, f.tenantID, f.brandID, f.playerID, f.walletID); err != nil {
			return err
		}
		// Two succeeded attempts, each linked to its OWN, independent
		// ledger posting (payment_attempts_check8 requires a deposit
		// attempt in 'succeeded' to carry a ledger_transaction_id) - the
		// pre-flight under test here is the ATTEMPTS index
		// (payment_attempts_one_succeeded_deposit_per_intent), which does
		// not care whether the two postings also share a correlation_id
		// (that is the OTHER pre-flight, tested separately below). Insert
		// submitting (the only guard-allowed INSERT states), then a
		// separate UPDATE performs the T7 transition, exactly like the
		// valid-data test above.
		accounts, err := ledger.GetOrCreateAccounts(ctx, tx, f.tenantID,
			ledger.AccountSpec{WalletID: &f.walletID, AccountType: ledger.AccountPlayerCash, AssetCode: "EUR"},
			ledger.AccountSpec{AccountType: ledger.AccountPSPClearing, AssetCode: "EUR"},
		)
		if err != nil {
			return err
		}
		for i, ref := range []string{"dup-ref-1", "dup-ref-2"} {
			id := uuid.New()
			if _, err := tx.Exec(ctx, `INSERT INTO payment_attempts (id, tenant_id, operation, deposit_intent_id, attempt_no, payment_method, asset_code, amount,
				state, last_evidence_kind, ever_possibly_sent, interactive, provider_id, merchant_reference, external_idempotency_key)
				VALUES ($1,$2,'deposit',$3,$4,'card','EUR',5000,'submitting','platform',false,false,'m0107dupattempt-provider',$5,$5)`,
				id, f.tenantID, intentID, i+1, id.String()); err != nil {
				return err
			}
			res, err := ledger.Post(ctx, tx, ledger.TransactionInput{
				TenantID: f.tenantID, TransactionType: ledger.TxDeposit, IdempotencyKey: "m0107dupattempt-provider:" + ref,
				ProviderID: strPtr("m0107dupattempt-provider"), ProviderTxID: strPtr(ref), CorrelationID: uuid.New(),
				Entries: []ledger.EntryInput{
					{LedgerAccountID: accounts[1], Direction: ledger.Debit, Amount: 5000},
					{LedgerAccountID: accounts[0], Direction: ledger.Credit, Amount: 5000},
				},
			})
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE payment_attempts SET state = 'succeeded', last_evidence_kind = 'callback',
				ever_possibly_sent = true, provider_id = 'm0107dupattempt-provider', provider_reference = $1,
				ledger_transaction_id = $2, resolved_at = now(), next_action_at = NULL WHERE id = $3`, ref, res.TransactionID, id); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed duplicate succeeded attempts: %v", err)
	}

	_, err := pool.MigrateUp(context.Background(), dir)
	if err == nil {
		t.Fatal("expected migration 0107 to refuse on two succeeded deposit attempts for one intent")
	}
	if !isCheckOrTriggerViolation(err) {
		t.Fatalf("expected a unique-violation-class refusal, got: %v", err)
	}
}

// --- up on SEEDED DUPLICATE ledger postings (same correlation_id): refused
// by the ledger pre-flight -----------------------------------------------

func TestMigration0107_Up_DuplicateLedgerPostings_RefusedByLedgerPreflight(t *testing.T) {
	v := migration0107Version(t)
	pool, dir := migration0101ScratchApplyThroughPrev(t, "m0107dupledger_", v)
	f := seedM0101Fixture(t, pool)

	intentID := uuid.New()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO deposit_intents (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, payment_method, status, idempotency_key)
			VALUES ($1,$2,$3,$4,$5,'EUR',5000,'card','succeeded','m0107dupledger')`,
			intentID, f.tenantID, f.brandID, f.playerID, f.walletID); err != nil {
			return err
		}
		accounts, err := ledger.GetOrCreateAccounts(ctx, tx, f.tenantID,
			ledger.AccountSpec{WalletID: &f.walletID, AccountType: ledger.AccountPlayerCash, AssetCode: "EUR"},
			ledger.AccountSpec{AccountType: ledger.AccountPSPClearing, AssetCode: "EUR"},
		)
		if err != nil {
			return err
		}
		// Two DISTINCT deposit postings sharing ONE correlation_id -
		// exactly the legacy "second capture posts" shape §28 makes
		// structurally unreachable going forward. Posted via ledger.Post
		// itself (pre-0107, so its own conflict path has no backstop
		// index to check yet - this must succeed on THIS scratch DB).
		for _, ref := range []string{"dup-ledger-1", "dup-ledger-2"} {
			if _, err := ledger.Post(ctx, tx, ledger.TransactionInput{
				TenantID: f.tenantID, TransactionType: ledger.TxDeposit, IdempotencyKey: "m0107dupledger-provider:" + ref,
				ProviderID: strPtr("m0107dupledger-provider"), ProviderTxID: strPtr(ref), CorrelationID: intentID,
				Entries: []ledger.EntryInput{
					{LedgerAccountID: accounts[1], Direction: ledger.Debit, Amount: 5000},
					{LedgerAccountID: accounts[0], Direction: ledger.Credit, Amount: 5000},
				},
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed duplicate ledger postings (pre-0107, must succeed): %v", err)
	}

	_, err := pool.MigrateUp(context.Background(), dir)
	if err == nil {
		t.Fatal("expected migration 0107 to refuse on two deposit ledger postings sharing one correlation_id")
	}
	if !isCheckOrTriggerViolation(err) {
		t.Fatalf("expected a unique-violation-class refusal, got: %v", err)
	}
}

// --- ledger.ErrDepositAlreadyPostedForIntent: the backstop sentinel,
// exercised directly against Post on a HEAD-migrated DB --------------------

func TestMigration0107_LedgerBackstop_ErrDepositAlreadyPostedForIntent(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	intentID := uuid.New()

	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		accounts, err := ledger.GetOrCreateAccounts(ctx, tx, f.tenantID,
			ledger.AccountSpec{WalletID: &f.walletID, AccountType: ledger.AccountPlayerCash, AssetCode: "EUR"},
			ledger.AccountSpec{AccountType: ledger.AccountPSPClearing, AssetCode: "EUR"},
		)
		if err != nil {
			return err
		}
		if _, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxDeposit, IdempotencyKey: "m0107backstop-provider:ref-1",
			ProviderID: strPtr("m0107backstop-provider"), ProviderTxID: strPtr("ref-1"), CorrelationID: intentID,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: accounts[1], Direction: ledger.Debit, Amount: 5000},
				{LedgerAccountID: accounts[0], Direction: ledger.Credit, Amount: 5000},
			},
		}); err != nil {
			return err
		}
		// A SECOND, DISTINCT reference (different idempotency key), same
		// correlation_id - the choke point in application code would have
		// refused this before ever reaching here (§28.3); calling
		// ledger.Post directly, bypassing it, exercises the DB backstop
		// itself and its typed sentinel (§28.8).
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxDeposit, IdempotencyKey: "m0107backstop-provider:ref-2",
			ProviderID: strPtr("m0107backstop-provider"), ProviderTxID: strPtr("ref-2"), CorrelationID: intentID,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: accounts[1], Direction: ledger.Debit, Amount: 5000},
				{LedgerAccountID: accounts[0], Direction: ledger.Credit, Amount: 5000},
			},
		})
		return err
	}); err == nil || !errors.Is(err, ledger.ErrDepositAlreadyPostedForIntent) {
		t.Fatalf("expected ledger.ErrDepositAlreadyPostedForIntent, got %v", err)
	}
}

func strPtr(s string) *string { return &s }

// TestMigration0107_T13tT13d_TerminalReasonTrigger_HEAD closes security
// review F-M1/F-M2 (rv-fh3-security.md, 81dd4b7): migration 0107's
// CREATE OR REPLACE of payment_attempts_guard() changed the T13t/T13d
// terminal_reason check from `IS DISTINCT FROM` (0101, NULL-safe) to
// `NOT IN (...)` (originally NULL-unsafe: `NULL NOT IN (...)` is NULL,
// never TRUE, so the IF never raised for a NULL terminal_reason - F-M1).
// This is the required HEAD trigger test with all three cases, run
// directly against the raw SQL trigger (not through any application-code
// helper, since ApplyMultipleSuccessForIntent/ApplyTombstonePrecedesSuccess
// always set a real reason - this test is the guard's OWN defense in
// depth, independent of what application code currently does):
//
//	(a) terminal_reason='multiple_success_for_intent' (T13d) is accepted;
//	(b) terminal_reason='some_other_reason' is refused;
//	(c) terminal_reason left NULL is refused - the exact F-M1 regression;
//	    this case FAILS against the pre-fix 0107 body (confirmed: reverting
//	    the fix locally and rerunning this test reproduces "expected the
//	    trigger to refuse a NULL terminal_reason, got state=disputed
//	    reason=<nil>").
func TestMigration0107_T13tT13d_TerminalReasonTrigger_HEAD(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedM0101Fixture(t, pool)

	newDeclinedAttempt := func(ref string) uuid.UUID {
		intentID := insertDepositIntent(t, pool, f, "declined", ptr("mock-psp"), ptr(ref), nil)
		id := uuid.New()
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO payment_attempts (id, tenant_id, operation, deposit_intent_id, attempt_no, provider_id, payment_method, asset_code, amount, interactive, merchant_reference, external_idempotency_key, state, last_evidence_kind)
				 VALUES ($1,$2,'deposit',$3,1,'mock-psp','card','EUR',1000,true,$4,$5,'submitting','platform')`,
				id, f.tenantID, intentID, id.String(), "pa:"+id.String())
			return err
		}); err != nil {
			t.Fatalf("seed submitting attempt: %v", err)
		}
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE payment_attempts SET state='declined', last_evidence_kind='sync', decline_stage='after_acceptance', cascadable=false, provider_reference=$2, resolved_at=now() WHERE id=$1`, id, ref)
			return err
		}); err != nil {
			t.Fatalf("submitting->declined: %v", err)
		}
		return id
	}

	toDisputed := func(id uuid.UUID, reason *string) error {
		return pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE payment_attempts SET state='disputed', last_evidence_kind='callback', terminal_reason=$2 WHERE id=$1`, id, reason)
			return err
		})
	}

	// (a) T13d: multiple_success_for_intent is accepted.
	accepted := newDeclinedAttempt("t13d-head-accepted-ref")
	if err := toDisputed(accepted, strPtr(TerminalReasonMultipleSuccessForIntent)); err != nil {
		t.Fatalf("(a) T13d with terminal_reason=%s must be accepted, got %v", TerminalReasonMultipleSuccessForIntent, err)
	}
	var state string
	var reason *string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state, terminal_reason FROM payment_attempts WHERE id=$1`, accepted).Scan(&state, &reason)
	}); err != nil || state != "disputed" || reason == nil || *reason != TerminalReasonMultipleSuccessForIntent {
		t.Fatalf("(a) expected disputed/%s, got %s/%v (err=%v)", TerminalReasonMultipleSuccessForIntent, state, reason, err)
	}

	// (b) another (unrecognized) reason is refused.
	otherReason := newDeclinedAttempt("t13d-head-other-ref")
	if err := toDisputed(otherReason, strPtr("some_other_reason")); !isCheckOrTriggerViolation(err) {
		t.Fatalf("(b) expected the trigger to refuse an unrecognized terminal_reason, got %v", err)
	}

	// (c) F-M1: a NULL terminal_reason is refused. This is the case that
	// FAILS against the pre-fix 0107 body (NULL NOT IN (...) is NULL, not
	// TRUE, so the old code's IF never raised).
	nullReason := newDeclinedAttempt("t13d-head-null-ref")
	if err := toDisputed(nullReason, nil); !isCheckOrTriggerViolation(err) {
		var gotState string
		var gotReason *string
		_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT state, terminal_reason FROM payment_attempts WHERE id=$1`, nullReason).Scan(&gotState, &gotReason)
		})
		t.Fatalf("(c) F-M1: expected the trigger to refuse a NULL terminal_reason, got err=%v state=%s reason=%v", err, gotState, gotReason)
	}
}

// migration0101ScratchApplyThroughPrev migrates a fresh scratch database to
// everything through migration (through-1) - i.e. right before the
// migration under test - and returns a migrations DIR that includes
// `through` itself, so the caller's LATER `pool.MigrateUp(ctx, dir)` call
// (after seeding pre-migration data) actually reaches the migration under
// test, rather than re-running a now-no-op up-to-(through-1).
func migration0101ScratchApplyThroughPrev(t *testing.T, prefix string, through int64) (pool *db.Pool, dir string) {
	t.Helper()
	url := scratchdb.New(t, prefix)
	p, err := db.Connect(context.Background(), url, 10, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	t.Cleanup(p.Close)
	prevDir := migration0101Dir(t, through-1)
	if _, err := p.MigrateUp(context.Background(), prevDir); err != nil {
		t.Fatalf("migrate scratch up through %d: %v", through-1, err)
	}
	return p, migration0101Dir(t, through)
}

// TestINVDEP1_Mutation1_ResolvedForOtherDepositPredicate is a direct
// truth-table test of resolvedForOtherDeposit itself (ADR 0095 §28.2) -
// the QA mutation checklist's items 1/2/3 target (the SAME predicate is
// called from every T7/T13 evidence-application site AND from
// postDepositSuccess's own internal re-check). Written separately from
// the higher-level INV-DEP-1 scenarios because, by design, the ledger's
// OWN backstop index (migration 0107) independently catches every case
// those scenarios can construct - deleting this predicate does not
// observably change TestINVDEP1_C/H/J/Inverted_T13's PASS/FAIL outcome
// (confirmed: with resolvedForOtherDeposit mutated to `return false, nil`,
// all four still pass, because postDepositSuccess's own ledger-backstop
// mapping catches the resulting ErrDepositAlreadyPostedForIntent and
// produces the identical disputed=true outcome - real, working defense in
// depth, disclosed rather than silently treated as a successful mutation
// kill it is not). This test instead pins the PREDICATE's own logic
// directly, so mutating it IS independently detectable.
func TestINVDEP1_Mutation1_ResolvedForOtherDepositPredicate(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)

	var intentID, attemptA uuid.UUID
	var postedTxID uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		intentID = uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO deposit_intents (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, payment_method, status, idempotency_key)
			VALUES ($1,$2,$3,$4,$5,'EUR',5000,'card','pending','m1-predicate')`,
			intentID, f.tenantID, f.brandID, f.playerAccountID, f.walletID); err != nil {
			return err
		}
		attemptA = uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO payment_attempts (id, tenant_id, operation, deposit_intent_id, attempt_no, payment_method, asset_code, amount,
			state, last_evidence_kind, ever_possibly_sent, interactive, provider_id, merchant_reference, external_idempotency_key)
			VALUES ($1,$2,'deposit',$3,1,'card','EUR',5000,'submitting','platform',false,false,'m1-predicate-provider',$4,$4)`,
			attemptA, f.tenantID, intentID, attemptA.String()); err != nil {
			return err
		}

		// (1) Nothing has succeeded yet: never resolved-for-other, for
		// either attempt id or the legacy nil.
		resolved, err := resolvedForOtherDeposit(ctx, tx, f.tenantID, intentID, &attemptA, "m1-predicate-provider:refA2")
		if err != nil {
			return err
		}
		if resolved {
			t.Error("mutation-1: expected false before any success exists")
		}
		resolved, err = resolvedForOtherDeposit(ctx, tx, f.tenantID, intentID, nil, "m1-predicate-provider:refA2")
		if err != nil {
			return err
		}
		if resolved {
			t.Error("mutation-1: expected false (legacy/nil attempt id) before any success exists")
		}

		// (2) attemptA succeeds via a REAL ledger posting.
		accounts, err := ledger.GetOrCreateAccounts(ctx, tx, f.tenantID,
			ledger.AccountSpec{WalletID: &f.walletID, AccountType: ledger.AccountPlayerCash, AssetCode: "EUR"},
			ledger.AccountSpec{AccountType: ledger.AccountPSPClearing, AssetCode: "EUR"},
		)
		if err != nil {
			return err
		}
		res, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxDeposit, IdempotencyKey: "m1-predicate-provider:refA2",
			ProviderID: strPtr("m1-predicate-provider"), ProviderTxID: strPtr("refA2"), CorrelationID: intentID,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: accounts[1], Direction: ledger.Debit, Amount: 5000},
				{LedgerAccountID: accounts[0], Direction: ledger.Credit, Amount: 5000},
			},
		})
		if err != nil {
			return err
		}
		postedTxID = res.TransactionID
		_, err = tx.Exec(ctx, `UPDATE payment_attempts SET state = 'succeeded', last_evidence_kind = 'callback',
			ever_possibly_sent = true, provider_reference = 'refA2', ledger_transaction_id = $1,
			resolved_at = now(), next_action_at = NULL WHERE id = $2`, postedTxID, attemptA)
		return err
	}); err != nil {
		t.Fatalf("seed real posting: %v", err)
	}

	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// (3) Exact redelivery: same attempt, same key -> NOT resolved-for-other.
		resolved, err := resolvedForOtherDeposit(ctx, tx, f.tenantID, intentID, &attemptA, "m1-predicate-provider:refA2")
		if err != nil {
			return err
		}
		if resolved {
			t.Error("mutation-1: exact redelivery (same attempt, same key) must NOT be resolved_for_other")
		}
		// (4) A DIFFERENT attempt id (even sharing the SAME already-posted
		// key, an impossible-in-practice combination used here only to
		// isolate the ATTEMPTS clause): resolved, because the attempts
		// EXISTS clause alone already sees attemptA as another succeeded
		// sibling ("id IS DISTINCT FROM otherID" is true) - it does not
		// matter what key is passed once the caller's own id differs from
		// the succeeded one.
		otherID := uuid.New()
		resolved, err = resolvedForOtherDeposit(ctx, tx, f.tenantID, intentID, &otherID, "m1-predicate-provider:refA2")
		if err != nil {
			return err
		}
		if !resolved {
			t.Error("mutation-1: a different attempt id must see attemptA as another succeeded sibling regardless of key")
		}
		// (5) A DIFFERENT attempt AND a different key: resolved_for_other
		// (the real T10/T13d case).
		resolved, err = resolvedForOtherDeposit(ctx, tx, f.tenantID, intentID, &otherID, "m1-predicate-provider:refB-different")
		if err != nil {
			return err
		}
		if !resolved {
			t.Error("mutation-1: a different attempt id AND a different key must BE resolved_for_other")
		}
		// (6) Legacy path (nil attempt id): the succeeded sibling counts
		// regardless of key.
		resolved, err = resolvedForOtherDeposit(ctx, tx, f.tenantID, intentID, nil, "m1-predicate-provider:refA2")
		if err != nil {
			return err
		}
		if !resolved {
			t.Error("mutation-1: legacy path (nil attempt id) must count the existing succeeded sibling even under its own key")
		}
		return nil
	}); err != nil {
		t.Fatalf("predicate checks: %v", err)
	}
}

// TestINVDEP1_Mutation6_TombstonePrecedesMultipleSuccessForIntent pins the
// QA mutation checklist's item 6 (§28.3 rule 1's check order): a late
// success on a DECLINED attempt whose (provider_id, provider_reference) a
// reversal has ALREADY tombstoned must take the TOMBSTONE branch (T13t),
// never multiple_success_for_intent (T13d), even when the intent is ALSO
// already resolved by a sibling - the tombstone check runs strictly
// BEFORE the resolved_for_other check (a tombstone means the PSP reversed
// that specific capture, which nets to zero and must never be reported as
// a standing pay_captured_unposted exposure).
func TestINVDEP1_Mutation6_TombstonePrecedesMultipleSuccessForIntent(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	pa := NewMockProvider("m6-a", "EUR")
	pb := NewMockProvider("m6-b", "EUR")
	pb.AcceptAllAmounts = true
	registerCapability(t, pool, f, pa, 100)
	registerCapability(t, pool, f, pb, 200)
	orch := NewOrchestrator(map[string]PaymentProvider{"m6-a": pa, "m6-b": pb},
		MultiWebhookCredentialResolver{"m6-a": NewMockWebhookCredentials(pa), "m6-b": NewMockWebhookCredentials(pb)})

	res := rvInit(t, pool, orch, f, 5000, "m6")
	ref := *res.Attempt.ProviderReference
	childID := declineCascadableAndFindChild(t, pool, orch, f, "m6-a", pa, ref)
	child := dispatchViaSweeper(t, pool, orch, f, childID)
	childRef := *child.ProviderReference

	// The intent resolves via the FALLBACK (child) first.
	if _, err := rvCallback(pool, orch, f, "m6-b", pb.CallbackPayload(f.tenantID, CallbackEventDeposit, childRef, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("fallback success: %v", err)
	}
	// A reversal tombstones the ORIGINAL (declined, never-posted)
	// reference BEFORE its own late success arrives.
	if _, err := rvCallback(pool, orch, f, "m6-a", pa.CallbackPayload(f.tenantID, CallbackEventDepositReversal, "m6-rev", ref, OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("tombstone reversal: %v", err)
	}
	// The original's late success: BOTH conditions now hold (tombstoned
	// AND the intent is already multiple-success-resolved) - tombstone
	// must win.
	if _, err := rvCallback(pool, orch, f, "m6-a", pa.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("late original success: %v", err)
	}
	final := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	if final.State != AttemptDisputed || final.TerminalReason == nil || *final.TerminalReason != "reversal_tombstone_precedes_success" {
		t.Fatalf("mutation-6: expected disputed/reversal_tombstone_precedes_success (tombstone wins), got state=%s reason=%v", final.State, final.TerminalReason)
	}
	if b := cashBalance(t, pool, f); b != 5000 {
		t.Errorf("mutation-6: expected exactly the fallback's one credit, got balance=%d", b)
	}
}

// TestINVDEP1_O2_ReDriveOfDisputedAttempt_RealMultipleSuccessReason_NoPost
// is the "real-reason copy of scenario O" the coordinator asked for:
// QA's own TestINVDEP1_O built its disputed fixture via the
// tombstone-precedes-success (T13t) shape, which needs nothing from §28
// to construct - it does NOT exercise a genuinely §28-disputed attempt
// (terminal_reason='multiple_success_for_intent', T13d). This copy
// builds THAT real shape (a genuine second capture, disputed rather than
// posted) and asserts the identical §28.9 "T17 never state-changes a
// terminal attempt except declined" guarantee against it.
func TestINVDEP1_O2_ReDriveOfDisputedAttempt_RealMultipleSuccessReason_NoPost(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	pa := NewMockProvider("m-o2-a", "EUR")
	pb := NewMockProvider("m-o2-b", "EUR")
	pb.AcceptAllAmounts = true
	registerCapability(t, pool, f, pa, 100)
	registerCapability(t, pool, f, pb, 200)
	orch := NewOrchestrator(map[string]PaymentProvider{"m-o2-a": pa, "m-o2-b": pb},
		MultiWebhookCredentialResolver{"m-o2-a": NewMockWebhookCredentials(pa), "m-o2-b": NewMockWebhookCredentials(pb)})

	res := rvInit(t, pool, orch, f, 5000, "o2")
	ref := *res.Attempt.ProviderReference
	childID := declineCascadableAndFindChild(t, pool, orch, f, "m-o2-a", pa, ref)
	child := dispatchViaSweeper(t, pool, orch, f, childID)
	childRef := *child.ProviderReference

	// The fallback resolves the intent FIRST (the real, first capture).
	if _, err := rvCallback(pool, orch, f, "m-o2-b", pb.CallbackPayload(f.tenantID, CallbackEventDeposit, childRef, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("fallback success: %v", err)
	}
	// The original's LATE success (T13d): a genuine real §28
	// multiple_success_for_intent dispute, no tombstone involved at all.
	if _, err := rvCallback(pool, orch, f, "m-o2-a", pa.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("late original success (T13d): %v", err)
	}
	disputed := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	if disputed.State != AttemptDisputed || disputed.TerminalReason == nil || *disputed.TerminalReason != TerminalReasonMultipleSuccessForIntent {
		t.Fatalf("setup: expected disputed/multiple_success_for_intent, got state=%s reason=%v", disputed.State, disputed.TerminalReason)
	}
	if disputed.LedgerTransactionID != nil {
		t.Fatalf("setup: a T13d attempt must never carry a ledger link, got %s", *disputed.LedgerTransactionID)
	}

	// T17: a staff/operator re-verify request against this TERMINAL,
	// disputed (real multiple_success_for_intent) attempt must be a
	// no-op - identical guarantee to scenario O, now proven against the
	// REAL §28 disputed shape rather than the pre-existing tombstone one.
	touchErr := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return Touch(ctx, tx, disputed.ID)
	})
	if touchErr == nil {
		t.Errorf("§28.9: T17/Touch must never re-arm a terminal multiple_success_for_intent attempt for the sweeper")
	}
	if st := NewSweeper(pool, orch, AllowAllDepositKYCGate{}, MockCredentialResolver{}).RunOnce(context.Background(), []uuid.UUID{f.tenantID}); len(st.Errors) != 0 {
		t.Fatalf("sweeper run: %v", st.Errors)
	}
	after := mustGetAttempt(t, pool, f.tenantID, disputed.ID)
	if after.State != AttemptDisputed || after.LedgerTransactionID != nil {
		t.Fatalf("T17/re-drive must never post for a multiple_success_for_intent-disputed attempt: state=%s ledger=%v", after.State, after.LedgerTransactionID)
	}
	if b := cashBalance(t, pool, f); b != 5000 {
		t.Errorf("PAY-DOUBLE-CREDIT-1: only the fallback's single credit may survive, got balance=%d", b)
	}
}

// TestRVLF_C2_DFR_CallbackSiteDeferredApplyBackstop kills mutant DFR
// (ledger-finance re-review C2): ApplyReceiptEvidence's OWN deferred-apply
// backstop call (receipt.go, the callback/T4-T9 site - NOT phase C's,
// which TestRVLF_P8 already isolates and kills as DFD). Constructed with
// NO phase C dispatch involved at all: a bare 'created' attempt is
// claimed for submission directly, a first callback names a provider
// reference no attempt has yet (deferred, unresolved), and a SECOND,
// purely callback-driven delivery (resolved by merchant reference) learns
// that same reference via T4 - which must, in the SAME transaction,
// resolve and apply the first callback's deferred success.
func TestRVLF_C2_DFR_CallbackSiteDeferredApplyBackstop(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-c2dfr", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-c2dfr": p}, MultiWebhookCredentialResolver{"mock-c2dfr": NewMockWebhookCredentials(p)})

	res := rvInit(t, pool, orch, f, 5000, "c2dfr-seed")
	// payment_attempts_one_live_per_intent (INV-IO-8) allows only ONE live
	// (created/submitting/pending/ambiguous) attempt per intent - decline
	// the seed attempt first so the isolated attempt below can be
	// inserted 'created' for the same intent.
	if _, err := rvCallback(pool, orch, f, "mock-c2dfr", p.CallbackPayload(f.tenantID, CallbackEventDeposit, *res.Attempt.ProviderReference, "", OutcomeDeclined, 0, "", "insufficient_funds", false)); err != nil {
		t.Fatalf("decline seed attempt: %v", err)
	}
	var attemptID uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		a, err := InsertCreatedAttempt(ctx, tx, NewCreatedAttempt{
			ID: uuid.New(), TenantID: f.tenantID, Operation: AttemptOperationDeposit,
			DepositIntentID: &res.Intent.ID, AttemptNo: 2, ExcludedProviderIDs: []string{"mock-c2dfr"},
			PaymentMethod: "card", AssetCode: "EUR", Amount: 5000,
		})
		attemptID = a.ID
		return err
	}); err != nil {
		t.Fatalf("insert isolated attempt: %v", err)
	}
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, res.Intent.ID); err != nil {
			return err
		}
		return ClaimCreatedForSubmission(ctx, tx, attemptID, "mock-c2dfr", uuid.New(), "rv-c2dfr", time.Now().Add(time.Minute))
	}); err != nil {
		t.Fatalf("claim for submission: %v", err)
	}

	// Callback #1: names a provider reference NO attempt has yet (no
	// provider_reference, no merchant reference given) - deferred.
	if _, err := rvApplyReceipt(pool, orch, f.tenantID, "mock-c2dfr", ReceiptEvidence{
		EventType: "deposit", ProviderReference: "c2dfr-ref", Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR",
	}); err != nil {
		t.Fatalf("deferred callback: %v", err)
	}
	var deferredCount int64
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_provider_events WHERE provider_reference = 'c2dfr-ref' AND resolved_at IS NULL`).Scan(&deferredCount)
	}); err != nil {
		t.Fatal(err)
	}
	if deferredCount != 1 {
		t.Fatalf("setup: expected the first callback deferred/unresolved, got %d", deferredCount)
	}

	// Callback #2: resolved by MERCHANT reference (this attempt's own id)
	// - a T4 transition that learns provider_reference="c2dfr-ref" for
	// THIS attempt, entirely inside the receipt/callback path (no phase C
	// call anywhere in this test).
	if _, err := rvApplyReceipt(pool, orch, f.tenantID, "mock-c2dfr", ReceiptEvidence{
		EventType: "deposit", ProviderReference: "c2dfr-ref", MerchantReference: attemptID.String(), Outcome: OutcomePending,
	}); err != nil {
		t.Fatalf("T4 callback: %v", err)
	}

	final := mustGetAttempt(t, pool, f.tenantID, attemptID)
	if final.State != AttemptSucceeded {
		t.Errorf("C2/DFR: the callback site's own deferred-apply backstop must resolve the first callback's deferred success, got state=%s", final.State)
	}
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_provider_events WHERE provider_reference = 'c2dfr-ref' AND resolved_at IS NULL`).Scan(&deferredCount)
	}); err != nil {
		t.Fatal(err)
	}
	if deferredCount != 0 {
		t.Errorf("C2/DFR: the deferred receipt must be resolved once applied, got %d still unresolved", deferredCount)
	}
}

// TestRVLF_C2_DFS_SweeperSiteDeferredApplyBackstop kills mutant DFS
// (ledger-finance re-review C2): the SWEEPER's own deferred-apply
// backstop call (sweeper.go's ErrorClassPending/T9 branch) - distinct
// from both DFD (phase C) and DFR (callback) above. A poll resolves an
// attempt to 'pending' with a NEWLY learned provider_reference, and a
// receipt sharing that same reference, deferred earlier because nothing
// held it yet, must be picked up and applied in the SAME transaction.
func TestRVLF_C2_DFS_SweeperSiteDeferredApplyBackstop(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-c2dfs", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-c2dfs": p}, MultiWebhookCredentialResolver{"mock-c2dfs": NewMockWebhookCredentials(p)})
	sweeper := NewSweeper(pool, orch, AllowAllDepositKYCGate{}, MockCredentialResolver{})

	res := rvInit(t, pool, orch, f, 5000, "c2dfs-seed")
	// payment_attempts_one_live_per_intent (INV-IO-8): see the identical
	// DFR comment above.
	if _, err := rvCallback(pool, orch, f, "mock-c2dfs", p.CallbackPayload(f.tenantID, CallbackEventDeposit, *res.Attempt.ProviderReference, "", OutcomeDeclined, 0, "", "insufficient_funds", false)); err != nil {
		t.Fatalf("decline seed attempt: %v", err)
	}
	var attemptID uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		a, err := InsertCreatedAttempt(ctx, tx, NewCreatedAttempt{
			ID: uuid.New(), TenantID: f.tenantID, Operation: AttemptOperationDeposit,
			DepositIntentID: &res.Intent.ID, AttemptNo: 2, ExcludedProviderIDs: []string{"mock-c2dfs"},
			PaymentMethod: "card", AssetCode: "EUR", Amount: 5000,
		})
		attemptID = a.ID
		return err
	}); err != nil {
		t.Fatalf("insert isolated attempt: %v", err)
	}
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, res.Intent.ID); err != nil {
			return err
		}
		return ClaimCreatedForSubmission(ctx, tx, attemptID, "mock-c2dfs", uuid.New(), "rv-c2dfs", time.Now().Add(time.Minute))
	}); err != nil {
		t.Fatalf("claim for submission: %v", err)
	}

	// A callback names a provider reference no attempt has yet - deferred.
	if _, err := rvApplyReceipt(pool, orch, f.tenantID, "mock-c2dfs", ReceiptEvidence{
		EventType: "deposit", ProviderReference: "c2dfs-ref", Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR",
	}); err != nil {
		t.Fatalf("deferred callback: %v", err)
	}

	// A QueryStatus poll learns the reference via T9 (Pending), entirely
	// through the SWEEPER's own applyStatusEvidence (called directly with
	// a synthetic GateResult, exactly like TestRVLF_C3 below and the H3
	// sweeper.go tests already do) - no callback and no phase C call in
	// this half of the test.
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, res.Intent.ID); err != nil {
			return err
		}
		attempt, err := GetAttemptByID(ctx, tx, attemptID)
		if err != nil {
			return err
		}
		intent, err := GetDepositIntentByID(ctx, tx, res.Intent.ID)
		if err != nil {
			return err
		}
		gr := GateResult[StatusResult]{Class: ErrorClassPending, Value: StatusResult{Outcome: OutcomePending, ProviderReference: "c2dfs-ref"}}
		return sweeper.applyStatusEvidence(ctx, tx, intent, attempt, gr)
	}); err != nil {
		t.Fatalf("sweeper T9: %v", err)
	}

	final := mustGetAttempt(t, pool, f.tenantID, attemptID)
	if final.State != AttemptSucceeded {
		t.Errorf("C2/DFS: the sweeper's own deferred-apply backstop must resolve the earlier deferred success, got state=%s", final.State)
	}
	var deferredCount int64
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_provider_events WHERE provider_reference = 'c2dfs-ref' AND resolved_at IS NULL`).Scan(&deferredCount)
	}); err != nil {
		t.Fatal(err)
	}
	if deferredCount != 0 {
		t.Errorf("C2/DFS: the deferred receipt must be resolved once applied, got %d still unresolved", deferredCount)
	}
}

// TestRVLF_C3_SIBS_SweeperSuccessRejectsCreatedSibling kills mutant SIBS
// (ledger-finance re-review C3): a T13 second capture landing via the
// SWEEPER's own poll path (not phase C, not the callback path - both
// already covered) must still reject any leftover 'created' cascade
// sibling of the same intent (H4), via sweeper.go's own
// rejectCreatedSiblings call.
func TestRVLF_C3_SIBS_SweeperSuccessRejectsCreatedSibling(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-c3sibs", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-c3sibs": p}, MultiWebhookCredentialResolver{"mock-c3sibs": NewMockWebhookCredentials(p)})
	sweeper := NewSweeper(pool, orch, AllowAllDepositKYCGate{}, MockCredentialResolver{})

	res := rvInit(t, pool, orch, f, 5000, "c3sibs")
	if _, err := rvCallback(pool, orch, f, "mock-c3sibs", p.CallbackPayload(f.tenantID, CallbackEventDeposit, *res.Attempt.ProviderReference, "", OutcomeDeclined, 0, "", "insufficient_funds", false)); err != nil {
		t.Fatalf("decline A1: %v", err)
	}
	var strayID uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		stray, err := InsertCreatedAttempt(ctx, tx, NewCreatedAttempt{
			ID: uuid.New(), TenantID: f.tenantID, Operation: AttemptOperationDeposit,
			DepositIntentID: &res.Intent.ID, AttemptNo: 2, ExcludedProviderIDs: []string{"mock-c3sibs"},
			PaymentMethod: "card", AssetCode: "EUR", Amount: 5000,
		})
		strayID = stray.ID
		return err
	}); err != nil {
		t.Fatalf("insert stray created sibling: %v", err)
	}

	// A1's late T13 success, driven via the SWEEPER's own QueryStatus poll
	// path (applyStatusEvidence's ErrorClassSucceeded branch) directly.
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, res.Intent.ID); err != nil {
			return err
		}
		attempt, err := GetAttemptByID(ctx, tx, res.Attempt.ID)
		if err != nil {
			return err
		}
		intent, err := GetDepositIntentByID(ctx, tx, res.Intent.ID)
		if err != nil {
			return err
		}
		gr := GateResult[StatusResult]{Class: ErrorClassSucceeded, Value: StatusResult{Outcome: OutcomeSucceeded, ProviderReference: *res.Attempt.ProviderReference, Amount: 5000, AssetCode: "EUR"}}
		return sweeper.applyStatusEvidence(ctx, tx, intent, attempt, gr)
	}); err != nil {
		t.Fatalf("sweeper T13: %v", err)
	}

	stray := mustGetAttempt(t, pool, f.tenantID, strayID)
	if stray.State != AttemptRejected {
		t.Errorf("C3/SIBS: the sweeper's own T13 success must reject the leftover created sibling, got %s", stray.State)
	}
}

// TestRVLF_La_ReversalReceiptStoresBoundedDeclineReason pins ledger-
// finance L-a: a deposit_reversal receipt's (already-bounded)
// chargeback/refund reason is stored in the receipt row's own
// decline_reason column, not just carried in the audit metadata.
func TestRVLF_La_ReversalReceiptStoresBoundedDeclineReason(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-la", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-la": p}, MultiWebhookCredentialResolver{"mock-la": NewMockWebhookCredentials(p)})

	res := rvInit(t, pool, orch, f, 5000, "la")
	ref := *res.Attempt.ProviderReference
	if _, err := rvCallback(pool, orch, f, "mock-la", p.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("success: %v", err)
	}
	if _, err := rvCallback(pool, orch, f, "mock-la", p.CallbackPayload(f.tenantID, CallbackEventDepositReversal, "la-rev", ref, OutcomeDeclined, 5000, "EUR", "chargeback_lost", false)); err != nil {
		t.Fatalf("reversal: %v", err)
	}
	var declineReason *string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT decline_reason FROM payment_provider_events WHERE provider_reference = 'la-rev'`).Scan(&declineReason)
	}); err != nil {
		t.Fatal(err)
	}
	if declineReason == nil || *declineReason != "chargeback_lost" {
		t.Errorf("L-a: expected the reversal receipt's own decline_reason='chargeback_lost', got %v", declineReason)
	}
}

// TestRVLF_Le_OversizeDeclineReasonAuditedOnceNotOnRedelivery pins ledger-
// finance L-e: the "payment.decline_reason_bounded" audit record is
// written ONCE, the first time a genuinely new (non-duplicate) receipt
// carrying an oversized decline_reason is stored - never again for a
// byte-identical redelivery of that SAME event, which would otherwise
// grow the audit log without bound for a PSP that retries an unresolved
// or already-applied receipt.
func TestRVLF_Le_OversizeDeclineReasonAuditedOnceNotOnRedelivery(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-le", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-le": p}, MultiWebhookCredentialResolver{"mock-le": NewMockWebhookCredentials(p)})

	res := rvInit(t, pool, orch, f, 5000, "le")
	ref := *res.Attempt.ProviderReference
	oversize := strings.Repeat("y", 90)
	payload := p.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeDeclined, 0, "", oversize, true)

	// Deliver the IDENTICAL payload three times (a redelivery storm).
	for i := 0; i < 3; i++ {
		if _, err := rvCallback(pool, orch, f, "mock-le", payload); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}

	var auditCount int64
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'payment.decline_reason_bounded' AND tenant_id = $1`, f.tenantID).Scan(&auditCount)
	}); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Errorf("L-e: expected exactly 1 audit record across 3 identical redeliveries, got %d", auditCount)
	}
}

// TestRVLF_C2N12_DeferredAmbiguousBackstopAlsoRecomputesIntentProjection
// closes code-review C2 (rv-prh-i1-callback-code-review.md, ad476d6):
// N12/M7 requires the deferred-apply backstop's own
// recomputeDepositIntentProjection call to be genuinely pinned. The
// pre-existing TestRVLF_P8's own N12 assertion checks a SUCCESS
// transition, and an earlier draft of this test checked a DECLINE
// transition - but BOTH postDepositSuccess (orchestrator.go) and
// finalizeDeclined (called from applyResolvedReceiptEvidence's own
// OutcomeDeclined branch, BEFORE the backstop's recompute call ever runs)
// already write deposit_intents.status directly, so both assertions pass
// even with recomputeDepositIntentProjection removed from the backstop
// entirely (confirmed by mutation against each - see the evidence file).
// An AMBIGUOUS transition (MarkAmbiguousFromPending) has NO such
// redundant direct deposit_intents write anywhere - the intent's status
// can only move from 'pending' to 'ambiguous' via
// recomputeDepositIntentProjection, making this the genuine kill.
func TestRVLF_C2N12_DeferredAmbiguousBackstopAlsoRecomputesIntentProjection(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	p := NewMockProvider("mock-c2n12", "EUR")
	registerCapability(t, pool, f, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-c2n12": p}, MultiWebhookCredentialResolver{"mock-c2n12": NewMockWebhookCredentials(p)})

	res := rvInit(t, pool, orch, f, 5000, "c2n12-seed")
	// payment_attempts_one_live_per_intent (INV-IO-8): decline the seed
	// attempt first so the isolated second attempt below can be inserted
	// 'created' for the same intent.
	if _, err := rvCallback(pool, orch, f, "mock-c2n12", p.CallbackPayload(f.tenantID, CallbackEventDeposit, *res.Attempt.ProviderReference, "", OutcomeDeclined, 0, "", "insufficient_funds", false)); err != nil {
		t.Fatalf("decline seed attempt: %v", err)
	}
	var attemptID uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		a, err := InsertCreatedAttempt(ctx, tx, NewCreatedAttempt{
			ID: uuid.New(), TenantID: f.tenantID, Operation: AttemptOperationDeposit,
			DepositIntentID: &res.Intent.ID, AttemptNo: 2, ExcludedProviderIDs: []string{"mock-c2n12"},
			PaymentMethod: "card", AssetCode: "EUR", Amount: 5000,
		})
		attemptID = a.ID
		return err
	}); err != nil {
		t.Fatalf("insert isolated attempt: %v", err)
	}
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, res.Intent.ID); err != nil {
			return err
		}
		return ClaimCreatedForSubmission(ctx, tx, attemptID, "mock-c2n12", uuid.New(), "rv-c2n12", time.Now().Add(time.Minute))
	}); err != nil {
		t.Fatalf("claim for submission: %v", err)
	}

	// Callback #1: an AMBIGUOUS outcome naming a provider reference no
	// attempt has yet - deferred, unresolved.
	if _, err := rvApplyReceipt(pool, orch, f.tenantID, "mock-c2n12", ReceiptEvidence{
		EventType: "deposit", ProviderReference: "c2n12-ref", Outcome: OutcomeAmbiguous,
	}); err != nil {
		t.Fatalf("deferred ambiguous callback: %v", err)
	}

	// Callback #2: resolved by MERCHANT reference - a T4 transition that
	// learns provider_reference="c2n12-ref" for THIS attempt (moving both
	// the attempt AND, via setIntentAttempt, the intent itself to
	// 'pending'), whose own deferred-apply backstop must then resolve and
	// apply callback #1's deferred ambiguous outcome (MarkAmbiguousFrom
	// Pending) in the SAME transaction.
	if _, err := rvApplyReceipt(pool, orch, f.tenantID, "mock-c2n12", ReceiptEvidence{
		EventType: "deposit", ProviderReference: "c2n12-ref", MerchantReference: attemptID.String(), Outcome: OutcomePending,
	}); err != nil {
		t.Fatalf("T4 callback: %v", err)
	}

	final := mustGetAttempt(t, pool, f.tenantID, attemptID)
	if final.State != AttemptAmbiguous {
		t.Fatalf("setup: expected the deferred ambiguous outcome to be applied, got state=%s", final.State)
	}

	var intentStatus string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM deposit_intents WHERE id = $1`, res.Intent.ID).Scan(&intentStatus)
	}); err != nil {
		t.Fatal(err)
	}
	if intentStatus != string(DepositIntentAmbiguous) {
		t.Errorf("C2/N12: the deferred-apply backstop must recompute the intent's own projection on an ambiguous transition too (no direct write does this), got status=%q", intentStatus)
	}
}

// TestINVDEP1_FL1_ApplicationChokePointCatchesItBeforeTheDBBackstop closes
// security review F-L1 (rv-fh3-security.md, 81dd4b7): two mutants
// survived the FH-3 subset because migration 0107's ledger index produces
// the SAME disputed outcome regardless of which layer actually caught the
// second success -
//
//	MT: resolvedForOtherDeposit queried with the wrong tenant (always
//	    returns false, since a wrong-tenant read finds nothing under RLS);
//	MC: both application-level checks (postDepositSuccessOrDispute's own
//	    pre-check AND postDepositSuccess's internal re-check) disabled.
//
// In BOTH cases, migration 0107's unique index still refuses the second
// ledger posting, mapped to the identical T13d disputed state - but
// reaching that DB-level refusal at all is itself a defect signal
// (auditMultipleSuccessForIntent's own backstopFired=true branch, which
// fires the ADDITIONAL payments_deposit_intent_index_backstop_fired P1).
// This test captures slog output around the SAME real T13d scenario
// TestINVDEP1_O2 uses and asserts the backstop-fired line did NOT fire -
// proving the application-level choke point (not the DB backstop) is what
// actually caught this specific, correctly-tenanted delivery. Under
// either MT or MC, this assertion fails (the backstop-fired line WOULD
// fire), independent of migration 0107's own index tests
// (TestMigration0107_LedgerBackstop_ErrDepositAlreadyPostedForIntent),
// which exercise the index directly and are not affected by either
// mutant.
func TestINVDEP1_FL1_ApplicationChokePointCatchesItBeforeTheDBBackstop(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	pa := NewMockProvider("m-fl1-a", "EUR")
	pb := NewMockProvider("m-fl1-b", "EUR")
	pb.AcceptAllAmounts = true
	registerCapability(t, pool, f, pa, 100)
	registerCapability(t, pool, f, pb, 200)
	orch := NewOrchestrator(map[string]PaymentProvider{"m-fl1-a": pa, "m-fl1-b": pb},
		MultiWebhookCredentialResolver{"m-fl1-a": NewMockWebhookCredentials(pa), "m-fl1-b": NewMockWebhookCredentials(pb)})

	res := rvInit(t, pool, orch, f, 5000, "fl1")
	ref := *res.Attempt.ProviderReference
	childID := declineCascadableAndFindChild(t, pool, orch, f, "m-fl1-a", pa, ref)
	child := dispatchViaSweeper(t, pool, orch, f, childID)
	childRef := *child.ProviderReference

	// The fallback resolves the intent FIRST (the real, first capture) -
	// not captured, only setup.
	if _, err := rvCallback(pool, orch, f, "m-fl1-b", pb.CallbackPayload(f.tenantID, CallbackEventDeposit, childRef, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("fallback success: %v", err)
	}

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	// The original's LATE success (T13d) - the delivery under test.
	if _, err := rvCallback(pool, orch, f, "m-fl1-a", pa.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("late original success (T13d): %v", err)
	}

	disputed := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	if disputed.State != AttemptDisputed || disputed.TerminalReason == nil || *disputed.TerminalReason != TerminalReasonMultipleSuccessForIntent {
		t.Fatalf("setup: expected disputed/multiple_success_for_intent, got state=%s reason=%v", disputed.State, disputed.TerminalReason)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "payments_multiple_success_for_intent_alert") {
		t.Fatalf("F-L1: expected the multiple_success_for_intent alert to fire, got: %s", logged)
	}
	if strings.Contains(logged, "payments_deposit_intent_index_backstop_fired") {
		t.Fatalf("F-L1: the DB backstop must NOT have fired for a correctly-tenanted, correctly-checked delivery - the application-level choke point must catch this BEFORE the ledger index ever needs to; got: %s", logged)
	}

	// Independent corroboration: exactly one dispute audit record exists
	// (the application path) - never a second one from the ledger path.
	var disputeAuditCount int64
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'payment.attempt_disputed' AND target_id = $1`, disputed.ID.String()).Scan(&disputeAuditCount)
	}); err != nil {
		t.Fatal(err)
	}
	if disputeAuditCount != 1 {
		t.Errorf("F-L1: expected exactly one payment.attempt_disputed audit record (the application path), got %d", disputeAuditCount)
	}
}

// TestINVDEP1_FL2_MultipleSuccessAlertLogContentIsPinned closes security
// review F-L2 (rv-fh3-security.md, 81dd4b7): the "ML SURVIVED" mutant
// (adding `amount` to payments_multiple_success_for_intent_alert) passed
// the FH-3 subset because no test captured this line's exact attribute
// set. Pins tenant_id/deposit_intent_id/attempt_id present and asserts NO
// amount or provider-reference key ever appears in either P1 log line, on
// the SAME real T13d scenario TestINVDEP1_FL1 uses.
func TestINVDEP1_FL2_MultipleSuccessAlertLogContentIsPinned(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	pa := NewMockProvider("m-fl2-a", "EUR")
	pb := NewMockProvider("m-fl2-b", "EUR")
	pb.AcceptAllAmounts = true
	registerCapability(t, pool, f, pa, 100)
	registerCapability(t, pool, f, pb, 200)
	orch := NewOrchestrator(map[string]PaymentProvider{"m-fl2-a": pa, "m-fl2-b": pb},
		MultiWebhookCredentialResolver{"m-fl2-a": NewMockWebhookCredentials(pa), "m-fl2-b": NewMockWebhookCredentials(pb)})

	res := rvInit(t, pool, orch, f, 5000, "fl2")
	ref := *res.Attempt.ProviderReference
	childID := declineCascadableAndFindChild(t, pool, orch, f, "m-fl2-a", pa, ref)
	child := dispatchViaSweeper(t, pool, orch, f, childID)
	childRef := *child.ProviderReference

	if _, err := rvCallback(pool, orch, f, "m-fl2-b", pb.CallbackPayload(f.tenantID, CallbackEventDeposit, childRef, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("fallback success: %v", err)
	}

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	if _, err := rvCallback(pool, orch, f, "m-fl2-a", pa.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("late original success (T13d): %v", err)
	}

	disputed := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)

	logged := logBuf.String()
	var alertLine string
	for _, line := range strings.Split(logged, "\n") {
		if strings.Contains(line, "payments_multiple_success_for_intent_alert") {
			alertLine = line
			break
		}
	}
	if alertLine == "" {
		t.Fatalf("F-L2: expected the multiple_success_for_intent alert line, got: %s", logged)
	}
	// Pinned: exactly tenant_id, deposit_intent_id and attempt_id - no
	// amount, asset_code, provider_id or provider_reference key, and the
	// real values must be present (proving these are not just absent by
	// coincidence of the log format).
	if !strings.Contains(alertLine, "tenant_id="+f.tenantID.String()) {
		t.Errorf("F-L2: expected tenant_id=%s in the alert line, got: %s", f.tenantID, alertLine)
	}
	if disputed.DepositIntentID == nil || !strings.Contains(alertLine, "deposit_intent_id="+disputed.DepositIntentID.String()) {
		t.Errorf("F-L2: expected deposit_intent_id in the alert line, got: %s", alertLine)
	}
	if !strings.Contains(alertLine, "attempt_id="+disputed.ID.String()) {
		t.Errorf("F-L2: expected attempt_id=%s in the alert line, got: %s", disputed.ID, alertLine)
	}
	forbidden := []string{"amount=", "asset_code=", "provider_id=", "provider_reference=", "5000"}
	for _, key := range forbidden {
		if strings.Contains(alertLine, key) {
			t.Errorf("F-L2: the alert line must never carry %q, got: %s", key, alertLine)
		}
	}
}
