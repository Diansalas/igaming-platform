//go:build integration

package payments

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// ADR 0101 C-17b / T-11 / C-17b+ (LF test 11, security T-11): an exhaustive
// differential matrix of the payment_attempts guard. The 0107 guard (extracted
// from the 0107 migration text and installed under another name) and the head
// guard (0115) are each attached to a shadow table of the same shape, and every
// (operation, OLD.state, NEW.state, NEW.last_evidence_kind, NEW.terminal_reason,
// OLD.ever_possibly_sent) is attempted on both. With no executing resolution
// present the two must give IDENTICAL accept/refuse for every deposit case, and
// for every payout case too (payment_m2_admits is false without a resolution).
// The guard function is the unit under test; the column-discipline trigger is
// tested separately (TestK3_T10).

var k3MatrixStates = []string{"created", "submitting", "pending", "ambiguous", "succeeded", "declined", "rejected", "disputed"}
var k3MatrixEvidence = []string{"sync", "callback", "query_status", "sweeper", "operator", "platform", "legacy"}
var k3MatrixReasons = []string{
	"", "reversal_tombstone_precedes_success", "multiple_success_for_intent", "sync_amount_mismatch", "provider_reference_conflict",
	"invalid_provider_reference:control_char", "poll_amount_mismatch", "poll_reference_mismatch", "callback_amount_asset_mismatch",
	"success_for_never_sent_attempt", "provider_reference_mismatch", "amount_asset_mismatch", "success_after_payout_declined", "zz_other_reason",
}

func k3MatrixDO(table, result, op string) string {
	return fmt.Sprintf(`
DO $m$
DECLARE
    v_old TEXT; v_new TEXT; v_ek TEXT; v_tr TEXT; v_sent BOOLEAN; v_id UUID; v_res TEXT;
    v_states TEXT[] := ARRAY['%s'];
    v_eks TEXT[] := ARRAY['%s'];
    v_trs TEXT[] := ARRAY['%s'];
BEGIN
    FOREACH v_old IN ARRAY v_states LOOP
      FOREACH v_new IN ARRAY v_states LOOP
        FOREACH v_ek IN ARRAY v_eks LOOP
          FOREACH v_tr IN ARRAY v_trs LOOP
            FOREACH v_sent IN ARRAY ARRAY[false, true] LOOP
              CONTINUE WHEN v_old = 'created' AND v_sent;
              v_id := gen_random_uuid();
              INSERT INTO %[4]s (id, tenant_id, operation, deposit_intent_id, withdrawal_request_id, attempt_no, provider_id, payment_method,
                                  asset_code, amount, interactive, merchant_reference, external_idempotency_key, provider_reference,
                                  state, last_evidence_kind, ever_possibly_sent, terminal_reason)
              VALUES (v_id, gen_random_uuid(), '%[5]s',
                      CASE WHEN '%[5]s' = 'deposit' THEN gen_random_uuid() END, CASE WHEN '%[5]s' = 'payout' THEN gen_random_uuid() END,
                      1, CASE WHEN v_old IN ('created','rejected') THEN NULL ELSE 'p' END, 'card', 'EUR', 100, false,
                      v_id::text, v_id::text, CASE WHEN v_old IN ('created','rejected','submitting') THEN NULL ELSE 'ref-' || v_id::text END,
                      v_old, 'sync', v_sent, CASE WHEN v_old = 'disputed' THEN 'provider_reference_mismatch' END);
              BEGIN
                UPDATE %[4]s SET state = v_new, last_evidence_kind = v_ek, terminal_reason = NULLIF(v_tr, '') WHERE id = v_id;
                v_res := 'accept';
              EXCEPTION WHEN OTHERS THEN
                v_res := 'refuse';
              END;
              INSERT INTO %[6]s (k, r) VALUES (v_old || '>' || v_new || '|' || v_ek || '|' || v_tr || '|' || v_sent::text, v_res);
            END LOOP;
          END LOOP;
        END LOOP;
      END LOOP;
    END LOOP;
END
$m$;`,
		strings.Join(k3MatrixStates, "','"), strings.Join(k3MatrixEvidence, "','"), strings.Join(k3MatrixReasons, "','"), table, op, result)
}

