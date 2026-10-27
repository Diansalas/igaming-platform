//go:build integration

package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/marketgate"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// Licensing slice 1 (audit G-6): the REAL market gate, at HTTP level, for a licence that is suspended, expired by
// status, expired by date (expires_at = today, the EvaluateLicenceValidity boundary) and for a tenant with NO licence
// (licence_id NULL): registration, deposit, casino launch and sportsbook placement are all refused with the one
// identical refusal, nothing is created, and a refusal audit row is written. The licence changes go through the new
// lifecycle writes (jurisdiction.SetLicence*). Restoring the licence re-opens the gate on the next request.
func TestMarketGate_HTTP_LicenceLifecycleRefusals(t *testing.T) {
	w := newMarketWorld(t)
	owner := launchfix.OwnerFor(t, w.pool)
	admin := mustSeedPlatformAdminStaffPrincipal(t, w.pool)
	var licenceID uuid.UUID
	if err := w.pool.WithTenant(context.Background(), w.tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT licence_id FROM tenants WHERE id = $1`, w.tenant.ID).Scan(&licenceID)
	}); err != nil {
		t.Fatal(err)
	}
	actor := jurisdiction.ActorContext{ActorID: admin, IPAddress: "127.0.0.1", RequestID: "g6", ReasonCode: "g6_test"}
	lifecycle := func(fn func(ctx context.Context, tx pgx.Tx) error) {
		t.Helper()
		if err := owner.WithPlatformAdmin(context.Background(), admin, fn); err != nil {
			t.Fatal(err)
		}
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	future := today.AddDate(1, 0, 0)
	restore := func() {
		lifecycle(func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE licences SET status = 'active', expires_at = $2 WHERE id = $1`, licenceID, future); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $2 WHERE id = $1`, w.tenant.ID, licenceID)
			return err
		})
	}

	causes := []struct {
		name  string
		apply func(ctx context.Context, tx pgx.Tx) error
	}{
		{"licence suspended", func(ctx context.Context, tx pgx.Tx) error {
			_, err := jurisdiction.SetLicenceStatus(ctx, tx, licenceID, "suspended", actor)
			return err
		}},
		{"licence status expired", func(ctx context.Context, tx pgx.Tx) error {
			_, err := jurisdiction.SetLicenceStatus(ctx, tx, licenceID, "expired", actor)
			return err
		}},
		{"licence expires_at is today", func(ctx context.Context, tx pgx.Tx) error {
			_, err := jurisdiction.SetLicenceValidityDates(ctx, tx, licenceID, nil, &today, actor)
			return err
		}},
		{"tenant has no licence (licence_id NULL)", func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = NULL WHERE id = $1`, w.tenant.ID)
			return err
		}},
	}
	// Sanity: the open market serves.
	if resp := w.deposit(t); resp.StatusCode != http.StatusCreated {
		t.Fatalf("open market: %d", resp.StatusCode)
	}
	audits := map[string]int{}
	canonical := ""
	for _, c := range causes {
		lifecycle(c.apply)
		p0, i0, s0, wl0 := w.counts(t)
		b0, l0 := w.sportsbookRows(t)
		points := map[string]*http.Response{"registration": w.register(t), "deposit": w.deposit(t), "launch": w.launch(t), "sportsbook_wagering": w.bet(t)}
		for op, resp := range points {
			status, body := refusalBody(t, resp)
			if status != http.StatusForbidden {
				t.Fatalf("%s / %s: want 403, got %d (%s)", c.name, op, status, body)
			}
			var m struct{ Code, Message string }
			_ = json.Unmarshal([]byte(body), &m)
			if m.Code != string(apierror.CodeMarketNotAvailable) || m.Message != marketgate.PlayerMessage {
				t.Fatalf("%s / %s: unexpected refusal %s", c.name, op, body)
			}
			if canonical == "" {
				canonical = body
			} else if body != canonical {
				t.Fatalf("%s / %s: refusal differs (oracle):\n got  %s\n want %s", c.name, op, body, canonical)
			}
			audits[op]++
		}
		p1, i1, s1, wl1 := w.counts(t)
		b1, l1 := w.sportsbookRows(t)
		if p1 != p0 || i1 != i0 || s1 != s0 || wl1 != wl0 || b1 != b0 || l1 != l0 {
			t.Fatalf("%s: a refused request created rows", c.name)
		}
		for op, n := range audits {
			if got := w.refusalAudits(t, op); got != n {
				t.Fatalf("%s / %s: want %d refusal audit rows, got %d", c.name, op, n, got)
			}
		}
		restore()
		if resp := w.deposit(t); resp.StatusCode != http.StatusCreated {
			t.Fatalf("%s: after restoring the licence, deposit want 201, got %d", c.name, resp.StatusCode)
		}
	}
}
