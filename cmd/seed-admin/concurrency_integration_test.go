//go:build integration

package main

// ADR 0119 S5 + S6 security re-review fix NR-3: the default mode's "first platform admin only" check is atomic. N
// parallel default runs on a database with no platform admin create EXACTLY ONE platform admin; every other run is
// refused with nothing created.

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestSeedAdmin_ParallelDefaultRuns_ExactlyOneFirstAdmin(t *testing.T) {
	ownerURL, pool := seedScratch(t)
	// A scratch-database-only widening of the window between the check and the INSERT (each run sleeps 2 s inside
	// its transaction before its staff row is written), so the runs overlap deterministically.
	if _, err := pool.Raw().Exec(context.Background(), `CREATE FUNCTION nr3_widen() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM pg_sleep(2); RETURN NEW; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Raw().Exec(context.Background(), `CREATE TRIGGER aa_nr3_widen BEFORE INSERT ON staff_users FOR EACH ROW EXECUTE FUNCTION nr3_widen()`); err != nil {
		t.Fatal(err)
	}
	bin := buildSeedAdmin(t)
	fk, mk := randKey(t), randKey(t)
	env := []string{"PATH=" + os.Getenv("PATH"), "DATABASE_URL=" + ownerURL, "SEED_ADMIN_PASSWORD=a-seed-admin-password-1",
		"STAFF_MFA_FACTOR_KEYS=f1:" + fk, "STAFF_MFA_FACTOR_ACTIVE_KID=f1", "STAFF_MFA_MAC_KEYS=m1:" + mk, "STAFF_MFA_MAC_ACTIVE_KID=m1"}
	const n = 4
	type res struct {
		code   int
		stderr string
	}
	results := make([]res, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, append([]string{"-email=par-" + uuid.NewString() + "@fixture.invalid", "-create-person"}, opArgs...)...)
			cmd.Env = env
			var e strings.Builder
			cmd.Stderr = &e
			err := cmd.Run()
			code := 0
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else if err != nil {
				code = -1
				e.WriteString(err.Error())
			}
			results[i] = res{code, e.String()}
		}(i)
	}
	close(start)
	wg.Wait()
	ok, refused := 0, 0
	for _, r := range results {
		switch {
		case r.code == 0:
			ok++
		case strings.Contains(r.stderr, "a platform_admin already exists"):
			refused++
		default:
			t.Fatalf("unexpected seed-admin outcome: %d %s", r.code, r.stderr)
		}
	}
	var admins int
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM staff_users WHERE tenant_id IS NULL AND role = 'platform_admin'`).Scan(&admins)
	}); err != nil {
		t.Fatal(err)
	}
	if ok != 1 || refused != n-1 || admins != 1 {
		t.Fatalf("%d parallel default runs: %d succeeded, %d refused, %d platform admins (want 1, %d, 1)", n, ok, refused, admins, n-1)
	}
}
