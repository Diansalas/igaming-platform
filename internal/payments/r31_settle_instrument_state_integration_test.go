//go:build integration

package payments

// R31 (ADR 0095 section 48 owner decision 3, ADR 0111 section 22): INSTRUMENT STATE MUST NOT GATE SETTLEMENT.
//
// Once a payout has been authorised, bound and snapshotted (T1p), a later change of the instrument's state
// (suspended, revoked, verification expired) must not prevent settlement of that already-authorised attempt
// through ANY evidence path: sync phase C, the QueryStatus poll (sweeper), the callback/receipt. Settlement is
// decided by snapshot integrity and the destination echo only. Instrument eligibility stays a rule of
// initiation and dispatch (request, T1p, phase B, T2/T12), which these tests also pin unchanged.
//
// Every flow below runs as the RUNTIME role (the owner pool only seeds fixtures, applies the "privileged
// corruption" of the row-change tests and writes the max-age configuration the runtime role cannot). Each test
// has its own scratch database. MOCK providers only; nothing here is a statement about a real PSP.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// ---- world -----------------------------------------------------------------------------------------------------

// r31W is a b13bW whose flows run through the runtime-role pool. owner is used for fixtures, the max-age
// configuration and the deliberate privileged corruption only.
type r31W struct {
	*b13bW
	owner *db.Pool
	rt    *db.Pool
}

// newR31W builds a scratch database migrated to the latest migration with the runtime role's grants applied
// BEFORE the migrations (the priv_db.sh pattern: database-level grants only; no role is created or altered), so the
// runtime role has exactly the privileges the migrations leave it. maxAge > 0 licenses the tenant under a
// jurisdiction with that verification max age and re-binds the fixture instrument, so its verification really
// expires after maxAge.
func newR31W(t *testing.T, id string, maxAge time.Duration) *r31W {
	t.Helper()
	rtBase := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if rtBase == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL not set; skipping runtime-role test")
	}
	scratchURL := scratchdb.New(t, "r31s_")
	su, err := url.Parse(scratchURL)
	if err != nil {
		t.Fatal(err)
	}
	ru, err := url.Parse(rtBase)
	if err != nil {
		t.Fatal(err)
	}
	ru.Path = su.Path
	ctx := context.Background()
	owner, err := db.Connect(ctx, scratchURL, 10, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	dbName := strings.TrimPrefix(su.Path, "/")
	for _, stmt := range []string{
		`GRANT CONNECT ON DATABASE ` + pgx.Identifier{dbName}.Sanitize() + ` TO igaming_runtime`,
		`GRANT USAGE ON SCHEMA public TO igaming_runtime`,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO igaming_runtime`,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO igaming_runtime`,
	} {
		if _, err := owner.Raw().Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if _, err := owner.MigrateUp(ctx, realMigrationsDir(t)); err != nil {
		t.Fatalf("migrate scratch to latest: %v", err)
	}
	prooftest.InstallVia(t, owner.Raw(), t.Name()+"/"+uuid.NewString())
	rt, err := db.Connect(ctx, ru.String(), 6, 5*time.Second)
	if err != nil {
		t.Fatalf("connect runtime role: %v", err)
	}
	t.Cleanup(rt.Close)
	var super, bypass bool
	if err := rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass)
	}); err != nil {
		t.Fatal(err)
	}
	if super || bypass {
		t.Fatalf("the runtime role must be neither superuser nor BYPASSRLS (super=%v bypassrls=%v)", super, bypass)
	}

	f := seedPayoutFixture(t, owner, 100_000, true)
	pid := "mock-r31-" + id
	// declared: the manifest declares the destination echo, so a success WITHOUT a (matching) echo cannot settle:
	// every settlement below is proven against the SNAPSHOT's fingerprint, not against anything of the instrument.
	p := &b13bProvider{MockProvider: NewMockProvider(pid, "EUR"), declared: true}
	registerCapability(t, owner, f.orchFixture, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{pid: p}, MultiWebhookCredentialResolver{pid: NewMockWebhookCredentials(p.MockProvider)}).
		WithPayoutDestinations(pitest.Shared())
	b := &b13bW{t: t, pool: owner, f: f, prov: p, orch: orch, svc: pitest.Shared(), pid: pid}
	w := &r31W{b13bW: b, owner: owner, rt: rt}
	if maxAge > 0 {
		b.licenceWithMaxAge(maxAge)
		b.f.instrumentID = pitest.Bind(t, owner, f.tenantID, f.playerAccountID, "EUR")
	}
	return w
}

// onRuntime selects the runtime-role pool for the embedded b13bW helpers.
func (w *r31W) onRuntime() { w.pool = w.rt }

// tamperOwner is the b13bW tamper (arbitrary SQL by the table owner, triggers disabled) on the owner pool.
func (w *r31W) tamperOwner(table, stmt string, args ...any) {
	w.t.Helper()
	prev := w.pool
	w.pool = w.owner
	defer func() { w.pool = prev }()
	w.tamper(table, stmt, args...)
}

// approvedOwner is an approved (requested -> pending_review -> approved) withdrawal for the fixture instrument; the
// request/approval steps are not under test and use the owner pool, like every other payments fixture.
func (w *r31W) approvedOwner(amount int64, key string) withdrawal.WithdrawalRequest {
	w.t.Helper()
	prev := w.pool
	w.pool = w.owner
	defer func() { w.pool = prev }()
	return w.approved(amount, key)
}

// ---- instrument state changes ----------------------------------------------------------------------------------------------

type r31Change string

const (
	chNone        r31Change = "none"
	chSuspended   r31Change = "suspended"
	chRevoked     r31Change = "revoked"
	chExpired     r31Change = "verification_expired_by_time"
	chExpiredSwep r31Change = "verification_expired_swept"
	chRowChanged  r31Change = "row_changed_state_mask_label"
)

