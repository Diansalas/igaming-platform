//go:build integration

package scratchdb

import (
	"context"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestReapDropsOnlyOldIdleScratchDatabases creates databases with
// pattern-matching and non-matching names and proves Reap drops exactly the
// old, idle, pattern-matching one and leaves the young, the in-use and the
// legacy-named ones alone. Every database it creates is dropped on cleanup.
func TestReapDropsOnlyOldIdleScratchDatabases(t *testing.T) {
	ownerURL, adminURL := os.Getenv("TEST_DATABASE_URL"), os.Getenv("TEST_ADMIN_DATABASE_URL")
	if ownerURL == "" || adminURL == "" {
		t.Skip("TEST_DATABASE_URL / TEST_ADMIN_DATABASE_URL not set")
	}
	ou, err := url.Parse(ownerURL)
	if err != nil || ou.User == nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	ownerRole := ou.User.Username()

	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect as test admin: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()

	now := time.Now()
	mk := func(age time.Duration, rnd string) string {
		n, err := BuildName("reaptest_", now.Add(-age), os.Getpid(), rnd)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	oldIdle, young, oldBusy := mk(30*time.Hour, "00000001"), mk(time.Minute, "00000002"), mk(30*time.Hour, "00000003")
	legacy := "reaptest_legacy0123456789abcdef"
	all := []string{oldIdle, young, oldBusy, legacy}

	t.Cleanup(func() {
		for _, n := range all {
			_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{n}.Sanitize()+" WITH (FORCE)")
		}
	})
	for _, n := range all {
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{n}.Sanitize()+" OWNER "+pgx.Identifier{ownerRole}.Sanitize()); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
	}

	busyURL := *ou
	busyURL.Path = "/" + oldBusy
	busy, err := pgx.Connect(ctx, busyURL.String())
	if err != nil {
		t.Fatalf("connect busy: %v", err)
	}
	defer func() { _ = busy.Close(ctx) }()

	if _, err := Reap(ctx, adminURL, ownerURL, 0); err == nil {
		t.Fatal("Reap with zero maxAge must refuse")
	}
	dropped, err := Reap(ctx, adminURL, ownerURL, 24*time.Hour)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	exists := func(n string) bool {
		var c int
		if err := admin.QueryRow(ctx, "SELECT count(*) FROM pg_database WHERE datname=$1", n).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c == 1
	}
	if exists(oldIdle) {
		t.Errorf("old idle scratch database %s not dropped", oldIdle)
	}
	for _, n := range []string{young, oldBusy, legacy} {
		if !exists(n) {
			t.Errorf("Reap wrongly dropped %s", n)
		}
	}
	for _, n := range dropped {
		if _, ok := ParseName(n); !ok {
			t.Errorf("Reap dropped non-pattern database %s", n)
		}
	}
}

// TestReapLeakedScratchDatabases is the explicit, opt-in reaper entry point:
//
//	SCRATCHDB_REAP=1 [SCRATCHDB_REAP_MAX_AGE_HOURS=6] \
//	  go test -tags integration -count=1 -run '^TestReapLeakedScratchDatabases$' ./internal/testsupport/scratchdb
//
// It is skipped unless SCRATCHDB_REAP=1, so a normal test run never drops
// anything. Default max age is 6 hours (minimum 1).
func TestReapLeakedScratchDatabases(t *testing.T) {
	if os.Getenv("SCRATCHDB_REAP") != "1" {
		t.Skip("SCRATCHDB_REAP != 1; reaper is opt-in (see docs/testing/testing-strategy.md)")
	}
	adminURL, ownerURL := os.Getenv("TEST_ADMIN_DATABASE_URL"), os.Getenv("TEST_DATABASE_URL")
	if adminURL == "" || ownerURL == "" {
		t.Skip("TEST_ADMIN_DATABASE_URL / TEST_DATABASE_URL not set")
	}
	hours := 6
	if v := os.Getenv("SCRATCHDB_REAP_MAX_AGE_HOURS"); v != "" {
		h, err := strconv.Atoi(v)
		if err != nil || h < 1 {
			t.Fatalf("SCRATCHDB_REAP_MAX_AGE_HOURS=%q must be an integer >= 1", v)
		}
		hours = h
	}
	dropped, err := Reap(context.Background(), adminURL, ownerURL, time.Duration(hours)*time.Hour)
	t.Logf("reaped %d scratch database(s) older than %dh", len(dropped), hours)
	for _, n := range dropped {
		t.Logf("dropped %s", n)
	}
	if err != nil {
		t.Fatal(err)
	}
}
