//go:build integration

package reconciliation

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

// MA020-SYNC-MISMATCH-1 (migration 0119, Class-B B4): the Go-vs-SQL parity of
// the preventive MA020 exposure check. For every deposit attempt of the tenant
// and provider, in ONE REPEATABLE READ snapshot opened by the REAL runtime role
// (asserted neither superuser nor BYPASSRLS):
//
//	Go  := a.boundCapture() && m.capturedUnposted(a)   (the recon rule, loaded by
//	       loadPlatform + loadK3Evidence exactly as RunPaymentStatement loads it)
//	SQL := payment_attempt_open_exposure(tenant, a.id)
//
// must be equal, and player_open_payment_exposure must be the OR over the
// player's attempts. The ONE documented asymmetry is F-VIS: a
// poll_reference_mismatch park with no visible typed Y row is OPEN in SQL (the
// K2 sessions cannot read the Y table, so the SQL fails closed), while Go
// applies the X-only rule; there SQL must be true and Go => SQL. Each fixture
// also pins its own expected value, so parity cannot pass with both sides wrong.

const ma020Amount = int64(1000)

func ma020RuntimePool(t *testing.T) *db.Pool {
	t.Helper()
	u := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL not set; the parity test must run as the runtime role")
	}
	rt, err := db.Connect(context.Background(), u, 4, 5*time.Second)
	if err != nil {
		t.Fatalf("connect runtime role: %v", err)
	}
	t.Cleanup(rt.Close)
	var super, bypass bool
	if err := rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass)
	}); err != nil {
		t.Fatal(err)
	}
	if super || bypass {
		t.Fatalf("vacuity: the parity session role is rolsuper=%v rolbypassrls=%v", super, bypass)
	}
	return rt
}

// ma020Park writes a disputed deposit attempt for the world's player through the
// payment_attempts guard: created -> submitting -> [pending with ref] ->
// [typed Y evidence, in the park's own transaction] -> disputed(reason).
// ref == "" leaves the park reference-less (the legacy / pre-B3 form).
func (w *payWorld) ma020Park(t *testing.T, reason, ref, y string) uuid.UUID {
	t.Helper()
	intent, attempt := uuid.New(), uuid.New()
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO deposit_intents (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, payment_method, idempotency_key)
			VALUES ($1, $2, $3, $4, $5, 'EUR', $6, 'card', $7)`, intent, w.f.tenantID, w.f.brandID, w.f.playerAccountID, w.f.walletID, ma020Amount, "ma020-"+intent.String()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO payment_attempts (id, tenant_id, operation, deposit_intent_id, attempt_no, provider_id, payment_method, asset_code, amount,
			interactive, merchant_reference, external_idempotency_key, state, last_evidence_kind)
			VALUES ($1, $2, 'deposit', $3, 1, $4, 'card', 'EUR', $5, false, $6, $7, 'created', 'platform')`,
			attempt, w.f.tenantID, intent, payProvA, ma020Amount, "ma020m-"+attempt.String()[:20], "ma020e-"+attempt.String()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE payment_attempts SET state = 'submitting', ever_possibly_sent = true, submit_count = 1, first_submitted_at = now(), last_sent_at = now() WHERE id = $1`, attempt); err != nil {
			return err
		}
		if ref != "" {
			if _, err := tx.Exec(ctx, `UPDATE payment_attempts SET state = 'pending', provider_reference = $2, last_evidence_kind = 'sync' WHERE id = $1`, attempt, ref); err != nil {
				return err
			}
		}
		if y != "" {
			if _, err := tx.Exec(ctx, `INSERT INTO payment_attempt_reference_evidence (tenant_id, attempt_id, provider_id, evidence_kind, reference)
				VALUES ($1, $2, $3, 'poll_returned_reference', $4)`, w.f.tenantID, attempt, payProvA, y); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE payment_attempts SET state = 'disputed', terminal_reason = $2, last_evidence_kind = 'callback', next_action_at = NULL WHERE id = $1`, attempt, reason)
		return err
	}); err != nil {
		t.Fatalf("park %s ref=%q y=%q: %v", reason, ref, y, err)
	}
	return attempt
}

