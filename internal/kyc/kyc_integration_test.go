//go:build integration

// Real-PostgreSQL tests for kyc_documents' immutability trigger and
// kyc_verifications/kyc_documents' RLS - migration 0040. Follows
// internal/rg/rg_integration_test.go's own fixture conventions.
package kyc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
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

type fixture struct {
	tenantID uuid.UUID
	brandID  uuid.UUID
	personID uuid.UUID
	playerID uuid.UUID
}

func seedFixture(t *testing.T, pool *db.Pool) fixture {
	t.Helper()
	var f fixture
	f.tenantID = uuid.New()
	// Stage 4I Phase E-SECURITY (migration 0077): `tenants` writes now
	// require a genuinely platform-admin-scoped transaction.
	// ADR 0112 / LF2: the callers of seedFixture may hold the RUNTIME-role pool, which may only
	// create pending_launch rows (LA021); the ACTIVE tenant and brand come from the owner pool.
	err := launchfix.OwnerFor(t, pool).WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO tenants (id, slug, name, licensing_model, status) VALUES ($1, $2, 'Test Tenant', 'under_platform_licence', 'active')`,
			f.tenantID, "t-"+f.tenantID.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 tenant row, inserted %d", tag.RowsAffected())
		}
		p, err := identity.CreatePerson(ctx, tx)
		f.personID = p.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant/person: %v", err)
	}

	f.brandID = uuid.New()
	if err := launchfix.OwnerFor(t, pool).WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name, status) VALUES ($1, $2, $3, 'Test Brand', 'active')`,
			f.brandID, f.tenantID, "b-"+f.brandID.String()[:8])
		return err
	}); err != nil {
		t.Fatalf("seed brand: %v", err)
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		f.playerID = uuid.New()
		_, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			f.playerID, f.tenantID, f.brandID, f.personID, f.playerID.String()+"@example.com")
		return err
	})
	if err != nil {
		t.Fatalf("seed brand/player: %v", err)
	}
	// PRH-2 E1: this test's workers claim only the tenants the test seeded, so
	// rows it leaves behind never reach another test (or package) and no test
	// deletes outbox rows (code review F1).
	registerScopeTenant(t, f.tenantID)
	return f
}

// seedVerification seeds a created verification the way production now does:
// phase A (RequestVerification) plus the outbox worker applying the MOCK's
// definitive create result (ADR 0106: no HTTP-path code calls a vendor).
func seedVerification(t *testing.T, pool *db.Pool, f fixture) uuid.UUID {
	t.Helper()
	return createViaWorker(t, pool, NewMockOutboundResolver(), NewMockKYCProvider(), f).ID
}

const pgRLSViolation = "42501"

func assertRLSViolation(t *testing.T, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgRLSViolation {
		t.Fatalf("expected a row-level-security violation (%s), got: %v", pgRLSViolation, err)
	}
}

// --- Immutability: only status/reviewed_at/reviewed_by/rejection_reason
// may ever change on kyc_documents; everything else, and any DELETE, is
// refused by migration 0040's own trigger ---