func TestK3_C17b_T11_GuardMatrixIsIdenticalOn0107AndHead(t *testing.T) {
	pool, _ := scratchThrough(t, "k3matrix_", migration0115Version)
	// The 0107 guard under another name.
	lines := guardBody(t, "0107_deposit_intent_double_credit_backstop.up.sql")
	fn := strings.Join(lines, "\n")
	fn = strings.Replace(fn, "CREATE OR REPLACE FUNCTION payment_attempts_guard()", "CREATE FUNCTION k3_guard_0107()", 1) + "\n$$ LANGUAGE plpgsql;"

	setup := []string{
		fn,
		`CREATE TABLE k3_shadow_old (LIKE payment_attempts INCLUDING DEFAULTS)`,
		`CREATE TABLE k3_shadow_new (LIKE payment_attempts INCLUDING DEFAULTS)`,
		`CREATE TRIGGER k3_old BEFORE UPDATE ON k3_shadow_old FOR EACH ROW EXECUTE FUNCTION k3_guard_0107()`,
		`CREATE TRIGGER k3_new BEFORE UPDATE ON k3_shadow_new FOR EACH ROW EXECUTE FUNCTION payment_attempts_guard()`,
	}
	for _, op := range []string{"deposit", "payout"} {
		setup = append(setup,
			fmt.Sprintf(`CREATE TABLE k3_res_old_%s (k TEXT PRIMARY KEY, r TEXT NOT NULL)`, op),
			fmt.Sprintf(`CREATE TABLE k3_res_new_%s (k TEXT PRIMARY KEY, r TEXT NOT NULL)`, op))
	}
	ctx := context.Background()
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for _, s := range setup {
			if _, err := tx.Exec(ctx, s); err != nil {
				return fmt.Errorf("%.80s: %w", s, err)
			}
		}
		for _, op := range []string{"deposit", "payout"} {
			if _, err := tx.Exec(ctx, k3MatrixDO("k3_shadow_old", "k3_res_old_"+op, op)); err != nil {
				return fmt.Errorf("matrix old %s: %w", op, err)
			}
			if _, err := tx.Exec(ctx, k3MatrixDO("k3_shadow_new", "k3_res_new_"+op, op)); err != nil {
				return fmt.Errorf("matrix new %s: %w", op, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("matrix setup/run: %v", err)
	}

	for _, op := range []string{"deposit", "payout"} {
		var total, accepted, refused, differing int
		var sample string
		if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT count(*), count(*) FILTER (WHERE r = 'accept'), count(*) FILTER (WHERE r = 'refuse') FROM k3_res_new_%s`, op)).Scan(&total, &accepted, &refused); err != nil {
				return err
			}
			return tx.QueryRow(ctx, fmt.Sprintf(`SELECT count(*), coalesce(min(o.k), '') FROM k3_res_old_%[1]s o JOIN k3_res_new_%[1]s n USING (k) WHERE o.r <> n.r`, op)).Scan(&differing, &sample)
		}); err != nil {
			t.Fatal(err)
		}
		if total < 10000 {
			t.Fatalf("%s: only %d matrix cases ran", op, total)
		}
		// C-17b+ non-vacuity: both outcomes occur.
		if accepted == 0 || refused == 0 {
			t.Fatalf("%s: matrix is vacuous (accepted %d, refused %d)", op, accepted, refused)
		}
		if differing != 0 {
			t.Errorf("%s: %d cases differ between the 0107 guard and the head guard, e.g. %q", op, differing, sample)
		}
	}
	// C-17b+: named anchors on the head matrix - the legal path is accepted, and the
	// deposit `-> declined` / `-> succeeded` operator gate is exercised and refused.
	anchors := map[string]string{
		"pending>succeeded|sync||true":                                "accept",
		"pending>succeeded|operator||true":                            "refuse",
		"pending>declined|operator||true":                             "refuse",
		"disputed>succeeded|operator||true":                           "refuse",
		"disputed>declined|callback||true":                            "refuse",
		"disputed>pending|sync||true":                                 "refuse",
		"declined>disputed|callback|multiple_success_for_intent|true": "accept",
		"declined>disputed|callback|zz_other_reason|true":             "refuse",
	}
	for k, want := range anchors {
		var got string
		if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT r FROM k3_res_new_deposit WHERE k = $1`, k).Scan(&got)
		}); err != nil {
			t.Errorf("anchor %q missing: %v", k, err)
			continue
		}
		if got != want {
			t.Errorf("anchor %q: deposit head guard = %s, want %s", k, got, want)
		}
	}
}
