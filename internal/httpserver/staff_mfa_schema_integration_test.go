//go:build integration

package httpserver

// ADR 0119 S3 schema controls on the shared database (migration 0138): the SF040 sessions matrix, the SF008
// database-clock rule on the hard-lock budget (S3 entry gate G2), and the runtime role's privileges on sessions and
// the cutover marker. Assertions about what the runtime role may do run AS the runtime role.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/staffmfa/store"
)

func TestSessions_SF040Matrix(t *testing.T) {
	w := newMFAW(t)
	s := w.staff(&w.tenant, "support")
	all := map[string]string{"aal": "2", "amr": "ARRAY['pwd','otp']", "auth_time": "now()", "mfa_at": "now()",
		"absolute_expires_at": "now() + interval '2 hours'", "assurance_kid": "'m1'", "assurance_seal": "decode(repeat('ab', 32), 'hex')"}
	insert := func(pool *db.Pool, principal string, cols map[string]string) error {
		names, vals := []string{}, []string{}
		for k, v := range cols {
			names, vals = append(names, k), append(vals, v)
		}
		extraN, extraV := "", ""
		if len(names) > 0 {
			extraN, extraV = ", "+strings.Join(names, ", "), ", "+strings.Join(vals, ", ")
		}
		sum := sha256.Sum256([]byte(uuid.NewString()))
		return store.WithScope(context.Background(), pool, s.ref, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO sessions (principal_type, principal_id, tenant_id, refresh_token_hash, expires_at`+extraN+`)
				VALUES ($1, $2, $3, $4, now() + interval '1 hour'`+extraV+`)`, principal, s.ref.StaffID, s.ref.TenantID, hex.EncodeToString(sum[:]))
			return err
		})
	}
	// A complete staff row is accepted (control), for both roles.
	for _, p := range []*db.Pool{w.rt, w.owner} {
		if err := insert(p, "staff", all); err != nil {
			t.Fatalf("a complete staff session must be accepted: %v", err)
		}
	}
	// Each missing assurance column: SF040 naming it, for the runtime role and the owner.
	for col := range all {
		partial := map[string]string{}
		for k, v := range all {
			if k != col {
				partial[k] = v
			}
		}
		for _, p := range []*db.Pool{w.rt, w.owner} {
			err := insert(p, "staff", partial)
			if pgCode(err) != "SF040" && col != "assurance_kid" && col != "assurance_seal" {
				t.Fatalf("staff session without %s: want SF040, got %v", col, err)
			}
			// kid / seal: the shape CHECK (kid and seal together) may fire first; either refusal is correct
			if (col == "assurance_kid" || col == "assurance_seal") && pgCode(err) != "SF040" && pgCode(err) != "23514" {
				t.Fatalf("staff session without %s: want SF040 or 23514, got %v", col, err)
			}
			if pgCode(err) == "SF040" && !strings.Contains(err.Error(), col) {
				t.Fatalf("the SF040 refusal must name %s: %v", col, err)
			}
		}
	}
	// A player row never carries assurance values; shape CHECKs.
	for name, c := range map[string]struct {
		principal string
		cols      map[string]string
	}{
		"player with aal": {"player", map[string]string{"aal": "2"}},
		"aal 1":           {"staff", merge(all, map[string]string{"aal": "1"})},
		"short seal":      {"staff", merge(all, map[string]string{"assurance_seal": "decode(repeat('ab', 31), 'hex')"})},
		"bad kid":         {"staff", merge(all, map[string]string{"assurance_kid": "'has space'"})},
		"idle past abs":   {"staff", merge(all, map[string]string{"absolute_expires_at": "now() + interval '1 minute'"})},
		"mfa before auth": {"staff", merge(all, map[string]string{"mfa_at": "now() - interval '1 hour'"})},
		"empty amr":       {"staff", merge(all, map[string]string{"amr": "ARRAY[]::text[]"})},
	} {
		if err := insert(w.owner, c.principal, c.cols); pgCode(err) != "23514" {
			t.Fatalf("%s: want CHECK 23514, got %v", name, err)
		}
	}
	// UPDATE: every immutable column of a staff row, as the owner (the runtime role has no grant at all).
	_, id := w.plantSession(s, nowUTC(), nowUTC().Add(time.Hour*2), nowUTC().Add(30*time.Minute), nil)
	for _, set := range []string{"aal = 2", "amr = ARRAY['pwd']", "auth_time = auth_time - interval '1 second'", "absolute_expires_at = absolute_expires_at + interval '1 hour'",
		"principal_id = gen_random_uuid()", "created_at = now() - interval '1 day'", "refresh_token_hash = 'x'", "principal_type = 'player'", "id = gen_random_uuid()",
		"expires_at = expires_at - interval '1 second'"} {
		err := store.WithScope(context.Background(), w.owner, s.ref, func(ctx context.Context, tx pgx.Tx) error {
			if err := db.SetSessionInternalOpID(ctx, tx, id); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE sessions SET `+set+` WHERE id = $1`, id)
			return err
		})
		if set == "aal = 2" {
			// re-setting the same value is not a change
			if err != nil {
				t.Fatalf("an unchanged aal must pass: %v", err)
			}
			continue
		}
		if pgCode(err) != "SF040" {
			t.Fatalf("owner UPDATE %s on a staff session: want SF040, got %v", set, err)
		}
	}
	// The runtime role holds no UPDATE on mfa_at or the seal (review F9: granted by S4b, the slice that writes them);
	// SF040 lets the owner move mfa_at forward and rewrite the seal (the S4b step-up re-seal shape).
	if err := w.runtimeSessionExec(s, id, `UPDATE sessions SET mfa_at = mfa_at + interval '1 second' WHERE id = $1`); pgCode(err) != "42501" {
		t.Fatalf("runtime UPDATE mfa_at: want 42501, got %v", err)
	}
	if err := w.ownerSessionExec(s, id, `UPDATE sessions SET mfa_at = mfa_at + interval '1 second', assurance_seal = decode(repeat('cd', 32), 'hex') WHERE id = $1`); err != nil {
		t.Fatalf("SF040 admits mfa_at moving forward and a rewritten seal: %v", err)
	}

	// Security review R1: revoked_at and replaced_by_session_id are WRITE-ONCE on a staff row, for the runtime role
	// (which holds the column grant) and the owner alike. Setting them once, and re-setting the same value (the
	// COALESCE of revokeChainFrom), passes.
	for name, exec := range map[string]func(s mfaStaff, id uuid.UUID, sql string, args ...any) error{"runtime": w.runtimeSessionExec, "owner": w.ownerSessionExec} {
		_, victim := w.plantSession(s, nowUTC(), nowUTC().Add(time.Hour*2), nowUTC().Add(30*time.Minute), nil)
		_, successor := w.plantSession(s, nowUTC(), nowUTC().Add(time.Hour*2), nowUTC().Add(30*time.Minute), nil)
		_, other := w.plantSession(s, nowUTC(), nowUTC().Add(time.Hour*2), nowUTC().Add(30*time.Minute), nil)
		if err := exec(s, victim, `UPDATE sessions SET revoked_at = now(), replaced_by_session_id = $2 WHERE id = $1`, successor); err != nil {
			t.Fatalf("%s: the first revocation and chain link must pass: %v", name, err)
		}
		if err := exec(s, victim, `UPDATE sessions SET revoked_at = COALESCE(revoked_at, now()), last_used_at = now() WHERE id = $1`); err != nil {
			t.Fatalf("%s: re-setting the same revoked_at must pass: %v", name, err)
		}
		for _, set := range []string{"revoked_at = NULL", "revoked_at = revoked_at + interval '1 second'", "replaced_by_session_id = NULL"} {
			if err := exec(s, victim, `UPDATE sessions SET `+set+` WHERE id = $1`); pgCode(err) != "SF040" {
				t.Fatalf("%s UPDATE %s: want SF040, got %v", name, set, err)
			}
		}
		if err := exec(s, victim, `UPDATE sessions SET replaced_by_session_id = $2 WHERE id = $1`, other); pgCode(err) != "SF040" {
			t.Fatalf("%s: re-pointing the chain link: want SF040, got %v", name, err)
		}
	}
}