func TestKYCDocuments_CoreFieldsAreImmutable(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)

	var docID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		d, err := UploadDocument(ctx, tx, NewMockDocumentStorageProvider(), NewMockMalwareScanner(), UploadDocumentParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
			VerificationID: verificationID, DocumentType: DocumentPassport, Filename: "p.png", Content: tinyPNGBytes,
		})
		docID = d.ID
		return err
	})
	if err != nil {
		t.Fatalf("upload document: %v", err)
	}

	// Attempting to change an immutable core field (storage_reference)
	// must fail, even from a fully tenant-scoped, otherwise-legitimate
	// connection.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kyc_documents SET storage_reference = 'forged' WHERE id = $1`, docID)
		return err
	})
	if err == nil {
		t.Fatal("expected an error attempting to mutate an immutable core field, got nil")
	}

	// The legitimate review-field update (status/reviewed_at/reviewed_by)
	// must still succeed - the trigger allows exactly this shape.
	var staffID uuid.UUID
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		staff, err := identity.CreateStaffUser(ctx, tx, f.tenantID, "reviewer-"+uuid.NewString()+"@example.com", "hash", identity.StaffRoleCompliance, nil)
		staffID = staff.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed staff user: %v", err)
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ReviewDocument(ctx, tx, ReviewDocumentParams{DocumentID: docID, StaffID: staffID, NewStatus: DocumentApproved})
		return err
	})
	if err != nil {
		t.Fatalf("expected the legitimate review update to succeed, got: %v", err)
	}

	// DELETE is refused outright - directive's "never silently delete
	// verification evidence".
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM kyc_documents WHERE id = $1`, docID)
		return err
	})
	if err == nil {
		t.Fatal("expected DELETE to be refused, got nil")
	}
}

// --- RLS: a different tenant cannot read or forge a row into another
// tenant's kyc_verifications/kyc_documents ---

func TestKYCVerifications_CrossTenantAccessDenied(t *testing.T) {
	pool := testPool(t)
	fA := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, fA)
	fB := seedFixture(t, pool)

	var count int
	err := pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM kyc_verifications WHERE id = $1`, verificationID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected tenant B to see zero rows for tenant A's verification, got %d", count)
	}

	err = pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id)
			 VALUES (gen_random_uuid(), $1, $2, $3, $4, 'unverified', 'mock')`,
			fA.tenantID, fA.brandID, fA.playerID, fA.personID,
		)
		return err
	})
	assertRLSViolation(t, err)
}

// --- Concurrency: two simultaneous document uploads of the SAME type for
// the SAME account never collide on version (the version-resolution
// query + insert is not itself protected by a unique constraint on
// version alone, only on (player_account_id, document_type, version) -
// this proves the database's own constraint, not merely the application
// query, is what prevents a duplicate version under a real race) ---

func TestKYCDocuments_ConcurrentUploadsNeverDuplicateVersion(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)

	const concurrency = 5
	errs := make([]error, concurrency)
	done := make(chan int, concurrency)
	for i := 0; i < concurrency; i++ {
		go func(i int) {
			errs[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := UploadDocument(ctx, tx, NewMockDocumentStorageProvider(), NewMockMalwareScanner(), UploadDocumentParams{
					TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
					VerificationID: verificationID, DocumentType: DocumentSelfie, Filename: "s.png", Content: tinyPNGBytes,
				})
				return err
			})
			done <- i
		}(i)
	}
	for i := 0; i < concurrency; i++ {
		<-done
	}

	successes := 0
	for _, err := range errs {
		if err == nil {
			successes++
		}
	}
	// Postgres serializes concurrent transactions touching the same rows
	// via its own locking - some may retry-worthy-fail on the unique
	// constraint if they interleave on the version SELECT, but NONE may
	// ever produce a stored duplicate version. Assert the database's own
	// invariant directly rather than assuming every goroutine succeeds.
	var distinctVersions, totalRows int
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(DISTINCT version) FROM kyc_documents WHERE player_account_id = $1 AND document_type = $2`,
			f.playerID, DocumentSelfie).Scan(&distinctVersions); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM kyc_documents WHERE player_account_id = $1 AND document_type = $2`,
			f.playerID, DocumentSelfie).Scan(&totalRows)
	})
	if err != nil {
		t.Fatalf("unexpected error verifying versions: %v", err)
	}
	if distinctVersions != totalRows {
		t.Fatalf("expected every stored row to have a DISTINCT version (no duplicates), got %d rows with %d distinct versions", totalRows, distinctVersions)
	}
	if totalRows == 0 {
		t.Fatal("expected at least one upload to succeed")
	}
}

// --- Fail-closed malware scanning: a scanner ERROR must be treated
// identically to a positive detection, never as "assume clean" -
// MalwareScanner's own documented contract, previously untested since
// MockMalwareScanner can never itself return an error ---

type erroringScanner struct{}

func (erroringScanner) Scan(ctx context.Context, content []byte) (bool, error) {
	return false, errors.New("scanner unavailable")
}

func TestUploadDocument_FailsClosedOnScannerError(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := UploadDocument(ctx, tx, NewMockDocumentStorageProvider(), erroringScanner{}, UploadDocumentParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
			VerificationID: verificationID, DocumentType: DocumentPassport, Filename: "p.png", Content: tinyPNGBytes,
		})
		return err
	})
	if err == nil {
		t.Fatal("expected upload to fail when the malware scanner errors, got nil")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("expected a scan-failure error, got ErrNotFound: %v", err)
	}

	var count int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM kyc_documents WHERE verification_id = $1`, verificationID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no document row to be stored when the scan could not complete, got %d", count)
	}
}

