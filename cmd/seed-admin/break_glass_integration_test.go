//go:build integration

package main

// ADR 0119 S6 section 11: cmd/seed-admin -break-glass (and the Person requirement of every created platform_admin).

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth/authtest"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

func TestSeedAdmin_BreakGlass_Subprocess(t *testing.T) {
	// A scratch database (review fix F4): the usable / not-usable platform admin state is this test's own.
	ownerURL, pool := seedScratch(t)
	bin := buildSeedAdmin(t)
	fk, mk := randKey(t), randKey(t)
	env := []string{"PATH=" + os.Getenv("PATH"), "DATABASE_URL=" + ownerURL, "SEED_ADMIN_PASSWORD=a-seed-admin-password-1",
		"STAFF_MFA_FACTOR_KEYS=f1:" + fk, "STAFF_MFA_FACTOR_ACTIVE_KID=f1", "STAFF_MFA_MAC_KEYS=m1:" + mk, "STAFF_MFA_MAC_ACTIVE_KID=m1"}
	exists := func(email string) bool {
		var ok bool
		_ = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM staff_users WHERE email = $1)`, email).Scan(&ok)
		})
		return ok
	}

	// A usable platform admin exists (an active row with an active factor).
	usable := uuid.New()
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `WITH p AS (INSERT INTO persons (id) VALUES ($1))
			INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id) VALUES ($1, NULL, $2, 'x', 'platform_admin', $1)`,
			usable, "bg-usable-"+usable.String()+"@fixture.invalid"); err != nil {
			return err
		}
		return authtest.EnsurePlaceholderFactor(ctx, tx, usable)
	}); err != nil {
		t.Fatal(err)
	}

	// 1. A platform_admin always needs a Person (SF014): the create and -break-glass modes refuse without one.
	for _, args := range [][]string{append([]string{}, opArgs...), {"-break-glass", "-operator=ops-alice", "-reason=INC-9", "-force-additional"}} {
		email := "bg-noperson-" + uuid.NewString() + "@fixture.invalid"
		_, stderr, code := runSeedAdmin(t, bin, env, append([]string{"-email=" + email}, args...)...)
		if code == 0 || !strings.Contains(stderr, "requires a Person") || exists(email) {
			t.Fatalf("no Person %v must refuse and create nothing: %d %s", args, code, stderr)
		}
	}
	// 1b. Review fix SR-1 / F1: the DEFAULT mode is refused while a platform_admin exists (with a Person, an operator
	// and a reason): no row, no token, no event.
	sneaky := "bg-sneaky-" + uuid.NewString() + "@fixture.invalid"
	stdout0, stderr0, code0 := runSeedAdmin(t, bin, env, append([]string{"-email=" + sneaky, "-create-person"}, opArgs...)...)
	if code0 == 0 || !strings.Contains(stderr0, "a platform_admin already exists") || exists(sneaky) || len(tokenLine.FindAllString(stdout0, -1)) != 0 {
		t.Fatalf("the default mode with an existing platform_admin must refuse and create nothing: %d %q %s", code0, stdout0, stderr0)
	}
	// 2. -break-glass needs -operator and -reason.
	email := "bg-" + uuid.NewString() + "@fixture.invalid"
	for _, missing := range [][]string{{"-operator=ops-alice"}, {"-reason=INC-9"}} {
		_, _, code := runSeedAdmin(t, bin, env, append([]string{"-email=" + email, "-break-glass", "-create-person"}, missing...)...)
		if code == 0 || exists(email) {
			t.Fatalf("-break-glass without %v must refuse", missing)
		}
	}
	// 3. Refused while a usable platform admin exists (no -force-additional); -force-additional applies only to it.
	_, stderr, code := runSeedAdmin(t, bin, env, "-email="+email, "-break-glass", "-create-person", "-operator=ops-alice", "-reason=INC-9")
	if code == 0 || !strings.Contains(stderr, "usable second factor exists") || exists(email) {
		t.Fatalf("-break-glass with a usable admin must refuse: %d %s", code, stderr)
	}
	if _, _, code := runSeedAdmin(t, bin, env, append([]string{"-email=" + email, "-create-person", "-force-additional"}, opArgs...)...); code == 0 || exists(email) {
		t.Fatal("-force-additional without -break-glass must refuse")
	}
	// 4. -force-additional: created active with a Person, one token printed, owner_provisioned event, audit row, P1 alert.
	stdout, stderr, code := runSeedAdmin(t, bin, env, "-email="+email, "-break-glass", "-force-additional", "-create-person",
		"-operator=ops-alice", "-reason=INC-9 all admins lost")
	if code != 0 {
		t.Fatalf("-break-glass -force-additional: %d %s", code, stderr)
	}
	if toks := tokenLine.FindAllString(stdout, -1); len(toks) != 1 || strings.Contains(stderr, toks[0]) {
		t.Fatalf("exactly one bootstrap token on stdout: %q", stdout)
	}
	var staff identity.StaffUser
	var events, audits, alerts int
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if staff, err = identity.GetStaffUserByEmail(ctx, tx, email); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM staff_lifecycle_events WHERE staff_user_id = $1 AND event = 'owner_provisioned'
			AND actor_type = 'owner' AND actor_staff_id IS NULL AND reason_code = 'break_glass'`, staff.ID).Scan(&events); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE target_id = $1 AND action = 'staff.break_glass_used'
			AND metadata->>'operator' = 'ops-alice' AND (metadata->>'force_additional')::boolean`, staff.ID.String()).Scan(&audits)
	}); err != nil {
		t.Fatal(err)
	}
	if staff.Role != identity.StaffRolePlatformAdmin || staff.Status != "active" || staff.PersonID == nil || events != 1 || audits != 1 {
		t.Fatalf("break-glass result: %+v events=%d audits=%d", staff, events, audits)
	}
	if err := pool.WithPlatformAdmin(context.Background(), staff.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = 'staff.platform_break_glass_used' AND discriminator = $1`,
			"break_glass:"+staff.ID.String()).Scan(&alerts)
	}); err != nil {
		t.Fatal(err)
	}
	if alerts != 1 {
		t.Fatalf("the staff.platform_break_glass_used P1 alert must be raised once, got %d", alerts)
	}
}
