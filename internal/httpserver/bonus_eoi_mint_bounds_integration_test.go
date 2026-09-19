//go:build integration

// Stage 4H-B1 Wave 3 Phase 6 (`security`): regression proof for the
// EOI-minting bound fix in bonus_governance_handlers.go.
//
// FINDING this closes: POST /v1/admin/bonus/economic-operations mints an
// EconomicOperationIdentity root already in approval_state='approved' on a
// SINGLE actor's authority, and before this fix
// intended_aggregate_value/recipient_ceiling/asset_code were all OPTIONAL
// request fields. economicop.ConsumeRootBudget treats an absent
// recipient_ceiling as "no recipient check at all" and an absent
// intended_aggregate_value (with a non-null asset_code) as "no value check
// at all" - so one actor could mint an UNBOUNDED, already-approved
// authorization and then decompose an arbitrarily large grant campaign
// beneath it. That is SEC-W15-02's decomposition vector reopened at the
// entry point of the very mechanism built to close it: a ceiling that is
// optional to declare is not a ceiling.
//
// Every case below returns 400 after the fix and 201 before it.
package httpserver

import (
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

func TestMintEconomicOperation_RefusesUnboundedRoot(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	bonusOps := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleBonusOperations, "ops-pw-eoi-bounds")
	token := mustLoginStaff(t, srv, tenant.Slug, bonusOps.Email, "ops-pw-eoi-bounds")

	cases := []struct {
		name string
		body map[string]any
	}{
		{
			// The exact live control gap: no ceiling, no aggregate value,
			// no asset - an unbounded, already-approved authorization.
			name: "no bounds at all",
			body: map[string]any{
				"operation_type": "bonus_manual_grant", "subject_scope": "single_subject",
				"subject_ref": uuid.NewString(),
			},
		},
		{
			name: "aggregate value declared but no recipient ceiling",
			body: map[string]any{
				"operation_type": "bonus_manual_grant", "subject_scope": "single_subject",
				"subject_ref": uuid.NewString(), "asset_code": "USD", "intended_aggregate_value": "100000",
			},
		},
		{
			name: "recipient ceiling declared but no value budget",
			body: map[string]any{
				"operation_type": "bonus_bulk_grant", "subject_scope": "enumerated_set",
				"subject_set_count": 50, "recipient_ceiling": 50, "asset_code": "USD",
			},
		},
		{
			name: "asset-less root (no enforceable value budget at all)",
			body: map[string]any{
				"operation_type": "bonus_bulk_grant", "subject_scope": "enumerated_set",
				"subject_set_count": 50, "recipient_ceiling": 50, "intended_aggregate_value": "100000",
			},
		},
		{
			name: "zero aggregate value is not a budget",
			body: map[string]any{
				"operation_type": "bonus_manual_grant", "subject_scope": "single_subject",
				"subject_ref": uuid.NewString(), "asset_code": "USD",
				"intended_aggregate_value": "0", "recipient_ceiling": 1,
			},
		},
		{
			// Pagination laundering at the mint point: a ceiling larger
			// than the set it claims to bound is not a bound.
			name: "recipient ceiling exceeds the declared subject set",
			body: map[string]any{
				"operation_type": "bonus_bulk_grant", "subject_scope": "enumerated_set",
				"subject_set_count": 10, "recipient_ceiling": 100000, "asset_code": "USD",
				"intended_aggregate_value": "100000",
			},
		},
		{
			name: "single_subject with no subject_ref",
			body: map[string]any{
				"operation_type": "bonus_manual_grant", "subject_scope": "single_subject",
				"asset_code": "USD", "intended_aggregate_value": "100000", "recipient_ceiling": 1,
			},
		},
		{
			// doc 34 §2.2: "An authorization that can be executed forever
			// is not an authorization. Mandatory; a platform maximum
			// bounds the configured value."
			name: "unbounded lifetime",
			body: map[string]any{
				"operation_type": "bonus_manual_grant", "subject_scope": "single_subject",
				"subject_ref": uuid.NewString(), "asset_code": "USD",
				"intended_aggregate_value": "100000", "recipient_ceiling": 1,
				"expires_in_seconds": 315360000,
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := map[string]any{}
			for k, v := range c.body {
				body[k] = v
			}
			body["idempotency_key"] = "eoi-bounds-" + uuid.NewString()
			resp := postJSON(t, srv, "/v1/admin/bonus/economic-operations", token.AccessToken, body)
			defer resp.Body.Close()
			if resp.StatusCode != 400 {
				var apiErr apierror.Error
				decodeBody(t, resp, &apiErr)
				t.Fatalf("expected 400 refusing an under-bounded EOI root, got %d: %+v", resp.StatusCode, apiErr)
			}
		})
	}

	// Non-regression: a fully-bounded root is still mintable.
	ok := postJSON(t, srv, "/v1/admin/bonus/economic-operations", token.AccessToken, map[string]any{
		"operation_type": "bonus_bulk_grant", "subject_scope": "enumerated_set",
		"subject_set_count": 10, "recipient_ceiling": 10, "asset_code": "USD",
		"intended_aggregate_value": "100000", "idempotency_key": "eoi-bounds-ok-" + uuid.NewString(),
	})
	defer ok.Body.Close()
	if ok.StatusCode != 201 {
		var apiErr apierror.Error
		decodeBody(t, ok, &apiErr)
		t.Fatalf("expected 201 for a fully-bounded EOI root, got %d: %+v", ok.StatusCode, apiErr)
	}
}