// --- Composite tenant FKs (PostgreSQL/RLS specialist review P1 fix):
// a row cannot name a player_account_id/verification_id/reviewed_by
// belonging to a DIFFERENT tenant than its own tenant_id column, even
// from a connection otherwise scoped to write that row's own tenant ---

func TestKYCVerifications_CompositeTenantFKRejectsCrossTenantPlayerAccount(t *testing.T) {
	pool := testPool(t)
	fA := seedFixture(t, pool)
	fB := seedFixture(t, pool)

	err := pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id)
			 VALUES (gen_random_uuid(), $1, $2, $3, $4, 'unverified', 'mock')`,
			fB.tenantID, fB.brandID, fA.playerID, fB.personID,
		)
		return err
	})
	if err == nil {
		t.Fatal("expected the composite (player_account_id, tenant_id) FK to reject a cross-tenant player_account_id, got nil")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("expected a foreign-key violation (23503), got: %v", err)
	}
}

// --- Audit trail: the key Stage 4F actions actually produce rows in
// audit_log, not merely return success from the Go call ---

func TestKYC_ActionsProduceAuditRecords(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	verificationID := seedVerification(t, pool, f)
	storage := NewMockDocumentStorageProvider()

	assertAudited := func(t *testing.T, action string) {
		t.Helper()
		var count int
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2`, f.tenantID, action).Scan(&count)
		})
		if err != nil {
			t.Fatalf("query audit_log for %q: %v", action, err)
		}
		if count == 0 {
			t.Fatalf("expected at least one audit_log row for action %q, got 0", action)
		}
	}

	assertAudited(t, "kyc.verification_submitted")

	var docID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		d, err := UploadDocument(ctx, tx, storage, NewMockMalwareScanner(), UploadDocumentParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
			VerificationID: verificationID, DocumentType: DocumentPassport, Filename: "p.png", Content: tinyPNGBytes,
		})
		docID = d.ID
		return err
	})
	if err != nil {
		t.Fatalf("upload document: %v", err)
	}
	assertAudited(t, "kyc.document_uploaded")

	var staffID uuid.UUID
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		staff, err := identity.CreateStaffUser(ctx, tx, f.tenantID, "reviewer-"+uuid.NewString()+"@example.com", "hash", identity.StaffRoleCompliance, nil)
		staffID = staff.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed staff user: %v", err)
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ReviewDocument(ctx, tx, ReviewDocumentParams{DocumentID: docID, StaffID: staffID, NewStatus: DocumentApproved})
		return err
	})
	if err != nil {
		t.Fatalf("review document: %v", err)
	}
	assertAudited(t, "kyc.document_reviewed")

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		doc, err := GetDocumentByID(ctx, tx, docID)
		if err != nil {
			return err
		}
		_, _, err = GetDocumentContent(ctx, tx, storage, doc, audit.ActorPlayer, f.playerID)
		return err
	})
	if err != nil {
		t.Fatalf("get document content: %v", err)
	}
	assertAudited(t, "kyc.document_accessed")
}
