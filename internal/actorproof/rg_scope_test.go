package actorproof

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// ADR 0115 S1: rg_limit_config:* is provable for scope platform (NULL tenant) only, and the
// operation set is closed (mirrors actor_proof_require in migration 0135).
func TestSign_RGLimitConfig_ScopeMatrix(t *testing.T) {
	i := newTestIssuer(t)
	base := Claims{Actor: uuid.New(), Scope: ScopePlatform, Target: uuid.NewString(), PayloadHash: strings.Repeat("cd", 32)}
	for _, op := range []string{OpRGConfigRequest, OpRGConfigApprove, OpRGConfigReject, OpRGConfigCancel, OpRGConfigApply} {
		c := base
		c.Operation = op
		if op == OpRGConfigRequest {
			c.Target = TargetNew
		}
		tok, err := i.Sign(c)
		if err != nil {
			t.Fatalf("%s must sign for scope platform: %v", op, err)
		}
		if parts := strings.Split(tok, "|"); parts[3] != ScopePlatform || parts[4] != "" {
			t.Fatalf("%s: platform scope must carry an EMPTY tenant field: %q", op, tok)
		}
		for _, scope := range []string{ScopeTenant, ScopePlatformActing} {
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
	c := base
	c.Operation = "rg_limit_config:delete"
	if _, err := i.Sign(c); !errors.Is(err, ErrInvalidClaims) {
		t.Fatalf("an operation outside the closed set must be refused, got %v", err)
	}
}
