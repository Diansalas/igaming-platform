package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/stepup"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// ADR 0116 s6 (A7). DB-free tests of the AML officer route layer: tenant scoping, actor construction from the token only,
// the closed error table (no database text, no tipping-off wording), fail-closed availability, no-store, strict bodies.
// The chain (player/service/aal1/unknown-role refusals on EVERY route) is the existing golden sweep
// TestStaffRoutes_DBFree_ChainOnEveryRoute; the step-up classes are pinned against the signer backstop below.

type fakeAML struct {
	calls   []string
	actors  []aml.Actor
	err     error
	lastIn  aml.PlaceHoldInput
	lastSeq int
}

func (f *fakeAML) rec(op string, a aml.Actor) error {
	f.calls = append(f.calls, op)
	f.actors = append(f.actors, a)
	return f.err
}
func (f *fakeAML) PlaceHold(_ context.Context, a aml.Actor, in aml.PlaceHoldInput) (uuid.UUID, error) {
	f.lastIn = in
	return uuid.New(), f.rec("PlaceHold", a)
}
func (f *fakeAML) LiftHold(_ context.Context, a aml.Actor, _ uuid.UUID, _ string) (uuid.UUID, error) {
	return uuid.New(), f.rec("LiftHold", a)
}
func (f *fakeAML) ListHolds(_ context.Context, a aml.Actor, _ uuid.UUID) ([]aml.Hold, error) {
	return []aml.Hold{{ID: uuid.New(), OwnerScope: "tenant", Scope: aml.ScopeAllValueMovement, ReasonClass: aml.ReasonManual, ReasonCode: "r", PlacedAt: time.Unix(0, 0)}}, f.rec("ListHolds", a)
}
func (f *fakeAML) OpenCase(_ context.Context, a aml.Actor, _ uuid.UUID, _ aml.CaseKind, _ string, _ []uuid.UUID) (uuid.UUID, error) {
	return uuid.New(), f.rec("OpenCase", a)
}
func (f *fakeAML) ReadCase(_ context.Context, a aml.Actor, id uuid.UUID) (aml.Case, error) {
	return aml.Case{ID: id, OwnerScope: "tenant", PersonID: uuid.New(), Kind: aml.CaseKindManual, Status: aml.StatusOpen, LastSeq: 1, OpenedAt: time.Unix(0, 0)}, f.rec("ReadCase", a)
}
func (f *fakeAML) CaseEvents(_ context.Context, a aml.Actor, _ uuid.UUID) ([]aml.CaseEvent, error) {
	return nil, f.rec("CaseEvents", a)
}
func (f *fakeAML) CaseNotes(_ context.Context, a aml.Actor, _ uuid.UUID) ([]aml.CaseNote, error) {
	return nil, f.rec("CaseNotes", a)
}
func (f *fakeAML) CaseQueue(_ context.Context, a aml.Actor, limit int) ([]aml.QueueItem, error) {
	f.lastSeq = limit
	return nil, f.rec("CaseQueue", a)
}
func (f *fakeAML) Transition(_ context.Context, a aml.Actor, _ uuid.UUID, _ int, _ aml.CaseStatus, _ string) (int, error) {
	return 3, f.rec("Transition", a)
}
func (f *fakeAML) AddNote(_ context.Context, a aml.Actor, _ uuid.UUID, _ int, _ string) (int, error) {
	return 3, f.rec("AddNote", a)
}
func (f *fakeAML) FlagSAR(_ context.Context, a aml.Actor, _ uuid.UUID, _ int, _ string) (int, error) {
	return 3, f.rec("FlagSAR", a)
}
func (f *fakeAML) Propose(_ context.Context, a aml.Actor, _ uuid.UUID, _ int, _ aml.ProposeInput) (aml.Proposal, error) {
	return aml.Proposal{Seq: 2, ContentHash: strings.Repeat("a", 64)}, f.rec("Propose", a)
}
func (f *fakeAML) SetScreeningTriggerPolicy(_ context.Context, a aml.Actor, _ aml.SetTriggerPolicyInput) (uuid.UUID, error) {
	return uuid.New(), f.rec("SetScreeningTriggerPolicy", a)
}
func (f *fakeAML) EffectiveScreeningTriggerPolicy(_ context.Context, a aml.Actor, _ uuid.UUID) (*aml.TriggerPolicy, error) {
	return nil, f.rec("EffectiveScreeningTriggerPolicy", a)
}
func (f *fakeAML) ListScreeningRequests(_ context.Context, a aml.Actor, _ uuid.UUID) ([]aml.ScreeningRequestRow, error) {
	return nil, f.rec("ListScreeningRequests", a)
}
func (f *fakeAML) ResolveScreeningRequest(_ context.Context, a aml.Actor, _, _ uuid.UUID, _ aml.ResolveScreeningInput) (aml.ScreeningRecorded, error) {
	return aml.ScreeningRecorded{ResultID: uuid.New()}, f.rec("ResolveScreeningRequest", a)
}
func (f *fakeAML) Approve(_ context.Context, a aml.Actor, _ uuid.UUID, _ int, _, _ string) (int, error) {
	return 4, f.rec("Approve", a)
}

