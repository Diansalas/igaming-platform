//go:build integration

// B5 (PAY-POLL-ECHO-HARDENING-1): provider-echoed strings are attacker-influenced and audit is
// append-only. Asset-code echoes are stored raw only when they have the asset-code shape
// (otherwise length + hash prefix); the unbound-attempt poll fallbacks run the foreign-binding
// check (no loop on a foreign-held echo); a differing echo on a bound Pending poll is audited;
// the bare invalid_provider_reference reason is a classified deposit dispute reason; a Missing
// poll reschedules into the future (Z4b).
package payments

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func b5AuditMetaText(t *testing.T, e *depRefEnv) string {
	t.Helper()
	return depScan[string](t, e.pool, e.f.tenantID,
		`SELECT COALESCE(string_agg(metadata::text, E'\n'), '') FROM audit_log WHERE tenant_id = $1`, e.f.tenantID)
}

// A hostile asset echo (control characters, markup, oversized, non-ASCII) is NEVER stored raw
// on the poll mismatch park, the poll Missing audit, or - for the shared helper - anywhere;
// a well-formed foreign asset code (USD) is still stored, so the evidence stays useful.
func TestB5_PollAssetEcho_HostileNeverStoredRaw_ValidStored(t *testing.T) {
	pool := testPool(t)
	hostile := []string{
		"EUR\n<script>alert(1)</script>",
		strings.Repeat("X", 300),
		"€UR‮",
		"eur",
	}
	for i, h := range hostile {
		t.Run("mismatch/"+string(rune('a'+i)), func(t *testing.T) {
			e := newDepRefEnv(t, pool, "mock-b5-mm"+string(rune('a'+i)))
			a, ref := e.ambiguousBound(t, "b5-mm")
			e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess(ref, 5000, h)))
			txt := b5AuditMetaText(t, e)
			if strings.Contains(txt, h) || strings.Contains(txt, "<script>") {
				t.Fatalf("hostile asset echo stored raw in audit")
			}
			if !strings.Contains(txt, "provider_asset_code_len") || !strings.Contains(txt, "provider_asset_code_sha256_prefix") {
				t.Fatalf("expected length + hash prefix in place of the raw echo, got: %.400s", txt)
			}
		})
	}
	t.Run("missing-amount/hostile", func(t *testing.T) {
		e := newDepRefEnv(t, pool, "mock-b5-miss")
		a, ref := e.ambiguousBound(t, "b5-miss")
		h := "AS\x00SET\n" + strings.Repeat("Z", 40)
		e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess(ref, 0, h)))
		if txt := b5AuditMetaText(t, e); strings.Contains(txt, "AS\\u0000SET") || strings.Contains(txt, strings.Repeat("Z", 40)) {
			t.Fatalf("hostile asset echo stored raw (missing-amount audit)")
		}
	})
	t.Run("valid shape is stored", func(t *testing.T) {
		e := newDepRefEnv(t, pool, "mock-b5-valid")
		a, ref := e.ambiguousBound(t, "b5-valid")
		e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess(ref, 5000, "USD")))
		txt := b5AuditMetaText(t, e)
		if !strings.Contains(txt, `"provider_asset_code": "USD"`) {
			t.Fatalf("a well-formed asset echo must be stored for the evidence trail, got: %.400s", txt)
		}
		if strings.Contains(txt, "provider_asset_code_len") {
			t.Fatalf("a well-formed asset echo must not be masked")
		}
	})
}

func TestB5_WithAssetEcho_Unit(t *testing.T) {
	for _, ok := range []string{"", "EUR", "USDT-ERC20", "BTC_LN", "A1B2C3D4E5F6G7H8"} {
		m := withAssetEcho(map[string]any{}, "k", ok)
		if m["k"] != ok || len(m) != 1 {
			t.Errorf("%q must be stored verbatim, got %v", ok, m)
		}
	}
	for _, bad := range []string{"eur", "EUR ", "EUR\n", strings.Repeat("A", 17), "É", "<b>", "A\x00"} {
		m := withAssetEcho(map[string]any{}, "k", bad)
		if _, raw := m["k"]; raw {
			t.Errorf("%q must not be stored raw: %v", bad, m)
		}
		if m["k_len"] != len(bad) || len(m["k_sha256_prefix"].(string)) != 12 {
			t.Errorf("%q: want k_len and a 12-hex prefix, got %v", bad, m)
		}
	}
}

