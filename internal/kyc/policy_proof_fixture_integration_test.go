//go:build integration

package kyc

// lic19 (migration 0185, S4): the KYC enforcement policy table accepts a write only with a verified ADR 0110 proof by an active
// platform_admin linked to a Person. These helpers let the raw-SQL lifecycle tests keep driving the table directly: they attach
// the same proof the production signer attaches, then run the statement under test. Fixtures use REAL synthetic administrators
// (testsupport/kycpolfix), never a bare uuid.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/kycpolicyproof"
	"github.com/Diansalas/igaming-platform/internal/testsupport/kycpolfix"
)

// polSigner signs for the fixtures (the process issuer is installed by kycpolfix.Install).
var polSigner = kycpolicyproof.Signer{}

// polAdmin installs the proof issuer and returns a fresh platform administrator (its own Person).
func polAdmin(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	kycpolfix.Install(t)
	return kycpolfix.Admin(t, pool)
}

// optStr maps a test-table value (string, *string or nil) to *string.
func optStr(v any) *string {
	switch x := v.(type) {
	case string:
		return &x
	case *string:
		return x
	}
	return nil
}

// rawDraftPolicy inserts a DRAFT row directly (with the creation proof) as actor, which must be tx's platform principal.
func rawDraftPolicy(ctx context.Context, tx pgx.Tx, actor, jurisdictionID uuid.UUID, trigger string, threshold, asset, tier, play any, legal, reason string) (uuid.UUID, error) {
	if err := polSigner.AttachCreate(ctx, tx, actor, PolicyContent{LicensingJurisdictionID: jurisdictionID, TriggerType: trigger,
		ThresholdMinorUnits: optStr(threshold), AssetCode: optStr(asset), RequiredTier: optStr(tier), PlayOperation: optStr(play),
		LegalReviewReference: legal, ReasonCode: reason}); err != nil {
		return uuid.Nil, err
	}
	var legalArg any
	if legal != "" {
		legalArg = legal
	}
	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO kyc_enforcement_policies
			(licensing_jurisdiction_id, trigger_type, status, threshold_minor_units, asset_code, required_tier, play_operation,
			 legal_review_reference, reason_code, created_by_actor_type, created_by_actor_id)
		VALUES ($1, $2, 'draft', $3::numeric, $4, $5, $6, $7, $8, 'platform_admin', $9)
		RETURNING id`, jurisdictionID, trigger, optStr(threshold), optStr(asset), optStr(tier), optStr(play), legalArg, reason, actor).Scan(&id)
	return id, err
}

// rawDraft is rawDraftPolicy in its own platform-admin transaction.
func rawDraft(t *testing.T, pool *db.Pool, actor, jurisdictionID uuid.UUID, trigger string, threshold, asset, tier, play any, legal string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.WithPlatformAdmin(context.Background(), actor, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = rawDraftPolicy(ctx, tx, actor, jurisdictionID, trigger, threshold, asset, tier, play, legal, "test")
		return err
	}); err != nil {
		t.Fatalf("seed draft policy: %v", err)
	}
	return id
}

// rawTransition changes a policy's status directly, with the reason code and the transition proof.
func rawTransition(ctx context.Context, tx pgx.Tx, actor, id uuid.UUID, from, to string, successor *uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SELECT set_config('app.kyc_policy_reason_code', 'fixture_transition', true)`); err != nil {
		return err
	}
	if err := polSigner.AttachTransition(ctx, tx, actor, PolicyTransition{ID: id, From: from, To: to, Successor: successor, ReasonCode: "fixture_transition"}); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE kyc_enforcement_policies SET status = $2, superseded_by_policy_id = $3 WHERE id = $1`, id, to, successor)
	return err
}

// pol wraps the library write arguments.
func pol(reason string) PolicyWrite { return PolicyWrite{Proof: polSigner, ReasonCode: reason} }