// ma020Tombstone posts the refund tombstone keyed on ref (LF K2-a).
func (w *payWorld) ma020Tombstone(t *testing.T, ref string) { w.ma020TombstoneFor(t, payProvA, ref) }

func (w *payWorld) ma020TombstoneFor(t *testing.T, provider, ref string) {
	t.Helper()
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		p := provider
		_, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: w.f.tenantID, TransactionType: ledger.TxTombstone,
			IdempotencyKey: "ma020-tomb:" + provider + ":" + ref, ProviderID: &p, ProviderTxID: &ref, CorrelationID: uuid.New()})
		return err
	}); err != nil {
		t.Fatalf("tombstone %s: %v", ref, err)
	}
}

var ma020Offset = 0

// ma020Import stores one statement (MOCK unless real) through the real fetch +
// ingest path.
func (w *payWorld) ma020Import(t *testing.T, real bool, lines ...statement.PaymentStatementLine) {
	t.Helper()
	ma020Offset++
	src := k3Cov(ma020Offset, lines...)
	if real {
		w.fetchIngest(t, d2RealSource{src}, PaymentStatementOptions{})
		return
	}
	w.fetchIngest(t, src, PaymentStatementOptions{})
}

func ma020Rev(original string) statement.PaymentStatementLine {
	return d2ReversalLine("ma020-rev-"+uuid.NewString(), original, ma020Amount)
}

func ma020Succ(ref string) statement.PaymentStatementLine {
	return d2Line(ref, "", statement.PaymentStatusSucceeded, ma020Amount)
}

func ma020Ref(tag string) string { return "ma020-" + tag + "-" + uuid.NewString()[:13] }

