package actorproof

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// HSEC-APPROVED-HOLD-RELEASE-1 (ADR 0110 verifier operation table, ADR 0111 M-10): the
// withdrawal_hold_resolution:* operations are provable ONLY for scope platform_acting
// with a tenant. The Go signer refuses every other shape before the database does.
func TestActorProof_HoldResolution_ScopeRestriction(t *testing.T) {
	i := newTestIssuer(t)
	tenant := uuid.New()
	target := uuid.NewString()
	hash := strings.Repeat("ab", 32)
	ops := []string{OpHoldResolutionRequest, OpHoldResolutionApprove, OpHoldResolutionReject, OpHoldResolutionCancel}
	for _, op := range ops {
		tg := target
		if op == OpHoldResolutionRequest {
			tg = TargetNew
		}
		ok := Claims{Actor: uuid.New(), Scope: ScopePlatformActing, Tenant: tenant, Operation: op, Target: tg, PayloadHash: hash}
		if _, err := i.Sign(ok); err != nil {
			t.Fatalf("%s for platform_acting must sign: %v", op, err)
		}
		for _, bad := range []Claims{
			{Actor: ok.Actor, Scope: ScopeTenant, Tenant: tenant, Operation: op, Target: tg, PayloadHash: hash},
			{Actor: ok.Actor, Scope: ScopePlatform, Operation: op, Target: tg, PayloadHash: hash},
			{Actor: ok.Actor, Scope: ScopePlatformActing, Operation: op, Target: tg, PayloadHash: hash}, // nil tenant
		} {
			if _, err := i.Sign(bad); !errors.Is(err, ErrInvalidClaims) {
				t.Fatalf("%s with scope %s / tenant %v must be refused, got %v", op, bad.Scope, bad.Tenant, err)
			}
		}
	}
	if OpHoldResolutionRequest != "withdrawal_hold_resolution:request" || OpHoldResolutionApprove != "withdrawal_hold_resolution:approve" ||
		OpHoldResolutionReject != "withdrawal_hold_resolution:reject" || OpHoldResolutionCancel != "withdrawal_hold_resolution:cancel" {
		t.Fatal("operation strings drifted from migration 0124")
	}
}
