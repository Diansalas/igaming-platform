//go:build integration

package alerting

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Code review #4: current routes with an identical effective_from (one
// transaction, one now()) resolve deterministically to the HIGHER id, in the
// delivery path and in the readiness evaluator alike. R3 (unique current route)
// is deferred, so raw SQL can still produce a tie. Twelve steps make a
// nondeterministic ordering fail with probability 1 - 2^-12.
func TestResolveRoute_TieOnEffectiveFromResolvesToTheHigherID(t *testing.T) {
	pool := scratchPool(t, "atie")
	admin := seedPlatformAdmin(t, pool)
	type pair struct{ a, b uuid.UUID }
	pairs := map[int]pair{}
	if err := pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		for step := 0; step < 12; step++ {
			var p pair
			for i, dst := range []*uuid.UUID{&p.a, &p.b} {
				if err := tx.QueryRow(ctx, `INSERT INTO alert_routes (scope, severity, escalation_step, channel_kind, recipient_ref, enabled, reason_code)
					VALUES ('platform','p2',$1,'mock',$2,$3,'initial_setup') RETURNING id`, step, "rota-"+string(rune('a'+i)), i == 0).Scan(dst); err != nil {
					return err
				}
			}
			pairs[step] = p
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	d := NewDispatcher(pool, DispatcherConfig{Clock: fakeInstantClock{}}, &MockSink{})
	now := fakeInstantClock{}.Now()
	routes, err := d.loadRouteConfigs(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	for step, p := range pairs {
		want := p.a
		if p.b.String() > p.a.String() {
			want = p.b
		}
		got, err := d.resolveRoute(context.Background(), SeverityP2, step, now)
		if err != nil || got == nil {
			t.Fatalf("step %d: %v %v", step, got, err)
		}
		if got.id != want {
			t.Fatalf("step %d: delivery resolved %s, want the higher id %s", step, got.id, want)
		}
		wantEnabled := want == p.a // a is the enabled one
		if got.enabled != wantEnabled {
			t.Fatalf("step %d: enabled=%v, want %v", step, got.enabled, wantEnabled)
		}
		// readiness evaluator picks the same row
		var onlyStep []RouteConfig
		for _, r := range routes {
			if r.Step == step {
				onlyStep = append(onlyStep, r)
			}
		}
		res := EvaluateReadiness(onlyStepAsZero(onlyStep), map[ChannelKind]bool{ChannelMock: false})
		p2 := find(res, SeverityP2)
		if wantEnabled && p2.Reason == ReadinessRouteDisabled || !wantEnabled && p2.Reason != ReadinessRouteDisabled {
			t.Fatalf("step %d: readiness (%s) disagrees with delivery (enabled wins=%v)", step, p2.Reason, wantEnabled)
		}
	}
}

// onlyStepAsZero re-labels a step's routes as step 0 so EvaluateReadiness (which
// judges step 0) evaluates that step's tie.
func onlyStepAsZero(in []RouteConfig) []RouteConfig {
	out := make([]RouteConfig, len(in))
	for i, r := range in {
		r.Step = 0
		out[i] = r
	}
	return out
}
