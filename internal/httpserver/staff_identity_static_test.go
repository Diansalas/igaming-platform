package httpserver

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ADR 0114 S0 (H1) static pin. DB-free.
//
// Every JSON field in this package whose name says "a staff/actor/approver/principal id" must be a
// staffRef (the only type the redaction helper can fill), or be listed below with the reason it is
// safe. A new `*_by` / `*_staff_id` / `approver*` field that is neither fails the build, so a new
// tenant-readable DTO cannot ship an unredacted platform staff id by omission.

var staffIDShapedTag = regexp.MustCompile(`(_by$|_by_|staff_id|actor_id|^actor$|approver|principal_id|grantee|person_id|reviewer|requester|initiator|executed_by|decided_by|superseded_by)`)

// not an identifier: scope/role/type/time/hash/name/count markers.
var staffIDNotAnID = regexp.MustCompile(`(_scope|_role|_type|_at|_hash|_name|_count|_class|_input)$`)

// staffIDFieldAllowlist: "StructType.FieldName" -> why this field needs no staffRef. A reason is
// mandatory. Stale entries (no such field any more) fail TestStaffIdentity_StaticPin_AllowlistNotStale.
var staffIDFieldAllowlist = map[string]string{
	// --- request bodies (input from the caller, never a response) ---
	"createCapabilityGrantRequestBody.GranteeStaffID": "request body: caller-supplied input, not a response field",

	// --- already redacted inline by ADR 0112 slice 6 (launchCall.dto / history / detail); covered by
	// TestLaunchHTTP_TenantView_HidesPlatformStaffIdentities_AndCoarsensReadiness ---
	"launchApprovalDTO.DecidedBy":     "ADR 0112 inline redaction (launchCall.tenantView) - launch precedent",
	"launchRequestDTO.RequestedBy":    "ADR 0112 launchCall.dto redaction - launch precedent",
	"launchTransitionDTO.ExecutedBy":  "ADR 0112 inline redaction in newLaunchHistoryHandler",
	"launchTransitionDTO.ApproverIDs": "ADR 0112 inline redaction in newLaunchHistoryHandler",

	// --- ADR 0104: the tenant-visible platform-actions audit identifies the platform actor BY DESIGN
	// (section 5.2 "actor.staff_id: always shown"); recorded as a tension in ADR 0114 section 9 ---
	"platformActionAuditActor.StaffID":           "ADR 0104 section 5.2: platform actor deliberately identified on this route",
	"platformActionAuditChainEntry.ActorStaffID": "ADR 0104 section 5.5 approval chain: deliberately identified",
	"platformActionAuditEntry.Actor":             "nested platformActionAuditActor (ADR 0104 section 5.2)",

	// --- routes that only PLATFORM roles can reach (permission set verified in internal/auth/permission.go) ---
	"providerCredentialApprovalDTO.ApproverPrincipalID":   "route requires provider_credential:request|approve, held by platform_admin only",
	"providerCredentialRequestDTO.RequestedByPrincipalID": "same: platform_admin only",
	"providerCredentialRequestDTO.AppliedByPrincipalID":   "same: platform_admin only",
	"alertRouteDTO.SupersededBy":                          "id of the superseding routing ROW, not a staff id; alert:route_manage is platform_admin only",
	"jurisdictionRestrictionResponse.CreatedByActorID":    "sportsbook_jurisdiction_restriction:* is platform_admin only",

	// --- routes behind RequireTenantScope or tenant-only roles: every actor is staff of the caller's own tenant ---
	"changeRequestResponse.RequestedByPrincipalID": "bonus change requests: RequireTenantScope - actors are the caller's own tenant staff",
	"changeRequestResponse.AppliedByPrincipalID":   "same",
	"kycCaseResponse.ReviewedBy":                   "verification:review is compliance only (tenant staff of the caller's own tenant)",
	"exposureLimitResponse.CreatedByActorID":       "sportsbook_exposure_limit:manage is risk_manager only (tenant staff); platform_admin does not hold it",

	// --- staff provisioning: the person link of a TENANT staff user, returned to its creator / linker ---
	"createStaffRequest.PersonID":     "request body (input)",
	"staffResponse.PersonID":          "the person link of the tenant staff user just created in the caller's own tenant (PermStaffManage); not a platform staff id",
	"linkStaffPersonRequest.PersonID": "request body (input)",
}

// staffIDOffender names a field that is staff-id shaped but neither a staffRef nor allowlisted.
type staffIDOffender struct{ Key, Tag, Type string }

