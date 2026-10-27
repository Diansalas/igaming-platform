package httpserver

// lic29: the governed screening-policy routes and the closed refusal codes of the screening routes.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

func policyMux(f *fakeAML) (*http.ServeMux, string) {
	tid, staff := uuid.New(), uuid.New()
	tc := tenant.Context{TenantID: tid, Subject: staff.String(), Role: string(auth.RoleCompliance), PrincipalType: string(auth.PrincipalStaff)}
	mux := amlTestMux(f, tc)
	_ = mux
	m := http.NewServeMux()
	inject := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.ServeHTTP(w, r.WithContext(tenant.WithContext(r.Context(), tc)))
		})
	}
	deps := Deps{AML: f, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	m.Handle("GET /t/{tenantID}/policy", inject(amlHandler(deps, "p", amlScreeningPolicyRead)))
	m.Handle("POST /t/{tenantID}/policy", inject(amlHandler(deps, "ps", amlScreeningPolicySet)))
	return m, "/t/" + tid.String() + "/policy"
}

func TestLic29_ScreeningPolicyRoute_NoDefaultsAndNothingFromTheBodyButTheVersion(t *testing.T) {
	f := &fakeAML{}
	mux, path := policyMux(f)
	jur := uuid.New().String()
	ok := func(over string) string {
		return fmt.Sprintf(`{"jurisdiction_id":%q,"lists":["sanctions","pep"],"rescreen_interval_seconds":86400,"backfill_grace_seconds":3600,"effective_from":"2030-01-01T00:00:00Z","legal_review_reference":"REF-1","reason_code":"r_1"%s}`, jur, over)
	}
	// Refused before the service: a missing duration is a 400 (no default), as are negative / zero / absurd values, a bad time and
	// body fields that name a tenant or an actor.
	for name, body := range map[string]string{
		"no rescreen interval": fmt.Sprintf(`{"jurisdiction_id":%q,"lists":["pep"],"backfill_grace_seconds":0,"effective_from":"2030-01-01T00:00:00Z","legal_review_reference":"R","reason_code":"r_1"}`, jur),
		"no grace":             fmt.Sprintf(`{"jurisdiction_id":%q,"lists":["pep"],"rescreen_interval_seconds":60,"effective_from":"2030-01-01T00:00:00Z","legal_review_reference":"R","reason_code":"r_1"}`, jur),
		"zero interval":        strings.Replace(ok(""), `"rescreen_interval_seconds":86400`, `"rescreen_interval_seconds":0`, 1),
		"negative grace":       strings.Replace(ok(""), `"backfill_grace_seconds":3600`, `"backfill_grace_seconds":-1`, 1),
		"absurd interval":      strings.Replace(ok(""), `"rescreen_interval_seconds":86400`, `"rescreen_interval_seconds":99999999999`, 1),
		"bad time":             strings.Replace(ok(""), "2030-01-01T00:00:00Z", "tomorrow", 1),
		"bad jurisdiction":     strings.Replace(ok(""), jur, "x", 1),
		"body tenant":          ok(`,"tenant_id":"` + uuid.NewString() + `"`),
		"body actor":           ok(`,"created_by":"` + uuid.NewString() + `"`),
	} {
		f.calls = nil
		if rec := amlDo(mux, "POST", path, body); rec.Code != http.StatusBadRequest || len(f.calls) != 0 {
			t.Errorf("%s: %d %s calls=%v, want 400 and no service call", name, rec.Code, rec.Body, f.calls)
		}
	}
	// A complete request reaches AuthorScreeningPolicy (never SetScreeningPolicy) with exactly the stated values.
	f.calls = nil
	rec := amlDo(mux, "POST", path, ok(""))
	if rec.Code != http.StatusCreated || len(f.calls) != 1 || f.calls[0] != "AuthorScreeningPolicy" {
		t.Fatalf("%d %s %v", rec.Code, rec.Body, f.calls)
	}
	in := f.lastPolicy
	if in.RescreenInterval != 24*time.Hour || in.BackfillGrace != time.Hour || len(in.Lists) != 2 || in.LegalReviewReference != "REF-1" || in.ReasonCode != "r_1" ||
		in.JurisdictionID.String() != jur || !in.EffectiveFrom.Equal(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("input: %+v", in)
	}
	if f.actors[0].TenantID == uuid.Nil || f.actors[0].StaffID == uuid.Nil {
		t.Fatal("the actor comes from the authenticated context")
	}
	// A zero grace is a STATED value and is accepted.
	f.calls = nil
	if rec := amlDo(mux, "POST", path, strings.Replace(ok(""), `"backfill_grace_seconds":3600`, `"backfill_grace_seconds":0`, 1)); rec.Code != http.StatusCreated {
		t.Fatalf("an explicit zero grace: %d %s", rec.Code, rec.Body)
	}
}