// amlTestMux mounts the AML handlers WITHOUT the staff chain (covered by the sweep) behind a context injector.
func amlTestMux(svc AMLOfficerAPI, tc tenant.Context) *http.ServeMux {
	deps := Deps{AML: svc, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	mux := http.NewServeMux()
	inject := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.ServeHTTP(w, r.WithContext(tenant.WithContext(r.Context(), tc)))
		})
	}
	for _, rt := range []struct {
		pat string
		op  string
		fn  amlFunc
	}{
		{"GET /t/{tenantID}/cases", "q", amlCaseQueue},
		{"GET /t/{tenantID}/cases/{caseID}", "r", amlCaseRead},
		{"GET /t/{tenantID}/persons/{personID}/holds", "h", amlHoldList},
		{"POST /t/{tenantID}/holds", "hp", amlHoldPlace},
		{"POST /t/{tenantID}/holds/{holdID}/lift", "hl", amlHoldLift},
		{"POST /t/{tenantID}/cases", "co", amlCaseOpen},
		{"POST /t/{tenantID}/cases/{caseID}/transition", "ct", amlCaseTransition},
		{"POST /t/{tenantID}/cases/{caseID}/notes", "cn", amlCaseNote},
		{"POST /t/{tenantID}/cases/{caseID}/sar-flag", "cf", amlCaseFlagSAR},
		{"POST /t/{tenantID}/cases/{caseID}/proposals", "cp", amlCasePropose},
		{"POST /t/{tenantID}/cases/{caseID}/approvals", "ca", amlCaseApprove},
	} {
		mux.Handle(rt.pat, inject(amlHandler(deps, rt.op, rt.fn)))
	}
	return mux
}

