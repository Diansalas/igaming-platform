//go:build integration

// Trigger, CHECK and unique-index tests of migration 0137 (ADR 0119 section 13.1 / 13.6): every SF code, each
// asserted by direct SQL. Tamper attempts run as the TABLE OWNER where the runtime role has no grant (the
// triggers bind every role, owner included), and as igaming_runtime where the grant exists.
package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/staffmfa"
)

// ownerExec runs one statement as the table owner in ref's scope and commits on success.
func (w *world) ownerExec(ref staffmfa.StaffRef, q string, args ...any) error {
	return w.ownerRun(ref, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, q, args...)
		return err
	})
}

func (w *world) rtExec(ref staffmfa.StaffRef, q string, args ...any) error {
	return w.run(ref, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, q, args...)
		return err
	})
}

func constraintOf(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.ConstraintName
	}
	return ""
}

func wantCheck(t testing.TB, what string, err error, constraint string) {
	t.Helper()
	if pgCode(err) != "23514" || constraintOf(err) != constraint {
		t.Fatalf("%s: want CHECK %s, got code=%q constraint=%q (%v)", what, constraint, pgCode(err), constraintOf(err), err)
	}
}

// insertSQL is a minimal valid INSERT per table, parameterised on ($1 tenant_id, $2 staff_user_id, $3 unique 32-byte MAC).
var insertSQL = map[string]string{
	"staff_mfa_factors": `INSERT INTO staff_mfa_factors (tenant_id, staff_user_id, factor_type, status, secret_kid, secret_nonce, secret_ct, pending_expires_at)
		VALUES ($1, $2, 'totp', 'pending', 'k', '\x000000000000000000000000', $3, now() + interval '10 minutes')`,
	"staff_mfa_recovery_codes": `INSERT INTO staff_mfa_recovery_codes (tenant_id, staff_user_id, batch_id, code_kid, code_mac)
		VALUES ($1, $2, gen_random_uuid(), 'k', $3)`,
	"staff_auth_challenges": `INSERT INTO staff_auth_challenges (tenant_id, staff_user_id, purpose, token_kid, token_mac, staff_version_at_issue, attempts_left, expires_at, via)
		VALUES ($1, $2, 'mfa', 'k', $3, 0, 5, now() + interval '5 minutes', 'login')`,
	"staff_enrolment_tokens": `INSERT INTO staff_enrolment_tokens (tenant_id, staff_user_id, purpose, token_kid, token_mac, expires_at)
		VALUES ($1, $2, 'invite', 'k', $3, now() + interval '72 hours')`,
}

func randMAC() []byte {
	b := make([]byte, 32)
	id := uuid.New()
	copy(b, id[:])
	id2 := uuid.New()
	copy(b[16:], id2[:])
	return b
}

