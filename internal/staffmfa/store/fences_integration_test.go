//go:build integration

package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/staffmfa"
)

// D14 (security review F1): player-scoped and every platform-service session are refused on all four tables:
// read, FOR UPDATE, insert, revoke and hard-lock. The control session (plain tenant scope) is allowed each
// statement, so a refusal cannot be a broken statement.
func TestFences_PlayerAndServiceSessionsAreRefused(t *testing.T) {
	w := newWorld(t)
	w.enrol(w.a1, t0)
	w.issueCodes(w.a1, t0)
	w.issueChallenge(w.a1, ChallengeMFA, OriginLogin, t0)
	w.issueToken(w.a1, EnrolInvite, t0)
	tenant := w.tenantA.String()
	scopes := map[string]guc{
		"control":                   {"app.tenant_id": tenant},
		"player":                    {"app.tenant_id": tenant, "app.player_account_id": uuid.NewString()},
		"player, no tenant":         {"app.player_account_id": uuid.NewString()},
		"alert_dispatcher":          {"app.tenant_id": tenant, "app.platform_service_id": "alert_dispatcher"},
		"sportsbook_catalogue_sync": {"app.tenant_id": tenant, "app.platform_service_id": "sportsbook_catalogue_sync"},
		"kyc_submission_worker":     {"app.tenant_id": tenant, "app.platform_service_id": "kyc_submission_worker"},
	}
	stmts := map[string]string{
		"read":       `SELECT count(*) FROM staff_mfa_factors WHERE staff_user_id = $1`,
		"for update": `SELECT count(*) FROM (SELECT 1 FROM staff_mfa_factors WHERE staff_user_id = $1 FOR UPDATE) s`,
		"revoke":     `WITH u AS (UPDATE staff_mfa_factors SET status = 'revoked', revoked_at = now(), revoked_reason = 'reset' WHERE staff_user_id = $1 AND status = 'active' RETURNING 1) SELECT count(*) FROM u`,
		"hard-lock":  `WITH u AS (UPDATE staff_mfa_factors SET failed_24h_count = 10, failed_24h_start = now(), hard_locked_at = now() WHERE staff_user_id = $1 AND status = 'active' RETURNING 1) SELECT count(*) FROM u`,
		"codes":      `WITH u AS (UPDATE staff_mfa_recovery_codes SET revoked_at = now() WHERE staff_user_id = $1 AND consumed_at IS NULL RETURNING 1) SELECT count(*) FROM u`,
		"challenge":  `WITH u AS (UPDATE staff_auth_challenges SET consumed_at = now() WHERE staff_user_id = $1 RETURNING 1) SELECT count(*) FROM u`,
		"token":      `WITH u AS (UPDATE staff_enrolment_tokens SET revoked_at = now() WHERE staff_user_id = $1 RETURNING 1) SELECT count(*) FROM u`,
	}
	for name, g := range scopes {
		control := name == "control"
		for sn, q := range stmts {
			var n int
			// every statement runs in a transaction that is rolled back, so the control's writes do not persist
			err := w.rtGUC(g, func(ctx context.Context, tx pgx.Tx) error { return tx.QueryRow(ctx, q, w.a1.StaffID).Scan(&n) })
			if err != nil {
				t.Fatalf("%s/%s: %v", name, sn, err)
			}
			if control && n == 0 {
				t.Fatalf("control %s touched nothing (vacuous fence test)", sn)
			}
			if !control && n != 0 {
				t.Fatalf("%s/%s: a fenced session touched %d rows", name, sn, n)
			}
		}
		for table, q := range insertSQL {
			if table == "staff_mfa_factors" || table == "staff_enrolment_tokens" || table == "staff_mfa_recovery_codes" || table == "staff_auth_challenges" {
				err := w.rtGUC(g, func(ctx context.Context, tx pgx.Tx) error {
					_, e := tx.Exec(ctx, q, nil, w.a2.StaffID, randMAC())
					return e
				})
				code := pgCode(err)
				if control && err != nil {
					t.Fatalf("control insert into %s: %v", table, err)
				}
				if !control && g["app.tenant_id"] != "" && code != "42501" && code != "SF001" {
					t.Fatalf("%s: a fenced insert into %s must be refused, got %v", name, table, err)
				}
				if !control && g["app.tenant_id"] == "" && err == nil {
					t.Fatalf("%s: insert into %s without a tenant arm admitted", name, table)
				}
			}
		}
	}
	// The legitimate paths still work end to end after the new fences.
	vr, err := w.verify(w.a1, "000000", t0.Add(time.Minute))
	if err != nil || vr.Outcome != staffmfa.OutcomeRejected {
		t.Fatalf("tenant staff verify: %+v %v", vr, err)
	}
	if _, err := w.verify(w.platform, "000000", t0); err == nil {
		t.Fatal("platform staff without a factor must report ErrNoActiveFactor")
	}
}

// C2 (security review F4): the factor id is bound in the AAD, so copying a revoked/hard-locked factor's ciphertext
// into a NEW active row of the same staff member under a new id never verifies.
func TestIntegrity_SameStaffNewFactorIDWithOldCiphertext(t *testing.T) {
	w := newWorld(t)
	_, secret := w.enrol(w.a1, t0)
	var kid string
	var nonce, ct []byte
	w.mustRun(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT secret_kid, secret_nonce, secret_ct FROM staff_mfa_factors WHERE staff_user_id = $1 AND status = 'active'`, w.a1.StaffID).Scan(&kid, &nonce, &ct)
	})
	// hard-lock, then revoke (the attacker reuses the sealed secret)
	w.mustRun(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE staff_mfa_factors SET failed_24h_count = 10, failed_24h_start = now(), hard_locked_at = now() WHERE staff_user_id = $1 AND status = 'active'`, w.a1.StaffID)
		return e
	})
	w.mustRun(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE staff_mfa_factors SET status = 'revoked', revoked_at = now(), revoked_reason = 'reset' WHERE staff_user_id = $1 AND status = 'active'`, w.a1.StaffID)
		return e
	})
	w.mustRun(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `INSERT INTO staff_mfa_factors (id, tenant_id, staff_user_id, factor_type, status, secret_kid, secret_nonce, secret_ct, activated_at)
			VALUES (gen_random_uuid(), $1, $2, 'totp', 'active', $3, $4, $5, now())`, w.tenantA, w.a1.StaffID, kid, nonce, ct)
		return e
	})
	now := t0.Add(31 * time.Second)
	vr, err := w.verify(w.a1, codeAt(t, secret, now), now)
	if err != nil || vr.Outcome != staffmfa.OutcomeIntegrity || vr.Accepted() {
		t.Fatalf("old ciphertext under a new factor id must fail integrity: %+v %v", vr, err)
	}
}