func amlDo(mux http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

type amlOpCase struct {
	name, method, path, body string
	wantCall                 string
}

func amlOps(tid, id uuid.UUID) []amlOpCase {
	t := "/t/" + tid.String()
	seqR := `{"expected_seq":1,"reason_code":"r_1"}`
	return []amlOpCase{
		{"queue", "GET", t + "/cases", "", "CaseQueue"},
		{"case read", "GET", t + "/cases/" + id.String(), "", "ReadCase"},
		{"holds", "GET", t + "/persons/" + id.String() + "/holds", "", "ListHolds"},
		{"place", "POST", t + "/holds", fmt.Sprintf(`{"person_id":%q,"scope":"all_value_movement","reason_class":"manual","reason_code":"r_1"}`, id), "PlaceHold"},
		{"lift", "POST", t + "/holds/" + id.String() + "/lift", `{"reason_code":"r_1"}`, "LiftHold"},
		{"open", "POST", t + "/cases", fmt.Sprintf(`{"person_id":%q,"kind":"manual","reason_code":"r_1"}`, id), "OpenCase"},
		{"transition", "POST", t + "/cases/" + id.String() + "/transition", `{"expected_seq":1,"to":"in_review","reason_code":"r_1"}`, "Transition"},
		{"note", "POST", t + "/cases/" + id.String() + "/notes", `{"expected_seq":1,"body":"checked"}`, "AddNote"},
		{"sar flag", "POST", t + "/cases/" + id.String() + "/sar-flag", seqR, "FlagSAR"},
		{"propose", "POST", t + "/cases/" + id.String() + "/proposals", `{"expected_seq":1,"kind":"close_sar_not_filed","reason_code":"r_1"}`, "Propose"},
		{"approve", "POST", t + "/cases/" + id.String() + "/approvals", fmt.Sprintf(`{"proposal_seq":2,"content_hash":%q,"reason_code":"r_1"}`, strings.Repeat("a", 64)), "Approve"},
	}
}

// A tenant-scoped token acts in its OWN tenant; the actor is built from the token (subject, tenant), never from the request;
// every response is no-store.
func TestAMLRoutes_TenantTokenActsInOwnTenant(t *testing.T) {
	tid, staff := uuid.New(), uuid.New()
	f := &fakeAML{}
	mux := amlTestMux(f, tenant.Context{TenantID: tid, Subject: staff.String(), Role: string(auth.RoleCompliance), PrincipalType: string(auth.PrincipalStaff)})
	for _, op := range amlOps(tid, uuid.New()) {
		f.calls, f.actors = nil, nil
		rec := amlDo(mux, op.method, op.path, op.body)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("%s: %d %s", op.name, rec.Code, rec.Body)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control %q, want no-store", op.name, rec.Header().Get("Cache-Control"))
		}
		if len(f.calls) == 0 || f.calls[0] != op.wantCall {
			t.Fatalf("%s: calls %v, want first %s", op.name, f.calls, op.wantCall)
		}
		for _, a := range f.actors {
			if a.Scope != aml.ScopeTenant || a.TenantID != tid || a.StaffID != staff {
				t.Errorf("%s: actor %+v, want tenant scope in own tenant as the token subject", op.name, a)
			}
		}
	}
}

// A platform token (no tenant) acts as platform_acting in the VALIDATED path tenant.
func TestAMLRoutes_PlatformTokenActsAsPlatformActingInPathTenant(t *testing.T) {
	target, staff := uuid.New(), uuid.New()
	f := &fakeAML{}
	mux := amlTestMux(f, tenant.Context{TenantID: uuid.Nil, Subject: staff.String(), Role: string(auth.RolePlatformAdmin), PrincipalType: string(auth.PrincipalStaff)})
	for _, op := range amlOps(target, uuid.New()) {
		f.calls, f.actors = nil, nil
		rec := amlDo(mux, op.method, op.path, op.body)
		if rec.Code >= 300 {
			t.Fatalf("%s: %d %s", op.name, rec.Code, rec.Body)
		}
		for _, a := range f.actors {
			if a.Scope != aml.ScopePlatformActing || a.TenantID != target || a.StaffID != staff {
				t.Errorf("%s: actor %+v, want platform_acting in the path tenant", op.name, a)
			}
		}
	}
}

