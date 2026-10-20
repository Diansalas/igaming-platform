//go:build integration

// Package seed (integration builds ONLY) creates an ACTIVE KYC enforcement policy through the real, signed path of migration
// 0185: two fresh platform administrators (two Persons), a proof-signed draft by the first and a proof-signed activation by the
// second. It imports kyc, so packages whose own tests are in package kyc use kycpolfix directly instead.
package seed

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/kycpolicyproof"
	"github.com/Diansalas/igaming-platform/internal/testsupport/kycpolfix"
)

// Draft creates a signed DRAFT as the given author.
func Draft(t testing.TB, pool *db.Pool, author uuid.UUID, p kyc.CreateEnforcementPolicyParams) uuid.UUID {
	t.Helper()
	kycpolfix.Install(t)
	p.Proof = kycpolicyproof.Signer{}
	var id uuid.UUID
	if err := pool.WithPlatformAdmin(context.Background(), author, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = kyc.CreateEnforcementPolicy(ctx, tx, p)
		return err
	}); err != nil {
		t.Fatalf("seed: create policy: %v", err)
	}
	return id
}

// Activate activates a draft as the given administrator.
func Activate(t testing.TB, pool *db.Pool, by, id uuid.UUID) {
	t.Helper()
	if err := pool.WithPlatformAdmin(context.Background(), by, func(ctx context.Context, tx pgx.Tx) error {
		return kyc.ActivateEnforcementPolicy(ctx, tx, id, kyc.PolicyWrite{Proof: kycpolicyproof.Signer{}, ReasonCode: "fixture_activate"})
	}); err != nil {
		t.Fatalf("seed: activate policy: %v", err)
	}
}

// Active creates and activates a policy with two fresh administrators and returns its id.
func Active(t testing.TB, pool *db.Pool, p kyc.CreateEnforcementPolicyParams) uuid.UUID {
	t.Helper()
	kycpolfix.Install(t)
	author, activator := kycpolfix.Admin(t, pool), kycpolfix.Admin(t, pool)
	id := Draft(t, pool, author, p)
	Activate(t, pool, activator, id)
	return id
}
