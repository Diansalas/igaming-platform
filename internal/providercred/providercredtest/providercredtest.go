//go:build integration

// Package providercredtest seeds real provider-credential handles through
// the full four-eyes path (file -> approve by a different Person -> apply)
// for other packages' integration tests (Stage 10.3 W2a: the domain
// statement-capture tests, the admin API tests). It carries the
// integration build tag, so it is never compiled into an application
// binary, and it never imports the test-only memory backend: the caller
// supplies the router's put function. Secrets are generated at runtime.
package providercredtest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/providercred"
)

// Principals are two platform admins linked to two distinct Persons.
type Principals struct {
	Requester uuid.UUID
	Approver  uuid.UUID
}

// SeedPrincipals creates the two platform staff (and their Persons).
func SeedPrincipals(t testing.TB, pool *db.Pool) Principals {
	t.Helper()
	return Principals{Requester: platformStaff(t, pool), Approver: platformStaff(t, pool)}
}

func platformStaff(t testing.TB, pool *db.Pool) uuid.UUID {
	t.Helper()
	person, id := uuid.New(), uuid.New()
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id)
			VALUES ($1, NULL, $2, 'x', 'platform_admin', $3)`, id, "pct-"+id.String()+"@test.example", person)
		return err
	}); err != nil {
		t.Fatalf("providercredtest: seed platform staff: %v", err)
	}
	return id
}

// Spec is one handle to register.
type Spec struct {
	TenantID   uuid.UUID
	Domain     string
	ProviderID string
	Purpose    string
	KeyID      string
}

// RandomSecret returns 32 bytes from crypto/rand.
func RandomSecret(t testing.TB) []byte {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// MemoryRef is the memory:// ref convention used by these tests.
func MemoryRef(s Spec) string {
	return fmt.Sprintf("memory://test/provider-creds/%s/%s/%s/%s-%s?version=v1", s.TenantID, s.Domain, s.ProviderID, s.KeyID, uuid.NewString()[:8])
}

// Register stores a fresh secret under a fresh memory ref (via put) and
// runs file -> approve -> apply. It returns the active handle and the
// secret.
func Register(t testing.TB, pool *db.Pool, sub *providercred.Subsystem, put func(ref string, secret []byte), p Principals, s Spec) (providercred.Handle, []byte) {
	t.Helper()
	ctx := context.Background()
	secret := RandomSecret(t)
	ref := MemoryRef(s)
	put(ref, secret)
	sum := sha256.Sum256(secret)
	req, err := sub.FileRequest(ctx, pool, providercred.FileRequestParams{
		TargetTenantID: s.TenantID, Domain: s.Domain, ProviderID: s.ProviderID, Purpose: s.Purpose, KeyID: s.KeyID,
		SecretRef: ref, Confirmation: "sha256:" + hex.EncodeToString(sum[:]), NotBefore: time.Now().Add(-time.Minute),
		PredecessorDisposition: providercred.DispositionNone, ReasonCode: providercred.RequestReasonInitialRegistration,
		RequestedBy: p.Requester,
	}, providercred.AuditContext{ActorID: p.Requester})
	if err != nil {
		t.Fatalf("providercredtest: file: %v (%s)", err, providercred.ClassOf(err))
	}
	if _, err := sub.DecideRequest(ctx, pool, providercred.DecideParams{
		TargetTenantID: s.TenantID, RequestID: req.ID, Approver: p.Approver, Decision: "approve", ContentHash: req.ContentHash,
	}, providercred.AuditContext{ActorID: p.Approver}); err != nil {
		t.Fatalf("providercredtest: approve: %v (%s)", err, providercred.ClassOf(err))
	}
	h, err := sub.ApplyRequest(ctx, pool, providercred.ApplyParams{TargetTenantID: s.TenantID, RequestID: req.ID, Applier: p.Requester},
		providercred.AuditContext{ActorID: p.Requester})
	if err != nil {
		t.Fatalf("providercredtest: apply: %v (%s)", err, providercred.ClassOf(err))
	}
	return h, secret
}

// Revoke revokes a handle as actor.
func Revoke(t testing.TB, pool *db.Pool, tenantID, handleID, actor uuid.UUID) {
	t.Helper()
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := providercred.TransitionHandle(ctx, tx, handleID, providercred.ActionRevoke, nil,
			providercred.RevokeReasonSuspectedCompromise, actor, providercred.AuditContext{ActorID: actor})
		return err
	}); err != nil {
		t.Fatalf("providercredtest: revoke: %v", err)
	}
}
