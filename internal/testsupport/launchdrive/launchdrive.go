//go:build integration

// Package launchdrive drives a REAL governed status change through internal/launchgov for
// integration tests that need the executor's own behaviour (the GP020 closure refusal and its
// audit rows, the success audit row, the LA023 hold check) instead of the raw-SQL owner fixture of
// internal/testsupport/launchfix. It uses the synthetic fixture platform operators (one requester
// and two approvers, three distinct Persons), the per-process test signing key
// (internal/actorproof/prooftest) and the RUNTIME-role pool the test passes in. Integration-tag
// only and imported by no non-test code (internal/tenant/launch_fixture_pin_test.go family).
package launchdrive

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/launchgov"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// Platform returns the fixture operators as platform actors (requester, approver 1, approver 2).
func Platform(tb testing.TB) (requester, approver1, approver2 launchgov.Actor) {
	tb.Helper()
	launchfix.SeedOperators(tb)
	ops := launchfix.Operators()
	mk := func(id uuid.UUID) launchgov.Actor {
		return launchgov.Actor{StaffID: id, Scope: launchgov.ScopePlatform, Role: auth.RolePlatformAdmin, RequestID: "launchdrive"}
	}
	return mk(ops.Requester), mk(ops.Approver1), mk(ops.Approver2)
}

// Service builds a launchgov.Service over the (runtime-role) pool with the process test issuer.
func Service(tb testing.TB, rt *db.Pool) *launchgov.Service {
	tb.Helper()
	// Idempotent: provisions the process key into the pool's database as the owner role.
	prooftest.Install(tb, os.Getenv("TEST_DATABASE_URL"))
	svc, err := launchgov.New(launchgov.Config{Pool: rt, Environment: "test"})
	if err != nil {
		tb.Fatalf("launchdrive: %v", err)
	}
	return svc
}

// Close drives a governed CLOSURE of the subject through the real service: any earlier pending
// request of the subject (left by a refused attempt) is cancelled, a close request is filed by the
// fixture requester with every non-passing readiness check acknowledged, and both fixture
// approvers decide it. The executor's refusals (for example *tenant.CloseBlockedError for open
// sportsbook bets, GP020) are returned as the final approval's error, exactly as a console
// operator would see them.
func Close(ctx context.Context, tb testing.TB, rt *db.Pool, sub launchgov.Subject) error {
	tb.Helper()
	svc := Service(tb, rt)
	req, ap1, ap2 := Platform(tb)

	pend, err := svc.ListRequests(ctx, req, sub.TenantID, launchgov.ListFilter{BrandID: sub.BrandID, Statuses: []string{launchgov.ReqPending}})
	if err != nil {
		return err
	}
	for _, p := range pend {
		if _, err := svc.Cancel(ctx, req, p.Subject(), p.ID, "other_documented"); err != nil {
			return err
		}
	}
	r, err := svc.EvaluateReadiness(ctx, launchgov.EvalInput{Subject: sub, Action: launchgov.ActionClose})
	if err != nil {
		return err
	}
	res, err := svc.Request(ctx, req, launchgov.RequestInput{
		Subject: sub, Action: launchgov.ActionClose, ReasonCode: "offboarding", AcknowledgedCheckCodes: r.NonPassing(),
	})
	if err != nil {
		return err
	}
	var last error
	for _, who := range []launchgov.Actor{ap1, ap2} {
		cur, err := svc.EvaluateReadiness(ctx, launchgov.EvalInput{Subject: sub, Action: launchgov.ActionClose})
		if err != nil {
			return err
		}
		_, last = svc.Decide(ctx, who, sub.TenantID, res.Request.ID, launchgov.DecideInput{
			Decision: launchgov.DecisionApprove, ReasonCode: "reviewed_ok", SeenReadinessHash: cur.Hash(),
		})
		if last != nil {
			return last
		}
	}
	return last
}