// waitExpiry sleeps until the instrument's in-force verification has really expired (database clock).
func (w *r31W) waitExpiry(instrumentID uuid.UUID) {
	w.t.Helper()
	var secs float64
	if err := w.owner.WithTenant(w.ctx(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT GREATEST(0, EXTRACT(EPOCH FROM (max(expires_at) - now())))::float8 FROM payout_instrument_verifications WHERE instrument_id = $1`,
			instrumentID).Scan(&secs)
	}); err != nil {
		w.t.Fatalf("read verification expiry: %v", err)
	}
	time.Sleep(time.Duration(secs*float64(time.Second)) + 400*time.Millisecond)
}

func (w *r31W) sweepExpiry() {
	w.t.Helper()
	if err := w.rt.WithTenant(w.ctx(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		r, err := w.svc.Sweep(ctx, tx, w.f.tenantID, 100)
		if err == nil && r.Expired < 1 {
			err = errors.New("the sweep expired nothing")
		}
		return err
	}); err != nil {
		w.t.Fatalf("sweep: %v", err)
	}
}

func (w *r31W) instrumentState(instrumentID uuid.UUID) string {
	w.t.Helper()
	var s string
	if err := w.rt.WithTenant(w.ctx(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM payout_instruments WHERE id = $1`, instrumentID).Scan(&s)
	}); err != nil {
		w.t.Fatal(err)
	}
	return s
}

// change applies one instrument-state change AFTER the snapshot exists (and after phase B for the sync path).
func (w *r31W) change(c r31Change, instrumentID uuid.UUID) {
	w.t.Helper()
	switch c {
	case chNone:
	case chSuspended:
		w.suspend(instrumentID)
		if s := w.instrumentState(instrumentID); s != "suspended" {
			w.t.Fatalf("instrument state = %s, want suspended", s)
		}
	case chRevoked:
		w.providerRevoke(instrumentID)
		if s := w.instrumentState(instrumentID); s != "revoked" {
			w.t.Fatalf("instrument state = %s, want revoked", s)
		}
	case chExpired:
		w.waitExpiry(instrumentID)
		if s := w.instrumentState(instrumentID); s != "verified" {
			w.t.Fatalf("by-time expiry must not write state, got %s", s)
		}
	case chExpiredSwep:
		w.waitExpiry(instrumentID)
		w.sweepExpiry()
		if s := w.instrumentState(instrumentID); s != "verification_expired" {
			w.t.Fatalf("instrument state = %s, want verification_expired", s)
		}
	case chRowChanged:
		// State (a real suspension through the service) plus the identity columns the runtime role can never
		// change: the mask and the (encrypted) detail that carries the label, rewritten by the table owner with the
		// triggers off. This is the strongest "the instrument row changed after the snapshot" there is.
		w.suspend(instrumentID)
		w.tamperOwner("payout_instruments", `UPDATE payout_instruments SET display_mask = 'ZZ****0000' WHERE id = $1`, instrumentID)
		w.tamperOwner("payout_instruments", `UPDATE payout_instruments SET detail_ciphertext = decode(repeat('ab', 40), 'hex') WHERE id = $1`, instrumentID)
	default:
		w.t.Fatalf("unknown change %q", c)
	}
}

// ---- evidence paths ---------------------------------------------------------------------------------------------------------

type r31Path string

const (
	pSync     r31Path = "sync"
	pPoll     r31Path = "poll"
	pCallback r31Path = "callback"
)

type r31Echo int

const (
	echoGood r31Echo = iota
	echoBad
)

func (w *r31W) echoFor(k r31Echo, attemptID uuid.UUID) *payoutinstrument.DestinationEcho {
	if k == echoBad {
		return w.badEcho(attemptID)
	}
	return w.goodEcho(attemptID)
}

// callbackDecl is the receipt cell with an explicit decline reason.
func (w *r31W) callbackDecl(a PaymentAttempt, outcome Outcome, ref string, echo *payoutinstrument.DestinationEcho) error {
	pending, err := alerting.InTx(w.ctx(), alerting.NewTenantRunner(w.pool, w.f.tenantID), func(ctx context.Context, tx pgx.Tx) error {
		ev := ReceiptEvidence{
			EventType: "payout", ProviderReference: ref, MerchantReference: a.MerchantReference,
			Outcome: outcome, Amount: 500, AssetCode: "EUR", DestinationEcho: echo,
		}
		if outcome == OutcomeDeclined {
			ev.DeclineReason = "account_closed"
		}
		_, err := ApplyReceiptEvidence(ctx, tx, w.orch, w.f.tenantID, w.pid, ev)
		return err
	})
	if err == nil {
		pending.Flush(w.ctx())
	}
	return err
}

