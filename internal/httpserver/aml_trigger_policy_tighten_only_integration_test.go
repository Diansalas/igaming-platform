//go:build integration

package httpserver

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/rgfix"
)

// d1 (migration 0191, D4): over the real HTTP stack as the runtime role, the trigger policy route is TIGHTEN-ONLY. A version that removes an
// event, switches the gate off or lengthens the alert threshold is a 409 with the closed code trigger_policy_loosening_refused in check_codes,
// the generic sentence only (never the database text), and nothing is written. Tightening and equal versions stay on the route.
func TestD1_TriggerPolicyRoute_LooseningIsRefusedWithAClosedCode_AndNothingIsWritten(t *testing.T) {
	rgfix.EnsureTestEnvironment(t, os.Getenv("TEST_DATABASE_URL"))
	w := newAMLHTTP(t, "under_platform_licence")
	jid := w.c1Jurisdiction()
	ctx := context.Background()
	q := amlfix.Bootstrap(t, w.owner, w.tenant, 2)
	tok := w.api.token(q.Officers[0].StaffID, uuid.Nil, auth.RolePlatformAdmin)
	base := "/v1/admin/tenants/" + w.tenant.String() + "/screening"
	count := func() int {
		return w.c1Count(`SELECT count(*) FROM aml_screening_trigger_policies WHERE tenant_id = $1`, w.tenant)
	}
	body := func(events []string, gate bool, alert int) map[string]any {
		m := map[string]any{"jurisdiction_id": jid, "events": events, "gate_withdrawal": gate, "gate_alert_after_seconds": 0,
			"effective_from": time.Now().Add(time.Minute).UTC().Format(time.RFC3339), "legal_review_reference": "D1-REVIEW", "reason_code": "d1_trigger"}
		if gate {
			m["gate_alert_after_seconds"] = alert
		}
		return m
	}
	// A screening policy first (the trigger policy needs one in effect before it).
	if r := w.api.do("POST", base+"/policy", tok, c1PolicyBody(jid)); r.status != http.StatusCreated {
		t.Fatalf("screening policy: %d %s", r.status, r.body)
	}
	// Heartbeat: a gate is authored only while a worker has reported in (the runtime-callable, proof-less worker API).
	if err := w.rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT screening_worker_heartbeat('mock-screening', true)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	all := []string{"registration", "kyc_approval", "scheduled"}
	// First version: a gate with three events. (Nothing before it to loosen.)
	if r := w.api.do("POST", base+"/trigger-policy", tok, body(all, true, 600)); r.status != http.StatusCreated {
		t.Fatalf("the first version: %d %s", r.status, r.body)
	}
	before := count()
	refuse := func(what string, b map[string]any) {
		t.Helper()
		r := w.api.do("POST", base+"/trigger-policy", tok, b)
		if r.status != http.StatusConflict {
			t.Fatalf("%s: status %d, want 409: %s", what, r.status, r.body)
		}
		// The generic class code of the sentinel (illegal_transition) rides with the one closed refusal code, as for policy_loosening_refused.
		if got := strings.Join(r.checkCodes(t), ","); got != "illegal_transition,trigger_policy_loosening_refused" {
			t.Fatalf("%s: check_codes %q: %s", what, got, r.body)
		}
		if strings.Contains(string(r.body), "less strict") || strings.Contains(string(r.body), "AT003") || strings.Contains(string(r.body), "aml_screening") {
			t.Fatalf("%s: the body leaks database wording: %s", what, r.body)
		}
		if n := count(); n != before {
			t.Fatalf("%s: a refused loosening wrote a row: %d -> %d", what, before, n)
		}
	}
	refuse("an event removed", body([]string{"registration", "scheduled"}, true, 600))
	refuse("the gate switched off", body(all, false, 0))
	refuse("the gate switched off and every event removed", body([]string{}, false, 0))
	refuse("a longer alert threshold", body(all, true, 1200))
	// An equal version and a stricter one stay on the route.
	if r := w.api.do("POST", base+"/trigger-policy", tok, body(all, true, 600)); r.status != http.StatusCreated {
		t.Fatalf("an equal version: %d %s", r.status, r.body)
	}
	if r := w.api.do("POST", base+"/trigger-policy", tok, body(all, true, 300)); r.status != http.StatusCreated {
		t.Fatalf("a shorter threshold tightens: %d %s", r.status, r.body)
	}
	if n := count(); n != before+2 {
		t.Fatalf("the two accepted versions: %d -> %d", before, n)
	}
}
