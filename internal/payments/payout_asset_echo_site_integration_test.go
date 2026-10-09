//go:build integration

// PAY-PAYOUT-ASSET-ECHO-TEST-1: site-level tests for the payout provider-asset echo site, the
// QueryStatus success cross-check in applyPayoutSuccessCheckedFromStatus (payout.go), which writes
// payments.payout_amount_asset_mismatch with the provider-echoed asset code run through
// withAssetEcho (B5). The unit test of the helper and the deposit-side sites already existed; this
// pins the payout site itself: valid echo stored raw, invalid echo only as _len + _sha256_prefix,
// the audit row's shape and atomicity, and its binding to the attempt/tenant.
package payments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

const payoutAmountAssetMismatchAudit = "payments.payout_amount_asset_mismatch"

// r8AuditRow is one audit_log row read back through the owning tenant's session.
type r8AuditRow struct {
	ActorType  string
	TargetType string
	TargetID   string
	Outcome    string
	Metadata   map[string]any
}

func r8ReadAudit(t *testing.T, e rbEnv, tenantID uuid.UUID, action string, attemptID uuid.UUID) []r8AuditRow {
	t.Helper()
	var out []r8AuditRow
	err := e.pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT actor_type, COALESCE(target_type,''), COALESCE(target_id,''), outcome, metadata
			FROM audit_log WHERE action = $1 AND target_id = $2 ORDER BY created_at, id`, action, attemptID.String())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r r8AuditRow
			var raw []byte
			if err := rows.Scan(&r.ActorType, &r.TargetType, &r.TargetID, &r.Outcome, &raw); err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &r.Metadata); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("read audit %s: %v", action, err)
	}
	return out
}

// r8StatusSuccess applies a QueryStatus success for a payout through the production entry point.
func (e rbEnv) r8StatusSuccess(t *testing.T, wr withdrawal.WithdrawalRequest, a PaymentAttempt, ref string, amount int64, asset string) {
	t.Helper()
	e.applyStatus(t, wr, a, GateResult[StatusResult]{Class: ErrorClassSucceeded,
		Value: StatusResult{Outcome: OutcomeSucceeded, ProviderReference: ref, Amount: amount, AssetCode: asset}})
}

func TestPayoutAssetEcho_Site_ValidEcho_StoredRaw(t *testing.T) {
	for i, asset := range []string{"USD", "USDT-ERC20", "BTC_LN", "A1B2C3D4E5F6G7H8"} { // last: exactly 16 bytes
		t.Run(asset, func(t *testing.T) {
			e := newRBEnv(t, fmt.Sprintf("mock-aecho-v%d", i))
			wr, a := e.claim(t, fmt.Sprintf("aecho-valid-%d", i))
			before := e.b12Snapshot(t, wr)
			e.r8StatusSuccess(t, wr, a, "aecho-ref-v", 500, asset) // asset differs from the requested EUR

			rows := r8ReadAudit(t, e, e.f.tenantID, payoutAmountAssetMismatchAudit, a.ID)
			if len(rows) != 1 {
				t.Fatalf("want exactly one %s row, got %d", payoutAmountAssetMismatchAudit, len(rows))
			}
			m := rows[0].Metadata
			if m["provider_asset"] != asset {
				t.Fatalf("a well-formed echo must be stored for the evidence trail, got %v", m)
			}
			if _, masked := m["provider_asset_len"]; masked {
				t.Fatalf("a well-formed echo must not be masked: %v", m)
			}
			if _, masked := m["provider_asset_sha256_prefix"]; masked {
				t.Fatalf("a well-formed echo must not carry a hash: %v", m)
			}
			// The requested side comes from the withdrawal, never from the echo.
			if m["requested_asset"] != "EUR" || m["requested_amount"] != float64(500) || m["provider_amount"] != float64(500) {
				t.Fatalf("requested/provider amounts: %v", m)
			}
			e.r8AssertParked(t, wr, a, before)
		})
	}
}

func TestPayoutAssetEcho_Site_InvalidEcho_OnlyLenAndHashPrefix_NeverRaw(t *testing.T) {
	hostile := []struct{ name, v string }{
		{"lowercase", "eur"},
		{"trailing_space", "EUR "},
		{"newline_markup", "EUR\n<script>alert(1)</script>"},
		{"17_bytes_one_over_the_limit", strings.Repeat("A", 17)},
		{"oversized", strings.Repeat("X", 300)},
		{"non_ascii", "\u20acUR\u202e"},
		{"nul_byte", "AS\x00SET"},
		{"provider_prose", "PROVIDER SAID: card 4111 closed"},
	}
	for i, h := range hostile {
		t.Run(h.name, func(t *testing.T) {
			e := newRBEnv(t, fmt.Sprintf("mock-aecho-h%d", i))
			wr, a := e.claim(t, fmt.Sprintf("aecho-hostile-%d", i))
			before := e.b12Snapshot(t, wr)
			e.r8StatusSuccess(t, wr, a, "aecho-ref-h", 500, h.v)

			rows := r8ReadAudit(t, e, e.f.tenantID, payoutAmountAssetMismatchAudit, a.ID)
			if len(rows) != 1 {
				t.Fatalf("want exactly one audit row, got %d", len(rows))
			}
			m := rows[0].Metadata
			if _, raw := m["provider_asset"]; raw {
				t.Fatalf("a hostile echo must never be stored raw under the key: %v", m)
			}
			sum := sha256.Sum256([]byte(h.v))
			if m["provider_asset_len"] != float64(len(h.v)) || m["provider_asset_sha256_prefix"] != hex.EncodeToString(sum[:])[:12] {
				t.Fatalf("want _len=%d and the 12-hex sha256 prefix, got %v", len(h.v), m)
			}
			blob, _ := json.Marshal(m)
			// The row's own server-generated UUIDs (withdrawal_request_id) are random hex and can
			// contain "4111" by chance (seen in the round-10 merged-tree run); strip them so the
			// probe only sees provider-influenced text.
			blobText := aechoUUIDRE.ReplaceAllString(string(blob), "<uuid>")
			for _, probe := range []string{h.v, "<script>", "4111", "PROVIDER"} {
				if strings.Contains(h.v, probe) && strings.Contains(blobText, probe) {
					t.Fatalf("the audit metadata leaks %q: %s", probe, blob)
				}
			}
			// The whole tenant audit trail (not just this row) must be free of the raw text.
			all := fpAuditText(t, e)
			if strings.Contains(all, h.v) {
				t.Fatalf("raw hostile echo found in the tenant audit trail")
			}
			e.r8AssertParked(t, wr, a, before)
			// The B12 alert shares the attempt: it must not echo the provider text either.
			e.b12AssertOneAlert(t, a.ID, "amount_asset_mismatch", h.v, "<script>", "4111")
		})
	}
}

var aechoUUIDRE = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

func fpAuditText(t *testing.T, e rbEnv) string {
	t.Helper()
	var s string
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COALESCE(string_agg(metadata::text, E'\n'), '') FROM audit_log WHERE tenant_id = $1`, e.f.tenantID).Scan(&s)
	}); err != nil {
		t.Fatalf("read audit text: %v", err)
	}
	return s
}

