package actorproof

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// ADR 0110 T8 amendment (migration 0165): withdrawal_payout_reversal:decide is provable ONLY for scope tenant
// with a tenant, and it is the only operation of its family. The Go signer refuses every other shape before
// the database does (the trigger pins scope 'tenant').
func TestActorProof_PayoutReversal_ScopeRestriction(t *testing.T) {
	i := newTestIssuer(t)
	tenant := uuid.New()
	target := uuid.NewString()
	hash := strings.Repeat("cd", 32)
	ok := Claims{Actor: uuid.New(), Scope: ScopeTenant, Tenant: tenant, Operation: OpPayoutReversalDecide, Target: target, PayloadHash: hash}
	if _, err := i.Sign(ok); err != nil {
		t.Fatalf("decide for scope tenant must sign: %v", err)
	}
	for name, bad := range map[string]Claims{
		"platform_acting":     {Actor: ok.Actor, Scope: ScopePlatformActing, Tenant: tenant, Operation: OpPayoutReversalDecide, Target: target, PayloadHash: hash},
		"platform":            {Actor: ok.Actor, Scope: ScopePlatform, Operation: OpPayoutReversalDecide, Target: target, PayloadHash: hash},
		"nil tenant":          {Actor: ok.Actor, Scope: ScopeTenant, Operation: OpPayoutReversalDecide, Target: target, PayloadHash: hash},
		"another operation":   {Actor: ok.Actor, Scope: ScopeTenant, Tenant: tenant, Operation: "withdrawal_payout_reversal:execute", Target: target, PayloadHash: hash},
		"target new":          {Actor: ok.Actor, Scope: ScopeTenant, Tenant: tenant, Operation: OpPayoutReversalDecide, Target: "payout", PayloadHash: hash},
		"payload not hex64":   {Actor: ok.Actor, Scope: ScopeTenant, Tenant: tenant, Operation: OpPayoutReversalDecide, Target: target, PayloadHash: "payout_reversal:" + hash},
		"payload upper hex64": {Actor: ok.Actor, Scope: ScopeTenant, Tenant: tenant, Operation: OpPayoutReversalDecide, Target: target, PayloadHash: strings.ToUpper(hash)},
	} {
		if _, err := i.Sign(bad); !errors.Is(err, ErrInvalidClaims) {
			t.Fatalf("%s must be refused, got %v", name, err)
		}
	}
	if OpPayoutReversalDecide != "withdrawal_payout_reversal:decide" {
		t.Fatal("operation string drifted from migration 0165")
	}
	if !RequiresStepUp(ok) {
		t.Fatal("a payout reversal decision is step-up classified (critical)")
	}
}