func TestLic29_ScreeningPolicyRoute_ReadReportsUnconfiguredNeverADefault(t *testing.T) {
	f := &fakeAML{}
	mux, path := policyMux(f)
	rec := amlDo(mux, "GET", path+"?jurisdiction_id="+uuid.NewString(), "")
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 || out["configured"] != false ||
		out["rescreen_interval_seconds"] != nil || out["backfill_grace_seconds"] != nil || out["effective_from"] != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := amlDo(mux, "GET", path, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("no jurisdiction: %d", rec.Code)
	}
}

// The tighten-only author is the ONLY policy-writing call the HTTP layer can reach: the interface has no SetScreeningPolicy and no
// production file in this package names it (SR7-3).
func TestLic29_ScreeningPolicyRoute_OnlyTheTightenOnlyAuthorIsReachable(t *testing.T) {
	src, err := os.ReadFile("aml_screening_routes.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	if strings.Contains(s, "SetScreeningPolicy") || !strings.Contains(s, "c.svc.AuthorScreeningPolicy(") {
		t.Fatal("the route must call AuthorScreeningPolicy and never SetScreeningPolicy")
	}
	b, _ := os.ReadFile("aml_routes.go")
	if strings.Contains(string(b), "SetScreeningPolicy(") {
		t.Fatal("AMLOfficerAPI must not gain SetScreeningPolicy")
	}
	svc, _ := os.ReadFile("../aml/screening_service.go")
	if !strings.Contains(string(svc), `"aml_screening_policy_author")`) {
		t.Fatal("AuthorScreeningPolicy must go through aml_screening_policy_author")
	}
}

// The closed refusal code travels in check_codes; the status class and the fixed sentence are unchanged.
func TestLic29_RefusalCodeIsInTheResponseAndTheStatusIsUnchanged(t *testing.T) {
	cases := []struct {
		sqlstate, hint string
		status         int
		check          string
	}{
		{"AT004", "screening_worker_heartbeat_missing", http.StatusBadRequest, ""},
		{"AT004", "gate_environment_refused", http.StatusBadRequest, ""},
		{"AM006", "sanctions_clear_refused", http.StatusConflict, "hold_refused"},
		{"AT003", "request_lease_live", http.StatusConflict, "illegal_transition"},
		{"AT003", "policy_loosening_refused", http.StatusConflict, "illegal_transition"},
	}
	for _, c := range cases {
		err := aml.ClassifyDBError(&pgconn.PgError{Code: c.sqlstate, Message: "database wording", Hint: c.hint})
		code, msg, checks := amlErrorCode(err)
		if apierror.Status(code) != c.status {
			t.Errorf("%s/%s: status %d, want %d", c.sqlstate, c.hint, apierror.Status(code), c.status)
		}
		if strings.Contains(msg, "database wording") {
			t.Errorf("the database text must never reach the response: %q", msg)
		}
		want := []string{c.hint}
		if c.check != "" {
			want = []string{c.check, c.hint}
		}
		if fmt.Sprint(checks) != fmt.Sprint(want) {
			t.Errorf("%s/%s: check codes %v, want %v", c.sqlstate, c.hint, checks, want)
		}
	}
	// Without a hint nothing is added; an internal error never carries a code.
	if _, _, checks := amlErrorCode(aml.ClassifyDBError(&pgconn.PgError{Code: "AT004", Message: "x"})); len(checks) != 0 {
		t.Errorf("no hint, no code: %v", checks)
	}
	if code, _, checks := amlErrorCode(errors.New("boom")); code != apierror.CodeInternal || len(checks) != 0 {
		t.Errorf("internal: %v %v", code, checks)
	}
}
