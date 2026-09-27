//go:build integration

// ADR 0095 §28.12: migration 0107 tests QA's test-first matrix
// (inv_dep1_matrix_integration_test.go) could not write, because they
// require migration-boundary control (a scratch DB pinned to a specific
// version, an up/down/up round trip, and pre-flight-refusal fixtures) that
// only this package's existing migration-test harness
// (migration_0101_integration_test.go's pattern) provides.
package payments

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

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