// ma020Parity evaluates both sides in one runtime-role snapshot and returns the
// SQL value per deposit attempt.
func ma020Parity(t *testing.T, rt *db.Pool, w *payWorld) map[uuid.UUID]bool {
	t.Helper()
	out := map[uuid.UUID]bool{}
	if err := rt.WithTenantSnapshot(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		m := &payMatcher{tenantID: w.f.tenantID, provider: payProvA, r: &sbRecorder{tenantID: w.f.tenantID}}
		if err := m.loadPlatform(ctx, tx); err != nil {
			return err
		}
		if err := m.loadK3Evidence(ctx, tx, true); err != nil {
			return err
		}
		m.matchLines(nil) // persisted eligible reversal originals -> reversalOriginals
		var visibleY int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM payment_attempt_reference_evidence WHERE tenant_id = $1`, w.f.tenantID).Scan(&visibleY); err != nil {
			return err
		}
		if visibleY != len(m.k3.yRef) {
			t.Fatalf("snapshot sees %d Y rows, Go loaded %d", visibleY, len(m.k3.yRef))
		}
		byPlayer := map[uuid.UUID]bool{}
		for _, a := range m.attempts {
			var sqlV bool
			if err := tx.QueryRow(ctx, `SELECT payment_attempt_open_exposure($1, $2)`, w.f.tenantID, a.id).Scan(&sqlV); err != nil {
				return err
			}
			if a.operation != "deposit" {
				// MA020 is a deposit control (0113 scope); Go's boundCapture has no
				// operation filter. Documented scope, pinned here.
				if sqlV {
					t.Errorf("attempt %s op=%s: SQL exposure must be false for a non-deposit attempt", a.id, a.operation)
				}
				continue
			}
			goV := a.boundCapture() && m.capturedUnposted(a)
			_, hasY := m.k3.yRef[a.id]
			if a.state == "disputed" && a.terminalReason == "poll_reference_mismatch" && a.providerRef != "" && !hasY {
				// F-VIS asymmetry: SQL is stricter.
				if !sqlV {
					t.Errorf("attempt %s: poll_reference_mismatch with no visible Y must be open in SQL (F-VIS), Go=%v", a.id, goV)
				}
			} else if goV != sqlV {
				t.Errorf("PARITY attempt %s reason=%q ref=%q state=%s: Go=%v SQL=%v", a.id, a.terminalReason, a.providerRef, a.state, goV, sqlV)
			}
			out[a.id] = sqlV
			if a.depositIntent != nil {
				var player uuid.UUID
				if err := tx.QueryRow(ctx, `SELECT player_account_id FROM deposit_intents WHERE id = $1`, *a.depositIntent).Scan(&player); err != nil {
					return err
				}
				byPlayer[player] = byPlayer[player] || sqlV
			}
		}
		for player, want := range byPlayer {
			var got bool
			if err := tx.QueryRow(ctx, `SELECT player_open_payment_exposure($1, $2)`, w.f.tenantID, player).Scan(&got); err != nil {
				return err
			}
			if got != want {
				t.Errorf("player %s: player_open_payment_exposure=%v, OR over attempts=%v", player, got, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("parity snapshot: %v", err)
	}
	return out
}

func ma020Want(t *testing.T, got map[uuid.UUID]bool, want map[uuid.UUID]bool, names map[uuid.UUID]string) {
	t.Helper()
	for id, w := range want {
		g, ok := got[id]
		if !ok {
			t.Errorf("%s (%s): attempt not loaded", names[id], id)
			continue
		}
		if g != w {
			t.Errorf("%s: SQL exposure=%v, want %v", names[id], g, w)
		}
	}
}

// The reason pin (R-MA-1) plus R-MA-2: every reason payments can write, with and
// without a reference, nothing cleared. SQL counts it exactly when recon
// classifies it bound or bound-if-referenced AND the attempt holds a reference.
func TestMA020_Parity_ReasonPinAndReferenceRule(t *testing.T) {
	rt := ma020RuntimePool(t)
	w := newPayWorld(t)
	want, names := map[uuid.UUID]bool{}, map[uuid.UUID]string{}
	for _, r := range d2ReasonsToCheck(t) {
		c := classifyDisputeReason(r)
		counted := c == reasonBound || c == reasonBoundIfReferenced
		id := w.ma020Park(t, r, ma020Ref("pin"), "")
		names[id], want[id] = r+" with reference", counted
		if r == payments.TerminalReasonPollReferenceMismatch {
			// no Y row: F-VIS open (and Go agrees here: X is not cleared)
			want[id] = true
		}
		id = w.ma020Park(t, r, "", "")
		names[id], want[id] = r+" reference-less", false
	}
	ma020Want(t, ma020Parity(t, rt, w), want, names)

	// The SQL list is exactly the counted set: no extra literal hides in the body.
	var def string
	if err := rt.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT pg_get_functiondef('payment_attempt_open_exposure(uuid, uuid)'::regprocedure)`).Scan(&def)
	}); err != nil {
		t.Fatal(err)
	}
	for r, c := range disputeReasonClasses {
		in := strings.Contains(def, "'"+r+"'")
		counted := c == reasonBound || c == reasonBoundIfReferenced
		if in != counted {
			t.Errorf("reason %q: in SQL list=%v, recon counts=%v", r, in, counted)
		}
	}
}

