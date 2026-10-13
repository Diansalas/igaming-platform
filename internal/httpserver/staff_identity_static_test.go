package httpserver

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
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
// Every wire-shaped struct field under internal/ whose effective JSON key says "a staff/actor/
// approver/principal/operator id" must be a staffRef (the only type the redaction helper fills), or
// be listed below with the reason it is safe. The scan covers:
//   - every ast.StructType anywhere (named, anonymous, nested, inside functions), in internal/** ;
//   - the effective key: the json tag name, or - when the tag has no name or there is no tag - the
//     Go field name in snake case (encoding/json's default);
//   - string-keyed map literals passed (at any depth) to writeJSON;
//   - assignments to a `.id` selector and `staffViewer{...}` literals outside the helper file.
// Known limit: a map built in a variable and passed to writeJSON later is not seen; response shapes
// must be struct types (or the key is caught here by name).

var staffIDShapedKey = regexp.MustCompile(`(_by$|_by_|staff_id|actor_id|^actor$|^actor_|^staff$|^staff_|approver|principal_id|grantee|person_id|reviewer|requester|initiator|executed_by|decided_by|superseded_by|operator_id|admin_id|user_id)`)

// not an identifier: scope/role/type/time/hash/count markers; "_name" is exempt ONLY for the display
// names below (a tag like `approver_name` holding an id would otherwise hide).
var staffIDNotAnID = regexp.MustCompile(`(_scope|_role|_type|_at|_hash|_count|_class|_input)$`)
var staffIDDisplayNameKeys = map[string]bool{"actor_display_name": true, "display_name": true, "responsible_operator_name": true}

const helperFile = "staff_identity_redaction.go"

var snakeRe = regexp.MustCompile(`([a-z0-9])([A-Z])`)

func snake(s string) string { return strings.ToLower(snakeRe.ReplaceAllString(s, "${1}_${2}")) }

// staffIDFieldAllowlist: "StructType.FieldName" (anonymous: "anon@file.FieldName"; map key:
// "map@file.key") -> why this field needs no staffRef. A reason is mandatory. Stale entries fail
// TestStaffIdentity_StaticPin_AllowlistNotStale.
var staffIDFieldAllowlist = map[string]string{
	// --- ADR 0119 S3 staff MFA (none is a staff identity of ANOTHER principal) ---
	"Deps.StaffMFA":                "dependency wiring (the staff MFA subsystem), never marshalled to a response",
	"enrolConfirmRequest.FactorID": "request body: the caller's own pending factor id (ADR 0119 S3), caller-supplied input",
	"enrolStartResponse.FactorID":  "the caller's own new pending factor id, returned only to the staff member enrolling it (ADR 0119 S3)",
	"enrolStartWire.FactorID":      "wire form of enrolStartResponse.FactorID: the caller's own pending factor id (ADR 0119 S3)",

	// --- request bodies (input from the caller, never a response) ---
	"createCapabilityGrantRequestBody.GranteeStaffID": "request body: caller-supplied input, not a response field",

	// --- already redacted inline by ADR 0112 slice 6 (launchCall.dto / history / detail); covered by
	// TestLaunchHTTP_TenantView_HidesPlatformStaffIdentities_AndCoarsensReadiness ---
	"launchApprovalDTO.DecidedBy":     "ADR 0112 inline redaction (launchCall.tenantView) - launch precedent",
	"launchRequestDTO.RequestedBy":    "ADR 0112 launchCall.dto redaction - launch precedent",
	"launchTransitionDTO.ExecutedBy":  "ADR 0112 inline redaction in newLaunchHistoryHandler",
	"launchTransitionDTO.ApproverIDs": "ADR 0112 inline redaction in newLaunchHistoryHandler",

	// --- ADR 0104 platform-actions audit: the actor is identified ONLY for payments_kill_switch.* actions
	// (identifiedActionPrefixes, an explicit allow-list; owner decision D-0104-1 is open), scope-only for
	// every other action; covered by TestStaffIdentityHTTP_PlatformActionsAudit_* ---
	"platformActionAuditActor.StaffID":           "scope-only unless actorIdentifiedToTenant(action) (kill-switch allow-list)",
	"platformActionAuditChainEntry.ActorStaffID": "same allow-list as the entry actor",
	"platformActionAuditEntry.Actor":             "nested platformActionAuditActor (scope-only unless allow-listed action)",

	// --- routes that only PLATFORM roles can reach (verified against internal/auth/permission.go: the
	// permissions below appear only in the RolePlatformAdmin set) ---
	"providerCredentialApprovalDTO.ApproverPrincipalID":   "provider_credential:request|approve are held by platform_admin only",
	"providerCredentialRequestDTO.RequestedByPrincipalID": "same: platform_admin only",
	"providerCredentialRequestDTO.AppliedByPrincipalID":   "same: platform_admin only",
	"alertRouteDTO.SupersededBy":                          "id of the superseding routing ROW, not a staff id; alert:route_manage is platform_admin only",
	"jurisdictionRestrictionResponse.CreatedByActorID":    "sportsbook_jurisdiction_restriction:* are platform_admin only",

	// --- routes behind RequireTenantScope (platform tokens are refused) and tenant-only roles: every actor is staff of the caller's own tenant ---
	"changeRequestResponse.RequestedByPrincipalID": "bonus change requests: RequireTenantScope - actors are the caller's own tenant staff",
	"changeRequestResponse.AppliedByPrincipalID":   "same",
	"kycCaseResponse.ReviewedBy":                   "kyc routes are RequireTenantScope; verification:review is compliance only",
	"exposureLimitResponse.CreatedByActorID":       "RequireTenantScope; sportsbook_exposure_limit:manage is risk_manager only, platform_admin holds neither manage nor read",

	// --- staff provisioning: the person link of a TENANT staff user, returned to its creator / linker ---
	"createStaffRequest.PersonID":     "request body (input)",
	"staffResponse.PersonID":          "the person link of the tenant staff user just created in the caller's own tenant (PermStaffManage); not a platform staff id",
	"linkStaffPersonRequest.PersonID": "request body (input)",
}

