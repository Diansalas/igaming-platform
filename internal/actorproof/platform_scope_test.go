package actorproof

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSign_PlatformScope_NullTenantEncoding(t *testing.T) {
	i := newTestIssuer(t)
	c := Claims{Actor: uuid.New(), Scope: ScopePlatform, Operation: OpGrantApprove, Target: uuid.NewString(), PayloadHash: strings.Repeat("ab", 32)}
	tok, err := i.Sign(c)
	if err != nil {
		t.Fatalf("a platform K1 claim must sign: %v", err)
	}
	parts := strings.Split(tok, "|")
	if len(parts) != 12 || parts[3] != "platform" || parts[4] != "" {
		t.Fatalf("platform scope must carry an EMPTY tenant field, got %q", tok)
	}
	for _, op := range []string{OpPolicyPropose, OpPolicyApprove, OpGrantRevoke} {
		c.Operation = op
		if _, err := i.Sign(c); err != nil {
			t.Fatalf("%s: %v", op, err)
		}
	}
	bad := []Claims{
		{Actor: c.Actor, Scope: ScopePlatform, Tenant: uuid.New(), Operation: OpGrantApprove, Target: c.Target, PayloadHash: c.PayloadHash},
		{Actor: c.Actor, Scope: ScopePlatform, Operation: OpAdjustmentApprove, Target: c.Target, PayloadHash: c.PayloadHash},
		{Actor: c.Actor, Scope: ScopePlatform, Operation: OpResolutionApprove, Target: c.Target, PayloadHash: c.PayloadHash},
		{Actor: c.Actor, Scope: ScopeTenant, Operation: OpGrantApprove, Target: c.Target, PayloadHash: c.PayloadHash},
		{Actor: c.Actor, Scope: ScopePlatformActing, Operation: OpAdjustmentApprove, Target: c.Target, PayloadHash: c.PayloadHash},
	}
	for n, b := range bad {
		if _, err := i.Sign(b); !errors.Is(err, ErrInvalidClaims) {
			t.Fatalf("bad claim %d must be refused, got %v", n, err)
		}
	}
}

func TestTS_CanonicalMicrosecondUTC(t *testing.T) {
	tm := time.Date(2026, 10, 6, 12, 34, 56, 123456789, time.FixedZone("x", 3600))
	if got := *TS(&tm); got != "2026-10-06T11:34:56.123456Z" {
		t.Fatalf("TS = %s", got)
	}
	if TS(nil) != nil {
		t.Fatal("TS(nil) must be nil")
	}
}
