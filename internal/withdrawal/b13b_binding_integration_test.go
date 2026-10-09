//go:build integration

package withdrawal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
)

// B13-B (ADR 0111 section 18, owner decisions 2 and 4): the binding happens at RequestWithdrawal, in the
// transaction of the hold. Every refusal leaves NOTHING behind (no row, no hold, no decision row) and
// does not consume the idempotency key. Ledger invariants asserted throughout: SUM(D) = SUM(C).

func req(t *testing.T, pool *db.Pool, f fixture, p RequestParams) (WithdrawalRequest, error) {
	t.Helper()
	var wr WithdrawalRequest
	err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		wr, err = RequestWithdrawal(ctx, tx, p)
		return err
	})
	return wr, err
}

func baseParams(f fixture, key string, amount int64) RequestParams {
	return RequestParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, PersonID: f.personID, WalletID: f.walletID,
		AssetCode: "EUR", Amount: amount, IdempotencyKey: key, PayoutInstrumentID: f.instrumentID, Destinations: pitest.Shared(),
	}
}

func nothingWritten(t *testing.T, pool *db.Pool, f fixture, startCash int64, what string) {
	t.Helper()
	if n := countRows(t, pool, f.tenantID, `SELECT count(*) FROM withdrawal_requests`); n != 0 {
		t.Fatalf("%s: %d withdrawal rows written", what, n)
	}
	if c := cashBalance(t, pool, f); c != startCash {
		t.Fatalf("%s: cash balance moved %d -> %d", what, startCash, c)
	}
	if h := holdBalance(t, pool, f); h != 0 {
		t.Fatalf("%s: hold balance %d", what, h)
	}
	assertLedgerBalanced(t, pool, f.tenantID)
}

func TestB13B_Request_BindsTheInstrument_AndPostsTheHold(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 10_000)
	wr, err := req(t, pool, f, baseParams(f, "b13b-ok", 400))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if !wr.Bound() || *wr.PayoutInstrumentID != f.instrumentID || wr.PayoutInstrumentFingerprint == nil || len(*wr.PayoutInstrumentFingerprint) != 64 {
		t.Fatalf("binding not recorded: %+v", wr)
	}
	if cashBalance(t, pool, f) != 9_600 || holdBalance(t, pool, f) != 400 {
		t.Fatalf("hold not posted: cash=%d hold=%d", cashBalance(t, pool, f), holdBalance(t, pool, f))
	}
	assertLedgerBalanced(t, pool, f.tenantID)
	// The audit row names the instrument id and nothing destination-derived.
	if n := countRows(t, pool, f.tenantID, `SELECT count(*) FROM audit_log WHERE action = 'withdrawal.requested' AND metadata ? 'payout_instrument_id'
		AND NOT (metadata::text ~* '(fingerprint|iban|mask|label)')`); n != 1 {
		t.Fatalf("withdrawal.requested audit rows with only the instrument id = %d, want 1", n)
	}
}