// X-only clearing, the non-disputed and non-deposit scopes, and RC-3 clearing by
// a MOCK reversal line while no real import exists.
func TestMA020_Parity_XClearingAndScope(t *testing.T) {
	rt := ma020RuntimePool(t)
	w := newPayWorld(t)
	want, names := map[uuid.UUID]bool{}, map[uuid.UUID]string{}
	add := func(name string, id uuid.UUID, v bool) { names[id], want[id] = name, v }

	add("X only, uncleared", w.ma020Park(t, payments.TerminalReasonSyncAmountMismatch, ma020Ref("x"), ""), true)
	xt := ma020Ref("xt")
	add("X tombstoned", w.ma020Park(t, payments.TerminalReasonSyncAmountMismatch, xt, ""), false)
	w.ma020Tombstone(t, xt)
	xr := ma020Ref("xr")
	add("X reversed by a MOCK line, no real import (RC-3 eligible)", w.ma020Park(t, payments.TerminalReasonPollAmountMismatch, xr, ""), false)
	xp := ma020Ref("xp")
	add("X evidenced by a succeeded line only (evidence is not a clear)", w.ma020Park(t, payments.TerminalReasonCallbackAmountAssetMismatch, xp, ""), true)
	w.ma020Import(t, false, ma020Rev(xr), ma020Succ(xp), ma020Rev(ma020Ref("other")))
	// A poll_reference_mismatch with no Y row and X tombstoned: the F-VIS asymmetry
	// (Go clears on X, SQL stays open).
	xf := ma020Ref("xf")
	add("poll_reference_mismatch, no Y, X tombstoned (F-VIS)", w.ma020Park(t, payments.TerminalReasonPollReferenceMismatch, xf, ""), true)
	w.ma020Tombstone(t, xf)
	// The same X string tombstoned and reversed under ANOTHER provider of the
	// tenant clears nothing (clearing is per (provider, reference)).
	xo := ma020Ref("xo")
	add("X tombstoned and reversed under another provider", w.ma020Park(t, payments.TerminalReasonSyncAmountMismatch, xo, ""), true)
	w.ma020TombstoneFor(t, payProvB, xo)
	revB := payLineFor(payProvB, "ma020-revb-"+uuid.NewString()[:12], "", statement.PaymentLineDepositReversal, statement.PaymentStatusSucceeded, ma020Amount)
	revB.OriginalProviderReference = xo
	w.fetchIngest(t, payFixedSource{provider: payProvB, stmt: wideCoverage(revB)}, PaymentStatementOptions{})
	// Non-disputed attempts never count.
	pend := w.deposit(t, ma020Amount)
	add("pending bound attempt", pend.ID, false)
	// A declined attempt that carries a counted reason string: only 'disputed' counts.
	dec := w.deposit(t, ma020Amount)
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment_attempts SET state = 'declined', terminal_reason = 'sync_amount_mismatch', last_evidence_kind = 'callback', next_action_at = NULL WHERE id = $1`, dec.ID)
		return err
	}); err != nil {
		t.Fatalf("declined fixture: %v", err)
	}
	add("declined attempt with a counted reason string", dec.ID, false)
	// A payout attempt carrying a deposit reason: out of MA020 scope.
	po := w.payoutFixture(t, payProvA, ma020Ref("po"), "", 500, false)
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment_attempts SET state = 'disputed', terminal_reason = 'sync_amount_mismatch', last_evidence_kind = 'callback', next_action_at = NULL WHERE id = $1`, po)
		return err
	}); err != nil {
		t.Fatalf("payout park: %v", err)
	}
	got := ma020Parity(t, rt, w)
	ma020Want(t, got, want, names)
	if _, loaded := got[po]; loaded {
		t.Fatal("a payout attempt must not be in the deposit parity set")
	}
}

