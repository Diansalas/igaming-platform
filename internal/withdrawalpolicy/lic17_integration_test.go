//go:build integration

// lic17 (migration 0184): F-2 (the first configured allow over the interim default needs two distinct approvers) and M-3 (the
// effective / set-version functions are bound to the session scope). Runs as igaming_runtime; the owner pool provisions fixtures.
// No real money, no real provider.
package withdrawalpolicy

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// F-2: over the interim default (source default_unconfigured, action allow) a request to `allow` is a LOOSENING: two approvals
// by distinct Persons, the requester alone cannot execute it, and the UNCONFIGURED notice stays until the second approval.
// After it executes, re-pinning the CONFIGURED allow is equal (single actor), and a tightening stays single actor.
func TestLic17_F2_FirstConfiguredAllowOverTheDefault_NeedsTwoApprovers(t *testing.T) {
	w := newGWorld(t)
	tn := w.tenant()
	for _, cause := range []string{"licence_suspended", "licence_expired", "licence_not_yet_issued", "licence_missing"} {
		res, err := w.svc.Request(context.Background(), w.req, RequestInput{TenantID: tn, Cause: cause, Action: ActionAllow, ReasonCode: "lic17_f2"})
		if err != nil || res.Executed || !res.Change.Widening || res.Change.RequiredApprovals != 2 {
			t.Fatalf("%s: a first configured allow over the default must wait for two approvals: %+v %v", cause, res, err)
		}
		if v := w.ownerInt(`SELECT count(*) FROM withdrawal_restriction_policies WHERE tenant_id = $1 AND cause = $2`, tn, cause); v != 0 {
			t.Fatalf("%s: nothing is written before the approvals, got %d rows", cause, v)
		}
		// The SQL classifier agrees (the guard recomputes it; a request carrying widening=false would be refused).
		var widening bool
		if err := w.rt.WithPlatformAdmin(context.Background(), w.req.StaffID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT withdrawal_restriction_is_widening($1, $2, 'allow')`, tn, cause).Scan(&widening)
		}); err != nil || !widening {
			t.Fatalf("%s: withdrawal_restriction_is_widening(allow over default) must be true: %v %v", cause, widening, err)
		}
		if cause != "licence_suspended" {
			continue
		}
		r1, err := w.svc.Approve(context.Background(), w.ap1, res.Change.ID, "ok")
		if err != nil || r1.Executed {
			t.Fatalf("one approval does not execute: %+v %v", r1, err)
		}
		r2, err := w.svc.Approve(context.Background(), w.ap2, res.Change.ID, "ok")
		if err != nil || !r2.Executed || r2.Policy == nil || r2.Policy.Action != ActionAllow {
			t.Fatalf("the second distinct approver executes: %+v %v", r2, err)
		}
		// A configured allow re-pinned: equal, single actor (unchanged behaviour).
		again, err := w.svc.Request(context.Background(), w.req, RequestInput{TenantID: tn, Cause: cause, Action: ActionAllow, ReasonCode: "repin"})
		if err != nil || !again.Executed || again.Change.Widening {
			t.Fatalf("re-pinning a configured allow is single actor: %+v %v", again, err)
		}
	}
	// Tightening over the default stays single actor (unchanged). A fresh tenant: the requests above are still open.
	res, err := w.svc.Request(context.Background(), w.req, RequestInput{TenantID: w.tenant(), Cause: "licence_expired", Action: ActionHoldForReview, ReasonCode: "tighten"})
	if err != nil || !res.Executed || res.Change.Widening {
		t.Fatalf("hold_for_review over the default is a tightening: %+v %v", res, err)
	}
}

// M-3: withdrawal_restriction_effective and withdrawal_restriction_set_version refuse (WR003) every session in which the RLS
// policies would hide the tenant's rows and so turn a configured block into the default: tenant + platform GUCs, a tenant
// session asking for another tenant, a player-scoped or acting session, a session without scope. The tenant's own session and a
// platform session resolve. The Go evaluator maps the refusal to ErrGateUnavailable (fail closed), never to allow.
func TestLic17_M3_RestrictionFunctionsAreBoundToTheSessionScope(t *testing.T) {
	w := newGWorld(t)
	tn, other := w.tenant(), w.tenant()
	if res, err := w.svc.Request(context.Background(), w.req, RequestInput{TenantID: tn, Cause: "licence_missing", Action: ActionBlock, ReasonCode: "lic17_m3"}); err != nil || !res.Executed {
		t.Fatalf("seed block: %+v %v", res, err)
	}
	both := func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, w.req.StaffID.String())
		return err
	}
	player := func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT set_config('app.player_account_id', $1, true)`, uuid.NewString())
		return err
	}
	actingT := func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT set_config('app.acting_tenant_id', $1, true)`, tn.String())
		return err
	}
	actingP := func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT set_config('app.acting_platform_principal_id', $1, true)`, w.req.StaffID.String())
		return err
	}
	none := func(context.Context, pgx.Tx) error { return nil }
	type session struct {
		name   string
		run    func(ctx context.Context, fn func(context.Context, pgx.Tx) error) error
		setup  func(context.Context, pgx.Tx) error
		target uuid.UUID
		ok     bool
	}
	inTenant := func(id uuid.UUID) func(context.Context, func(context.Context, pgx.Tx) error) error {
		return func(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
			return w.rt.WithTenant(ctx, id, fn)
		}
	}
	inPlatform := func(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
		return w.rt.WithPlatformAdmin(ctx, w.req.StaffID, fn)
	}
	inNone := func(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
		return w.rt.WithoutTenant(ctx, fn)
	}
	for _, s := range []session{
		{"own tenant", inTenant(tn), none, tn, true},
		{"platform", inPlatform, none, tn, true},
		{"tenant + platform", inTenant(tn), both, tn, false},
		{"other tenant's session", inTenant(other), none, tn, false},
		{"player-scoped", inTenant(tn), player, tn, false},
		{"acting tenant", inTenant(tn), actingT, tn, false},
		{"acting platform", inTenant(tn), actingP, tn, false},
		{"no scope", inNone, none, tn, false},
	} {
		for _, q := range []string{
			`SELECT action FROM withdrawal_restriction_effective($1, 'licence_missing', 'request')`,
			`SELECT withdrawal_restriction_set_version($1)::text`,
		} {
			var got string
			err := s.run(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
				if err := s.setup(ctx, tx); err != nil {
					return err
				}
				return tx.QueryRow(ctx, q, s.target).Scan(&got)
			})
			switch {
			case s.ok && err != nil:
				t.Fatalf("%s: %s must resolve: %v", s.name, q, err)
			case s.ok && got != "block" && got != "1":
				t.Fatalf("%s: %s must see the configured row, got %q", s.name, q, got)
			case !s.ok && sqlstate(err) != "WR003":
				t.Fatalf("%s: %s must be refused WR003, got %q / %v", s.name, q, got, err)
			}
		}
	}
	// The Go evaluator in the tenant + platform session: ErrGateUnavailable, never a silent allow.
	err := w.rt.WithTenant(context.Background(), tn, func(ctx context.Context, tx pgx.Tx) error {
		if err := both(ctx, tx); err != nil {
			return err
		}
		_, err := withdrawal.EvaluateRestriction(ctx, tx, tn, uuid.Nil, withdrawal.RestrictionStageRequest)
		return err
	})
	if !errors.Is(err, withdrawal.ErrGateUnavailable) {
		t.Fatalf("a mis-scoped evaluation must fail closed, got %v", err)
	}
}

// L-2 (database half): every configurable cause's registry default is `allow` at every stage, so no default_unconfigured
// decision can refuse (the Go tripwire assertRecordable is TestLic17_L2_RefusingDefaultIsNeverRecordable). Changing a default
// to a non-allow action must revisit WD056 recording first.
func TestLic17_L2_RegistryDefaultsAreAllow(t *testing.T) {
	w := newGWorld(t)
	if n := w.ownerInt(`SELECT count(*) FROM withdrawal_restriction_causes WHERE configurable`); n != 4 {
		t.Fatalf("four configurable causes expected, got %d", n)
	}
	if n := w.ownerInt(`SELECT count(*) FROM withdrawal_restriction_causes WHERE configurable
		AND (default_request IS DISTINCT FROM 'allow' OR default_dispatch IS DISTINCT FROM 'allow' OR default_redispatch IS DISTINCT FROM 'allow')`); n != 0 {
		t.Fatalf("a configurable cause has a non-allow registry default (%d): revisit WD056 recording (lic17 L-2) first", n)
	}
}