func TestB13B_Request_RefusalsWriteNothing_AndDoNotConsumeTheKey(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 10_000)
	svc := pitest.Shared()
	other := seedFixture(t, pool, 0) // another tenant, player and instrument

	// A second verified instrument of THIS player to suspend, one pending-verification, one for another asset.
	suspended := pitest.Bind(t, pool, f.tenantID, f.playerAccountID, "EUR")
	if err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := svc.Suspend(ctx, tx, payoutinstrument.BlockParams{TenantID: f.tenantID, InstrumentID: suspended,
			Actor: payoutinstrument.Actor{Type: payoutinstrument.ActorStaff, ID: uuid.NewString()}, ReasonCode: "aml_review"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var pending uuid.UUID
	if err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		r, err := svc.Register(ctx, tx, payoutinstrument.RegisterParams{TenantID: f.tenantID, PlayerAccountID: f.playerAccountID,
			Kind: payoutinstrument.KindSyntheticTest, Rail: "bank_transfer", AssetCodes: []string{"EUR"}, Detail: []byte(`{"label":"pending-one"}`)})
		pending = r.Instrument.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	usdOnly := pitest.Bind(t, pool, f.tenantID, f.playerAccountID, "USD")

	cases := []struct {
		name   string
		mutate func(p *RequestParams)
		want   error
		reason string
	}{
		{"no instrument", func(p *RequestParams) { p.PayoutInstrumentID = uuid.Nil }, ErrPayoutInstrumentRequired, ""},
		{"no gate configured", func(p *RequestParams) { p.Destinations = nil }, ErrDestinationGateUnavailable, ""},
		{"unknown instrument", func(p *RequestParams) { p.PayoutInstrumentID = uuid.New() }, ErrPayoutInstrumentNotUsable, payoutinstrument.ReasonNotFound},
		{"another tenant's instrument", func(p *RequestParams) { p.PayoutInstrumentID = other.instrumentID }, ErrPayoutInstrumentNotUsable, payoutinstrument.ReasonNotFound},
		{"suspended instrument", func(p *RequestParams) { p.PayoutInstrumentID = suspended }, ErrPayoutInstrumentNotUsable, payoutinstrument.ReasonNotVerified},
		{"pending-verification instrument", func(p *RequestParams) { p.PayoutInstrumentID = pending }, ErrPayoutInstrumentNotUsable, payoutinstrument.ReasonNotVerified},
		{"asset not listed", func(p *RequestParams) { p.PayoutInstrumentID = usdOnly }, ErrPayoutInstrumentNotUsable, payoutinstrument.ReasonAssetNotListed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := baseParams(f, "b13b-refuse", 400)
			c.mutate(&p)
			_, err := req(t, pool, f, p)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if c.reason != "" {
				g, ok := payoutinstrument.IsGateRefusal(err)
				if !ok || g.Reason != c.reason {
					t.Fatalf("gate refusal = %+v, want reason %q", g, c.reason)
				}
			}
			nothingWritten(t, pool, f, 10_000, c.name)
		})
	}
	// Not one of those consumed the key: the same key now succeeds.
	if _, err := req(t, pool, f, baseParams(f, "b13b-refuse", 400)); err != nil {
		t.Fatalf("the idempotency key must not have been consumed by the refusals: %v", err)
	}
}