// Y attribution (G-Y1 a..e, G-Y3, security F-1): only an unheld Y clears.
func TestMA020_Parity_YAttribution(t *testing.T) {
	rt := ma020RuntimePool(t)
	w := newPayWorld(t)
	want, names := map[uuid.UUID]bool{}, map[uuid.UUID]string{}
	add := func(name string, id uuid.UUID, v bool) { names[id], want[id] = name, v }
	var revs []statement.PaymentStatementLine

	// Y held by a posted (succeeded) attempt.
	b := w.deposit(t, ma020Amount)
	w.succeed(t, w.mockA, payProvA, b)
	yb := *b.ProviderReference
	add("Y held by a posted attempt, Y reversed", w.ma020Park(t, payments.TerminalReasonPollReferenceMismatch, ma020Ref("x1"), yb), true)
	revs = append(revs, ma020Rev(yb))
	// Y held by an unposted (pending) attempt.
	c := w.deposit(t, ma020Amount)
	yc := *c.ProviderReference
	add("Y held by an unposted attempt, Y reversed", w.ma020Park(t, payments.TerminalReasonPollReferenceMismatch, ma020Ref("x2"), yc), true)
	revs = append(revs, ma020Rev(yc))
	// Y is a withdrawal_completed key (payout settlement reference).
	ys := ma020Ref("settle")
	w.payoutFixture(t, payProvA, ma020Ref("instr"), ys, 300, true)
	add("Y = withdrawal_completed key, Y reversed", w.ma020Park(t, payments.TerminalReasonPollReferenceMismatch, ma020Ref("x3"), ys), true)
	revs = append(revs, ma020Rev(ys))
	// Y is a deposit_reversal key.
	d := w.deposit(t, ma020Amount)
	w.succeed(t, w.mockA, payProvA, d)
	yr := ma020Ref("revkey")
	w.deliverReversal(t, yr, *d.ProviderReference, ma020Amount)
	add("Y = deposit_reversal key, Y reversed", w.ma020Park(t, payments.TerminalReasonPollReferenceMismatch, ma020Ref("x4"), yr), true)
	revs = append(revs, ma020Rev(yr))
	// Y is a legacy attempt-less deposit key.
	yl := w.legacyDeposit(t, ma020Amount)
	add("Y = legacy attempt-less deposit key, Y reversed", w.ma020Park(t, payments.TerminalReasonPollReferenceMismatch, ma020Ref("x5"), yl), true)
	revs = append(revs, ma020Rev(yl))
	// G-Y3: tombstone on an unheld Y clears.
	yt := ma020Ref("ytomb")
	add("unheld Y tombstoned (G-Y3)", w.ma020Park(t, payments.TerminalReasonPollReferenceMismatch, ma020Ref("x6"), yt), false)
	w.ma020Tombstone(t, yt)
	// Unheld Y reversed by a MOCK line clears.
	yu := ma020Ref("yrev")
	add("unheld Y reversed", w.ma020Park(t, payments.TerminalReasonPollReferenceMismatch, ma020Ref("x7"), yu), false)
	revs = append(revs, ma020Rev(yu))
	// Unheld Y, nothing cleared.
	add("unheld Y, nothing cleared", w.ma020Park(t, payments.TerminalReasonPollReferenceMismatch, ma020Ref("x8"), ma020Ref("yopen")), true)
	// Security F-1: the same Y recorded by two parks: one reversal clears neither.
	ysh := ma020Ref("shared")
	add("shared Y park 1", w.ma020Park(t, payments.TerminalReasonPollReferenceMismatch, ma020Ref("x9"), ysh), true)
	add("shared Y park 2", w.ma020Park(t, payments.TerminalReasonPollReferenceMismatch, ma020Ref("x10"), ysh), true)
	revs = append(revs, ma020Rev(ysh))
	// Held Y: X still clears.
	xh := ma020Ref("x11")
	add("held Y, X reversed", w.ma020Park(t, payments.TerminalReasonPollReferenceMismatch, xh, yb), false)
	revs = append(revs, ma020Rev(xh))

	w.ma020Import(t, false, revs...)
	ma020Want(t, ma020Parity(t, rt, w), want, names)
}

