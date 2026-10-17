//go:build integration

package prooftest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
)

// F4 residual R1 (migration 0171): the WD-RG-1 dispatch gate ALLOW is now a signed system attestation, so every test that
// drives the real dispatch paths (T1p, T2, T12) or records a fixture ALLOW needs the process key provisioned in the
// database its transaction runs on. The hook below does that lazily, once per database name, over a separate OWNER
// connection (TEST_DATABASE_URL with the database of the transaction): the key insert is idempotent and autocommit, so it
// never takes part in (or breaks) the caller's transaction. A database without the key table (below 0120) is left alone.
// Importing this package (any test of a package that signs, or wdpath) installs the hook.
var (
	hookMu     sync.Mutex
	hookLoaded = map[string]bool{}

	hookInstalledDefault bool
)

func init() {
	actorproof.TestEnsureKeyHook = ensureKeyForTx
}

func ensureKeyForTx(ctx context.Context, tx pgx.Tx) error {
	var dbName string
	if err := tx.QueryRow(ctx, `SELECT current_database()`).Scan(&dbName); err != nil {
		return fmt.Errorf("prooftest: current_database: %w", err)
	}
	hookMu.Lock()
	defer hookMu.Unlock()
	if hookLoaded[dbName] {
		return nil
	}
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		return fmt.Errorf("prooftest: TEST_DATABASE_URL is not set")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("prooftest: TEST_DATABASE_URL: %w", err)
	}
	u.Path = "/" + dbName
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		return fmt.Errorf("prooftest: connect owner to %s: %w", dbName, err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var hasKeys bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('actorproof_private.actor_proof_keys') IS NOT NULL OR to_regclass('public.actor_proof_keys') IS NOT NULL`).Scan(&hasKeys); err != nil {
		return err
	}
	if !hasKeys {
		return nil
	}
	// Provision the key; install the process issuer as the default only the FIRST time this hook runs in the process (a
	// test that clears the default on purpose, to prove the fail-closed path, must not have it silently reinstalled).
	mu.Lock()
	defer mu.Unlock()
	if err := ensureProcessErrLocked(); err != nil {
		return err
	}
	if err := execKeyTable(ctx, conn, insertKeyIfNone, processKID, processKey); err != nil {
		return fmt.Errorf("provision process key: %w", err)
	}
	if !hookInstalledDefault {
		hookInstalledDefault = true
		if actorproof.Default() == nil {
			actorproof.SetDefault(process)
		}
	}
	hookLoaded[dbName] = true
	return nil
}