// run drives one approved withdrawal through T1p (runtime role, snapshot written), then the evidence path, applying
// the instrument change in the window AFTER the snapshot/phase-B gate and BEFORE the evidence is applied.
func (w *r31W) run(key string, path r31Path, outcome Outcome, chg r31Change, echo r31Echo) (withdrawal.WithdrawalRequest, ClaimResult) {
	w.t.Helper()
	inst := w.f.instrumentID
	wr := w.approvedOwner(500, key)
	w.onRuntime()
	cl := w.mustClaim(wr)
	ref := "r31-ref-" + key
	decl := ""
	if outcome == OutcomeDeclined {
		decl = "account_closed"
	}
	e := w.echoFor(echo, cl.Attempt.ID)
	switch path {
	case pSync:
		w.prov.set(WithdrawResult{Outcome: outcome, ProviderReference: ref, DeclineReason: decl, DestinationEcho: e}, StatusResult{})
		gr := w.dispatch(cl) // phase B: the dispatch gate still sees a usable instrument and the provider is called
		w.change(chg, inst)
		if err := w.apply(wr, cl, gr); err != nil {
			w.t.Fatalf("sync apply: %v", err)
		}
	case pPoll:
		w.prov.set(WithdrawResult{Outcome: OutcomePending, ProviderReference: ref}, StatusResult{})
		if err := w.apply(wr, cl, w.dispatch(cl)); err != nil {
			w.t.Fatalf("apply pending: %v", err)
		}
		w.change(chg, inst)
		w.prov.set(WithdrawResult{}, StatusResult{ProviderReference: ref, Outcome: outcome, Amount: 500, AssetCode: "EUR", DeclineReason: decl, DestinationEcho: e})
		if err := w.sweeper().processPayoutAttempt(w.ctx(), w.f.tenantID, w.attempt(cl.Attempt.ID)); err != nil {
			w.t.Fatalf("sweeper poll: %v", err)
		}
	case pCallback:
		w.prov.set(WithdrawResult{Outcome: OutcomePending, ProviderReference: ref}, StatusResult{})
		if err := w.apply(wr, cl, w.dispatch(cl)); err != nil {
			w.t.Fatalf("apply pending: %v", err)
		}
		w.change(chg, inst)
		if err := w.callbackDecl(w.attempt(cl.Attempt.ID), outcome, ref, e); err != nil {
			w.t.Fatalf("callback: %v", err)
		}
	default:
		w.t.Fatalf("unknown path %q", path)
	}
	return wr, cl
}

// ---- outcome capture ----------------------------------------------------------------------------------------------------------

type r31Outcome struct {
	Withdrawal, Attempt string
	Entries             []string // transaction type|account type|direction|amount, sorted: ids and timestamps excluded
	Hold, Cash          int64    // net (credit - debit) per account type
	CompletedAudit      int
	FailedAudit         int
	ParkAudit           int
}