// finding is one violation of the pin.
type finding struct{ Kind, Key, Detail string }

// scanResult is what one scan pass produced.
type scanResult struct {
	Findings []finding
	Seen     map[string]bool // every staff-id-shaped allowlistable key encountered (for the stale check)
}

func isStaffRefType(t string) bool { return t == "staffRef" || t == "*staffRef" }

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
	case *ast.MapType:
		return "map[" + exprString(x.Key) + "]" + exprString(x.Value)
	case *ast.InterfaceType:
		return "interface{}"
	}
	return "?"
}

func keyIsStaffID(key string) bool {
	if key == "" || key == "-" || staffIDDisplayNameKeys[key] {
		return false
	}
	if staffIDNotAnID.MatchString(key) {
		return false
	}
	return staffIDShapedKey.MatchString(key)
}

// scanStaffIDSources scans Go sources (path -> source). wireOnlyOutsideHTTP: for files not in the
// httpserver package every struct is checked only when at least one of its fields has a json tag
// (a domain struct without any tag is never marshalled by encoding/json).
func scanStaffIDSources(t *testing.T, srcs map[string]string) scanResult {
	t.Helper()
	res := scanResult{Seen: map[string]bool{}}
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
		inHTTP := f.Name.Name == "httpserver"
		isHelper := strings.HasSuffix(name, helperFile)
		base := filepath.Base(name)
		structName := map[*ast.StructType]string{}
		ast.Inspect(f, func(n ast.Node) bool {
			if ts, ok := n.(*ast.TypeSpec); ok {
				if st, ok := ts.Type.(*ast.StructType); ok {
					structName[st] = ts.Name.Name
				}
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.StructType:
				owner, named := structName[x]
				if !named {
					owner = "anon@" + base
				}
				anyTag := false
				for _, fld := range x.Fields.List {
					if fld.Tag != nil {
						anyTag = true
					}
				}
				if !inHTTP && !anyTag {
					return true
				}
				for _, fld := range x.Fields.List {
					jsonName := ""
					if fld.Tag != nil {
						raw, _ := strconv.Unquote(fld.Tag.Value)
						jsonName = strings.Split(reflect.StructTag(raw).Get("json"), ",")[0]
					}
					fieldNames := []string{}
					for _, id := range fld.Names {
						fieldNames = append(fieldNames, id.Name)
					}
					if len(fieldNames) == 0 { // embedded
						continue
					}
					for _, fn := range fieldNames {
						if len(fn) == 0 || fn[0] < 'A' || fn[0] > 'Z' { // unexported: never marshalled
							continue
						}
						key := jsonName
						if key == "" {
							key = snake(fn)
						}
						if !keyIsStaffID(key) {
							continue
						}
						k := owner + "." + fn
						res.Seen[k] = true
						typ := exprString(fld.Type)
						if isStaffRefType(typ) {
							continue
						}
						res.Findings = append(res.Findings, finding{"field", k, "json key " + strconv.Quote(key) + ", type " + typ})
					}
				}
			case *ast.CompositeLit:
				if id, ok := x.Type.(*ast.Ident); ok && !isHelper {
					if id.Name == "staffRef" && len(x.Elts) > 0 {
						res.Findings = append(res.Findings, finding{"forged-staffRef", fset.Position(x.Pos()).String(), "staffRef literal with content outside the helper"})
					}
					if id.Name == "staffViewer" {
						res.Findings = append(res.Findings, finding{"viewer-literal", fset.Position(x.Pos()).String(), "staffViewer literal outside the helper: use staffViewerOf"})
					}
				}
			case *ast.AssignStmt:
				if inHTTP && !isHelper {
					for _, l := range x.Lhs {
						if sel, ok := l.(*ast.SelectorExpr); ok && sel.Sel.Name == "id" {
							res.Findings = append(res.Findings, finding{"id-assign", fset.Position(x.Pos()).String(), "assignment to .id outside the helper"})
						}
					}
				}
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "writeJSON" {
					for _, a := range x.Args {
						ast.Inspect(a, func(m ast.Node) bool {
							cl, ok := m.(*ast.CompositeLit)
							if !ok {
								return true
							}
							_, isMap := cl.Type.(*ast.MapType)
							if cl.Type == nil && len(cl.Elts) > 0 { // elided inner type: {"k": v} inside a []map / map literal
								_, isMap = cl.Elts[0].(*ast.KeyValueExpr)
							}
							if !isMap {
								return true
							}
							for _, el := range cl.Elts {
								kv, ok := el.(*ast.KeyValueExpr)
								if !ok {
									continue
								}
								if bl, ok := kv.Key.(*ast.BasicLit); ok && bl.Kind == token.STRING {
									key, _ := strconv.Unquote(bl.Value)
									if keyIsStaffID(key) {
										k := "map@" + base + "." + key
										res.Seen[k] = true
										res.Findings = append(res.Findings, finding{"map-key", k, "staff-id-shaped key in a map passed to writeJSON"})
									}
								}
							}
							return true
						})
					}
				}
			}
			return true
		})
	}
	return res
}