func merge(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// S3 entry gate G2, option "database-clock rule" (migration 0138, SF008): the runtime role (threat M5) cannot reset
// the hard-lock budget - failed_24h_count decreases only once its window start is 24 hours old by the database clock,
// and the window start never moves backwards.
func TestFactorLockoutClock_SF008(t *testing.T) {
	w := newMFAW(t)
	s := w.staff(&w.tenant, "support")
	plant := func(start string, count int) uuid.UUID {
		id := uuid.New()
		w.inScope(s, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE staff_mfa_factors SET status = 'revoked', revoked_at = now(), revoked_reason = 'reset' WHERE staff_user_id = $1 AND status = 'active'`, s.ref.StaffID); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO staff_mfa_factors (id, tenant_id, staff_user_id, factor_type, status, secret_kid, secret_nonce, secret_ct, activated_at,
					failed_24h_count, failed_24h_start)
				VALUES ($1, $2, $3, 'totp', 'active', 'f1', '\x000000000000000000000000', '\x01', now(), $4, now() - $5::interval)`, id, s.ref.TenantID, s.ref.StaffID, count, start)
			return err
		})
		return id
	}
	rt := func(id uuid.UUID, set string) error {
		return store.WithScope(context.Background(), w.rt, s.ref, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE staff_mfa_factors SET `+set+` WHERE id = $1`, id)
			return err
		})
	}
	id := plant("1 hour", 8)
	for _, set := range []string{
		"failed_24h_count = 0, failed_24h_start = NULL",
		"failed_24h_count = 1, failed_24h_start = now()",
		"failed_24h_count = 7",
	} {
		if err := rt(id, set); pgCode(err) != "SF008" {
			t.Fatalf("a decrease inside the window (%s): want SF008, got %v", set, err)
		}
	}
	if err := rt(id, "failed_24h_start = failed_24h_start - interval '2 days'"); pgCode(err) != "SF008" {
		t.Fatalf("backdating the window start: want SF008, got %v", err)
	}
	// Increases and a forward move stay possible (the application's own failure writes).
	if err := rt(id, "failed_24h_count = 9"); err != nil {
		t.Fatalf("an increase must pass: %v", err)
	}
	if err := rt(id, "failed_24h_start = failed_24h_start + interval '1 minute'"); err != nil {
		t.Fatalf("a forward move must pass: %v", err)
	}
	// After 24 hours by the DATABASE clock the window may restart.
	old := plant("25 hours", 8)
	if err := rt(old, "failed_24h_count = 1, failed_24h_start = now()"); err != nil {
		t.Fatalf("a window older than 24 h may restart: %v", err)
	}
}

func nowUTC() time.Time { return time.Now().UTC() }

// The runtime role's privileges (ADR 0119 s13.4, migration 0138): UPDATE on sessions only on the columns the
// application writes; SELECT only on the cutover marker.
func TestGrants_SessionsAndCutover_RuntimeRole(t *testing.T) {
	w := newMFAW(t)
	ctx := context.Background()
	var cols []string
	var tableUpdate bool
	if err := w.rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT has_table_privilege(current_user, 'public.sessions', 'UPDATE')`).Scan(&tableUpdate); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT attname FROM pg_attribute WHERE attrelid = 'public.sessions'::regclass AND attnum > 0 AND NOT attisdropped
			AND has_column_privilege(current_user, 'public.sessions', attname, 'UPDATE')`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				return err
			}
			cols = append(cols, c)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(cols)
	want := []string{"last_used_at", "replaced_by_session_id", "revoked_at"}
	if tableUpdate || strings.Join(cols, ",") != strings.Join(want, ",") {
		t.Fatalf("runtime UPDATE on sessions: table=%v columns=%v, want table=false columns=%v", tableUpdate, cols, want)
	}
	// Security review C3: the runtime role has no path to rewrite refresh_token_hash or expires_at (pinned by the
	// column list above) and no permissive DELETE or ALL policy exists on sessions (its DELETE privilege matches no
	// row); the permissive policies are exactly these.
	var permissive []string
	if err := w.owner.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT policyname || ':' || cmd FROM pg_policies WHERE schemaname = 'public' AND tablename = 'sessions'
			AND permissive = 'PERMISSIVE' ORDER BY 1`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				return err
			}
			permissive = append(permissive, p)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(permissive, ","); got != "session_scoped_insert:INSERT,session_scoped_update:UPDATE,session_select_by_token_hash:SELECT,session_select_internal_op:SELECT,session_select_own_principal:SELECT" {
		t.Fatalf("permissive policies on sessions changed (no DELETE / ALL policy may exist): %s", got)
	}
	var sel, ins, upd, del bool
	if err := w.rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT has_table_privilege(current_user, 'public.staff_session_cutover', 'SELECT'),
			has_table_privilege(current_user, 'public.staff_session_cutover', 'INSERT'),
			has_table_privilege(current_user, 'public.staff_session_cutover', 'UPDATE'),
			has_table_privilege(current_user, 'public.staff_session_cutover', 'DELETE')`).Scan(&sel, &ins, &upd, &del)
	}); err != nil {
		t.Fatal(err)
	}
	if !sel || ins || upd || del {
		t.Fatalf("runtime on staff_session_cutover: select=%v insert=%v update=%v delete=%v, want SELECT only", sel, ins, upd, del)
	}
	// The runtime role reads the marker in an ordinary session, never as a platform service.
	var n int
	if err := w.rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM staff_session_cutover`).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("the cutover marker must be readable by the runtime role: %d %v", n, err)
	}
	if err := w.rt.WithPlatformService(ctx, db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM staff_session_cutover`).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("a platform-service session must not read the cutover marker: %d %v", n, err)
	}
	// deploy/init-app-role.sql mirrors the narrowing (re-asserted after its blanket backfill GRANT).
	b, err := os.ReadFile("../../deploy/init-app-role.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, must := range []string{"REVOKE UPDATE ON sessions FROM igaming_runtime",
		"GRANT UPDATE (revoked_at, last_used_at, replaced_by_session_id) ON sessions TO igaming_runtime",
		"GRANT SELECT ON staff_session_cutover TO igaming_runtime"} {
		if !strings.Contains(string(b), must) {
			t.Fatalf("deploy/init-app-role.sql must mirror migration 0138: missing %q", must)
		}
	}
}
