package actorproof

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Migration 0182 (lic11): withdrawal_restriction_policy:* is provable for scope platform (NULL tenant) only, and the operation
// set is closed (mirrors actor_proof_require in migration 0182).
func TestSign_WithdrawalRestrictionPolicy_ScopeMatrix(t *testing.T) {
	i := newTestIssuer(t)
	base := Claims{Actor: uuid.New(), Scope: ScopePlatform, Target: uuid.NewString(), PayloadHash: strings.Repeat("ab", 32)}
	for _, op := range []string{OpWithdrawalRestrictionRequest, OpWithdrawalRestrictionApprove, OpWithdrawalRestrictionReject,
		OpWithdrawalRestrictionCancel, OpWithdrawalRestrictionExecute} {
		c := base
		c.Operation = op
		if op == OpWithdrawalRestrictionRequest {
			c.Target = TargetNew
		}
		tok, err := i.Sign(c)
		if err != nil {
			t.Fatalf("%s must sign for scope platform: %v", op, err)
		}
		if parts := strings.Split(tok, "|"); parts[3] != ScopePlatform || parts[4] != "" {
			t.Fatalf("%s: platform scope must carry an EMPTY tenant field: %q", op, tok)
		}
		for _, scope := range []string{ScopeTenant, ScopePlatformActing, ScopeSystem} {
			bad := c
			bad.Scope, bad.Tenant = scope, uuid.New()
			if _, err := i.Sign(bad); !errors.Is(err, ErrInvalidClaims) {
				t.Fatalf("%s must be refused for scope %s, got %v", op, scope, err)
			}
		}
		withTenant := c
		withTenant.Tenant = uuid.New()
		if _, err := i.Sign(withTenant); !errors.Is(err, ErrInvalidClaims) {
			t.Fatalf("%s with a tenant must be refused, got %v", op, err)
		}
	}
	for _, op := range []string{"withdrawal_restriction_policy:override", "withdrawal_restriction_policy:apply"} {
		c := base
		c.Operation = op
		if _, err := i.Sign(c); !errors.Is(err, ErrInvalidClaims) {
			t.Fatalf("an operation outside the closed set (%s) must be refused, got %v", op, err)
		}
	}
}