// LF L1: a poll Pending for a BOUND attempt with a DIFFERING non-empty echo is audited (it is
// still never rebound); an identical or empty echo writes nothing.
func TestB5_PollPending_BoundAttempt_DifferingEchoAudited(t *testing.T) {
	pool := testPool(t)
	for _, c := range []struct {
		name  string
		echo  func(ref string) string
		audit int64
	}{
		{"differing valid echo", func(string) string { return "other-" + uuid.NewString() }, 1},
		{"differing invalid echo", func(string) string { return "bad\nref" }, 1},
		{"identical echo", func(ref string) string { return ref }, 0},
		{"empty echo", func(string) string { return "" }, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newDepRefEnv(t, pool, "mock-b5-pe-"+strings.ReplaceAll(c.name, " ", ""))
			a, ref := e.ambiguousBound(t, "b5-pe")
			e.mustNoSweepErrors(t, e.poll(t, a, ref, StatusResult{ProviderReference: c.echo(ref), Outcome: OutcomePending}))
			if n := e.auditCount(t, "payments.poll_pending_reference_echo_differs", a.ID); n != c.audit {
				t.Fatalf("audit rows = %d, want %d", n, c.audit)
			}
			got := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID)
			if got.ProviderReference == nil || *got.ProviderReference != ref {
				t.Fatalf("the bound reference must never change, got %v", got.ProviderReference)
			}
		})
	}
}

// applyForUnbound runs applyStatusEvidence for a copy of the attempt with its reference
// cleared in memory (the unbound shape the sweeper does not poll today), under the intent
// lock, exactly as processViaQueryStatus would.
func (e *depRefEnv) applyForUnbound(t *testing.T, a PaymentAttempt, gr GateResult[StatusResult]) error {
	t.Helper()
	unbound := a
	unbound.ProviderReference = nil
	return e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		intent, err := GetDepositIntentByID(ctx, tx, *a.DepositIntentID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, intent.ID); err != nil {
			return err
		}
		return e.sweeper().applyStatusEvidence(ctx, tx, intent, unbound, gr)
	})
}

// D1-L1 remainder: the unbound-attempt Pending/Decline fallbacks refuse a VALID echo that
// another attempt of the same tenant already holds - one audit row, no error, no bind, no loop.
// Controls: a fresh echo binds (Pending) / is adopted (Decline); another tenant's identical
// string does not count as a conflict.
func TestB5_UnboundFallbacks_ForeignHeldEcho_RefusedNoLoop(t *testing.T) {
	pool := testPool(t)
	t.Run("pending", func(t *testing.T) {
		e := newDepRefEnv(t, pool, "mock-b5-fb-p")
		a, _ := e.ambiguousBound(t, "b5-fb-p1")
		other, otherRef := e.ambiguousBound(t, "b5-fb-p2")
		_ = other
		err := e.applyForUnbound(t, a, GateResult[StatusResult]{Class: ErrorClassPending, Value: StatusResult{ProviderReference: otherRef, Outcome: OutcomePending}})
		if err != nil {
			t.Fatalf("a foreign-held echo must never be an error (it would loop): %v", err)
		}
		if n := e.auditCount(t, "payments.poll_echo_reference_refused", a.ID); n != 1 {
			t.Fatalf("expected one refusal audit, got %d", n)
		}
		if got := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID); got.State != AttemptAmbiguous {
			t.Fatalf("a refused echo must not transition the attempt, got %s", got.State)
		}
	})
	t.Run("pending control: fresh echo binds", func(t *testing.T) {
		e := newDepRefEnv(t, pool, "mock-b5-fb-pc")
		a, ref := e.ambiguousBound(t, "b5-fb-pc")
		if err := e.applyForUnbound(t, a, GateResult[StatusResult]{Class: ErrorClassPending, Value: StatusResult{ProviderReference: ref, Outcome: OutcomePending}}); err != nil {
			t.Fatalf("control: %v", err)
		}
		if n := e.auditCount(t, "payments.poll_echo_reference_refused", a.ID); n != 0 {
			t.Fatalf("control must not audit a refusal, got %d", n)
		}
		if got := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID); got.State != AttemptPending {
			t.Fatalf("control: the fresh echo must transition ambiguous->pending, got %s", got.State)
		}
	})
	t.Run("decline", func(t *testing.T) {
		e := newDepRefEnv(t, pool, "mock-b5-fb-d")
		a, _ := e.ambiguousBound(t, "b5-fb-d1")
		_, otherRef := e.ambiguousBound(t, "b5-fb-d2")
		err := e.applyForUnbound(t, a, GateResult[StatusResult]{Class: ErrorClassDefiniteDecline, Value: StatusResult{ProviderReference: otherRef, Outcome: OutcomeDeclined, DeclineReason: "provider_unavailable"}})
		if err != nil {
			t.Fatalf("a foreign-held echo must never be an error (it would loop): %v", err)
		}
		if n := e.auditCount(t, "payments.poll_echo_reference_refused", a.ID); n != 1 {
			t.Fatalf("expected one refusal audit, got %d", n)
		}
	})
	t.Run("tenant isolation: same string in another tenant is not a conflict", func(t *testing.T) {
		e := newDepRefEnv(t, pool, "mock-b5-fb-iso")
		a, _ := e.ambiguousBound(t, "b5-fb-iso1")
		f2 := e.addTenant(t)
		ref2 := "iso-ref-" + uuid.NewString()
		e.p.setScript(scriptSyncEcho(ref2, 0, "EUR"))
		res := rvInit(t, e.pool, e.orch, f2, 5000, "b5-fb-iso2")
		if got := mustGetAttempt(t, e.pool, f2.tenantID, res.Attempt.ID); got.ProviderReference == nil {
			t.Fatalf("setup: second tenant attempt unbound")
		}
		if err := e.applyForUnbound(t, a, GateResult[StatusResult]{Class: ErrorClassPending, Value: StatusResult{ProviderReference: ref2, Outcome: OutcomePending}}); err != nil {
			t.Fatalf("%v", err)
		}
		if n := e.auditCount(t, "payments.poll_echo_reference_refused", a.ID); n != 0 {
			t.Fatalf("another tenant's holder must not count as a conflict, refusal audits=%d", n)
		}
	})
}

