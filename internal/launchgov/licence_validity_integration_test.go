//go:build integration

package launchgov_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/launchgov"
)

// Licensing slice 1: G-8 (the recorded licensing_status in_force must not contradict the licence registry) and the
// acknowledgement path (an activation under a suspended / expired licence stays possible only with the R-LIC-VALID
// failure explicitly acknowledged; whether it should hard-block is a pending human decision, L-22 / G-8).

func (w *world) setLicenceSQL(lid uuid.UUID, sql string) {
	w.t.Helper()
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, sql, lid)
		if err == nil && tag.RowsAffected() != 1 {
			return errors.New("licence update affected no row")
		}
		return err
	}); err != nil {
		w.t.Fatalf("set licence: %v", err)
	}
}

func TestLicensing_RecordedInForceMustNotContradictTheRegistry_AndInvalidLicenceIsAcknowledgeable(t *testing.T) {
	cases := []struct {
		name     string
		sql      string
		wantFail string // R-LIC-VALID detail
		recorded string // an honest recorded status for that licence
	}{
		{"suspended", `UPDATE licences SET status = 'suspended' WHERE id = $1`, "licence_suspended", launchgov.LicStatusSuspended},
		{"status expired", `UPDATE licences SET status = 'expired' WHERE id = $1`, "licence_status_expired", launchgov.LicStatusExpired},
		{"expires today (boundary)", `UPDATE licences SET expires_at = (now() AT TIME ZONE 'UTC')::date WHERE id = $1`, "licence_date_expired", launchgov.LicStatusExpired},
		{"not yet issued", `UPDATE licences SET issued_at = ((now() AT TIME ZONE 'UTC')::date + 10) WHERE id = $1`, "licence_not_yet_issued", launchgov.LicStatusConditional},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			ctx := context.Background()
			tid := w.activeTenant()
			jid := w.jurisdiction(tid)
			lid := w.licence(tid, jid, "platform", "active")
			brand := w.pendingBrand(tid)
			sub := launchgov.Subject{TenantID: tid, BrandID: &brand}
			w.setLicenceSQL(lid, c.sql)

			mk := func(status string) launchgov.RequestInput {
				in := w.activation(sub, jid)
				in.Licensing = &launchgov.LicensingRecord{Model: launchgov.LicModelPlatform, Status: status, LicenceID: &lid}
				in.AcknowledgedCheckCodes = w.nonPassing(in)
				return in
			}

			// An untrue record: in_force next to an invalid licence is refused by H-2 and stores nothing.
			var pe *launchgov.PreconditionError
			_, err := w.svc.Request(ctx, w.req, mk(launchgov.LicStatusInForce))
			if !errors.Is(err, launchgov.ErrPreconditionFailed) || !errors.As(err, &pe) || !contains(pe.Codes, launchgov.H2StatusContradictsRegistry) {
				t.Fatalf("in_force with an invalid licence must be refused with %s: %v", launchgov.H2StatusContradictsRegistry, err)
			}
			if n := w.countRequests(sub); n != 0 {
				t.Fatalf("a refused request must not be stored: %d", n)
			}

			// An honest record is accepted; R-LIC-VALID is a recorded FAIL that must be acknowledged.
			in := mk(c.recorded)
			rd, err := w.svc.EvaluateReadiness(ctx, launchgov.EvalInput{Subject: sub, Action: in.Action, Licensing: in.Licensing, JurisdictionIDs: in.JurisdictionIDs})
			if err != nil {
				t.Fatal(err)
			}
			var found bool
			for _, ck := range rd.Checks {
				if ck.Code == launchgov.CheckLicenceValid {
					found = true
					if ck.Result != launchgov.ResultFail || ck.DetailCode != c.wantFail {
						t.Fatalf("R-LIC-VALID = %s/%s, want fail/%s", ck.Result, ck.DetailCode, c.wantFail)
					}
				}
			}
			if !found {
				t.Fatal("R-LIC-VALID missing")
			}
			unack := in
			unack.AcknowledgedCheckCodes = without(in.AcknowledgedCheckCodes, launchgov.CheckLicenceValid)
			if _, err := w.svc.Request(ctx, w.req, unack); !errors.Is(err, launchgov.ErrUnacknowledged) {
				t.Fatalf("not acknowledging R-LIC-VALID must be refused: %v", err)
			}
			r := w.mustRequest(w.req, in)
			if _, err := w.approve(w.ap1, r); err != nil {
				t.Fatalf("activation under an invalid licence with acknowledgement: %v", err)
			}
			if got := w.status(sub); got != "active" {
				t.Fatalf("brand status = %s (activation under an invalid licence stays acknowledgeable; the market gate is the enforcement)", got)
			}
		})
	}
}

// A valid licence still accepts in_force (the new rule does not over-reach).
func TestLicensing_InForceWithAValidLicenceIsAccepted(t *testing.T) {
	w := newWorld(t)
	tid := w.activeTenant()
	jid := w.jurisdiction(tid)
	lid := w.licence(tid, jid, "platform", "active")
	brand := w.pendingBrand(tid)
	sub := launchgov.Subject{TenantID: tid, BrandID: &brand}
	in := w.activation(sub, jid)
	in.Licensing = &launchgov.LicensingRecord{Model: launchgov.LicModelPlatform, Status: launchgov.LicStatusInForce, LicenceID: &lid}
	in.AcknowledgedCheckCodes = w.nonPassing(in)
	if _, err := w.svc.Request(context.Background(), w.req, in); err != nil {
		t.Fatalf("valid licence + in_force: %v", err)
	}
}

func without(in []string, drop string) []string {
	var out []string
	for _, s := range in {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}