func (w *r31W) outcome(wr withdrawal.WithdrawalRequest, cl ClaimResult) r31Outcome {
	w.t.Helper()
	o := r31Outcome{Withdrawal: string(w.wr(wr.ID).State), Attempt: string(w.attempt(cl.Attempt.ID).State)}
	if err := w.rt.WithTenant(w.ctx(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT lt.transaction_type, la.account_type, le.direction, le.amount::text
			FROM ledger_entries le
			JOIN ledger_transactions lt ON lt.id = le.ledger_transaction_id AND lt.tenant_id = le.tenant_id
			JOIN ledger_accounts la ON la.id = le.ledger_account_id AND la.tenant_id = le.tenant_id
			WHERE le.tenant_id = $1`, w.f.tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var tt, at, dir, amt string
			if err := rows.Scan(&tt, &at, &dir, &amt); err != nil {
				return err
			}
			o.Entries = append(o.Entries, strings.Join([]string{tt, at, dir, amt}, "|"))
		}
		return rows.Err()
	}); err != nil {
		w.t.Fatal(err)
	}
	sort.Strings(o.Entries)
	for _, e := range o.Entries {
		p := strings.Split(e, "|")
		var amt int64
		for _, c := range p[3] {
			amt = amt*10 + int64(c-'0')
		}
		if p[2] == "debit" {
			amt = -amt
		}
		switch p[1] {
		case "player_withdrawal_hold":
			o.Hold += amt
		case "player_cash":
			o.Cash += amt
		}
	}
	o.CompletedAudit = w.count(`SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action='withdrawal.completed' AND target_id=$2`, w.f.tenantID, wr.ID.String())
	o.FailedAudit = w.count(`SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action='withdrawal.failed' AND target_id=$2`, w.f.tenantID, wr.ID.String())
	o.ParkAudit = w.count(`SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action LIKE 'payments.payout_parked%' AND metadata->>'attempt_id' = $2`, w.f.tenantID, cl.Attempt.ID.String())
	return o
}

// ---- (a) settlement is independent of the instrument's state -------------------------------------------------------------------

func TestR31_A_InstrumentStateDoesNotGateSettlement(t *testing.T) {
	type key struct {
		path    r31Path
		outcome Outcome
	}
	baseline := map[key]r31Outcome{}
	for _, path := range []r31Path{pSync, pPoll, pCallback} {
		for _, out := range []Outcome{OutcomeSucceeded, OutcomeDeclined} {
			t.Run("baseline/"+string(path)+"/"+string(out), func(t *testing.T) {
				w := newR31W(t, "ab", 0)
				wr, cl := w.run("r31-a-base", path, out, chNone, echoGood)
				o := w.outcome(wr, cl)
				if out == OutcomeSucceeded {
					if o.Withdrawal != "completed" || o.Attempt != "succeeded" || o.CompletedAudit != 1 || o.FailedAudit != 0 || o.Hold != 0 || o.Cash != 99_500 {
						t.Fatalf("baseline success outcome unexpected: %+v", o)
					}
				} else if o.Withdrawal != "failed" || o.Attempt != "declined" || o.FailedAudit != 1 || o.CompletedAudit != 0 || o.Hold != 0 || o.Cash != 100_000 {
					t.Fatalf("baseline decline outcome unexpected: %+v", o)
				}
				w.balanced()
				baseline[key{path, out}] = o
			})
		}
	}
	type scenario struct {
		chg      r31Change
		outcomes []Outcome
	}
	both := []Outcome{OutcomeSucceeded, OutcomeDeclined}
	for _, sc := range []scenario{
		{chSuspended, both}, {chRevoked, both}, {chExpiredSwep, both},
		{chExpired, []Outcome{OutcomeSucceeded}}, {chRowChanged, []Outcome{OutcomeSucceeded}},
	} {
		for _, path := range []r31Path{pSync, pPoll, pCallback} {
			for _, out := range sc.outcomes {
				t.Run(string(sc.chg)+"/"+string(path)+"/"+string(out), func(t *testing.T) {
					base, ok := baseline[key{path, out}]
					if !ok {
						t.Skip("baseline did not run")
					}
					maxAge := time.Duration(0)
					if sc.chg == chExpired || sc.chg == chExpiredSwep {
						maxAge = 4 * time.Second
					}
					w := newR31W(t, "ac", maxAge)
					wr, cl := w.run("r31-a-"+string(sc.chg), path, out, sc.chg, echoGood)
					got := w.outcome(wr, cl)
					if !reflect.DeepEqual(got, base) {
						t.Fatalf("%s/%s after %s settled differently from an untouched instrument:\n got  %+v\n want %+v", path, out, sc.chg, got, base)
					}
					if got.ParkAudit != 0 {
						t.Fatalf("a settlement must not park: %+v", got)
					}
					w.balanced() // SUM(D) = SUM(C) and projection = rebuild
					if n := w.count(`SELECT count(*) FROM payout_attempt_destination_snapshots WHERE attempt_id = $1`, cl.Attempt.ID); n != 1 {
						t.Fatalf("snapshots = %d, want exactly 1 (written at T1p, never touched by evidence)", n)
					}
				})
			}
		}
	}
}

// ---- (b) a NEW withdrawal request with an unusable instrument is refused -------------------------------------------------------

func TestR31_B_NewRequestWithUnusableInstrumentIsRefused(t *testing.T) {
	w := newR31W(t, "b", 4*time.Second)
	start := w.count(`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, w.f.tenantID)

	pending := func() uuid.UUID { // registered, never verified
		var id uuid.UUID
		if err := w.rt.WithTenant(w.ctx(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			r, err := w.svc.Register(ctx, tx, payoutinstrument.RegisterParams{TenantID: w.f.tenantID, PlayerAccountID: w.f.playerAccountID,
				Kind: payoutinstrument.KindSyntheticTest, Rail: "bank_transfer", AssetCodes: []string{"EUR"}, Detail: []byte(`{"label":"pendingone"}`)})
			id = r.Instrument.ID
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	fresh := func() uuid.UUID { return pitest.Bind(t, w.owner, w.f.tenantID, w.f.playerAccountID, "EUR") }
	refused := func(t *testing.T, id uuid.UUID, gate string) {
		t.Helper()
		p := withdrawal.RequestParams{
			TenantID: w.f.tenantID, BrandID: w.f.brandID, PlayerAccountID: w.f.playerAccountID, PersonID: w.f.personID,
			WalletID: w.f.walletID, AssetCode: "EUR", Amount: 400, IdempotencyKey: "r31-b-key", PayoutInstrumentID: id, Destinations: w.svc,
		}
		rows0 := w.count(`SELECT count(*) FROM withdrawal_requests WHERE tenant_id = $1`, w.f.tenantID)
		err := w.rt.WithTenant(w.ctx(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := withdrawal.RequestWithdrawal(ctx, tx, p)
			return err
		})
		if !errors.Is(err, withdrawal.ErrPayoutInstrumentNotUsable) {
			t.Fatalf("err = %v, want ErrPayoutInstrumentNotUsable", err)
		}
		if g, ok := payoutinstrument.IsGateRefusal(err); !ok || g.Reason != gate {
			t.Fatalf("gate refusal = %+v, want reason %q", g, gate)
		}
		// The generic player-facing refusal is a 409 (the handler maps this error to exactly this code).
		rec := httptest.NewRecorder()
		apierror.Write(rec, "r31", apierror.CodePayoutInstrumentNotUsable, "the payout instrument cannot be used for this withdrawal")
		if rec.Code != http.StatusConflict {
			t.Fatalf("PAYOUT_INSTRUMENT_NOT_USABLE status = %d, want 409", rec.Code)
		}
		if w.count(`SELECT count(*) FROM withdrawal_requests WHERE tenant_id = $1`, w.f.tenantID) != rows0 {
			t.Fatal("a refusal wrote a withdrawal row")
		}
		if n := w.count(`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, w.f.tenantID); n != start {
			t.Fatalf("a refusal posted %d ledger transaction(s)", n-start)
		}
		if n := w.count(`SELECT count(*) FROM withdrawal_requests WHERE tenant_id = $1 AND idempotency_key = 'r31-b-key'`, w.f.tenantID); n != 0 {
			t.Fatal("the idempotency key was consumed")
		}
		w.balanced()
	}
	// The two expiry instruments are bound together (max age 4s) so a single wait covers both.
	expByTime, expSwept := fresh(), fresh()
	t.Run("unverified (pending_verification)", func(t *testing.T) { refused(t, pending(), payoutinstrument.ReasonNotVerified) })
	t.Run("suspended", func(t *testing.T) {
		id := fresh()
		w.change(chSuspended, id)
		refused(t, id, payoutinstrument.ReasonNotVerified)
	})
	t.Run("revoked", func(t *testing.T) {
		id := fresh()
		w.change(chRevoked, id)
		refused(t, id, payoutinstrument.ReasonNotVerified)
	})
	w.waitExpiry(expSwept)
	t.Run("verification expired (by time, state still verified)", func(t *testing.T) {
		if s := w.instrumentState(expByTime); s != "verified" {
			t.Fatalf("state = %s", s)
		}
		refused(t, expByTime, payoutinstrument.ReasonVerificationExpired)
	})
	w.sweepExpiry()
	t.Run("verification expired (swept to verification_expired)", func(t *testing.T) {
		if s := w.instrumentState(expSwept); s != "verification_expired" {
			t.Fatalf("state = %s", s)
		}
		refused(t, expSwept, payoutinstrument.ReasonNotVerified)
	})
	// None of the refusals consumed the key and none left a hold: the same key succeeds with a usable instrument.
	good := fresh()
	err := w.rt.WithTenant(w.ctx(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := withdrawal.RequestWithdrawal(ctx, tx, withdrawal.RequestParams{
			TenantID: w.f.tenantID, BrandID: w.f.brandID, PlayerAccountID: w.f.playerAccountID, PersonID: w.f.personID,
			WalletID: w.f.walletID, AssetCode: "EUR", Amount: 400, IdempotencyKey: "r31-b-key", PayoutInstrumentID: good, Destinations: w.svc,
		})
		return err
	})
	if err != nil {
		t.Fatalf("the key must not have been consumed by the refusals: %v", err)
	}
	// The database is the second line (runtime role, direct SQL): a suspended or revoked instrument cannot be bound.
	for _, st := range []string{"suspended", "revoked"} {
		id := fresh()
		if st == "suspended" {
			w.change(chSuspended, id)
		} else {
			w.change(chRevoked, id)
		}
		code := w.rawInsertWithdrawal(w.f.playerAccountID, w.f.walletID, id, w.fingerprintOf(id), "r31-b-raw-"+st)
		if code != "PI042" {
			t.Fatalf("direct INSERT binding a %s instrument: SQLSTATE %q, want PI042", st, code)
		}
	}
	w.balanced()
}

// ---- (c) initiation / dispatch keep refusing an instrument that became unusable BEFORE dispatch (unchanged policy) ------------

func TestR31_C_DispatchStillRefusesAnInstrumentThatBecameUnusableBeforeDispatch(t *testing.T) {
	type tc struct {
		chg  r31Change
		gate string
	}
	for _, c := range []tc{
		{chSuspended, payoutinstrument.ReasonNotVerified},
		{chRevoked, payoutinstrument.ReasonNotVerified},
		{chExpired, payoutinstrument.ReasonVerificationExpired},
		{chExpiredSwep, payoutinstrument.ReasonNotVerified},
	} {
		t.Run("T1p/"+string(c.chg), func(t *testing.T) {
			maxAge := time.Duration(0)
			if c.chg == chExpired || c.chg == chExpiredSwep {
				maxAge = 4 * time.Second
			}
			w := newR31W(t, "c1", maxAge)
			wr := w.approvedOwner(500, "r31-c-t1p")
			w.onRuntime()
			w.change(c.chg, w.f.instrumentID)
			ledger := w.ledgerTx()
			_, err := w.claim(wr, "bank_transfer")
			if !errors.Is(err, ErrPayoutDestinationNotUsable) {
				t.Fatalf("T1p err = %v, want ErrPayoutDestinationNotUsable", err)
			}
			if g, ok := payoutinstrument.IsGateRefusal(err); !ok || g.Reason != c.gate {
				t.Fatalf("gate refusal = %+v, want %q", g, c.gate)
			}
			// Parked behaviour unchanged (HD-R15-5 stays OPEN): the request stays approved with its hold; no attempt, no
			// snapshot, no provider call, no posting.
			if got := w.wr(wr.ID); got.State != withdrawal.StateApproved {
				t.Fatalf("state = %s, want approved", got.State)
			}
			if countAttempts(t, w.pool, w.f.tenantID, wr.ID) != 0 || w.count(`SELECT count(*) FROM payout_attempt_destination_snapshots`) != 0 {
				t.Fatal("a refused T1p must leave no attempt and no snapshot")
			}
			if w.prov.calls() != 0 || w.ledgerTx() != ledger {
				t.Fatal("a refused T1p must make no provider call and post nothing")
			}
			w.balanced()
		})
		t.Run("phaseB/"+string(c.chg), func(t *testing.T) {
			maxAge := time.Duration(0)
			if c.chg == chExpired || c.chg == chExpiredSwep {
				maxAge = 4 * time.Second
			}
			w := newR31W(t, "c2", maxAge)
			wr := w.approvedOwner(500, "r31-c-pb")
			w.onRuntime()
			cl := w.mustClaim(wr) // authorised and snapshotted while the instrument is usable
			w.change(c.chg, w.f.instrumentID)
			w.prov.set(WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: "r31-c-pb", DestinationEcho: w.goodEcho(cl.Attempt.ID)}, StatusResult{})
			// Dispatch (phase B) is initiation: refused with NO provider call (NotSent class, T5 -> created).
			w.wantNotSent(w.dispatch(cl), "phase B after "+string(c.chg))
			w.balanced()
		})
	}
}

// ---- (d) the snapshot stays authoritative for the existing attempt; it is write-once ---------------------------------------------

func TestR31_D_SnapshotIsAuthoritativeAndWriteOnce(t *testing.T) {
	w := newR31W(t, "d", 0)
	wr := w.approvedOwner(500, "r31-d")
	w.onRuntime()
	cl := w.mustClaim(wr)
	inst := w.f.instrumentID
	before := w.snapshot(cl.Attempt.ID)

	// The runtime role cannot change the instrument's identity columns nor rewrite or delete the snapshot.
	for _, s := range []struct{ name, stmt, want string }{
		{"instrument mask", `UPDATE payout_instruments SET display_mask = 'XX****1111' WHERE id = $1`, "PI011"},
		{"instrument detail (label)", `UPDATE payout_instruments SET detail_ciphertext = decode(repeat('cd', 40), 'hex') WHERE id = $1`, "PI011"},
	} {
		if code := w.rtExecCode(s.stmt, inst); code != s.want {
			t.Fatalf("runtime UPDATE of %s: SQLSTATE %q, want %q", s.name, code, s.want)
		}
	}
	for _, stmt := range []string{
		`UPDATE payout_attempt_destination_snapshots SET display_mask = 'XX****1111' WHERE attempt_id = $1`,
		`UPDATE payout_attempt_destination_snapshots SET fingerprint = repeat('a', 64) WHERE attempt_id = $1`,
		`UPDATE payout_attempt_destination_snapshots SET instrument_id = gen_random_uuid() WHERE attempt_id = $1`,
		`UPDATE payout_attempt_destination_snapshots SET snapshot_seal = repeat('0', 64) WHERE attempt_id = $1`,
		`DELETE FROM payout_attempt_destination_snapshots WHERE attempt_id = $1`,
	} {
		if code := w.rtExecCode(stmt, cl.Attempt.ID); code == "" {
			t.Fatalf("the runtime role rewrote or deleted the snapshot: %s", stmt)
		}
	}
	// A second snapshot for the same attempt is refused as well (write-once: primary key).
	if code := w.rtExecCode(`INSERT INTO payout_attempt_destination_snapshots
		(attempt_id, tenant_id, withdrawal_request_id, instrument_id, kind, rail, fingerprint, fingerprint_kid, verification_id, verification_source,
		 ownership_assertion, verified_at, verification_expires_at, display_mask, amount, asset_code, snapshot_seal, seal_kid, created_txid)
		SELECT attempt_id, tenant_id, withdrawal_request_id, instrument_id, kind, rail, fingerprint, fingerprint_kid, verification_id, verification_source,
		 ownership_assertion, verified_at, verification_expires_at, display_mask, amount, asset_code, snapshot_seal, seal_kid, 0
		FROM payout_attempt_destination_snapshots WHERE attempt_id = $1`, cl.Attempt.ID); code == "" {
		t.Fatal("a second snapshot for the same attempt was accepted")
	}
	if after := w.snapshot(cl.Attempt.ID); !reflect.DeepEqual(after, before) {
		t.Fatalf("the snapshot changed:\n before %+v\n after  %+v", before, after)
	}

	// Now the instrument row is changed under the attempt: state (suspended), mask and label (privileged corruption).
	w.prov.set(WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: "r31-d", DestinationEcho: w.goodEcho(cl.Attempt.ID)}, StatusResult{})
	gr := w.dispatch(cl)
	w.change(chRowChanged, inst)
	if err := w.apply(wr, cl, gr); err != nil {
		t.Fatal(err)
	}
	// The attempt settled against its SNAPSHOT: the echo equals the snapshot fingerprint, the snapshot is unchanged, and
	// the live (changed) instrument took no part.
	if got := w.wr(wr.ID); got.State != withdrawal.StateCompleted {
		t.Fatalf("withdrawal = %s, want completed", got.State)
	}
	if after := w.snapshot(cl.Attempt.ID); !reflect.DeepEqual(after, before) || after.DisplayMask == "ZZ****0000" {
		t.Fatalf("the snapshot must still carry the original mask: %+v", after)
	}
	w.balanced()
}

// A second attempt: the evidence compares with the snapshot, never with the (changed) instrument. An echo that matches the
// snapshot settles; the same instrument being changed does not make a different echo acceptable.
func TestR31_D_EchoIsComparedWithTheSnapshotNotTheInstrument(t *testing.T) {
	w := newR31W(t, "d2", 0)
	wr := w.approvedOwner(500, "r31-d2")
	w.onRuntime()
	cl := w.mustClaim(wr)
	w.prov.set(WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: "r31-d2", DestinationEcho: w.badEcho(cl.Attempt.ID)}, StatusResult{})
	gr := w.dispatch(cl)
	w.change(chRowChanged, w.f.instrumentID)
	if err := w.apply(wr, cl, gr); err != nil {
		t.Fatal(err)
	}
	w.wantParked(wr, w.attempt(cl.Attempt.ID), TerminalReasonDestinationMismatch)
}

// ---- (e) no cross-player substitution ------------------------------------------------------------------------------------------

func (w *r31W) fingerprintOf(id uuid.UUID) string {
	w.t.Helper()
	var fp string
	if err := w.rt.WithTenant(w.ctx(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT fingerprint FROM payout_instruments WHERE id = $1`, id).Scan(&fp)
	}); err != nil {
		w.t.Fatal(err)
	}
	return fp
}

// rtExecCode runs one statement as the runtime role (tenant scope set) and returns the SQLSTATE of the failure ("" if
// it succeeded). A failure inside the transaction rolls it back.
func (w *r31W) rtExecCode(stmt string, args ...any) string {
	w.t.Helper()
	err := w.rt.WithTenant(w.ctx(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, stmt, args...)
		return err
	})
	if err == nil {
		return ""
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return "non-pg:" + err.Error()
}

func (w *r31W) rawInsertWithdrawal(player, wallet, instrument uuid.UUID, fp, key string) string {
	w.t.Helper()
	return w.rtExecCode(`INSERT INTO withdrawal_requests (tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, idempotency_key,
		payout_instrument_id, payout_instrument_fingerprint) VALUES ($1,$2,$3,$4,'EUR',100,$5,$6,$7)`,
		w.f.tenantID, w.f.brandID, player, wallet, key, instrument, fp)
}

func TestR31_E_NoCrossPlayerInstrumentSubstitution(t *testing.T) {
	w := newR31W(t, "e", 0)
	// Player B in the same tenant and brand, with its own verified instrument.
	pB, personB := uuid.New(), uuid.New()
	if err := w.owner.WithTenant(w.ctx(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personB); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			VALUES ($1,$2,$3,$4,$5,'x','active')`, pB, w.f.tenantID, w.f.brandID, personB, pB.String()+"@example.test")
		return err
	}); err != nil {
		t.Fatalf("seed player B: %v", err)
	}
	instB := pitest.Bind(t, w.owner, w.f.tenantID, pB, "EUR")
	fpB := w.fingerprintOf(instB)

	// (1) request: A cannot bind B's instrument (the gate's relation check), nothing is written.
	rows0 := w.count(`SELECT count(*) FROM withdrawal_requests WHERE tenant_id = $1`, w.f.tenantID)
	err := w.rt.WithTenant(w.ctx(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := withdrawal.RequestWithdrawal(ctx, tx, withdrawal.RequestParams{
			TenantID: w.f.tenantID, BrandID: w.f.brandID, PlayerAccountID: w.f.playerAccountID, PersonID: w.f.personID,
			WalletID: w.f.walletID, AssetCode: "EUR", Amount: 400, IdempotencyKey: "r31-e-x", PayoutInstrumentID: instB, Destinations: w.svc,
		})
		return err
	})
	if !errors.Is(err, withdrawal.ErrPayoutInstrumentNotUsable) {
		t.Fatalf("A binding B's instrument: %v, want ErrPayoutInstrumentNotUsable", err)
	}
	if w.count(`SELECT count(*) FROM withdrawal_requests WHERE tenant_id = $1`, w.f.tenantID) != rows0 {
		t.Fatal("a cross-player request wrote a row")
	}
	// (2) direct SQL as the runtime role: the binding guard refuses (PI041), the binding is immutable afterwards.
	if code := w.rawInsertWithdrawal(w.f.playerAccountID, w.f.walletID, instB, fpB, "r31-e-raw"); code != "PI041" {
		t.Fatalf("direct INSERT of A's withdrawal bound to B's instrument: SQLSTATE %q, want PI041", code)
	}
	wr := w.approvedOwner(500, "r31-e")
	w.onRuntime()
	if code := w.rtExecCode(`UPDATE withdrawal_requests SET payout_instrument_id = $2, payout_instrument_fingerprint = $3 WHERE id = $1`, wr.ID, instB, fpB); code == "" {
		t.Fatal("the runtime role re-bound A's withdrawal to B's instrument")
	}
	if got := w.wr(wr.ID); got.PayoutInstrumentID == nil || *got.PayoutInstrumentID != w.f.instrumentID {
		t.Fatalf("A's withdrawal binding changed: %v", got.PayoutInstrumentID)
	}
	// (3) staff body payment_method cannot influence the destination: a differing body is refused at T1p.
	if _, err := w.claim(wr, "card"); !errors.Is(err, ErrPaymentMethodMismatch) {
		t.Fatalf("a body naming another method: %v, want ErrPaymentMethodMismatch", err)
	}
	if countAttempts(t, w.pool, w.f.tenantID, wr.ID) != 0 {
		t.Fatal("a refused claim created an attempt")
	}
	cl := w.mustClaim(wr)
	// (4) a snapshot for A's withdrawal naming B's instrument is refused by the database (PI050), also as the runtime role.
	if code := w.rtExecCode(`INSERT INTO payout_attempt_destination_snapshots
		(attempt_id, tenant_id, withdrawal_request_id, instrument_id, kind, rail, fingerprint, fingerprint_kid, verification_id, verification_source,
		 ownership_assertion, verified_at, verification_expires_at, display_mask, amount, asset_code, snapshot_seal, seal_kid, created_txid)
		SELECT gen_random_uuid(), tenant_id, withdrawal_request_id, $2, kind, rail, $3, fingerprint_kid, verification_id, verification_source,
		 ownership_assertion, verified_at, verification_expires_at, display_mask, amount, asset_code, snapshot_seal, seal_kid, 0
		FROM payout_attempt_destination_snapshots WHERE attempt_id = $1`, cl.Attempt.ID, instB, fpB); code != "PI050" {
		t.Fatalf("a snapshot naming B's instrument for A's withdrawal: SQLSTATE %q, want PI050", code)
	}
	// (5) even privileged corruption that re-points A's withdrawal at B's instrument cannot make the evidence settle on
	// B's destination: the snapshot no longer equals the withdrawal, so the evidence PARKS (integrity), nothing settles.
	w.prov.set(WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: "r31-e", DestinationEcho: w.goodEcho(cl.Attempt.ID)}, StatusResult{})
	gr := w.dispatch(cl)
	w.tamperOwner("withdrawal_requests", `UPDATE withdrawal_requests SET payout_instrument_id = $2, payout_instrument_fingerprint = $3 WHERE id = $1`, wr.ID, instB, fpB)
	if err := w.apply(wr, cl, gr); err != nil {
		t.Fatal(err)
	}
	if a := w.attempt(cl.Attempt.ID); a.State != AttemptDisputed || a.TerminalReason == nil || *a.TerminalReason != TerminalReasonDestinationIntegrityFailure {
		t.Fatalf("attempt = %s reason=%v, want disputed/%s", a.State, a.TerminalReason, TerminalReasonDestinationIntegrityFailure)
	}
	if got := w.wr(wr.ID); got.State != withdrawal.StateSubmitted {
		t.Fatalf("withdrawal = %s: nothing may settle on a substituted destination", got.State)
	}
	w.balanced()
}

// ---- (f) a destination echo mismatch still parks, whatever the instrument's state ----------------------------------------------

func TestR31_F_EchoMismatchStillParksRegardlessOfInstrumentState(t *testing.T) {
	for _, chg := range []r31Change{chNone, chSuspended, chRevoked, chExpiredSwep} {
		for _, path := range []r31Path{pSync, pPoll, pCallback} {
			t.Run(string(chg)+"/"+string(path), func(t *testing.T) {
				maxAge := time.Duration(0)
				if chg == chExpiredSwep {
					maxAge = 4 * time.Second
				}
				w := newR31W(t, "f", maxAge)
				wr, cl := w.run("r31-f", path, OutcomeSucceeded, chg, echoBad)
				// disputed / destination_mismatch (NOT destination_integrity_failure, NOT succeeded), hold kept, nothing posted
				// beyond the original hold, P1 raised.
				w.wantParked(wr, w.attempt(cl.Attempt.ID), TerminalReasonDestinationMismatch)
				o := w.outcome(wr, cl)
				if o.Withdrawal != "submitted" || o.CompletedAudit != 0 || o.Hold != 500 {
					t.Fatalf("a mismatch under %s must not settle and must keep the hold: %+v", chg, o)
				}
			})
		}
	}
	// A definite decline carrying a mismatching echo is parked as well, not released, under a suspended instrument.
	t.Run("suspended/sync/decline", func(t *testing.T) {
		w := newR31W(t, "f2", 0)
		wr, cl := w.run("r31-f-d", pSync, OutcomeDeclined, chSuspended, echoBad)
		w.wantParked(wr, w.attempt(cl.Attempt.ID), TerminalReasonDestinationMismatch)
	})
}

// ---- (a2) an ESCALATED ambiguous attempt still settles from a later callback --------------------------------------------------

// runAmbiguousEscalated drives an attempt to `ambiguous` (the provider's answer was ambiguous), applies the instrument change,
// and (when escalate) lets the sweeper's T12 pass run: the poll stays ambiguous, then the destination gate refuses the RESEND and
// escalates (T16). It asserts that nothing was resent, and then delivers a callback with the given outcome.
func (w *r31W) runAmbiguousEscalated(key string, outcome Outcome, chg r31Change) (withdrawal.WithdrawalRequest, ClaimResult) {
	w.t.Helper()
	w.prov.idem = true // the manifest permits a resend, so ONLY the destination gate can stop it
	inst := w.f.instrumentID
	wr := w.approvedOwner(500, key)
	w.onRuntime()
	cl := w.mustClaim(wr)
	ref := "r31-amb-" + key
	w.prov.set(WithdrawResult{Outcome: OutcomeAmbiguous, ProviderReference: ref}, StatusResult{ProviderReference: ref, Outcome: OutcomeAmbiguous})
	if err := w.apply(wr, cl, w.dispatch(cl)); err != nil {
		w.t.Fatalf("apply ambiguous: %v", err)
	}
	if a := w.attempt(cl.Attempt.ID); a.State != AttemptAmbiguous {
		w.t.Fatalf("attempt = %s, want ambiguous", a.State)
	}
	w.change(chg, inst)
	if chg != chNone {
		calls := w.prov.calls()
		if err := w.sweeper().processPayoutAttempt(w.ctx(), w.f.tenantID, w.attempt(cl.Attempt.ID)); err != nil {
			w.t.Fatalf("T12 pass: %v", err)
		}
		a := w.attempt(cl.Attempt.ID)
		if a.State != AttemptAmbiguous || a.EscalatedAt == nil {
			w.t.Fatalf("after the T12 gate attempt = %s escalated=%v, want ambiguous and escalated", a.State, a.EscalatedAt != nil)
		}
		if w.prov.calls() != calls || w.prov.calls() != 1 {
			w.t.Fatalf("provider Withdraw calls = %d (was %d): the escalation must resend nothing", w.prov.calls(), calls)
		}
		if n := w.count(`SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action='payments.payout_destination_gate_denied' AND target_id=$2`, w.f.tenantID, cl.Attempt.ID.String()); n != 1 {
			w.t.Fatalf("destination gate denial audit rows = %d, want 1", n)
		}
		if o := w.outcome(wr, cl); o.Withdrawal != "submitted" || o.Hold != 500 {
			w.t.Fatalf("an escalation must keep the hold: %+v", o)
		}
	}
	if err := w.callbackDecl(w.attempt(cl.Attempt.ID), outcome, ref, w.goodEcho(cl.Attempt.ID)); err != nil {
		w.t.Fatalf("callback: %v", err)
	}
	if w.prov.calls() != 1 {
		w.t.Fatalf("provider Withdraw calls = %d after settlement, want 1 (no resend, ever)", w.prov.calls())
	}
	return wr, cl
}

func TestR31_A2_EscalatedAmbiguousAttemptStillSettlesFromACallback(t *testing.T) {
	baseline := map[Outcome]r31Outcome{}
	for _, out := range []Outcome{OutcomeSucceeded, OutcomeDeclined} {
		t.Run("baseline/"+string(out), func(t *testing.T) {
			w := newR31W(t, "g0", 0)
			wr, cl := w.runAmbiguousEscalated("r31-a2-base", out, chNone)
			o := w.outcome(wr, cl)
			if out == OutcomeSucceeded && (o.Withdrawal != "completed" || o.Attempt != "succeeded" || o.CompletedAudit != 1 || o.Hold != 0 || o.Cash != 99_500) ||
				out == OutcomeDeclined && (o.Withdrawal != "failed" || o.Attempt != "declined" || o.FailedAudit != 1 || o.Hold != 0 || o.Cash != 100_000) {
				t.Fatalf("baseline outcome unexpected: %+v", o)
			}
			w.balanced()
			baseline[out] = o
		})
	}
	for _, chg := range []r31Change{chSuspended, chRevoked, chExpiredSwep, chExpired} {
		for _, out := range []Outcome{OutcomeSucceeded, OutcomeDeclined} {
			t.Run(string(chg)+"/"+string(out), func(t *testing.T) {
				base, ok := baseline[out]
				if !ok {
					t.Fatal("baseline did not run")
				}
				maxAge := time.Duration(0)
				if chg == chExpired || chg == chExpiredSwep {
					maxAge = 4 * time.Second
				}
				w := newR31W(t, "g1", maxAge)
				wr, cl := w.runAmbiguousEscalated("r31-a2-"+string(chg), out, chg)
				got := w.outcome(wr, cl)
				if !reflect.DeepEqual(got, base) {
					t.Fatalf("escalated ambiguous attempt after %s settled differently from an untouched instrument:\n got  %+v\n want %+v", chg, got, base)
				}
				if got.ParkAudit != 0 {
					t.Fatalf("a settlement must not park: %+v", got)
				}
				w.balanced()
			})
		}
	}
}