// G-Y2 (both evidenced: both must clear) and its one-sided variants, plus LF
// D-7 (a pending/declined line is not evidence).
func TestMA020_Parity_GY2(t *testing.T) {
	rt := ma020RuntimePool(t)
	w := newPayWorld(t)
	want, names := map[uuid.UUID]bool{}, map[uuid.UUID]string{}
	add := func(name string, id uuid.UUID, v bool) { names[id], want[id] = name, v }
	var lines []statement.PaymentStatementLine
	park := func(name string, evX, evY, revX, revY bool, v bool) {
		x, y := ma020Ref("gx"), ma020Ref("gy")
		add(name, w.ma020Park(t, payments.TerminalReasonPollReferenceMismatch, x, y), v)
		if evX {
			lines = append(lines, ma020Succ(x))
		}
		if evY {
			lines = append(lines, ma020Succ(y))
		}
		if revX {
			lines = append(lines, ma020Rev(x))
		}
		if revY {
			lines = append(lines, ma020Rev(y))
		}
	}
	park("both evidenced, Y-only reversal", true, true, false, true, true)
	park("both evidenced, X-only reversal", true, true, true, false, true)
	park("both evidenced, both reversed", true, true, true, true, false)
	park("both evidenced, nothing reversed", true, true, false, false, true)
	park("only Y evidenced, Y reversed", false, true, false, true, false)
	park("only X evidenced, X reversed", true, false, true, false, false)
	park("nothing evidenced, Y reversed", false, false, false, true, false)
	for _, st := range []string{statement.PaymentStatusPending, statement.PaymentStatusDeclined} {
		x, y := ma020Ref("dx"), ma020Ref("dy")
		add("X succeeded, Y only "+st+", X reversed (D-7)", w.ma020Park(t, payments.TerminalReasonPollReferenceMismatch, x, y), false)
		lines = append(lines, ma020Succ(x), d2Line(y, "", st, ma020Amount), ma020Rev(x))
	}
	// G-Y2 by tombstones (no reversal lines): both evidenced, X tombstoned only.
	tx, ty := ma020Ref("tx"), ma020Ref("ty")
	add("both evidenced, X tombstoned only", w.ma020Park(t, payments.TerminalReasonPollReferenceMismatch, tx, ty), true)
	lines = append(lines, ma020Succ(tx), ma020Succ(ty))
	w.ma020Import(t, false, lines...)
	w.ma020Tombstone(t, tx)
	ma020Want(t, ma020Parity(t, rt, w), want, names)
	w.ma020Tombstone(t, ty)
	want[func() uuid.UUID {
		for id, n := range names {
			if n == "both evidenced, X tombstoned only" {
				return id
			}
		}
		t.Fatal("fixture lost")
		return uuid.Nil
	}()] = false
	ma020Want(t, ma020Parity(t, rt, w), want, names)
}

// RC-3 in both orders: a MOCK line may clear (and evidence) only while no real
// import exists for the tenant and provider.
func TestMA020_Parity_RC3MockVsRealImport(t *testing.T) {
	rt := ma020RuntimePool(t)
	t.Run("mock reversal first, then a real import appears", func(t *testing.T) {
		w := newPayWorld(t)
		x := ma020Ref("rc3a")
		id := w.ma020Park(t, payments.TerminalReasonSyncAmountMismatch, x, "")
		w.ma020Import(t, false, ma020Rev(x))
		ma020Want(t, ma020Parity(t, rt, w), map[uuid.UUID]bool{id: false}, map[uuid.UUID]string{id: "MOCK reversal, no real import"})
		w.ma020Import(t, true, ma020Succ(ma020Ref("unrelated")))
		ma020Want(t, ma020Parity(t, rt, w), map[uuid.UUID]bool{id: true}, map[uuid.UUID]string{id: "MOCK reversal after a real import exists"})
		w.ma020Import(t, true, ma020Rev(x))
		ma020Want(t, ma020Parity(t, rt, w), map[uuid.UUID]bool{id: false}, map[uuid.UUID]string{id: "real reversal"})
	})
	t.Run("real import first, then a mock reversal", func(t *testing.T) {
		w := newPayWorld(t)
		x := ma020Ref("rc3b")
		id := w.ma020Park(t, payments.TerminalReasonMultipleSuccessForIntent, x, "")
		w.ma020Import(t, true, ma020Succ(ma020Ref("unrelated")))
		w.ma020Import(t, false, ma020Rev(x))
		ma020Want(t, ma020Parity(t, rt, w), map[uuid.UUID]bool{id: true}, map[uuid.UUID]string{id: "MOCK reversal ineligible"})
	})
	t.Run("G-Y2 evidence only from an eligible import", func(t *testing.T) {
		w := newPayWorld(t)
		x, y := ma020Ref("rc3x"), ma020Ref("rc3y")
		id := w.ma020Park(t, payments.TerminalReasonPollReferenceMismatch, x, y)
		w.ma020Import(t, true, ma020Succ(x))  // real: X evidenced
		w.ma020Import(t, false, ma020Succ(y)) // MOCK: Y line ineligible
		w.ma020Import(t, true, ma020Rev(y))   // real reversal on Y: X-or-Y rule clears
		ma020Want(t, ma020Parity(t, rt, w), map[uuid.UUID]bool{id: false}, map[uuid.UUID]string{id: "Y evidenced only by ineligible MOCK line"})
	})
}

