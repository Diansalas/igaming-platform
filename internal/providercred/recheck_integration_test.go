//go:build integration

// ADR 0094 §5 (the post-verification re-check) and INV-POOL for the
// outbound path, at the resolver level.
package providercred

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// TestRecheck_KeyImplicitPredecessorRevokedBetweenPhases is security
// condition C5's test-8 variant (and C4): a KeyImplicit callback verified
// by the verify_only PREDECESSOR, whose handle is revoked between the two
// phases, is rejected by the re-check - which must therefore be about the
// predecessor's handle, not the (still active) Active one.
func TestRecheck_KeyImplicitPredecessorRevokedBetweenPhases(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		name := "control: predecessor still in its window"
		if revoke {
			name = "predecessor revoked between the phases"
		}
		t.Run(name, func(t *testing.T) {
			f := newFx(t)
			tenant := f.tenant()
			h1, s1 := f.register(f.spec(tenant, "acme", "k1"))
			f.rotateInbound(tenant, "acme", "k2", h1, time.Now().Add(time.Hour))
			scheme := implicitTestScheme{}
			in := webhookauth.Inbound{TenantID: tenant, ProviderID: "acme", Body: []byte(`{"e":1}`)}
			in.Header = map[string][]string{"X-Sig": {signFor(s1, in.Body)}}
			m, authErr := webhookauth.ExtractInbound(scheme, in)
			if authErr != nil {
				t.Fatal(authErr)
			}
			resolver := f.sub.Resolver("casino")
			v, cred, authErr := webhookauth.ResolveAndSeal(context.Background(), f.rt, "casino", scheme, resolver, in, m)
			if authErr != nil || cred.KeyID != "k1" || cred.HandleID != h1.ID {
				t.Fatalf("the predecessor must verify with its own handle id: %v %v", cred, authErr)
			}
			if revoke {
				f.revoke(tenant, h1.ID)
			}
			var redeemErr *webhookauth.AuthError
			if err := f.rt.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
				_, _, redeemErr = v.Redeem(ctx, tx, "casino", tenant, "acme", resolver)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			switch {
			case revoke && (redeemErr == nil || redeemErr.Reason != webhookauth.ReasonCredentialUnavailable):
				t.Fatalf("a revoked predecessor must fail the re-check, got %v", redeemErr)
			case !revoke && redeemErr != nil:
				t.Fatalf("an in-window predecessor must pass the re-check, got %v", redeemErr)
			}
		})
	}
}

// TestOutboundResolve_InsideTenantTxRefused is ADR 0094 §9.3 test 12:
// OutboundResolver.Resolve called inside WithTenant fails closed before
// any nested pool acquisition or store call.
func TestOutboundResolve_InsideTenantTxRefused(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	f.registerOutbound(tenant, "psp-a", "api-1")
	o := f.sub.Outbound("payments")
	calls := f.mem.Calls()
	before := strings.Count(f.logs.String(), `"entry_point":"providercred.OutboundResolver.Resolve"`)
	var nestedAcquires int64
	err := f.rt.WithTenant(context.Background(), tenant, func(ctx context.Context, _ pgx.Tx) error {
		start := f.rt.Raw().Stat().AcquireCount()
		_, err := o.Resolve(ctx, f.rt, tenant, "psp-a")
		nestedAcquires = f.rt.Raw().Stat().AcquireCount() - start
		return err
	})
	if !errors.Is(err, ErrOutboundCredentialUnavailable) {
		t.Fatalf("outbound resolve inside a held transaction must fail closed, got %v", err)
	}
	if f.mem.Calls() != calls {
		t.Fatal("a refused outbound resolve reached the store")
	}
	if nestedAcquires != 0 {
		t.Fatalf("a refused outbound resolve must not acquire a nested connection: %d acquisitions", nestedAcquires)
	}
	if got := strings.Count(f.logs.String(), `"entry_point":"providercred.OutboundResolver.Resolve"`) - before; got != 1 {
		t.Fatalf("the outbound guard itself must refuse (one secret_fetch_with_tx_held line for its entry point), got %d", got)
	}
	// Outside a transaction the same call resolves.
	if _, err := o.Resolve(context.Background(), f.rt, tenant, "psp-a"); err != nil {
		t.Fatalf("outbound resolve with no transaction held: %v", err)
	}
}

// TestRecheck_BindsSecretToFingerprint is security review 17, S-3 option
// (b): the real Recheck recomputes the keyed fingerprint of the
// credential's secret and requires it to equal the fingerprint the handle
// row pins. In-process code that reads a live handle's id and fingerprint
// and pairs them with a secret of its own choosing is refused.
func TestRecheck_BindsSecretToFingerprint(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	f.register(f.spec(tenant, "acme", "k1"))
	set, err := f.resolve("casino", tenant, "acme", "k1", webhookauth.KeyFromHeader)
	if err != nil {
		t.Fatal(err)
	}
	resolver := f.sub.Resolver("casino")
	recheck := func(c webhookauth.Credential) error {
		var out error
		if err := f.rt.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
			out = resolver.Recheck(ctx, tx, tenant, c)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if err := recheck(set.Active); err != nil {
		t.Fatalf("the genuine credential must pass the re-check: %v", err)
	}
	forged := set.Active
	forged.Secret = randBytes(t, 32)
	if err := recheck(forged); !errors.Is(err, webhookauth.ErrCredentialUnavailable) {
		t.Fatalf("a secret that does not match the row's fingerprint must fail the re-check, got %v", err)
	}
}