// readInternalSources reads every non-test .go file under internal/ (this package is internal/httpserver).
func readInternalSources(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir("..", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[p] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 200 {
		t.Fatalf("static pin read only %d source files under internal/: wrong working directory?", len(out))
	}
	return out
}

func TestStaffIdentity_StaticPin_EveryStaffIDFieldGoesThroughTheHelper(t *testing.T) {
	res := scanStaffIDSources(t, readInternalSources(t))
	for _, f := range res.Findings {
		if f.Kind == "field" || f.Kind == "map-key" {
			if reason, ok := staffIDFieldAllowlist[f.Key]; ok {
				if strings.TrimSpace(reason) == "" {
					t.Errorf("%s is allowlisted without a reason", f.Key)
				}
				continue
			}
		}
		t.Errorf("%s %s (%s): build staff-id fields with staffViewer.ref/optRef (staff_identity_redaction.go) or add a justified allowlist entry (ADR 0114 H1)", f.Kind, f.Key, f.Detail)
	}
}

func TestStaffIdentity_StaticPin_AllowlistNotStale(t *testing.T) {
	res := scanStaffIDSources(t, readInternalSources(t))
	for key := range staffIDFieldAllowlist {
		if !res.Seen[key] {
			t.Errorf("allowlist entry %s matches no staff-id-shaped field any more: remove it", key)
		}
	}
}

// Planted-offender negative tests: each evasion the security review executed must be flagged.
func TestStaffIdentity_StaticPin_PlantedOffendersAreCaught(t *testing.T) {
	const hdr = "package httpserver\n"
	cases := []struct {
		name, src string
		kind      string
		key       string
	}{
		{"plain *_by string", "type d struct { ClosedBy string `json:\"closed_by\"` }", "field", "d.ClosedBy"},
		{"slice of ids", "type d struct { Approvers []string `json:\"approver_ids\"` }", "field", "d.Approvers"},
		{"pointer id", "type d struct { Owner *string `json:\"grantee_staff_id,omitempty\"` }", "field", "d.Owner"},
		{"no tag at all (default key)", "type d struct { ApprovedBy string }", "field", "d.ApprovedBy"},
		{"tag without a name", "type d struct { ApprovedBy string `json:\",omitempty\"` }", "field", "d.ApprovedBy"},
		{"_name tag holding an id", "type d struct { X string `json:\"approver_name\"` }", "field", "d.X"},
		{"operator_id", "type d struct { X string `json:\"operator_id\"` }", "field", "d.X"},
		{"admin_id", "type d struct { X string `json:\"admin_id\"` }", "field", "d.X"},
		{"user_id", "type d struct { X string `json:\"user_id\"` }", "field", "d.X"},
		{"bare staff", "type d struct { X string `json:\"staff\"` }", "field", "d.X"},
		{"actor_staff", "type d struct { X string `json:\"actor_staff\"` }", "field", "d.X"},
		{"anonymous struct in a func", "func f() any { return struct { ClosedBy string `json:\"closed_by\"` }{} }", "field", "anon@planted.go.ClosedBy"},
		{"nested slice of anonymous structs", "type d struct { Items []struct { RequestedBy string `json:\"requested_by\"` } `json:\"items\"` }", "field", "anon@planted.go.RequestedBy"},
		{"map literal passed to writeJSON", "func h(w any) { writeJSON(w, 200, map[string]any{\"approved_by\": 1}) }", "map-key", "map@planted.go.approved_by"},
		{"map literal nested in a response", "func h(w any) { writeJSON(w, 200, map[string]any{\"items\": []map[string]any{{\"actor_id\": 1}}}) }", "map-key", "map@planted.go.actor_id"},
		{"staffRef built empty then .id assigned", "func f() { var r staffRef; r.id = \"x\"; _ = r }", "id-assign", ""},
		{"staffViewer literal outside the helper", "func f() { _ = staffViewer{platform: true} }", "viewer-literal", ""},
		{"visible staffRef literal", "func f() any { return staffRef{id: \"x\"} }", "forged-staffRef", ""},
	}
	for _, c := range cases {
		res := scanStaffIDSources(t, map[string]string{"planted.go": hdr + c.src})
		hit := false
		for _, f := range res.Findings {
			if f.Kind == c.kind && (c.key == "" || f.Key == c.key) {
				hit = true
			}
		}
		if !hit {
			t.Errorf("planted evasion %q was not flagged as %s %s (findings: %+v)", c.name, c.kind, c.key, res.Findings)
		}
		if _, ok := staffIDFieldAllowlist[c.key]; ok && c.key != "" {
			t.Errorf("planted key %s must not be in the real allowlist", c.key)
		}
	}
	// and clean shapes pass: staffRef fields, scope markers, display names, plain ids, helper file.
	clean := hdr + "type d struct { ID string `json:\"id\"`; By staffRef `json:\"closed_by\"`; P *staffRef `json:\"approved_by,omitempty\"`; S string `json:\"closed_by_scope\"`; N *string `json:\"actor_display_name\"` }"
	if r := scanStaffIDSources(t, map[string]string{"clean.go": clean}); len(r.Findings) != 0 {
		t.Errorf("clean shapes flagged: %+v", r.Findings)
	}
	if r := scanStaffIDSources(t, map[string]string{helperFile: hdr + "func f() any { return staffRef{id: \"x\"} }; func g() { _ = staffViewer{platform: true}; var r staffRef; r.id = \"y\" }"}); len(r.Findings) != 0 {
		t.Errorf("the helper file itself must be exempt: %+v", r.Findings)
	}
}