// r8AssertParked: the cross-check parked the attempt (T10, amount_asset_mismatch), the withdrawal
// still holds its funds, and no money moved.
func (e rbEnv) r8AssertParked(t *testing.T, wr withdrawal.WithdrawalRequest, a PaymentAttempt, before b12Before) {
	t.Helper()
	got := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID)
	if got.State != AttemptDisputed || got.TerminalReason == nil || *got.TerminalReason != "amount_asset_mismatch" {
		t.Fatalf("want disputed/amount_asset_mismatch, got %s %v", got.State, got.TerminalReason)
	}
	e.b12AssertNoMoneyMoved(t, b12Out{wr: wr, a: a, before: before})
	if w := fpReqState(t, e.pool, e.f, wr.ID); w.State != withdrawal.StateSubmitted {
		t.Fatalf("the hold must be kept, withdrawal state %s", w.State)
	}
}

// Audit behaviour: actor system, outcome denied, target = the attempt, tenant-scoped, one row per
// park; a matching echo writes no mismatch row and settles; a second attempt gets its own row.
func TestPayoutAssetEcho_Site_AuditShape_AttemptAndTenantBinding(t *testing.T) {
	e := newRBEnv(t, "mock-aecho-bind")
	wr1, a1 := e.claim(t, "aecho-bind-1")
	wr2, a2 := e.claim(t, "aecho-bind-2")
	other, _, _ := fpOrch(t, e.pool, "mock-aecho-bind-other")

	e.r8StatusSuccess(t, wr1, a1, "aecho-bind-ref-1", 500, "bad echo\n")
	e.r8StatusSuccess(t, wr2, a2, "aecho-bind-ref-2", 499, "EUR") // amount mismatch only

	for _, c := range []struct {
		wr withdrawal.WithdrawalRequest
		a  PaymentAttempt
	}{{wr1, a1}, {wr2, a2}} {
		rows := r8ReadAudit(t, e, e.f.tenantID, payoutAmountAssetMismatchAudit, c.a.ID)
		if len(rows) != 1 {
			t.Fatalf("attempt %s: want 1 audit row, got %d", c.a.ID, len(rows))
		}
		r := rows[0]
		if r.ActorType != "system" || r.Outcome != "denied" || r.TargetType != "payment_attempt" || r.TargetID != c.a.ID.String() {
			t.Fatalf("audit shape: %+v", r)
		}
		if r.Metadata["withdrawal_request_id"] != c.wr.ID.String() {
			t.Fatalf("the row must name its own withdrawal %s, got %v", c.wr.ID, r.Metadata["withdrawal_request_id"])
		}
		// Binding to the right attempt: the row of attempt 1 never appears under attempt 2.
	}
	if rows := r8ReadAudit(t, e, e.f.tenantID, payoutAmountAssetMismatchAudit, uuid.New()); len(rows) != 0 {
		t.Fatalf("an unrelated attempt id must read no rows")
	}
	// Tenant binding: another tenant's session reads none of these rows (RLS), even by attempt id.
	if rows := r8ReadAudit(t, e, other.tenantID, payoutAmountAssetMismatchAudit, a1.ID); len(rows) != 0 {
		t.Fatalf("another tenant must not read the audit row: %+v", rows)
	}
	// Exactly the two attempts' rows exist for the tenant.
	if n := fpCount(t, e.pool, e.f.tenantID, `SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action=$2`, e.f.tenantID, payoutAmountAssetMismatchAudit); n != 2 {
		t.Fatalf("tenant rows = %d, want 2", n)
	}
	// Each attempt's alert carries its own attempt id only.
	if rows := e.b12Rows(t); len(rows) != 2 {
		t.Fatalf("want two alerts (one per parked attempt), got %+v", rows)
	}
}