// Another PLAYER of the same tenant cannot bind your instrument, and a revoked instrument cannot be bound.
func TestB13B_Request_CrossPlayerAndRevoked(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 10_000)
	svc := pitest.Shared()
	// A second player in the SAME tenant/brand holding its own instrument.
	var p2 uuid.UUID
	if err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		p2 = uuid.New()
		person := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			VALUES ($1,$2,$3,$4,$5,'x','active')`, p2, f.tenantID, f.brandID, person, p2.String()+"@example.test")
		return err
	}); err != nil {
		t.Skipf("cannot seed a second player in this harness: %v", err)
	}
	theirs := pitest.Bind(t, pool, f.tenantID, p2, "EUR")
	p := baseParams(f, "b13b-xplayer", 400)
	p.PayoutInstrumentID = theirs
	_, err := req(t, pool, f, p)
	if g, ok := payoutinstrument.IsGateRefusal(err); !errors.Is(err, ErrPayoutInstrumentNotUsable) || !ok || !g.Integrity() {
		t.Fatalf("another player's instrument: %v (integrity refusal expected)", err)
	}
	nothingWritten(t, pool, f, 10_000, "cross-player")

	mine := pitest.Bind(t, pool, f.tenantID, f.playerAccountID, "EUR")
	if err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := svc.Revoke(ctx, tx, payoutinstrument.BlockParams{TenantID: f.tenantID, InstrumentID: mine,
			Actor: payoutinstrument.Actor{Type: payoutinstrument.ActorPlayer, ID: f.playerAccountID.String()}, ReasonCode: "player_revoked", PlayerAccountID: f.playerAccountID})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	p.PayoutInstrumentID = mine
	if _, err := req(t, pool, f, p); !errors.Is(err, ErrPayoutInstrumentNotUsable) {
		t.Fatalf("revoked instrument: %v", err)
	}
	nothingWritten(t, pool, f, 10_000, "revoked")
}

func TestB13B_Request_Replay(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 10_000)
	first, err := req(t, pool, f, baseParams(f, "b13b-replay", 400))
	if err != nil {
		t.Fatal(err)
	}
	// Same key, same instrument: the original, no second hold.
	again, err := req(t, pool, f, baseParams(f, "b13b-replay", 400))
	if err != nil || again.ID != first.ID {
		t.Fatalf("replay = %v %v, want the original", again.ID, err)
	}
	if holdBalance(t, pool, f) != 400 {
		t.Fatalf("a replay posted a second hold: %d", holdBalance(t, pool, f))
	}
	// Same key, a DIFFERENT instrument: reused.
	second := pitest.Bind(t, pool, f.tenantID, f.playerAccountID, "EUR")
	p := baseParams(f, "b13b-replay", 400)
	p.PayoutInstrumentID = second
	if _, err := req(t, pool, f, p); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("different instrument on replay: %v, want ErrIdempotencyKeyReused", err)
	}
	// Same key, NO instrument, for a bound request: reused too (the original is bound).
	p = baseParams(f, "b13b-replay", 400)
	p.PayoutInstrumentID = uuid.Nil
	if _, err := req(t, pool, f, p); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("no instrument on replay of a bound request: %v, want ErrIdempotencyKeyReused", err)
	}
	// A replay does not re-evaluate the gate: after the instrument is suspended the original is still returned.
	if err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := pitest.Shared().Suspend(ctx, tx, payoutinstrument.BlockParams{TenantID: f.tenantID, InstrumentID: f.instrumentID,
			Actor: payoutinstrument.Actor{Type: payoutinstrument.ActorStaff, ID: uuid.NewString()}, ReasonCode: "aml_review"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if again, err := req(t, pool, f, baseParams(f, "b13b-replay", 400)); err != nil || again.ID != first.ID {
		t.Fatalf("replay after suspension = %v %v, want the original (ADR 0096 s8 item 3)", again.ID, err)
	}
	assertLedgerBalanced(t, pool, f.tenantID)
}

// Concurrency: N identical concurrent requests create exactly one row and one hold; a request racing a
// suspension either binds before it (and the suspension then parks it at the dispatch gates) or is refused -
// never a row bound to a blocked instrument without the gate having seen the block.
func TestB13B_Request_ConcurrentSameKey_OneRowOneHold(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 10_000)
	const n = 8
	var wg sync.WaitGroup
	ids := make([]uuid.UUID, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			wr, err := req(t, pool, f, baseParams(f, "b13b-conc", 400))
			ids[i], errs[i] = wr.ID, err
		}(i)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("racer %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("racers returned different requests: %v vs %v", ids[i], ids[0])
		}
	}
	if c := countRows(t, pool, f.tenantID, `SELECT count(*) FROM withdrawal_requests`); c != 1 {
		t.Fatalf("rows = %d, want 1", c)
	}
	if holdBalance(t, pool, f) != 400 || cashBalance(t, pool, f) != 9_600 {
		t.Fatalf("exactly one hold expected: cash=%d hold=%d", cashBalance(t, pool, f), holdBalance(t, pool, f))
	}
	assertLedgerBalanced(t, pool, f.tenantID)
}

func TestB13B_Request_RacingASuspension_NeverBindsABlockedInstrumentUnseen(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 100_000)
	const n = 6
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = req(t, pool, f, baseParams(f, "b13b-race-"+uuid.NewString(), 100))
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := pitest.Shared().Suspend(ctx, tx, payoutinstrument.BlockParams{TenantID: f.tenantID, InstrumentID: f.instrumentID,
				Actor: payoutinstrument.Actor{Type: payoutinstrument.ActorStaff, ID: uuid.NewString()}, ReasonCode: "aml_review"})
			return err
		})
	}()
	wg.Wait()
	ok := 0
	for _, e := range results {
		switch {
		case e == nil:
			ok++
		case errors.Is(e, ErrPayoutInstrumentNotUsable):
		default:
			t.Fatalf("unexpected error: %v", e)
		}
	}
	// Every created row has a matching hold; every refusal wrote nothing: rows == holds == ok.
	rows := countRows(t, pool, f.tenantID, `SELECT count(*) FROM withdrawal_requests`)
	holds := countRows(t, pool, f.tenantID, `SELECT count(*) FROM withdrawal_requests WHERE hold_ledger_transaction_id IS NOT NULL`)
	if rows != ok || holds != ok {
		t.Fatalf("rows=%d holds=%d ok=%d", rows, holds, ok)
	}
	// Once the suspension has committed, no further request binds.
	if _, err := req(t, pool, f, baseParams(f, "b13b-after-suspend", 100)); !errors.Is(err, ErrPayoutInstrumentNotUsable) {
		t.Fatalf("after the suspension: %v", err)
	}
	assertLedgerBalanced(t, pool, f.tenantID)
}

// Partial failure: a request that passes the instrument gate but fails later (insufficient funds, KYC) rolls
// back as a whole - no row, no hold, and the instrument is untouched.
func TestB13B_Request_PartialFailure_RollsBackWholly(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 100)
	if _, err := req(t, pool, f, baseParams(f, "b13b-insufficient", 101)); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("err = %v, want ErrInsufficientFunds", err)
	}
	nothingWritten(t, pool, f, 100, "insufficient funds")
	st := countRows(t, pool, f.tenantID, `SELECT count(*) FROM payout_instruments WHERE id = $1 AND state = 'verified'`, f.instrumentID)
	if st != 1 {
		t.Fatalf("the instrument changed state")
	}
}

// The DB is the second line: a direct INSERT without a binding is refused for the runtime role as well.
func TestB13B_DatabaseRefusesAnUnboundInsert(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 1_000)
	err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO withdrawal_requests (tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, idempotency_key)
			VALUES ($1,$2,$3,$4,'EUR',100,'unbound')`, f.tenantID, f.brandID, f.playerAccountID, f.walletID)
		return err
	})
	if err == nil {
		t.Fatal("an unbound INSERT must be refused by the database")
	}
}