// scanStaffIDFields parses Go sources (name -> source) and returns every staff-id-shaped JSON field
// whose type is not staffRef / *staffRef, plus every staffRef composite literal with content
// (only the helper file may build a visible ref).
func scanStaffIDFields(t *testing.T, srcs map[string]string) (fields []staffIDOffender, forged []string, seen map[string]bool) {
	t.Helper()
	seen = map[string]bool{}
	fset := token.NewFileSet()
	names := make([]string, 0, len(srcs))
	for n := range srcs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		f, err := parser.ParseFile(fset, name, srcs[name], 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.TypeSpec:
				st, ok := x.Type.(*ast.StructType)
				if !ok {
					return true
				}
				for _, fld := range st.Fields.List {
					if fld.Tag == nil || len(fld.Names) == 0 {
						continue
					}
					raw, _ := strconv.Unquote(fld.Tag.Value)
					tag := reflect.StructTag(raw).Get("json")
					jsonName := strings.Split(tag, ",")[0]
					if jsonName == "" || jsonName == "-" {
						continue
					}
					if !staffIDShapedTag.MatchString(jsonName) || staffIDNotAnID.MatchString(jsonName) {
						continue
					}
					typ := exprString(fld.Type)
					key := x.Name.Name + "." + fld.Names[0].Name
					seen[key] = true
					if typ == "staffRef" || typ == "*staffRef" {
						continue
					}
					fields = append(fields, staffIDOffender{Key: key, Tag: jsonName, Type: typ})
				}
			case *ast.CompositeLit:
				if id, ok := x.Type.(*ast.Ident); ok && id.Name == "staffRef" && len(x.Elts) > 0 && !strings.HasSuffix(name, "staff_identity_redaction.go") {
					forged = append(forged, fset.Position(x.Pos()).String())
				}
			}
			return true
		})
	}
	return fields, forged, seen
}

func exprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.StarExpr:
		return "*" + exprString(x.X)
	case *ast.ArrayType:
		return "[]" + exprString(x.Elt)
	case *ast.SelectorExpr:
		return exprString(x.X) + "." + x.Sel.Name
	}
	return "?"
}

func readPackageSources(t *testing.T) map[string]string {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out[p] = string(b)
	}
	if len(out) < 20 {
		t.Fatalf("static pin read only %d source files: wrong working directory?", len(out))
	}
	return out
}

func TestStaffIdentity_StaticPin_EveryStaffIDFieldGoesThroughTheHelper(t *testing.T) {
	fields, forged, _ := scanStaffIDFields(t, readPackageSources(t))
	for _, f := range fields {
		if reason, ok := staffIDFieldAllowlist[f.Key]; ok {
			if strings.TrimSpace(reason) == "" {
				t.Errorf("%s is allowlisted without a reason", f.Key)
			}
			continue
		}
		t.Errorf("response field %s (json %q, type %s) looks like a staff/actor id but is not a staffRef and is not allowlisted: "+
			"build it with staffViewer.ref/optRef (staff_identity_redaction.go) or add a justified entry (ADR 0114 H1)", f.Key, f.Tag, f.Type)
	}
	for _, pos := range forged {
		t.Errorf("staffRef built outside the helper at %s: only staffViewer.ref/optRef may produce a visible staffRef", pos)
	}
}

func TestStaffIdentity_StaticPin_AllowlistNotStale(t *testing.T) {
	_, _, seen := scanStaffIDFields(t, readPackageSources(t))
	for key := range staffIDFieldAllowlist {
		if !seen[key] {
			t.Errorf("allowlist entry %s matches no staff-id-shaped field any more: remove it", key)
		}
	}
}

// Planted-offender negative test: the scanner itself must flag a new unredacted field, an
// unredacted slice of ids, and a hand-built visible staffRef.
func TestStaffIdentity_StaticPin_PlantedOffendersAreCaught(t *testing.T) {
	const planted = `package httpserver
type newThingDTO struct {
	ID         string   ` + "`json:\"id\"`" + `
	ClosedBy   string   ` + "`json:\"closed_by\"`" + `
	Approvers  []string ` + "`json:\"approver_ids\"`" + `
	Owner      *string  ` + "`json:\"grantee_staff_id,omitempty\"`" + `
	ClosedByOK staffRef ` + "`json:\"closed_by_ok\"`" + `
	Scope      string   ` + "`json:\"closed_by_scope\"`" + `
}
func f() any { return staffRef{id: "x"} }
`
	fields, forged, _ := scanStaffIDFields(t, map[string]string{"planted.go": planted})
	got := map[string]bool{}
	for _, f := range fields {
		got[f.Key] = true
	}
	for _, want := range []string{"newThingDTO.ClosedBy", "newThingDTO.Approvers", "newThingDTO.Owner"} {
		if !got[want] {
			t.Errorf("planted offender %s was not flagged (flagged: %v)", want, got)
		}
	}
	if got["newThingDTO.ClosedByOK"] || got["newThingDTO.Scope"] || got["newThingDTO.ID"] {
		t.Errorf("scanner flagged a staffRef, a scope marker or a plain id: %v", got)
	}
	if len(forged) != 1 {
		t.Errorf("planted hand-built staffRef literal not flagged: %v", forged)
	}
	// The same planted file, once allowlisted-by-key, would be accepted: the allowlist is the only escape hatch.
	if _, ok := staffIDFieldAllowlist["newThingDTO.ClosedBy"]; ok {
		t.Fatal("planted offender must not be in the real allowlist")
	}
}
