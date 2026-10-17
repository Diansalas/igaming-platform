//go:build integration

package db

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// ADR 0115 S1 launch gate: the REAL database_environment table, read through the RUNTIME role exactly
// as platform-api does, across the four anchor states x the production / non-production guard, and
// fail-closed on query error / cancelled context.
//
// database_environment is write-once, so this test must rewrite the anchor between states. It does so
// as the TABLE OWNER by disabling the table's user triggers inside one transaction (an owner right; no
// role, password or grant change) and restores the original anchor on cleanup. Because that mutates a
// "write-once" fact, it runs ONLY against a private, throwaway database and refuses otherwise:
// TEST_ANCHOR_PRIVATE_DB=1 must be set by the operator who created that database, and a database that
// is anchored 'production' or 'staging' is never touched.
func TestVerifyEnvironmentAnchor_RealDatabase_FourStates(t *testing.T) {
	ownerURL := os.Getenv("TEST_DATABASE_URL")
	rtURL := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if ownerURL == "" || rtURL == "" {
		t.Skip("TEST_DATABASE_URL and TEST_RUNTIME_DATABASE_URL are required (a skip is not evidence)")
	}
	if os.Getenv("TEST_ANCHOR_PRIVATE_DB") != "1" {
		t.Skip("TEST_ANCHOR_PRIVATE_DB=1 is required: this test rewrites the write-once anchor and must only run on a private test database")
	}
	ctx := context.Background()
	owner, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		t.Fatalf("connect owner: %v", err)
	}
	t.Cleanup(func() { _ = owner.Close(context.Background()) })

	original := readOwnerAnchor(t, ctx, owner)
	if original == "production" || original == "staging" {
		t.Fatalf("refusing to rewrite a database anchored %q", original)
	}
	setAnchor := func(state string) {
		t.Helper()
		tx, err := owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		for _, q := range []string{
			`ALTER TABLE database_environment DISABLE TRIGGER USER`,
			`DELETE FROM database_environment`,
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		if state != "" {
			if _, err := tx.Exec(ctx, `INSERT INTO database_environment (environment) VALUES ($1)`, state); err != nil {
				t.Fatalf("record %q: %v", state, err)
			}
		}
		if _, err := tx.Exec(ctx, `ALTER TABLE database_environment ENABLE TRIGGER USER`); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { setAnchor(original) })

	pool, err := Connect(ctx, rtURL, 2, 5*time.Second)
	if err != nil {
		t.Fatalf("connect runtime: %v", err)
	}
	defer pool.Close()

	states := []struct {
		anchor  string
		wantErr error
	}{
		{"", ErrEnvironmentAnchorUnrecorded},
		{"production", nil},
		{"development", ErrEnvironmentAnchorMismatch},
		{"test", ErrEnvironmentAnchorMismatch},
		{"staging", ErrEnvironmentAnchorMismatch}, // NA-6
	}
	for _, s := range states {
		setAnchor(s.anchor)
		got, err := pool.ReadEnvironmentAnchor(ctx)
		if err != nil || got != s.anchor {
			t.Fatalf("anchor %q: ReadEnvironmentAnchor = %q, %v", s.anchor, got, err)
		}
		_, err = VerifyEnvironmentAnchor(ctx, "production", pool)
		if s.wantErr == nil && err != nil {
			t.Fatalf("production guard on anchor %q must start: %v", s.anchor, err)
		}
		if s.wantErr != nil && !errors.Is(err, s.wantErr) {
			t.Fatalf("production guard on anchor %q: want %v, got %v", s.anchor, s.wantErr, err)
		}
		for _, g := range []string{"development", "staging", "test"} {
			if _, err := VerifyEnvironmentAnchor(ctx, g, pool); err != nil {
				t.Fatalf("guard %q on anchor %q must not be affected: %v", g, s.anchor, err)
			}
		}
	}

	// The documented owner-side provisioning step (docs/runbooks/production-configuration-checklist.md)
	// turns an unrecorded database into one the production guard accepts, and is write-once.
	setAnchor("")
	if _, err := VerifyEnvironmentAnchor(ctx, "production", pool); !errors.Is(err, ErrEnvironmentAnchorUnrecorded) {
		t.Fatalf("precondition: unrecorded must refuse, got %v", err)
	}
	if _, err := owner.Exec(ctx, `INSERT INTO database_environment (environment) VALUES ('production');`); err != nil {
		t.Fatalf("documented provisioning step: %v", err)
	}
	if _, err := VerifyEnvironmentAnchor(ctx, "production", pool); err != nil {
		t.Fatalf("after the provisioning step production must start: %v", err)
	}
	var env, by string
	if err := owner.QueryRow(ctx, `SELECT environment, recorded_by FROM database_environment`).Scan(&env, &by); err != nil || env != "production" || by == "" {
		t.Fatalf("documented verify query: %q %q %v", env, by, err)
	}
	if _, err := owner.Exec(ctx, `UPDATE database_environment SET environment = 'test'`); err == nil {
		t.Fatal("the recorded anchor must stay write-once")
	}

	// Fail closed on query error / cancelled context / closed pool: production refuses with the read
	// error, and that error is neither "unrecorded" nor a mismatch (no row is NOT inferred).
	setAnchor("production")
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := VerifyEnvironmentAnchor(cancelled, "production", pool); err == nil ||
		errors.Is(err, ErrEnvironmentAnchorUnrecorded) || errors.Is(err, ErrEnvironmentAnchorMismatch) {
		t.Fatalf("a cancelled context must fail closed with a read error, got %v", err)
	}
	if _, err := VerifyEnvironmentAnchor(cancelled, "development", pool); err != nil {
		t.Fatalf("non-production must not read the anchor at all: %v", err)
	}
	// A query error: the same pool, a table the reader cannot select (renamed within a rolled-back
	// tx is not visible to other sessions), so use a reader on a separate pool that is closed.
	closed, err := Connect(ctx, rtURL, 1, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	if _, err := VerifyEnvironmentAnchor(ctx, "production", closed); err == nil ||
		errors.Is(err, ErrEnvironmentAnchorUnrecorded) || errors.Is(err, ErrEnvironmentAnchorMismatch) {
		t.Fatalf("a closed pool must fail closed with a read error, got %v", err)
	}
}

func readOwnerAnchor(t *testing.T, ctx context.Context, c *pgx.Conn) string {
	t.Helper()
	var env string
	err := c.QueryRow(ctx, `SELECT environment FROM database_environment`).Scan(&env)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("read anchor: %v", err)
	}
	return env
}
