package httpserver

// lic30: the policy route's engineering floor, the 403 / 404 behaviour security asked for, and the scope of the refusal codes.

import (
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

func TestLic30_PolicyRoute_RescreenIntervalFloor(t *testing.T) {
	f := &fakeAML{}
	mux, path := policyMux(f)
	body := func(secs int) string {
		return fmt.Sprintf(`{"jurisdiction_id":%q,"lists":["pep"],"rescreen_interval_seconds":%d,"backfill_grace_seconds":0,"effective_from":"2030-01-01T00:00:00Z","legal_review_reference":"R","reason_code":"r_1"}`, uuid.NewString(), secs)
	}
	for _, secs := range []int{1, 60, 3599} {
		f.calls = nil
		rec := amlDo(mux, "POST", path, body(secs))
		if rec.Code != http.StatusBadRequest || len(f.calls) != 0 || !strings.Contains(rec.Body.String(), `"rescreen_interval_below_floor"`) {
			t.Errorf("%d s: %d %s calls=%v, want 400 with the closed code and no service call", secs, rec.Code, rec.Body, f.calls)
		}
	}
	f.calls = nil
	if rec := amlDo(mux, "POST", path, body(3600)); rec.Code != http.StatusCreated || len(f.calls) != 1 {
		t.Fatalf("exactly the floor is accepted: %d %s", rec.Code, rec.Body)
	}
	// The 1-year cap is kept.
	if rec := amlDo(mux, "POST", path, body(31536001)); rec.Code != http.StatusBadRequest {
		t.Fatalf("above the cap: %d", rec.Code)
	}
}

// A tenant-scope officer is refused with 403 and the closed code platform_officer_required (the database decides; nothing else changes).
func TestLic30_PolicyRoute_TenantScopeOfficerIs403WithTheClosedCode(t *testing.T) {
	err := aml.ClassifyDBError(&pgconn.PgError{Code: "AM002", Message: "database wording", Hint: "platform_officer_required"})
	code, _, checks := amlErrorCode(err)
	if apierror.Status(code) != http.StatusForbidden || fmt.Sprint(checks) != "[platform_officer_required]" {
		t.Fatalf("%d %v", apierror.Status(code), checks)
	}
	f := &fakeAML{err: err}
	mux, path := policyMux(f)
	rec := amlDo(mux, "POST", path, fmt.Sprintf(`{"jurisdiction_id":%q,"lists":["pep"],"rescreen_interval_seconds":3600,"backfill_grace_seconds":0,"effective_from":"2030-01-01T00:00:00Z","legal_review_reference":"R","reason_code":"r_1"}`, uuid.NewString()))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"platform_officer_required"`) || strings.Contains(rec.Body.String(), "database wording") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// An officer of tenant B posting tenant A's id gets 404 on both policy routes and the service is never reached.
func TestLic30_PolicyRoutes_ForeignTenantIs404(t *testing.T) {
	f := &fakeAML{}
	staff := uuid.New()
	tc := tenant.Context{TenantID: uuid.New(), Subject: staff.String(), Role: string(auth.RoleCompliance), PrincipalType: string(auth.PrincipalStaff)}
	m := http.NewServeMux()
	inject := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.ServeHTTP(w, r.WithContext(tenant.WithContext(r.Context(), tc)))
		})
	}
	deps := Deps{AML: f, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	m.Handle("GET /t/{tenantID}/policy", inject(amlHandler(deps, "p", amlScreeningPolicyRead)))
	m.Handle("POST /t/{tenantID}/policy", inject(amlHandler(deps, "ps", amlScreeningPolicySet)))
	foreign := "/t/" + uuid.NewString() + "/policy" // tenant A's id, the caller belongs to tenant B
	if rec := amlDo(m, "GET", foreign+"?jurisdiction_id="+uuid.NewString(), ""); rec.Code != http.StatusNotFound {
		t.Errorf("GET: %d %s", rec.Code, rec.Body)
	}
	body := fmt.Sprintf(`{"jurisdiction_id":%q,"lists":["pep"],"rescreen_interval_seconds":3600,"backfill_grace_seconds":0,"effective_from":"2030-01-01T00:00:00Z","legal_review_reference":"R","reason_code":"r_1"}`, uuid.NewString())
	if rec := amlDo(m, "POST", foreign, body); rec.Code != http.StatusNotFound {
		t.Errorf("POST: %d %s", rec.Code, rec.Body)
	}
	if len(f.calls) != 0 {
		t.Fatalf("the service was reached: %v", f.calls)
	}
}

// The closed refusal codes are produced only by the AML route layer: no other handler (player, payments, wallet...) reads them.
func TestLic30_RefusalCodesAreNotUsedOutsideTheAMLRoutes(t *testing.T) {
	var offenders []string
	err := filepath.WalkDir("../..", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", ".claude", "backoffice", "worktrees":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		src := string(b)
		if !strings.Contains(src, "RefusalCode(") && !strings.Contains(src, "RefusalCodes") {
			return nil
		}
		slash := filepath.ToSlash(p)
		if strings.HasSuffix(slash, "internal/httpserver/aml_routes.go") || strings.Contains(slash, "internal/aml/") {
			return nil
		}
		// c4 (migration 0190), INTENTIONAL pin change: the governed screening policy loosening is an AML-owned decision executed by the
		// licensing change primitive, so its refusals reach the client through the licensing change routes. That one file may do exactly one
		// thing with the table: test a database HINT for membership (aml.RefusalCodes[hint]); it never calls RefusalCode(...).
		if strings.HasSuffix(slash, "internal/httpserver/licensing_change_routes.go") && !strings.Contains(src, "RefusalCode(") &&
			strings.Count(src, "RefusalCodes") == 1 && strings.Contains(src, "aml.RefusalCodes[ge.Hint]") {
			return nil
		}
		offenders = append(offenders, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) != 0 {
		t.Fatalf("refusal codes are read outside the AML layer: %v", offenders)
	}
}