// Serialisation with a revocation in flight (ADR 0111 2.4): the request takes the instrument FOR SHARE, a
// revocation/suspension takes it FOR UPDATE, so a request cannot bind an instrument whose block is uncommitted.
// Deterministic: the suspension is held open; the request must WAIT, and once the suspension commits it is
// refused. (Without the lock the request would bind the instrument immediately, ignoring the uncommitted block.)
func TestB13B_Request_WaitsForARevocationInFlight_ThenIsRefused(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 10_000)
	locked := make(chan struct{})
	release := make(chan struct{})
	blockDone := make(chan error, 1)
	go func() {
		blockDone <- runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := pitest.Shared().Suspend(ctx, tx, payoutinstrument.BlockParams{TenantID: f.tenantID, InstrumentID: f.instrumentID,
				Actor: payoutinstrument.Actor{Type: payoutinstrument.ActorStaff, ID: uuid.NewString()}, ReasonCode: "aml_review"}); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	type result struct {
		wr  WithdrawalRequest
		err error
	}
	reqDone := make(chan result, 1)
	go func() {
		wr, err := req(t, pool, f, baseParams(f, "b13b-lockwait", 400))
		reqDone <- result{wr, err}
	}()
	select {
	case r := <-reqDone:
		close(release)
		<-blockDone
		t.Fatalf("the request must WAIT for the in-flight suspension, but it finished: %+v %v", r.wr.ID, r.err)
	case <-time.After(700 * time.Millisecond):
	}
	close(release)
	if err := <-blockDone; err != nil {
		t.Fatal(err)
	}
	r := <-reqDone
	if !errors.Is(r.err, ErrPayoutInstrumentNotUsable) {
		t.Fatalf("after the suspension committed the request must be refused, got %v", r.err)
	}
	nothingWritten(t, pool, f, 10_000, "request behind a revocation")
}
