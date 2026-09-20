//go:build integration

// Real-PostgreSQL tests for economic_operations (migration 0053):
// repository CRUD round-trips, mint-once idempotency, lineage, and
// cross-tenant RLS isolation, direct SQL - not merely HTTP - per this
// project's established discipline (CLAUDE.md).
package economicop

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
)

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := db.Connect(context.Background(), url, 5, 5*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Stage 4I Phase E-SECURITY (migration 0077): `tenants` writes now require
// a genuinely platform-admin-scoped transaction.
func seedTenant(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	tenantID := uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'Test Tenant', 'under_platform_licence')`,
			tenantID, "t-"+tenantID.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 tenant row, inserted %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return tenantID
}

func assertPgErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("expected a Postgres error %s, got: %v", code, err)
	}
}

const pgRLSViolation = "42501"

func newRootOperation(tenantID uuid.UUID, idempotencyKey string) EconomicOperation {
	return EconomicOperation{
		TenantID:            tenantID,
		OperationType:       OperationBonusBulkGrant,
		InitiatingActorType: "staff",
		InitiatingActorID:   uuid.New(),
		SubjectScope:        SubjectScopeCriteriaDefined,
		EconomicOwner:       "tenant",
		LineageKind:         LineageRoot,
		ApprovalState:       ApprovalApproved,
		RequiredApprovals:   2,
		ApprovalsReceived:   2,
		IdempotencyKey:      idempotencyKey,
		CorrelationID:       uuid.New(),
		Status:              StatusOpen,
		ExpiresAt:           time.Now().Add(24 * time.Hour),
	}
}

// --- Repository CRUD round-trip and root/lineage self-consistency ---

func TestCreate_RootOperationSelfReferencesItsOwnRoot(t *testing.T) {
	pool := testPool(t)
	tenantID := seedTenant(t, pool)

	var created EconomicOperation
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		created, err = Create(ctx, tx, newRootOperation(tenantID, "root-key-1"))
		return err
	})
	if err != nil {
		t.Fatalf("create root operation: %v", err)
	}
	if created.RootOperationID != created.OperationID {
		t.Errorf("expected a root operation's root_operation_id to equal its own operation_id, got root=%s op=%s", created.RootOperationID, created.OperationID)
	}
	if created.LineageKind != LineageRoot {
		t.Errorf("expected lineage_kind 'root', got %q", created.LineageKind)
	}
}

func TestCreate_ChildInheritsParentsRoot(t *testing.T) {
	pool := testPool(t)
	tenantID := seedTenant(t, pool)

	var root, child EconomicOperation
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		root, err = Create(ctx, tx, newRootOperation(tenantID, "root-key-2"))
		if err != nil {
			return err
		}
		childOp := newRootOperation(tenantID, "child-key-2")
		childOp.LineageKind = LineageItem
		childOp.ParentOperationID = &root.OperationID
		childOp.RootOperationID = root.RootOperationID
		child, err = Create(ctx, tx, childOp)
		return err
	})
	if err != nil {
		t.Fatalf("create root+child: %v", err)
	}
	if child.RootOperationID != root.OperationID {
		t.Errorf("expected child's root_operation_id to equal the root's operation_id, got %s want %s", child.RootOperationID, root.OperationID)
	}

	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		children, err := ListByRoot(ctx, tx, tenantID, root.OperationID)
		if err != nil {
			return err
		}
		if len(children) != 2 {
			t.Errorf("expected 2 operations in the root's lineage subtree (itself + 1 child), got %d", len(children))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("list by root: %v", err)
	}
}

func TestCreate_RejectsNonRootWithoutParent(t *testing.T) {
	pool := testPool(t)
	tenantID := seedTenant(t, pool)

	op := newRootOperation(tenantID, "bad-lineage-1")
	op.LineageKind = LineageItem // non-root, but no ParentOperationID set

	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Create(ctx, tx, op)
		return err
	})
	if err == nil {
		t.Fatal("expected the schema's CHECK ((lineage_kind = 'root') = (parent_operation_id IS NULL)) to reject a non-root operation with no parent")
	}
	assertPgErrorCode(t, err, "23514")
}

// --- Mint-once idempotency (doc 34 §3.3) ---

func TestGetByIdempotencyKey_ResolvesToTheSameOperation(t *testing.T) {
	pool := testPool(t)
	tenantID := seedTenant(t, pool)

	var created EconomicOperation
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		created, err = Create(ctx, tx, newRootOperation(tenantID, "mint-once-key"))
		return err
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A retried minting call resolves to the SAME EOI (doc 34 §3.3) -
	// this package does not itself implement the "retry -> lookup first"
	// flow (Phase 3's job), but the lookup primitive it depends on must
	// work, and the DB-level UNIQUE (tenant_id, idempotency_key)
	// constraint must reject a second attempt to insert under the same
	// key.
	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		got, err := GetByIdempotencyKey(ctx, tx, tenantID, "mint-once-key")
		if err != nil {
			return err
		}
		if got.OperationID != created.OperationID {
			t.Errorf("expected idempotency-key lookup to resolve to the same operation")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("get by idempotency key: %v", err)
	}

	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		dup := newRootOperation(tenantID, "mint-once-key")
		_, err := Create(ctx, tx, dup)
		return err
	})
	if err == nil {
		t.Fatal("expected a second mint under the same idempotency key to be rejected")
	}
	assertPgErrorCode(t, err, "23505")
}

// --- Value budget field discipline (RK-W15P2-5: a null asset_code EOI
// has no enforceable value budget) ---

func TestCreate_RejectsIntendedAggregateValueWithoutAssetCode(t *testing.T) {
	pool := testPool(t)
	tenantID := seedTenant(t, pool)

	op := newRootOperation(tenantID, "no-asset-with-value")
	op.IntendedAggregateValue = big.NewInt(1000)
	op.AssetCode = nil

	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Create(ctx, tx, op)
		return err
	})
	if err == nil {
		t.Fatal("expected the schema to reject an intended_aggregate_value with no asset_code (RK-W15P2-5)")
	}
	assertPgErrorCode(t, err, "23514")
}

func TestCreate_WithAssetCodeAndValue_RoundTrips(t *testing.T) {
	pool := testPool(t)
	tenantID := seedTenant(t, pool)

	op := newRootOperation(tenantID, "with-asset-and-value")
	eur := "EUR"
	op.AssetCode = &eur
	op.IntendedAggregateValue = big.NewInt(123456789)

	var created EconomicOperation
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		created, err = Create(ctx, tx, op)
		return err
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.IntendedAggregateValue == nil || created.IntendedAggregateValue.Cmp(big.NewInt(123456789)) != 0 {
		t.Errorf("expected IntendedAggregateValue to round-trip as 123456789, got %v", created.IntendedAggregateValue)
	}
}

// --- RLS: tenant isolation, direct SQL (EOI-10: staff/system only, no
// player-facing read policy at all) ---

func TestEconomicOperations_CrossTenantIsolation(t *testing.T) {
	pool := testPool(t)
	tenantA := seedTenant(t, pool)
	tenantB := seedTenant(t, pool)

	var created EconomicOperation
	err := pool.WithTenant(context.Background(), tenantA, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		created, err = Create(ctx, tx, newRootOperation(tenantA, "isolation-key"))
		return err
	})
	if err != nil {
		t.Fatalf("create tenant A operation: %v", err)
	}

	err = pool.WithTenant(context.Background(), tenantB, func(ctx context.Context, tx pgx.Tx) error {
		_, err := GetByID(ctx, tx, created.OperationID)
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected tenant B to see nothing for tenant A's operation, got: %v", err)
	}

	// Tenant B's connection cannot forge a row claiming tenant A's tenant_id.
	err = pool.WithTenant(context.Background(), tenantB, func(ctx context.Context, tx pgx.Tx) error {
		op := newRootOperation(tenantA, "forged-key")
		_, err := Create(ctx, tx, op)
		return err
	})
	if err == nil {
		t.Fatal("expected tenant B's connection to be unable to insert a row claiming tenant A's tenant_id")
	}
	assertPgErrorCode(t, err, pgRLSViolation)
}

// TestEconomicOperations_NoPlayerReadPolicy confirms EOI-10's "no
// player-facing or affiliate-facing read policy at all": a player-scoped
// connection sees zero rows even for an operation that names them as
// subject_ref, because economic_operations carries no player_self_scope
// policy of any kind.
func TestEconomicOperations_NoPlayerReadPolicy(t *testing.T) {
	pool := testPool(t)
	tenantID := seedTenant(t, pool)

	playerID := uuid.New()
	var created EconomicOperation
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		op := newRootOperation(tenantID, "player-subject-key")
		op.SubjectScope = SubjectScopeSingle
		op.SubjectRef = &playerID
		var err error
		created, err = Create(ctx, tx, op)
		return err
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// personID is unused beyond the fixture requirement that
	// player_accounts.person_id be non-null elsewhere in this schema;
	// this test only needs a player-scoped RLS session, not a real
	// player_accounts row, since economic_operations' policies check
	// current_setting('app.player_account_id') directly.
	err = pool.WithPlayerScope(context.Background(), tenantID, playerID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := GetByID(ctx, tx, created.OperationID)
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected a player-scoped connection to see nothing (no player-facing read policy, EOI-10), got: %v", err)
	}
}