// A tenant token naming ANOTHER tenant: 404 not_found on every route, the service is never reached (no existence oracle).
func TestAMLRoutes_ForeignTenantIs404AndNeverReachesTheService(t *testing.T) {
	own, other := uuid.New(), uuid.New()
	f := &fakeAML{}
	mux := amlTestMux(f, tenant.Context{TenantID: own, Subject: uuid.NewString(), Role: string(auth.RoleCompliance), PrincipalType: string(auth.PrincipalStaff)})
	for _, op := range amlOps(other, uuid.New()) {
		rec := amlDo(mux, op.method, op.path, op.body)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"code":"not_found"`) {
			t.Errorf("%s: %d %s, want 404 not_found", op.name, rec.Code, rec.Body)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("a foreign tenant reached the service: %v", f.calls)
	}
}

// A body can never choose the tenant or the actor: the strict decoder refuses unknown fields.
func TestAMLRoutes_BodyCannotCarryTenantOrActor(t *testing.T) {
	tid := uuid.New()
	f := &fakeAML{}
	mux := amlTestMux(f, tenant.Context{TenantID: tid, Subject: uuid.NewString(), Role: string(auth.RoleCompliance), PrincipalType: string(auth.PrincipalStaff)})
	p := uuid.New()
	for _, body := range []string{
		fmt.Sprintf(`{"person_id":%q,"scope":"all_value_movement","reason_class":"manual","reason_code":"r_1","tenant_id":%q}`, p, uuid.New()),
		fmt.Sprintf(`{"person_id":%q,"scope":"all_value_movement","reason_class":"manual","reason_code":"r_1","actor_id":%q}`, p, uuid.New()),
		`{"person_id":"not-a-uuid","scope":"all_value_movement","reason_class":"manual","reason_code":"r_1"}`,
		`not json`,
		``,
	} {
		rec := amlDo(mux, "POST", "/t/"+tid.String()+"/holds", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: %d, want 400", body, rec.Code)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("an invalid body reached the service: %v", f.calls)
	}
	// Malformed ids and an out-of-range limit are 400 before the service.
	for _, path := range []string{"/t/" + tid.String() + "/cases/not-a-uuid", "/t/not-a-uuid/cases", "/t/" + tid.String() + "/cases?limit=0", "/t/" + tid.String() + "/cases?limit=101", "/t/" + tid.String() + "/cases?limit=x"} {
		if rec := amlDo(mux, "GET", path, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", path, rec.Code)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("an invalid path reached the service: %v", f.calls)
	}
}

// With no AML service every route is a 503 (no bypass); without an authenticated context it is 401.
func TestAMLRoutes_FailClosedWithoutServiceOrContext(t *testing.T) {
	tid := uuid.New()
	mux := amlTestMux(nil, tenant.Context{TenantID: tid, Subject: uuid.NewString()})
	// A nil interface (not a typed nil pointer).
	deps := Deps{}
	rec := httptest.NewRecorder()
	amlHandler(deps, "q", amlCaseQueue).ServeHTTP(rec, httptest.NewRequest("GET", "/t/"+tid.String()+"/cases", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("nil service: %d, want 503", rec.Code)
	}
	_ = mux
	// No tenant context at all.
	f := &fakeAML{}
	bare := http.NewServeMux()
	bare.Handle("GET /t/{tenantID}/cases", amlHandler(Deps{AML: f}, "q", amlCaseQueue))
	if rec := amlDo(bare, "GET", "/t/"+tid.String()+"/cases", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no context: %d, want 401", rec.Code)
	}
	// A malformed subject is unauthenticated, not a panic or a nil-actor call.
	bad := amlTestMux(f, tenant.Context{TenantID: tid, Subject: "not-a-uuid"})
	if rec := amlDo(bad, "GET", "/t/"+tid.String()+"/cases", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad subject: %d, want 401", rec.Code)
	}
	if len(f.calls) != 0 {
		t.Fatalf("service reached: %v", f.calls)
	}
}

// The closed error table: each sentinel has one status, one code and a fixed body; database text never appears.
func TestAMLErrorCode_Table(t *testing.T) {
	guard := &aml.GuardError{Code: "AM005", Message: "SECRET DATABASE TEXT person 123"}
	ap := &aml.GuardError{Code: "AP003", Message: "SECRET proof text"}
	for _, c := range []struct {
		err    error
		code   apierror.Code
		status int
		check  string
	}{
		{aml.ErrNotFound, apierror.CodeNotFound, 404, ""},
		{aml.ErrForbidden, apierror.CodeForbidden, 403, ""},
		{aml.ErrActorScope, apierror.CodeForbidden, 403, ""},
		{aml.ErrProofUnavailable, apierror.CodeUnavailable, 503, ""},
		{aml.ErrInvalidInput, apierror.CodeValidation, 400, ""},
		{aml.ErrNoteRefused, apierror.CodeValidation, 400, "note_refused"},
		{aml.ErrQuorum, apierror.CodeConflict, 409, "quorum_not_met"},
		{aml.ErrHoldRefused, apierror.CodeConflict, 409, "hold_refused"},
		{aml.ErrFourEyes, apierror.CodeConflict, 409, "four_eyes_refused"},
		{aml.ErrStale, apierror.CodeConflict, 409, "stale"},
		{aml.ErrIllegalTransition, apierror.CodeConflict, 409, "illegal_transition"},
		{guard, apierror.CodeConflict, 409, "guard_refused"},
		{ap, apierror.CodeForbidden, 403, ""},
		{errors.New("pq: SECRET connection string"), apierror.CodeInternal, 500, ""},
		{fmt.Errorf("wrapped: %w", aml.ErrStale), apierror.CodeConflict, 409, "stale"},
	} {
		code, msg, checks := amlErrorCode(c.err)
		if code != c.code || apierror.Status(code) != c.status {
			t.Errorf("%v: %s/%d, want %s/%d", c.err, code, apierror.Status(code), c.code, c.status)
		}
		if strings.Contains(msg, "SECRET") || strings.Contains(strings.Join(checks, ","), "SECRET") {
			t.Errorf("%v: leaked text %q %v", c.err, msg, checks)
		}
		if c.check == "" && len(checks) != 0 || c.check != "" && (len(checks) != 1 || checks[0] != c.check) {
			t.Errorf("%v: checks %v, want %q", c.err, checks, c.check)
		}
	}
}

// No AML wording in any error body that a non-officer could trigger, and no tipping-off vocabulary in the response set.
func TestAMLRoutes_ErrorBodiesCarryNoPersonOrCaseData(t *testing.T) {
	tid := uuid.New()
	for _, e := range []error{aml.ErrNotFound, aml.ErrForbidden, aml.ErrQuorum, aml.ErrHoldRefused, aml.ErrFourEyes, aml.ErrStale,
		aml.ErrIllegalTransition, aml.ErrNoteRefused, aml.ErrInvalidInput, errors.New("boom person 7")} {
		f := &fakeAML{err: e}
		mux := amlTestMux(f, tenant.Context{TenantID: tid, Subject: uuid.NewString(), Role: string(auth.RoleCompliance), PrincipalType: string(auth.PrincipalStaff)})
		rec := amlDo(mux, "POST", "/t/"+tid.String()+"/holds/"+uuid.NewString()+"/lift", `{"reason_code":"r_1"}`)
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		for k := range body {
			switch k {
			case "code", "message", "request_id", "check_codes":
			default:
				t.Errorf("unexpected error field %q", k)
			}
		}
		if strings.Contains(rec.Body.String(), "person") || strings.Contains(rec.Body.String(), "boom") {
			t.Errorf("%v: body leaks: %s", e, rec.Body)
		}
	}
}

// The step-up class of every AML route equals the signer backstop's class for the operation it signs: a route that is
// `none` while its operation is in actorproof.StepUpOperations would 500 at the backstop; a route that is critical while the
// operation is unregistered would prompt for nothing.
func TestAMLRoutes_StepUpClassMatchesSignerBackstop(t *testing.T) {
	ops := map[string]string{
		"POST /v1/admin/tenants/{tenantID}/aml/holds":                     aml.OpProofHoldPlace,
		"POST /v1/admin/tenants/{tenantID}/aml/holds/{holdID}/lift":       aml.OpProofHoldLift,
		"POST /v1/admin/tenants/{tenantID}/aml/cases":                     aml.OpProofCaseOpen,
		"POST /v1/admin/tenants/{tenantID}/aml/cases/{caseID}/transition": aml.OpProofCaseTransition,
		"POST /v1/admin/tenants/{tenantID}/aml/cases/{caseID}/notes":      aml.OpProofCaseNote,
		"POST /v1/admin/tenants/{tenantID}/aml/cases/{caseID}/sar-flag":   aml.OpProofCaseFlagSAR,
		"POST /v1/admin/tenants/{tenantID}/aml/cases/{caseID}/proposals":  aml.OpProofCasePropose,
		"POST /v1/admin/tenants/{tenantID}/aml/cases/{caseID}/approvals":  aml.OpProofCaseApprove,
		"GET /v1/admin/tenants/{tenantID}/aml/cases":                      aml.OpProofCaseRead,
		"GET /v1/admin/tenants/{tenantID}/aml/cases/{caseID}":             aml.OpProofCaseRead,
		"GET /v1/admin/tenants/{tenantID}/aml/persons/{personID}/holds":   aml.OpProofHoldRead,
	}
	n := 0
	for pat, class := range staffRouteGolden {
		if !strings.Contains(pat, "/aml/") {
			continue
		}
		n++
		op, ok := ops[pat]
		if !ok {
			t.Errorf("%s: AML route with no operation mapping in this test", pat)
			continue
		}
		reg, inRegistry := actorproof.StepUpOperations[op]
		switch class {
		case "none":
			if inRegistry {
				t.Errorf("%s: route class none but %s is step-up classified (%s)", pat, op, reg)
			}
		case "critical":
			if !inRegistry || reg != stepup.Critical {
				t.Errorf("%s: route class critical but %s is registered as %q (in registry %v)", pat, op, reg, inRegistry)
			}
		case "classifier:amlHoldPlaceClass", "classifier:amlTransitionClass":
			// State-dependent classes (a confirmed_sanctions hold; a move to closed_no_action): the operation alone cannot say, so it is
			// NOT in the signer registry (which is keyed by operation); the HTTP gate is the control, pinned by the classifier tests.
			if inRegistry {
				t.Errorf("%s: a classifier route's operation %s must not be in the operation-keyed registry", pat, op)
			}
		default:
			t.Errorf("%s: unexpected class %q", pat, class)
		}
	}
	for pat, want := range map[string]string{
		"POST /v1/admin/tenants/{tenantID}/aml/holds":                     "classifier:amlHoldPlaceClass",
		"POST /v1/admin/tenants/{tenantID}/aml/cases/{caseID}/transition": "classifier:amlTransitionClass",
	} {
		if staffRouteGolden[pat] != want {
			t.Errorf("%s: class %q, want %q", pat, staffRouteGolden[pat], want)
		}
	}
	if n != len(ops) || n != 11 {
		t.Errorf("AML golden routes %d, mapped %d, want 11", n, len(ops))
	}
	// Reads never step up.
	for pat, class := range staffRouteGolden {
		if strings.HasPrefix(pat, "GET ") && strings.Contains(pat, "/aml/") && class != "none" {
			t.Errorf("%s: a read is %s", pat, class)
		}
	}
}

// Every mounted AML route is documented in OpenAPI under the same path and method, names its static permission and the
// no-store header, and nothing under /aml/ is documented but unmounted (the UI-to-OpenAPI contract test in backoffice reads
// the same document).
func TestOpenAPI_AML_RoutesDocumented(t *testing.T) {
	src, err := os.ReadFile("aml_routes.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`mux\.Handle\("(GET|POST|PUT|DELETE) (/v1/[^"]*)"`)
	routes := re.FindAllStringSubmatch(string(src), -1)
	if len(routes) != 11 {
		t.Fatalf("expected 11 mounted AML operations, found %d (update the spec and this test together)", len(routes))
	}
	spec := launchSpec(t)
	mounted := map[string]bool{}
	for _, r := range routes {
		mounted[strings.ToLower(r[1])+" "+r[2]] = true
		block := openAPIPathBlock(t, spec, r[2])
		if !strings.Contains(block, "    "+strings.ToLower(r[1])+":\n") {
			t.Errorf("%s %s is mounted but not documented", r[1], r[2])
			continue
		}
		for _, want := range []string{"aml_", "bearerAuth", `"401"`, `"403"`, "no-store"} {
			if !strings.Contains(block, want) {
				t.Errorf("%s %s: OpenAPI entry lacks %q", r[1], r[2], want)
			}
		}
	}
	pathRe := regexp.MustCompile(`(?m)^  (/v1/admin/tenants/\{tenantID\}/aml/[^:\s]*):\n((?:    .*\n|\n)*)`)
	opRe := regexp.MustCompile(`(?m)^    (get|post|put|delete):\n`)
	documented := 0
	for _, m := range pathRe.FindAllStringSubmatch(spec, -1) {
		for _, op := range opRe.FindAllStringSubmatch(m[2], -1) {
			documented++
			if !mounted[op[1]+" "+m[1]] {
				t.Errorf("documented but not mounted: %s %s", op[1], m[1])
			}
		}
	}
	if documented != len(routes) {
		t.Errorf("documented %d AML operations, mounted %d", documented, len(routes))
	}
	// Every golden AML route is mounted by the file scanned above (the golden table and the mount list agree).
	var golden []string
	for pat := range staffRouteGolden {
		if strings.Contains(pat, "/aml/") {
			m, p, _ := strings.Cut(pat, " ")
			golden = append(golden, strings.ToLower(m)+" "+p)
		}
	}
	var mountedList []string
	for k := range mounted {
		mountedList = append(mountedList, k)
	}
	sort.Strings(golden)
	sort.Strings(mountedList)
	if strings.Join(golden, "|") != strings.Join(mountedList, "|") {
		t.Errorf("golden AML routes %v != mounted %v", golden, mountedList)
	}
}