// D1-CR-4: the bare reason drive.go writes when a returned reference fails validation is a
// classified deposit dispute reason like the prefixed family.
func TestB5_BareInvalidProviderReference_IsClassifiedDisputeReason(t *testing.T) {
	if !IsDepositDisputeTerminalReason(TerminalReasonInvalidProviderReference) {
		t.Fatal("the bare invalid_provider_reference reason must be a classified deposit dispute reason")
	}
	if !IsDepositDisputeTerminalReason(TerminalReasonInvalidProviderReference + ":too_long") {
		t.Fatal("the prefixed family must stay classified")
	}
	found := false
	for _, r := range DepositDisputeTerminalReasons() {
		if r == TerminalReasonInvalidProviderReference {
			found = true
		}
	}
	if !found {
		t.Fatal("the bare reason must be listed explicitly")
	}
}

// Z4b: a Missing poll success reschedules the attempt into the FUTURE (a past/now value would
// re-drive it at once - a hot loop against a PSP that keeps omitting the amount).
func TestB5_PollMissing_RescheduleIsInTheFuture(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-b5-z4b")
	a, ref := e.ambiguousBound(t, "b5-z4b")
	e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess(ref, 0, "EUR")))
	if inFuture := depScan[bool](t, pool, e.f.tenantID, `SELECT next_action_at > now() FROM payment_attempts WHERE id = $1`, a.ID); !inFuture {
		t.Fatal("a Missing poll must reschedule into the future")
	}
}

// Sync-path park (drive.go) and terminal callback mismatch (receipt.go): the same helper is
// applied at those sites, so a hostile asset echo is never stored raw there either.
func TestB5_SyncAndTerminalCallbackSites_HostileAssetNeverStoredRaw(t *testing.T) {
	pool := testPool(t)
	hostile := "EUR\n<script>x</script>"
	t.Run("sync amount mismatch park", func(t *testing.T) {
		e := newDepRefEnv(t, pool, "mock-b5-sync")
		ref := "b5-sync-" + uuid.NewString()
		e.p.setScript(scriptSyncEcho(ref, 4999, hostile))
		rvInit(t, e.pool, e.orch, e.f, 5000, "b5-sync")
		txt := b5AuditMetaText(t, e)
		if strings.Contains(txt, "<script>") {
			t.Fatalf("hostile asset echo stored raw (sync park)")
		}
		if !strings.Contains(txt, "provider_asset_code_len") {
			t.Fatalf("expected the masked form at the sync park site (did the park happen?), got: %.400s", txt)
		}
	})
	t.Run("terminal callback amount/asset mismatch", func(t *testing.T) {
		e := newDepRefEnv(t, pool, "mock-b5-term")
		a, ref := e.ambiguousBound(t, "b5-term")
		e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess(ref, 5000, "EUR")))
		if got := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID); got.State != AttemptSucceeded {
			t.Fatalf("setup: attempt must be succeeded, got %s", got.State)
		}
		// The callback path is already bounded at ingress (providerref rejects control
		// characters) and by the payment_provider_events_asset_code_check CHECK, so this
		// site can only ever see a CHECK-passing code: the helper there is defence in
		// depth. Pin that the well-formed echo is still recorded for the evidence trail.
		if _, err := rvCallback(e.pool, e.orch, e.f, e.id, e.p.CallbackPayload(e.f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 4999, "USD", "", false)); err != nil {
			t.Fatalf("callback: %v", err)
		}
		if e.auditCount(t, "payments.callback_amount_asset_mismatch_terminal", a.ID) != 1 {
			t.Fatalf("setup: the terminal mismatch audit was not written")
		}
		if txt := b5AuditMetaText(t, e); !strings.Contains(txt, `"echoed_asset_code": "USD"`) {
			t.Fatalf("a well-formed echoed asset code must be recorded, got: %.300s", txt)
		}
	})
}