func TestSF001_TenantForcedOnEveryTable(t *testing.T) {
	w := newWorld(t)
	for table, q := range insertSQL {
		// (a) NULL supplied, tenant staff, own scope: the tenant is FORCED from the staff row.
		if err := w.rtExec(w.a1, q, nil, w.a1.StaffID, randMAC()); err != nil {
			t.Fatalf("%s: forced insert: %v", table, err)
		}
		var tid *uuid.UUID
		w.mustOwner(w.a1, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT tenant_id FROM `+table+` WHERE staff_user_id = $1`, w.a1.StaffID).Scan(&tid)
		})
		if tid == nil || *tid != w.tenantA {
			t.Fatalf("%s: tenant_id not forced from the staff row: %v", table, tid)
		}
		// (d) platform staff: NULL stays NULL.
		if err := w.rtExec(w.platform, q, nil, w.platform.StaffID, randMAC()); err != nil {
			t.Fatalf("%s: platform insert: %v", table, err)
		}
		// (b) a differing tenant supplied.
		err := w.rtExec(w.a1, q, w.tenantB, w.a1.StaffID, randMAC())
		wantCode(t, table+": supplied tenant B for tenant A staff", err, "SF001")
		// (c) a tenant supplied for platform staff.
		err = w.rtExec(w.platform, q, w.tenantA, w.platform.StaffID, randMAC())
		wantCode(t, table+": supplied tenant for platform staff", err, "SF001")
		// (e)-(g) staff the scope cannot see: another tenant's, a tenant row from platform scope, a platform row from tenant scope.
		wantCode(t, table+": scope B, staff A", w.rtExec(w.b1, q, nil, w.a1.StaffID, randMAC()), "SF001")
		wantCode(t, table+": platform scope, staff A", w.rtExec(w.platform, q, nil, w.a1.StaffID, randMAC()), "SF001")
		wantCode(t, table+": tenant scope, platform staff", w.rtExec(w.a1, q, nil, w.platform.StaffID, randMAC()), "SF001")
		// (h) an unknown staff id: refused by the guard before the FK.
		wantCode(t, table+": unknown staff", w.rtExec(w.a1, q, nil, uuid.New(), randMAC()), "SF001")
		// The right tenant supplied explicitly also works.
		if err := w.rtExec(w.a2, q, w.tenantA, w.a2.StaffID, randMAC()); err != nil {
			t.Fatalf("%s: explicit own tenant: %v", table, err)
		}
	}
}

func TestSF002_SF003_FactorGuard(t *testing.T) {
	w := newWorld(t)
	id, _ := w.enrol(w.a1, t0)
	upd := func(set string, args ...any) error {
		return w.ownerExec(w.a1, `UPDATE staff_mfa_factors SET `+set+` WHERE id = $1`, append([]any{id}, args...)...)
	}
	// Immutable columns (SF002), as the owner: only the trigger stands in the way.
	for name, set := range map[string]string{
		"id":            `id = gen_random_uuid()`,
		"tenant_id":     `tenant_id = '` + w.tenantB.String() + `'`,
		"staff_user_id": `staff_user_id = '` + w.a2.StaffID.String() + `'`,
		"secret_kid":    `secret_kid = 'other'`,
		"secret_nonce":  `secret_nonce = '\x010101010101010101010101'`,
		"secret_ct":     `secret_ct = '\x01'`,
		"created_at":    `created_at = created_at + interval '1 second'`,
	} {
		wantCode(t, "immutable "+name, upd(set), "SF002")
	}
	// Status graph.
	err := upd(`status = 'pending', pending_expires_at = now(), activated_at = NULL`)
	wantCode(t, "active -> pending", err, "SF002")
	if !strings.Contains(err.Error(), "illegal status transition") {
		t.Fatalf("active -> pending must be refused by the status graph itself: %v", err)
	}
	if err := upd(`last_used_step = last_used_step`); err != nil {
		t.Fatalf("a no-op update on an active factor must pass: %v", err)
	}
	// SF003: last_used_step never decreases; equal and higher are fine.
	wantCode(t, "last_used_step decrease", upd(`last_used_step = last_used_step - 1`), "SF003")
	if err := upd(`last_used_step = last_used_step + 1`); err != nil {
		t.Fatalf("increase: %v", err)
	}
	// Write-once: activated_at, hard_locked_at.
	wantCode(t, "activated_at rewrite", upd(`activated_at = activated_at + interval '1 second'`), "SF002")
	if err := upd(`failed_24h_count = 10, failed_24h_start = now(), hard_locked_at = now()`); err != nil {
		t.Fatalf("setting the hard lock: %v", err)
	}
	wantCode(t, "hard_locked_at cleared", upd(`hard_locked_at = NULL`), "SF002")
	wantCode(t, "hard_locked_at moved", upd(`hard_locked_at = hard_locked_at + interval '1 second'`), "SF002")
	// active -> revoked, then frozen.
	if err := upd(`status = 'revoked', revoked_at = now(), revoked_reason = 'reset'`); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	err = upd(`status = 'active'`)
	wantCode(t, "revoked -> active", err, "SF002")
	if !strings.Contains(err.Error(), "illegal status transition") {
		t.Fatalf("revoked -> active must be refused by the status graph itself: %v", err)
	}
	err = upd(`status = 'pending', pending_expires_at = now()`)
	wantCode(t, "revoked -> pending", err, "SF002")
	if !strings.Contains(err.Error(), "illegal status transition") {
		t.Fatalf("revoked -> pending must be refused by the status graph itself: %v", err)
	}
	wantCode(t, "revoked frozen (counters)", upd(`failed_count = 1, failed_window_start = now()`), "SF002")
	wantCode(t, "revocation rewrite", upd(`revoked_reason = 'replaced'`), "SF002")

	// pending: expiry may only be cleared; pending -> active / revoked are legal.
	pid, _ := w.beginPending(w.a2, t0)
	pupd := func(set string) error {
		return w.ownerExec(w.a2, `UPDATE staff_mfa_factors SET `+set+` WHERE id = $1`, pid)
	}
	wantCode(t, "pending expiry extended", pupd(`pending_expires_at = pending_expires_at + interval '1 hour'`), "SF002")
	if err := pupd(`failed_count = 1, failed_window_start = now()`); err != nil {
		t.Fatalf("counters on a pending factor: %v", err)
	}
	if err := pupd(`status = 'active', activated_at = now(), pending_expires_at = NULL`); err != nil {
		t.Fatalf("pending -> active: %v", err)
	}
	pid2, _ := w.beginPending(w.a1, t0.Add(time.Hour))
	if err := w.ownerExec(w.a1, `UPDATE staff_mfa_factors SET status = 'revoked', revoked_at = now(), revoked_reason = 'pending_superseded', pending_expires_at = NULL WHERE id = $1`, pid2); err != nil {
		t.Fatalf("pending -> revoked: %v", err)
	}

	// The runtime role has no UPDATE grant on the immutable columns at all (permission denied before any trigger).
	for _, col := range []string{"secret_ct", "secret_nonce", "secret_kid", "tenant_id", "staff_user_id", "factor_type", "created_at", "id"} {
		err := w.rtExec(w.a2, `UPDATE staff_mfa_factors SET `+col+` = `+col+` WHERE id = $1`, pid)
		wantCode(t, "runtime update of "+col, err, "42501")
	}
}

// beginPending begins (and commits) a pending factor, returning its id and secret.
func (w *world) beginPending(ref staffmfa.StaffRef, at time.Time) (uuid.UUID, []byte) {
	w.t.Helper()
	var id uuid.UUID
	var secret []byte
	w.mustRun(ref, func(ctx context.Context, tx pgx.Tx) error {
		var m staffmfa.EnrolmentMaterial
		var err error
		id, m, err = w.factor.Begin(ctx, tx, ref, at)
		secret = secretFrom(w.t, m)
		return err
	})
	return id, secret
}

func TestChecks_FactorTable(t *testing.T) {
	w := newWorld(t)
	id, _ := w.enrol(w.a1, t0)
	upd := func(set string) error {
		return w.ownerExec(w.a1, `UPDATE staff_mfa_factors SET `+set+` WHERE id = $1`, id)
	}
	// C4 (security review of S1): the 24 hour count at the hard-lock threshold needs hard_locked_at.
	wantCheck(t, "C4: count 10 without hard_locked_at", upd(`failed_24h_count = 10, failed_24h_start = now()`), "staff_mfa_factors_hard_lock_consistent")
	wantCheck(t, "count above 10", upd(`failed_24h_count = 11, failed_24h_start = now(), hard_locked_at = now()`), "staff_mfa_factors_failed_24h_range")
	// ADR 0119 S3 (migration 0138): probed BEFORE the count is raised below - from a non-zero count this update would
	// be a decrease, which the SF008 database-clock rule refuses before any CHECK runs.
	wantCheck(t, "24h window without a count", upd(`failed_24h_count = 0, failed_24h_start = now()`), "staff_mfa_factors_window_24h_agrees")
	if err := upd(`failed_24h_count = 9, failed_24h_start = now()`); err != nil {
		t.Fatalf("count 9 without a hard lock must pass: %v", err)
	}
	// Counters mirror staffmfa.Counters.valid().
	wantCheck(t, "failed_count 5", upd(`failed_count = 5, failed_window_start = now()`), "staff_mfa_factors_failed_count_range")
	wantCheck(t, "failed_count 1 without a window", upd(`failed_count = 1`), "staff_mfa_factors_window_agrees")
	wantCheck(t, "a window without a count", upd(`failed_window_start = now()`), "staff_mfa_factors_window_agrees")
	// Row shape.
	ins := func(status, extra string) error {
		return w.ownerExec(w.a2, `INSERT INTO staff_mfa_factors (tenant_id, staff_user_id, factor_type, status, secret_kid, secret_nonce, secret_ct`+
			extraCols(extra)+`) VALUES ($1, $2, 'totp', '`+status+`', 'k', '\x000000000000000000000000', '\x01'`+extraVals(extra)+`)`, w.tenantA, w.a2.StaffID)
	}
	wantCheck(t, "pending without an expiry", ins("pending", ""), "staff_mfa_factors_pending_expiry")
	wantCheck(t, "active with a pending expiry", ins("active", "pending_expires_at,now()+interval '1 minute'|activated_at,now()"), "staff_mfa_factors_pending_expiry")
	wantCheck(t, "active without activated_at", ins("active", ""), "staff_mfa_factors_activation")
	wantCheck(t, "pending with activated_at", ins("pending", "pending_expires_at,now()+interval '1 minute'|activated_at,now()"), "staff_mfa_factors_activation")
	wantCheck(t, "revoked without a reason", ins("revoked", "revoked_at,now()"), "staff_mfa_factors_revocation")
	wantCheck(t, "reason without revocation", ins("pending", "pending_expires_at,now()+interval '1 minute'|revoked_reason,'reset'"), "staff_mfa_factors_revocation")
	if err := w.ownerExec(w.a2, `INSERT INTO staff_mfa_factors (tenant_id, staff_user_id, factor_type, status, secret_kid, secret_nonce, secret_ct, activated_at)
		VALUES ($1, $2, 'totp', 'active', 'k', '\x00000000000000000000', '\x01', now())`, w.tenantA, w.a2.StaffID); pgCode(err) != "23514" {
		t.Fatalf("a short nonce must fail its CHECK: %v", err)
	}
	if err := w.ownerExec(w.a2, `INSERT INTO staff_mfa_factors (tenant_id, staff_user_id, factor_type, status, secret_kid, secret_nonce, secret_ct, activated_at)
		VALUES ($1, $2, 'webauthn', 'active', 'k', '\x000000000000000000000000', '\x01', now())`, w.tenantA, w.a2.StaffID); pgCode(err) != "23514" {
		t.Fatalf("an unknown factor type must fail its CHECK: %v", err)
	}
	if err := w.ownerExec(w.a2, `INSERT INTO staff_mfa_factors (tenant_id, staff_user_id, factor_type, status, secret_kid, secret_nonce, secret_ct, activated_at)
		VALUES ($1, $2, 'totp', 'active', 'k', '\x000000000000000000000000', '\x', now())`, w.tenantA, w.a2.StaffID); pgCode(err) != "23514" {
		t.Fatalf("an empty ciphertext must fail its CHECK: %v", err)
	}
	if err := w.ownerExec(w.a2, `INSERT INTO staff_mfa_factors (tenant_id, staff_user_id, factor_type, status, secret_kid, secret_nonce, secret_ct, activated_at, last_used_step)
		VALUES ($1, $2, 'totp', 'active', 'k', '\x000000000000000000000000', '\x01', now(), -1)`, w.tenantA, w.a2.StaffID); pgCode(err) != "23514" {
		t.Fatalf("a negative last_used_step must fail its CHECK: %v", err)
	}
}

func extraCols(extra string) string {
	cols := ""
	for _, kv := range splitPairs(extra) {
		cols += ", " + kv[0]
	}
	return cols
}

func extraVals(extra string) string {
	vals := ""
	for _, kv := range splitPairs(extra) {
		vals += ", " + kv[1]
	}
	return vals
}

func splitPairs(s string) [][2]string {
	if s == "" {
		return nil
	}
	var out [][2]string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '|' {
			part := s[start:i]
			for j := 0; j < len(part); j++ {
				if part[j] == ',' {
					out = append(out, [2]string{part[:j], part[j+1:]})
					break
				}
			}
			start = i + 1
		}
	}
	return out
}

func TestUniqueIndexes(t *testing.T) {
	w := newWorld(t)
	w.enrol(w.a1, t0)
	// A second ACTIVE factor of the type.
	err := w.ownerExec(w.a1, `INSERT INTO staff_mfa_factors (tenant_id, staff_user_id, factor_type, status, secret_kid, secret_nonce, secret_ct, activated_at)
		VALUES ($1, $2, 'totp', 'active', 'k', '\x000000000000000000000000', '\x01', now())`, w.tenantA, w.a1.StaffID)
	wantCode(t, "second active factor", err, "23505")
	w.beginPending(w.a1, t0.Add(time.Hour))
	err = w.ownerExec(w.a1, `INSERT INTO staff_mfa_factors (tenant_id, staff_user_id, factor_type, status, secret_kid, secret_nonce, secret_ct, pending_expires_at)
		VALUES ($1, $2, 'totp', 'pending', 'k', '\x000000000000000000000000', '\x01', now() + interval '1 minute')`, w.tenantA, w.a1.StaffID)
	wantCode(t, "second pending factor", err, "23505")
	// A MAC is globally unique (recovery codes, challenges, tokens).
	mac := randMAC()
	for table, q := range insertSQL {
		if table == "staff_mfa_factors" {
			continue
		}
		if err := w.rtExec(w.a1, q, nil, w.a1.StaffID, mac); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		wantCode(t, table+": duplicate MAC", w.rtExec(w.a2, q, nil, w.a2.StaffID, mac), "23505")
	}
	// One live token per (staff, purpose).
	err = w.rtExec(w.a1, insertSQL["staff_enrolment_tokens"], nil, w.a1.StaffID, randMAC())
	wantCode(t, "second live invite token", err, "23505")
	// A MAC must be 32 bytes; a challenge needs 0..5 attempts and a future expiry.
	wantCheck(t, "short MAC (recovery)", w.rtExec(w.a1, insertSQL["staff_mfa_recovery_codes"], nil, w.a1.StaffID, []byte{1, 2, 3}), "staff_mfa_recovery_codes_code_mac_check")
	wantCheck(t, "short MAC (challenge)", w.rtExec(w.a1, insertSQL["staff_auth_challenges"], nil, w.a1.StaffID, []byte{1}), "staff_auth_challenges_token_mac_check")
	wantCheck(t, "short MAC (token)", w.rtExec(w.a2, insertSQL["staff_enrolment_tokens"], nil, w.a2.StaffID, []byte{1}), "staff_enrolment_tokens_token_mac_check")
	err = w.rtExec(w.a1, `INSERT INTO staff_auth_challenges (tenant_id, staff_user_id, purpose, token_kid, token_mac, staff_version_at_issue, attempts_left, expires_at, via)
		VALUES (NULL, $1, 'mfa', 'k', $2, 0, 6, now() + interval '5 minutes', 'login')`, w.a1.StaffID, randMAC())
	if pgCode(err) != "23514" {
		t.Fatalf("attempts_left 6: %v", err)
	}
	wantCheck(t, "expiry before creation", w.rtExec(w.a1, `INSERT INTO staff_auth_challenges (tenant_id, staff_user_id, purpose, token_kid, token_mac, staff_version_at_issue, attempts_left, expires_at, via)
		VALUES (NULL, $1, 'mfa', 'k', $2, 0, 5, now() - interval '1 minute', 'login')`, w.a1.StaffID, randMAC()), "staff_auth_challenges_expiry_after_creation")
	for name, bad := range map[string]string{
		"purpose": `'bogus', 'k', $2, 0, 5, now() + interval '5 minutes', 'login'`,
		"via":     `'mfa', 'k', $2, 0, 5, now() + interval '5 minutes', 'bogus'`,
	} {
		err := w.rtExec(w.a1, `INSERT INTO staff_auth_challenges (tenant_id, staff_user_id, purpose, token_kid, token_mac, staff_version_at_issue, attempts_left, expires_at, via)
			VALUES (NULL, $1, `+bad+`)`, w.a1.StaffID, randMAC())
		if pgCode(err) != "23514" {
			t.Fatalf("challenge %s vocabulary: %v", name, err)
		}
	}
	err = w.rtExec(w.a1, `INSERT INTO staff_enrolment_tokens (tenant_id, staff_user_id, purpose, token_kid, token_mac, expires_at)
		VALUES (NULL, $1, 'bogus', 'k', $2, now() + interval '1 hour')`, w.a1.StaffID, randMAC())
	if pgCode(err) != "23514" {
		t.Fatalf("token purpose vocabulary: %v", err)
	}
}

func TestSF004_RecoveryCodeConsumeOrRevokeOnly(t *testing.T) {
	w := newWorld(t)
	w.enrol(w.a1, t0)
	w.issueCodes(w.a1, t0)

	// SF004 recovery codes.
	for name, set := range map[string]string{
		"code_mac":   `code_mac = '\x0101010101010101010101010101010101010101010101010101010101010101'`,
		"code_kid":   `code_kid = 'other'`,
		"batch_id":   `batch_id = gen_random_uuid()`,
		"created_at": `created_at = created_at + interval '1 second'`,
		"tenant_id":  `tenant_id = '` + w.tenantB.String() + `'`,
	} {
		wantCode(t, "recovery "+name, w.ownerExec(w.a1, `UPDATE staff_mfa_recovery_codes SET `+set+` WHERE id = (SELECT id FROM staff_mfa_recovery_codes WHERE staff_user_id = $1 LIMIT 1)`, w.a1.StaffID), "SF004")
	}
	one := `WHERE id = (SELECT id FROM staff_mfa_recovery_codes WHERE staff_user_id = $1 ORDER BY id LIMIT 1)`
	if err := w.rtExec(w.a1, `UPDATE staff_mfa_recovery_codes SET consumed_at = now() `+one, w.a1.StaffID); err != nil {
		t.Fatalf("consume: %v", err)
	}
	wantCode(t, "recovery consumed rewrite", w.ownerExec(w.a1, `UPDATE staff_mfa_recovery_codes SET consumed_at = NULL `+one, w.a1.StaffID), "SF004")
	wantCode(t, "recovery consumed moved", w.ownerExec(w.a1, `UPDATE staff_mfa_recovery_codes SET consumed_at = now() + interval '1 hour' `+one, w.a1.StaffID), "SF004")
	wantCode(t, "recovery consumed AND revoked", w.ownerExec(w.a1, `UPDATE staff_mfa_recovery_codes SET revoked_at = now() `+one, w.a1.StaffID), "SF004")
	other := `WHERE id = (SELECT id FROM staff_mfa_recovery_codes WHERE staff_user_id = $1 AND consumed_at IS NULL ORDER BY id LIMIT 1)`
	if err := w.rtExec(w.a1, `UPDATE staff_mfa_recovery_codes SET revoked_at = now() `+other, w.a1.StaffID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	wantCode(t, "recovery revoked then consumed", w.ownerExec(w.a1, `UPDATE staff_mfa_recovery_codes SET consumed_at = now() WHERE staff_user_id = $1 AND revoked_at IS NOT NULL`, w.a1.StaffID), "SF004")

}

func TestSF005_SF006_ChallengeAndTokenGuards(t *testing.T) {
	w := newWorld(t)
	chRaw := w.issueChallenge(w.a1, ChallengeMFA, OriginLogin, t0).RevealForDelivery()
	chRef, _ := w.st.ResolveChallenge(context.Background(), w.rt, ChallengeMFA, chRaw)
	cu := func(set string) error {
		return w.ownerExec(w.a1, `UPDATE staff_auth_challenges SET `+set+` WHERE id = $1`, chRef.ID)
	}
	for name, set := range map[string]string{
		"token_mac":              `token_mac = '\x0101010101010101010101010101010101010101010101010101010101010101'`,
		"token_kid":              `token_kid = 'other'`,
		"purpose":                `purpose = 'enrol'`,
		"via":                    `via = 'invite'`,
		"expires_at":             `expires_at = expires_at + interval '1 hour'`,
		"staff_version_at_issue": `staff_version_at_issue = 9`,
		"staff_user_id":          `staff_user_id = '` + w.a2.StaffID.String() + `'`,
		"created_at":             `created_at = created_at - interval '1 hour'`,
	} {
		wantCode(t, "challenge "+name, cu(set), "SF005")
	}
	if err := cu(`attempts_left = 4`); err != nil {
		t.Fatalf("decrement: %v", err)
	}
	wantCode(t, "attempts increased", cu(`attempts_left = 5`), "SF005")
	if err := cu(`attempts_left = 4`); err != nil {
		t.Fatalf("no-op: %v", err)
	}
	if err := cu(`attempts_left = 3, consumed_at = now()`); err != nil {
		t.Fatalf("decrement + consume: %v", err)
	}
	wantCode(t, "consumed challenge frozen (attempts)", cu(`attempts_left = 2`), "SF005")
	wantCode(t, "consumed challenge frozen (consumed_at)", cu(`consumed_at = NULL`), "SF005")
	wantCode(t, "consumed challenge frozen (consumed_at moved)", cu(`consumed_at = now() + interval '1 hour'`), "SF005")

	// SF006 enrolment tokens.
	w.issueToken(w.a1, EnrolInvite, t0)
	tu := func(set string) error {
		return w.ownerExec(w.a1, `UPDATE staff_enrolment_tokens SET `+set+` WHERE staff_user_id = $1 AND purpose = 'invite'`, w.a1.StaffID)
	}
	for name, set := range map[string]string{
		"token_mac":            `token_mac = '\x0101010101010101010101010101010101010101010101010101010101010101'`,
		"token_kid":            `token_kid = 'other'`,
		"purpose":              `purpose = 'bootstrap'`,
		"expires_at":           `expires_at = expires_at + interval '1 hour'`,
		"issued_by":            `issued_by = '` + w.a2.StaffID.String() + `'`,
		"lifecycle_request_id": `lifecycle_request_id = gen_random_uuid()`,
		"created_at":           `created_at = created_at - interval '1 hour'`,
		"tenant_id":            `tenant_id = '` + w.tenantB.String() + `'`,
	} {
		wantCode(t, "token "+name, tu(set), "SF006")
	}
	if err := w.rtExec(w.a1, `UPDATE staff_enrolment_tokens SET consumed_at = now() WHERE staff_user_id = $1 AND purpose = 'invite'`, w.a1.StaffID); err != nil {
		t.Fatalf("consume: %v", err)
	}
	wantCode(t, "token consumed rewrite", tu(`consumed_at = NULL`), "SF006")
	wantCode(t, "token consumed AND revoked", tu(`revoked_at = now()`), "SF006")
	w.issueToken(w.a1, EnrolMFAReset, t0)
	if err := w.rtExec(w.a1, `UPDATE staff_enrolment_tokens SET revoked_at = now() WHERE staff_user_id = $1 AND purpose = 'mfa_reset'`, w.a1.StaffID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	wantCode(t, "token revoked then consumed", w.ownerExec(w.a1, `UPDATE staff_enrolment_tokens SET consumed_at = now() WHERE staff_user_id = $1 AND purpose = 'mfa_reset'`, w.a1.StaffID), "SF006")
}

// SF099: no DELETE and no TRUNCATE, for every role (the runtime role has no grant; the owner is stopped by the trigger).
func TestSF099_AppendOnly_EveryRole(t *testing.T) {
	w := newWorld(t)
	w.enrol(w.a1, t0)
	w.issueCodes(w.a1, t0)
	w.issueChallenge(w.a1, ChallengeMFA, OriginLogin, t0)
	w.issueToken(w.a1, EnrolInvite, t0)
	for _, table := range []string{"staff_mfa_factors", "staff_mfa_recovery_codes", "staff_auth_challenges", "staff_enrolment_tokens"} {
		wantCode(t, table+": runtime DELETE", w.rtExec(w.a1, `DELETE FROM `+table+` WHERE staff_user_id = $1`, w.a1.StaffID), "42501")
		wantCode(t, table+": owner DELETE", w.ownerExec(w.a1, `DELETE FROM `+table+` WHERE staff_user_id = $1`, w.a1.StaffID), "SF099")
		wantCode(t, table+": runtime TRUNCATE", w.rtExec(w.a1, `TRUNCATE `+table), "42501")
		wantCode(t, table+": owner TRUNCATE", w.ownerExec(w.a1, `TRUNCATE `+table), "SF099")
		wantCode(t, table+": owner TRUNCATE CASCADE", w.ownerExec(w.a1, `TRUNCATE `+table+` CASCADE`), "SF099")
	}
	// Nothing was removed.
	var n int
	w.mustOwner(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM staff_mfa_factors WHERE staff_user_id = $1)
			+ (SELECT count(*) FROM staff_mfa_recovery_codes WHERE staff_user_id = $1)
			+ (SELECT count(*) FROM staff_auth_challenges WHERE staff_user_id = $1)
			+ (SELECT count(*) FROM staff_enrolment_tokens WHERE staff_user_id = $1)`, w.a1.StaffID).Scan(&n)
	})
	if n != 1+staffmfa.RecoveryCodesPerBatch+1+1 {
		t.Fatalf("rows after the refused deletes = %d", n)
	}
}