// Tenant isolation: the same strings in another tenant neither block nor clear,
// and the function called with a foreign tenant id sees nothing.
func TestMA020_Parity_TenantIsolation(t *testing.T) {
	rt := ma020RuntimePool(t)
	w1 := newPayWorld(t)
	w2 := newPayWorld(t)
	x, y := ma020Ref("tix"), ma020Ref("tiy")
	p := w1.ma020Park(t, payments.TerminalReasonPollReferenceMismatch, x, y)
	// Tenant 2: a tombstone on X's string, and its own park bound to Y's string,
	// cleared there by a reversal line on Y's string.
	w2.ma020Tombstone(t, x)
	p2 := w2.ma020Park(t, payments.TerminalReasonSyncAmountMismatch, y, "")
	w2.ma020Import(t, false, ma020Rev(y))
	names := map[uuid.UUID]string{p: "tenant 1 park"}
	ma020Want(t, ma020Parity(t, rt, w1), map[uuid.UUID]bool{p: true}, names)
	ma020Want(t, ma020Parity(t, rt, w2), map[uuid.UUID]bool{p2: false}, map[uuid.UUID]string{p2: "tenant 2 park cleared by its own reversal"})
	// Y is still attributable in tenant 1 (tenant 2's holder is not tenant 1's): a
	// tenant-1 tombstone on Y clears tenant 1's park.
	w1.ma020Tombstone(t, y)
	ma020Want(t, ma020Parity(t, rt, w1), map[uuid.UUID]bool{p: false}, names)
	// A foreign tenant id in tenant 1's session sees nothing.
	p3 := w2.ma020Park(t, payments.TerminalReasonSyncAmountMismatch, ma020Ref("open"), "")
	var v bool
	if err := rt.WithTenantSnapshot(context.Background(), w1.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT payment_attempt_open_exposure($1, $2) OR player_open_payment_exposure($1, $3)`, w2.f.tenantID, p3, w2.f.playerAccountID).Scan(&v)
	}); err != nil {
		t.Fatal(err)
	}
	if v {
		t.Fatal("a foreign tenant's exposure must not be readable from another tenant's session")
	}
}

// The function shapes (TRIGGER-SEARCH-PATH-1 discipline): STABLE, not SECURITY
// DEFINER, search_path pinned, owned by the migration role (the owner of
// payment_attempts), and EXECUTE-able by the runtime role.
func TestMA020_0119_FunctionShape(t *testing.T) {
	rt := ma020RuntimePool(t)
	for _, sig := range []string{
		"payment_ref_cleared(uuid, text, text)",
		"payment_ref_evidenced(uuid, text, text)",
		"payment_y_attributable(uuid, text, uuid, text)",
		"payment_attempt_open_exposure(uuid, uuid)",
		"player_open_payment_exposure(uuid, uuid)",
	} {
		var secdef, exec bool
		var vol, owner, tableOwner string
		var cfg []string
		if err := rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT p.prosecdef, p.provolatile::text, COALESCE(p.proconfig, '{}'), pg_get_userbyid(p.proowner),
				(SELECT pg_get_userbyid(c.relowner) FROM pg_class c WHERE c.oid = 'public.payment_attempts'::regclass),
				has_function_privilege(current_user, p.oid, 'EXECUTE')
				FROM pg_proc p WHERE p.oid = $1::regprocedure`, sig).Scan(&secdef, &vol, &cfg, &owner, &tableOwner, &exec)
		}); err != nil {
			t.Fatalf("%s: %v", sig, err)
		}
		if secdef || vol != "s" || owner != tableOwner || !exec ||
			len(cfg) != 1 || cfg[0] != "search_path=pg_catalog, public, pg_temp" {
			t.Errorf("%s: secdef=%v volatile=%s owner=%s (want %s) exec=%v config=%v", sig, secdef, vol, owner, tableOwner, exec, cfg)
		}
	}
}