func TestPayoutAssetEcho_Site_MatchingEcho_NoMismatchAudit_Settles(t *testing.T) {
	e := newRBEnv(t, "mock-aecho-match")
	wr, a := e.claim(t, "aecho-match")
	e.r8StatusSuccess(t, wr, a, "aecho-match-ref", 500, "EUR")
	if rows := r8ReadAudit(t, e, e.f.tenantID, payoutAmountAssetMismatchAudit, a.ID); len(rows) != 0 {
		t.Fatalf("a matching amount/asset must write no mismatch audit: %+v", rows)
	}
	if got := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID); got.State != AttemptSucceeded {
		t.Fatalf("a matching success settles, got %s", got.State)
	}
	if rows := e.b12Rows(t); len(rows) != 0 {
		t.Fatalf("no alert for a clean settlement: %+v", rows)
	}
}

// Error behaviour: the park and its audit row are one transaction. When the audit INSERT fails the
// whole evidence application errors and rolls back (attempt not disputed, no alert); once the fault
// is gone the same evidence converges to one park, one audit row and one alert.
func TestPayoutAssetEcho_Site_AuditFailure_RollsBackThePark_ThenConverges(t *testing.T) {
	e := newRBEnv(t, "mock-aecho-fail")
	wr, a := e.claim(t, "aecho-fail")
	before := e.b12Snapshot(t, wr)
	apply := func() error {
		return applyPayoutStatusEvidence(context.Background(), e.pool, e.f.tenantID, wr.ID, a,
			GateResult[StatusResult]{Class: ErrorClassSucceeded, Value: StatusResult{Outcome: OutcomeSucceeded,
				ProviderReference: "aecho-fail-ref", Amount: 500, AssetCode: "bad\necho"}},
			EvidenceQueryStatus, time.Now().Add(time.Minute), nil, WithDestinations(pitest.Shared()))
	}
	t.Run("faulty", func(t *testing.T) {
		r8InstallAuditFailure(t, e, payoutAmountAssetMismatchAudit)
		if err := apply(); err == nil {
			t.Fatalf("an audit failure must propagate, never be swallowed")
		}
		if got := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID); got.State == AttemptDisputed {
			t.Fatalf("the park must roll back with its failed audit row, got %s", got.State)
		}
		if rows := r8ReadAudit(t, e, e.f.tenantID, payoutAmountAssetMismatchAudit, a.ID); len(rows) != 0 {
			t.Fatalf("no audit row may survive the rollback")
		}
		if rows := e.b12Rows(t); len(rows) != 0 {
			t.Fatalf("no alert on a rolled-back park: %+v", rows)
		}
		e.b12AssertNoMoneyMoved(t, b12Out{wr: wr, a: a, before: before})
	}) // trigger dropped here
	if err := apply(); err != nil {
		t.Fatalf("redelivery after the fault: %v", err)
	}
	if rows := r8ReadAudit(t, e, e.f.tenantID, payoutAmountAssetMismatchAudit, a.ID); len(rows) != 1 {
		t.Fatalf("want exactly one audit row after recovery, got %d", len(rows))
	}
	e.r8AssertParked(t, wr, a, before)
	e.b12AssertOneAlert(t, a.ID, "amount_asset_mismatch")
}

// r8InstallAuditFailure installs a TEST-ONLY trigger that makes the INSERT of one audit action for
// this env's tenant fail (the test tenant id is a fresh uuid, so nothing else is affected). It
// changes no role or grant and is dropped on cleanup, like internal/testsupport/alertinject.
func r8InstallAuditFailure(t *testing.T, e rbEnv, action string) {
	t.Helper()
	if strings.ContainsAny(action, "';") {
		t.Fatalf("bad action %q", action)
	}
	name := "iw_r8_auditfail_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	fn := fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger AS $$
BEGIN
  IF NEW.tenant_id = '%s'::uuid AND NEW.action = '%s' THEN
    RAISE EXCEPTION 'r8: injected audit failure' USING ERRCODE = 'P0001';
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql`, name, e.f.tenantID, action)
	if err := e.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, fn); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON audit_log FOR EACH ROW EXECUTE FUNCTION %s()`, name, name))
		return err
	}); err != nil {
		t.Fatalf("install audit failure: %v", err)
	}
	t.Cleanup(func() {
		_ = e.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON audit_log`, name)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, name))
			return err
		})
	})
}
