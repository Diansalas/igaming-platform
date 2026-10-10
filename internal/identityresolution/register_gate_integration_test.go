//go:build integration

package identityresolution

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/identity"
)

type spyResolver struct{ calls int }

func (s *spyResolver) Resolve(context.Context, ResolutionInput) (ResolutionResult, error) {
	s.calls++
	return ResolutionResult{Outcome: NoMatch}, nil
}

// ADR 0112 7.3 (code review C-6): the resolution layer refuses a non-active brand BEFORE the
// resolver is called and before any identity_resolution audit row is written. The inner gate in
// identity.insertPlayerAccount would still refuse the account, so only this ordering (no resolver
// call, no audit row) fails if the gate at the top of RegisterPlayerWithResolution is removed.
func TestRegisterPlayerWithResolution_RefusesNonActiveBrandBeforeTheResolverRuns(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tenant := createTestTenant(t, pool)
	for name, status := range map[string]string{"pending_launch": "", "suspended": "suspended", "closed": "closed"} {
		t.Run(name, func(t *testing.T) {
			b := identity.Brand{ID: uuid.New(), TenantID: tenant.ID, Name: "g", Slug: "g-" + uuid.NewString(), Status: status}
			if err := pool.WithTenant(ctx, tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
				if status == "" {
					_, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, name, slug) VALUES ($1, $2, $3, $4)`, b.ID, b.TenantID, b.Name, b.Slug)
					return err
				}
				_, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, name, slug, status) VALUES ($1, $2, $3, $4, $5)`, b.ID, b.TenantID, b.Name, b.Slug, status)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			spy := &spyResolver{}
			err := pool.WithTenant(ctx, tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
				_, _, err := RegisterPlayerWithResolution(ctx, tx, spy, RegisterPlayerWithResolutionParams{Brand: b, Email: uuid.NewString() + "@example.com", PasswordHash: "h"})
				return err
			})
			if !errors.Is(err, identity.ErrNotAcceptingRegistrations) {
				t.Fatalf("want ErrNotAcceptingRegistrations, got %v", err)
			}
			if spy.calls != 0 {
				t.Fatalf("the resolver was called %d times for a refused brand", spy.calls)
			}
		})
	}
}