// Static: no AML route file takes a tenant or actor from a body/query, signs a proof itself, opens a database session, or
// writes audit_log (the AML trail is the AML-scoped event tables, ADR 0116 s5.2).
func TestAMLRoutes_StaticSafetyPins(t *testing.T) {
	b, err := os.ReadFile("aml_routes.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	// strip comments so prose cannot satisfy or trip a pin
	code := regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(src, "")
	for _, bad := range []string{"deps.DB", "WithTenant", "WithPlatform", "audit.Record", "audit_log", "actorproof.", ".Attach(", ".Sign(", "URL.Query().Get(\"tenant", "FormValue"} {
		if strings.Contains(code, bad) {
			t.Errorf("aml_routes.go must not contain %q", bad)
		}
	}
	// No REQUEST struct can carry a tenant, actor or staff id.
	reqStructs := regexp.MustCompile(`(?s)type aml\w+Request struct \{(.*?)\n\}`).FindAllStringSubmatch(src, -1)
	if len(reqStructs) != 8 {
		t.Errorf("found %d request structs, want 8 (the scan must not be vacuous)", len(reqStructs))
	}
	for _, m := range reqStructs {
		if regexp.MustCompile(`json:"(tenant_id|actor\w*|staff\w*)"`).MatchString(m[1]) {
			t.Errorf("a request struct carries a tenant/actor field:\n%s", m[1])
		}
	}
	// Every mux.Handle goes through staffRoute with a literal class identifier.
	handles := regexp.MustCompile(`mux\.Handle\("[^"]+",\n\s+staffRoute\(deps, (stepUp\w+), auth\.RequirePermission\(auth\.PermAML\w+\)\(amlHandler\(`).FindAllStringSubmatch(src, -1)
	if len(handles) != 11 {
		t.Errorf("%d AML registrations go through staffRoute + RequirePermission(PermAML*) + amlHandler, want 11", len(handles))
	}
	// amlActorFrom is the only constructor of an aml.Actor.
	if n := strings.Count(code, "aml.Actor{"); n != 2 { // the zero-value return and the real construction
		t.Errorf("aml.Actor literal appears %d times, want 2", n)
	}
}

// Each route is guarded by exactly the permission the ADR 0116 s6 table names for it. Today the four permissions are held by the
// same two roles, so a swapped permission would be invisible to behaviour tests; this pin keeps the routes honest if the
// holder sets ever diverge (e.g. a read-only analyst role).
func TestAMLRoutes_EachRouteHasItsPermission(t *testing.T) {
	b, err := os.ReadFile("aml_routes.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`mux\.Handle\("([A-Z]+ /v1/[^"]+)",\n\s+staffRoute\(deps, \w+, auth\.RequirePermission\(auth\.(PermAML\w+)\)`)
	got := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		got[m[1]] = m[2]
	}
	const base = " /v1/admin/tenants/{tenantID}/aml"
	want := map[string]string{
		"GET" + base + "/cases":                      "PermAMLCaseRead",
		"GET" + base + "/cases/{caseID}":             "PermAMLCaseRead",
		"GET" + base + "/persons/{personID}/holds":   "PermAMLCaseRead",
		"POST" + base + "/holds":                     "PermAMLHoldManage",
		"POST" + base + "/holds/{holdID}/lift":       "PermAMLHoldManage",
		"POST" + base + "/cases":                     "PermAMLCaseWork",
		"POST" + base + "/cases/{caseID}/transition": "PermAMLCaseWork",
		"POST" + base + "/cases/{caseID}/notes":      "PermAMLCaseWork",
		"POST" + base + "/cases/{caseID}/sar-flag":   "PermAMLCaseWork",
		"POST" + base + "/cases/{caseID}/proposals":  "PermAMLCaseWork",
		"POST" + base + "/cases/{caseID}/approvals":  "PermAMLCaseCloseApprove",
	}
	if len(got) != len(want) {
		t.Fatalf("found %d guarded registrations, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s guarded by %q, want %q", k, got[k], v)
		}
	}
}

// SR7-3 (HARD PRE-ROUTE CONDITION, pinned): aml.Service.SetScreeningPolicy is NOT exposed on any route, command or other
// production caller yet. Policy authoring can LOOSEN screening (remove a list, lengthen the grace period, move effective_from
// earlier) and has no four-eyes step at service level; any route that exposes it MUST be step-up CRITICAL and MUST add a
// four-eyes (propose / approve) path for loosening BEFORE it is wired. This test fails the moment a production file outside
// internal/aml references the method or the AMLOfficerAPI interface gains it; whoever wires the route must replace this pin
// with the route's own class pin and the four-eyes test (docs/plans/a7-aml-record.md SR7-3).
func TestSetScreeningPolicy_IsNotRoutedYet_SR7_3(t *testing.T) {
	if _, ok := reflect.TypeOf((*AMLOfficerAPI)(nil)).Elem().MethodByName("SetScreeningPolicy"); ok {
		t.Fatal("SR7-3: AMLOfficerAPI must not expose SetScreeningPolicy until the route is step-up critical with four-eyes for loosening")
	}
	var offenders []string
	err := filepath.WalkDir("../..", func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", "aml", "backoffice", ".claude":
				if d.Name() != "aml" || strings.HasSuffix(filepath.ToSlash(p), "internal/aml") {
					return filepath.SkipDir
				}
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
		if strings.Contains(string(b), "SetScreeningPolicy") {
			offenders = append(offenders, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) != 0 {
		t.Fatalf("SR7-3: SetScreeningPolicy is referenced outside internal/aml (a route needs step-up critical + four-eyes for loosening first): %v", offenders)
	}
}