// Down then up on a throwaway database migrated through 0119: the down restores
// the exact 0113 body (no pin) and removes the helpers; the re-up and verify are
// clean. The only tolerated gap is 0118 (reserved for the threat-model
// mitigation, not yet merged).
func TestMA020_0119_DownUpRoundTrip(t *testing.T) {
	src := "../../migrations"
	dir := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	var orig0113 string
	for _, e := range entries {
		n, perr := strconv.ParseInt(e.Name()[:4], 10, 64)
		if perr != nil || n > 119 {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(src, e.Name()))
		if e.Name() == "0113_governed_manual_adjustments.up.sql" {
			s := string(b)
			i := strings.Index(s, "CREATE FUNCTION player_open_payment_exposure")
			j := strings.Index(s[i:], "$$ LANGUAGE sql STABLE;")
			orig0113 = s[i : i+j]
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pool, err := db.Connect(context.Background(), scratchdb.New(t, "ma020_rt"), 4, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ctx := context.Background()
	if applied, err := pool.MigrateUp(ctx, dir); err != nil || applied[len(applied)-1] != 119 {
		t.Fatalf("migrate through 0119: %v %v", applied, err)
	}
	if _, err := pool.MigrateDown(ctx, dir, 1); err != nil {
		t.Fatalf("down: %v", err)
	}
	var body string
	var cfg []string
	var helpers int
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT prosrc, COALESCE(proconfig, '{}') FROM pg_proc WHERE oid = 'player_open_payment_exposure(uuid, uuid)'::regprocedure`).Scan(&body, &cfg); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM pg_proc WHERE proname IN ('payment_ref_cleared', 'payment_ref_evidenced', 'payment_y_attributable', 'payment_attempt_open_exposure')`).Scan(&helpers)
	}); err != nil {
		t.Fatal(err)
	}
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	if orig0113 == "" || !strings.Contains(norm(orig0113), norm(body)) || len(cfg) != 0 || helpers != 0 {
		t.Fatalf("down did not restore 0113: body=%q config=%v helpers=%d", body, cfg, helpers)
	}
	if applied, err := pool.MigrateUp(ctx, dir); err != nil || len(applied) != 1 || applied[0] != 119 {
		t.Fatalf("re-up: %v %v", applied, err)
	}
	report, err := pool.VerifyMigrations(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range report.Results {
		if res.Status == db.MigrationCheckMismatch || res.Status == db.MigrationCheckMissingFile {
			t.Fatalf("checksum drift after the round trip: %+v", res)
		}
	}
	for _, g := range report.VersionGaps {
		if !strings.Contains(g, "version 118 ") {
			t.Fatalf("unexpected migration gap %v", report.VersionGaps)
		}
	}
}
