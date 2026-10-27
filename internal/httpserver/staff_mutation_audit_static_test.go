package httpserver

// U-7 (provider-independent closure): every MUTATING staff route of the ADR 0119
// route-registration freeze (POST/PUT/PATCH/DELETE registered through
// staffRoute) must reach an audit write - audit.Record, the platform's single
// append-only audit writer (internal/audit) - or be in the exact, reviewed
// staffMutationAuditAllowList with a reason.
//
// How reachability is established (static, type-aware, bounded):
//
//   - The module's packages are type-checked from source (go/types); every
//     dependency outside the module is imported from compiler export data
//     located with `go list -deps -export`. No third-party analysis library.
//   - Call graph: a function's callees are every module function or method it
//     references inside its body (called OR passed as a value, which covers
//     http.HandlerFunc(h.handleX) and transaction callbacks), with function
//     literals attributed to their enclosing declaration. A call through an
//     interface method declared in this module resolves to every concrete
//     module type implementing that interface (class-hierarchy analysis).
//     Interfaces declared outside the module (http.Handler, pgx.Tx, io.Writer,
//     error, ...) are NOT expanded, so the graph cannot route through "every
//     ServeHTTP in the module".
//   - Roots: the module functions referenced by the route's inner handler
//     expression (the third staffRoute argument), following local variables to
//     their defining expression - EXCLUDING the auth middleware package
//     (RequirePermission, RequireTenantScope, ...) so a gate that audits a
//     denial can never count as the route's own audit write.
//   - Non-counting audit sinks: staffAuditSinksNotCounted are functions that
//     write an audit row which is NOT the mutation's record (read audits); the
//     search never traverses them.
//   - The search is bounded by staffMutationAuditMaxDepth calls.
//
// What this does NOT prove (disclosed, see docs/plans/u67-record.md): that the
// audit write is on the success path of every branch (the analysis is
// path-insensitive), or that the entry carries the right action/target. The
// companion runtime test (staff_mutation_audit_integration_test.go) proves an
// actual audit row with actor, tenant, entity and reason for one representative
// handler per route family.

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

const (
	staffAuditModulePath       = "github.com/Diansalas/igaming-platform"
	staffAuditRecordPkg        = staffAuditModulePath + "/internal/audit"
	staffAuditRecordFunc       = "Record"
	staffMutationAuditMaxDepth = 12
)

// staffMutationAuditAllowList is the exact, reviewed set of mutating staff
// routes that do NOT reach audit.Record, each with the reason that is
// acceptable. A route here that DOES reach audit.Record is a stale entry and
// fails the test; a mutating route that neither reaches it nor is listed here
// fails the test.
var staffMutationAuditAllowList = map[string]string{
	"POST /v1/admin/tenants/{tenantID}/screening/persons/{personID}/requests/replace":                "lic23 (migration 0181): audited by the SECURITY DEFINER function aml_screening_request_replace, which keeps the officer, reason code and the replacement id on the cancelled aml_screening_requests row when a request is replaced, and in EVERY case the officer, officer Person and reason code on the new repl: request row itself (requested_by, requested_by_person_id, request_reason_code; lic26 F-3), under the append-only state machine, under officer quorum and a signed critical-class actor proof; deliberately NOT audit_log, which is tenant-readable (no tipping-off; internal/aml/screening_trigger_service.go)",
	"POST /v1/admin/tenants/{tenantID}/screening/policy":                                             "lic29 (migration 0189): audited by the SECURITY DEFINER function aml_screening_policy_author, which appends an immutable aml_screening_policies version row carrying the actor, reason code and legal review reference under a signed critical-class actor proof and refuses any version less strict than the latest; deliberately NOT audit_log, which is tenant-readable (no tipping-off; internal/aml/screening_service.go)",
	"POST /v1/admin/tenants/{tenantID}/screening/trigger-policy":                                     "lic18 (migration 0181): audited by the SECURITY DEFINER function aml_screening_trigger_policy_set, which appends an immutable aml_screening_trigger_policies version row carrying the actor, reason code and legal review reference under a signed critical-class actor proof; deliberately NOT audit_log, which is tenant-readable (no tipping-off; internal/aml/screening_trigger_service.go)",
	"POST /v1/admin/tenants/{tenantID}/screening/persons/{personID}/requests/{requestID}/resolution": "lic18 (migration 0181): audited by the SECURITY DEFINER function aml_screening_record (called under the request's idempotency key), which appends an immutable aml_screenings row carrying the officer actor, the request binding and the decision, plus the hold row where the decision holds; deliberately NOT audit_log, which is tenant-readable (no tipping-off; internal/aml/screening_trigger_service.go)",
	"POST /v1/admin/tenants/{tenantID}/aml/holds":                                                    "AML (ADR 0116 s5.2): audited by the SECURITY DEFINER function aml_hold_place, which writes an append-only aml_holds row carrying the actor (o_actor, person, scope) and reason code; deliberately NOT audit_log, which is tenant-readable (no tipping-off; internal/aml/holds.go)",
	"POST /v1/admin/tenants/{tenantID}/aml/holds/{holdID}/lift":                                      "AML (ADR 0116 s5.2): audited by the SECURITY DEFINER function aml_hold_lift, which writes an append-only aml_hold_lifts row carrying the actor (o_actor, person, scope) and reason code; deliberately NOT audit_log, which is tenant-readable (no tipping-off; internal/aml/holds.go)",
	"POST /v1/admin/tenants/{tenantID}/aml/cases":                                                    "AML (ADR 0116 s5.2): audited by the SECURITY DEFINER function aml_case_open, which appends to aml_case_events (actor, sequence, reason/content hash) under the officer's acting session; deliberately NOT audit_log, which is tenant-readable (no tipping-off; internal/aml/cases.go)",
	"POST /v1/admin/tenants/{tenantID}/aml/cases/{caseID}/transition":                                "AML (ADR 0116 s5.2): audited by the SECURITY DEFINER function aml_case_transition, which appends to aml_case_events (actor, sequence, reason/content hash) under the officer's acting session; deliberately NOT audit_log, which is tenant-readable (no tipping-off; internal/aml/cases.go)",
	"POST /v1/admin/tenants/{tenantID}/aml/cases/{caseID}/notes":                                     "AML (ADR 0116 s5.2): audited by the SECURITY DEFINER function aml_case_add_note, which appends to aml_case_events (actor, sequence, reason/content hash) under the officer's acting session; deliberately NOT audit_log, which is tenant-readable (no tipping-off; internal/aml/cases.go)",
	"POST /v1/admin/tenants/{tenantID}/aml/cases/{caseID}/sar-flag":                                  "AML (ADR 0116 s5.2): audited by the SECURITY DEFINER function aml_case_flag_sar, which appends to aml_case_events (actor, sequence, reason/content hash) under the officer's acting session; deliberately NOT audit_log, which is tenant-readable (no tipping-off; internal/aml/cases.go)",
	"POST /v1/admin/tenants/{tenantID}/aml/cases/{caseID}/proposals":                                 "AML (ADR 0116 s5.2): audited by the SECURITY DEFINER function aml_case_propose, which appends to aml_case_events (actor, sequence, reason/content hash) under the officer's acting session; deliberately NOT audit_log, which is tenant-readable (no tipping-off; internal/aml/cases.go)",
	"POST /v1/admin/tenants/{tenantID}/aml/cases/{caseID}/approvals":                                 "AML (ADR 0116 s5.2): audited by the SECURITY DEFINER function aml_case_approve, which appends to aml_case_events (actor, sequence, reason/content hash) under the officer's acting session; deliberately NOT audit_log, which is tenant-readable (no tipping-off; internal/aml/cases.go)",
}

// staffAuditSinksNotCounted are module functions whose audit.Record call
// records something other than a staff mutation (a READ audit), so reaching
// them proves nothing about the route's write. Keyed by types.Func.FullName().
var staffAuditSinksNotCounted = map[string]string{
	"(github.com/Diansalas/igaming-platform/internal/httpserver.staffReadCall).auditPlatformRead": "records a staff READ (ADR 0119 read audit), not a mutation",
}

// staffAuditExcludedRootPkgs are packages whose functions never count as a
// route ROOT: the shared staff gates wrapped around every handler.
var staffAuditExcludedRootPkgs = map[string]bool{
	staffAuditModulePath + "/internal/auth": true,
}

func isMutatingMethod(m string) bool {
	return m == "POST" || m == "PUT" || m == "PATCH" || m == "DELETE"
}

// ---- type-checked module program --------------------------------------------

type auditProgram struct {
	fset    *token.FileSet
	pkgs    map[string]*types.Package
	infos   map[*types.Package]*types.Info
	files   map[*types.Package][]*ast.File
	bodies  map[*types.Func]*ast.FuncDecl
	fnInfo  map[*types.Func]*types.Info
	named   []*types.Named // concrete module named types, for CHA
	callees map[*types.Func][]*types.Func
}

var (
	auditProgOnce sync.Once
	auditProg     *auditProgram
	auditProgErr  error
)

type goListPkg struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	Export     string
	Standard   bool
	Module     *struct{ Path string }
}

func loadAuditProgram() (*auditProgram, error) {
	auditProgOnce.Do(func() { auditProg, auditProgErr = buildAuditProgram() })
	return auditProg, auditProgErr
}

func buildAuditProgram() (*auditProgram, error) {
	cmd := exec.Command("go", "list", "-deps", "-export", "-json=ImportPath,Dir,GoFiles,Export,Standard,Module", ".")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list -deps -export: %v: %s", err, stderr.String())
	}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	var order []goListPkg
	exports := map[string]string{}
	for {
		var p goListPkg
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}
		order = append(order, p)
		if p.Export != "" {
			exports[p.ImportPath] = p.Export
		}
	}
	fset := token.NewFileSet()
	gc := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		f, ok := exports[path]
		if !ok {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(f)
	})
	prog := &auditProgram{
		fset: fset, pkgs: map[string]*types.Package{}, infos: map[*types.Package]*types.Info{},
		files: map[*types.Package][]*ast.File{}, bodies: map[*types.Func]*ast.FuncDecl{},
		fnInfo: map[*types.Func]*types.Info{}, callees: map[*types.Func][]*types.Func{},
	}
	imp := importerFunc(func(path string) (*types.Package, error) {
		if p, ok := prog.pkgs[path]; ok {
			return p, nil
		}
		if strings.HasPrefix(path, staffAuditModulePath+"/") {
			return nil, fmt.Errorf("module package %s imported before it was checked (go list order)", path)
		}
		return gc.Import(path)
	})
	// go list -deps prints dependencies before dependents, so a module package
	// is always checked after every module package it imports.
	for _, p := range order {
		if p.Module == nil || p.Module.Path != staffAuditModulePath {
			continue
		}
		var files []*ast.File
		for _, g := range p.GoFiles {
			f, err := parser.ParseFile(fset, filepath.Join(p.Dir, g), nil, parser.SkipObjectResolution)
			if err != nil {
				return nil, err
			}
			files = append(files, f)
		}
		info := &types.Info{
			Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{},
			Selections: map[*ast.SelectorExpr]*types.Selection{},
		}
		conf := types.Config{Importer: imp, Error: func(error) {}}
		pkg, err := conf.Check(p.ImportPath, fset, files, info)
		if err != nil {
			return nil, fmt.Errorf("type-check %s: %v", p.ImportPath, err)
		}
		prog.pkgs[p.ImportPath] = pkg
		prog.infos[pkg] = info
		prog.files[pkg] = files
		for _, f := range files {
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				if fn, ok := info.Defs[fd.Name].(*types.Func); ok {
					prog.bodies[fn] = fd
					prog.fnInfo[fn] = info
				}
			}
		}
		for _, name := range pkg.Scope().Names() {
			tn, ok := pkg.Scope().Lookup(name).(*types.TypeName)
			if !ok || tn.IsAlias() {
				continue
			}
			named, ok := tn.Type().(*types.Named)
			if !ok || types.IsInterface(named) || named.TypeParams().Len() > 0 {
				continue
			}
			prog.named = append(prog.named, named)
		}
	}
	if prog.pkgs[staffAuditRecordPkg] == nil {
		return nil, fmt.Errorf("%s is not a dependency of internal/httpserver", staffAuditRecordPkg)
	}
	return prog, nil
}

type importerFunc func(string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }

func inModule(fn *types.Func) bool {
	return fn.Pkg() != nil && (fn.Pkg().Path() == staffAuditModulePath || strings.HasPrefix(fn.Pkg().Path(), staffAuditModulePath+"/"))
}

// resolve maps a referenced function to the module function bodies it may run:
// itself (normalized to its generic origin), or, for a module-declared
// interface method, every concrete module implementation (CHA).
func (p *auditProgram) resolve(fn *types.Func) []*types.Func {
	fn = fn.Origin()
	if !inModule(fn) {
		return nil
	}
	sig, _ := fn.Type().(*types.Signature)
	if sig == nil || sig.Recv() == nil || !types.IsInterface(sig.Recv().Type()) {
		return []*types.Func{fn}
	}
	iface, _ := sig.Recv().Type().Underlying().(*types.Interface)
	if iface == nil {
		return nil
	}
	var out []*types.Func
	for _, n := range p.named {
		for _, t := range []types.Type{n, types.NewPointer(n)} {
			if !types.Implements(t, iface) {
				continue
			}
			obj, _, _ := types.LookupFieldOrMethod(t, false, fn.Pkg(), fn.Name())
			if m, ok := obj.(*types.Func); ok && inModule(m) {
				out = append(out, m.Origin())
			}
			break
		}
	}
	return out
}

// refsIn returns every module function referenced in node (called or as a
// value), resolved through resolve.
func (p *auditProgram) refsIn(info *types.Info, node ast.Node) []*types.Func {
	seen := map[*types.Func]bool{}
	var out []*types.Func
	add := func(fns ...*types.Func) {
		for _, r := range fns {
			if !seen[r] {
				seen[r] = true
				out = append(out, r)
			}
		}
	}
	// Pass 1: classify audit write call sites. An audit.Record call (or a call
	// through a reviewed audit function field) counts only when its Entry does
	// not name a constant non-success Outcome; a bare VALUE reference to
	// audit.Record (cfg.Audit = audit.Record) is not an audit write.
	auditCallee := map[*ast.Ident]bool{}
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id := selOrIdent(call.Fun)
		if id == nil {
			return true
		}
		if passesNonSuccessOutcome(info, call) {
			// stepUpAudit(..., audit.OutcomeDenied, ...): a denial/failure
			// recording call is not an edge toward the mutation's audit write.
			auditCallee[id] = true
			return true
		}
		isWrite := false
		switch obj := info.Uses[id].(type) {
		case *types.Func:
			isWrite = isAuditRecord(obj)
		case *types.Var:
			isWrite = obj.IsField() && staffAuditFuncFields[auditFieldKey(info, call.Fun, obj)] != ""
		}
		if !isWrite {
			return true
		}
		auditCallee[id] = true
		if len(call.Args) == 3 && !nonSuccessEntry(info, node, call.Args[2]) {
			add(p.auditRecordFunc())
		}
		return true
	})
	// Pass 2: every other module function referenced (called or as a value).
	ast.Inspect(node, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || auditCallee[id] {
			return true
		}
		fn, ok := info.Uses[id].(*types.Func)
		if !ok || isAuditRecord(fn) {
			return true
		}
		add(p.resolve(fn)...)
		return true
	})
	return out
}

// staffAuditFuncFields are func-typed struct fields that hold audit.Record
// (dependency injection for tests); a call through one is an audit write.
// Keyed "<pkg path>.<struct type>.<field>", with the reason.
var staffAuditFuncFields = map[string]string{
	staffAuditModulePath + "/internal/rg.LimitsConfig.Audit": "rg.NewLimits defaults it to audit.Record (limits_service.go); every l.cfg.Audit(...) call is an audit_log write",
}

func auditFieldKey(info *types.Info, fun ast.Expr, field *types.Var) string {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	s := info.Selections[sel]
	if s == nil {
		return ""
	}
	recv := s.Recv()
	if ptr, ok := recv.(*types.Pointer); ok {
		recv = ptr.Elem()
	}
	named, ok := recv.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return ""
	}
	return named.Obj().Pkg().Path() + "." + named.Obj().Name() + "." + field.Name()
}

func (p *auditProgram) auditRecordFunc() *types.Func {
	return p.pkgs[staffAuditRecordPkg].Scope().Lookup(staffAuditRecordFunc).(*types.Func)
}

// nonSuccessEntry reports whether the audit.Entry argument e names a constant
// audit.OutcomeDenied / audit.OutcomeFailure Outcome - directly as a literal,
// or through a local variable whose every definition in scope is such a
// literal. A denial or failure record is not the mutation's own audit write.
// Anything the analysis cannot see (a parameter, a helper's return value, a
// variable Outcome, audit.OutcomeSuccess) counts as a write.
func nonSuccessEntry(info *types.Info, scope ast.Node, e ast.Expr) bool {
	e = ast.Unparen(e)
	if call, ok := e.(*ast.CallExpr); ok {
		return passesNonSuccessOutcome(info, call)
	}
	if id, ok := e.(*ast.Ident); ok {
		v, ok := info.Uses[id].(*types.Var)
		if !ok {
			return false
		}
		defs := localDefs(info, scope, v)
		if len(defs) == 0 {
			return false
		}
		for _, d := range defs {
			if !nonSuccessEntry(info, scope, d) {
				return false
			}
		}
		return true
	}
	lit, ok := e.(*ast.CompositeLit)
	if !ok {
		return false
	}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok || !isIdent(kv.Key, "Outcome") {
			continue
		}
		return isNonSuccessOutcomeConst(info, kv.Value)
	}
	return false
}

func isNonSuccessOutcomeConst(info *types.Info, e ast.Expr) bool {
	id := selOrIdent(e)
	if id == nil {
		return false
	}
	c, ok := info.Uses[id].(*types.Const)
	return ok && c.Pkg() != nil && c.Pkg().Path() == staffAuditRecordPkg && (c.Name() == "OutcomeDenied" || c.Name() == "OutcomeFailure")
}

// passesNonSuccessOutcome reports whether call passes a constant
// audit.OutcomeDenied / audit.OutcomeFailure argument (a helper that records
// or builds a denial/failure entry, e.g. stepUpAudit, launchgov's auditEntry).
func passesNonSuccessOutcome(info *types.Info, call *ast.CallExpr) bool {
	for _, a := range call.Args {
		if isNonSuccessOutcomeConst(info, a) {
			return true
		}
	}
	return false
}

// localDefs returns every expression assigned to v inside scope.
func localDefs(info *types.Info, scope ast.Node, v *types.Var) []ast.Expr {
	var out []ast.Expr
	ast.Inspect(scope, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			for i, l := range s.Lhs {
				id, ok := l.(*ast.Ident)
				if !ok || (info.Defs[id] != v && info.Uses[id] != v) {
					continue
				}
				if len(s.Rhs) == len(s.Lhs) {
					out = append(out, s.Rhs[i])
				} else {
					out = append(out, &ast.BadExpr{}) // multi-value: unknown
				}
			}
		case *ast.ValueSpec:
			for i, nm := range s.Names {
				if info.Defs[nm] == v {
					if i < len(s.Values) {
						out = append(out, s.Values[i])
					} else {
						out = append(out, &ast.BadExpr{})
					}
				}
			}
		}
		return true
	})
	return out
}

func selOrIdent(e ast.Expr) *ast.Ident {
	switch v := ast.Unparen(e).(type) {
	case *ast.SelectorExpr:
		return v.Sel
	case *ast.Ident:
		return v
	}
	return nil
}

func (p *auditProgram) calleesOf(fn *types.Func) []*types.Func {
	if c, ok := p.callees[fn]; ok {
		return c
	}
	var c []*types.Func
	if fd, ok := p.bodies[fn]; ok {
		c = p.refsIn(p.fnInfo[fn], fd.Body)
	}
	p.callees[fn] = c
	return c
}

func isAuditRecord(fn *types.Func) bool {
	return fn.Pkg() != nil && fn.Pkg().Path() == staffAuditRecordPkg && fn.Name() == staffAuditRecordFunc
}

// auditWitness returns a shortest call chain (root ... audit.Record) within the
// depth bound, or nil.
func (p *auditProgram) auditWitness(roots []*types.Func) []*types.Func {
	parent := map[*types.Func]*types.Func{}
	depth := map[*types.Func]int{}
	var queue []*types.Func
	for _, r := range roots {
		if _, ok := depth[r]; !ok {
			depth[r] = 0
			queue = append(queue, r)
		}
	}
	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		if isAuditRecord(fn) {
			var chain []*types.Func
			for c := fn; c != nil; c = parent[c] {
				chain = append([]*types.Func{c}, chain...)
			}
			return chain
		}
		if _, blocked := staffAuditSinksNotCounted[fn.FullName()]; blocked || depth[fn] >= staffMutationAuditMaxDepth {
			continue
		}
		for _, c := range p.calleesOf(fn) {
			if _, ok := depth[c]; ok {
				continue
			}
			depth[c] = depth[fn] + 1
			parent[c] = fn
			queue = append(queue, c)
		}
	}
	return nil
}

// ---- route enumeration --------------------------------------------------------

type mutatingStaffRoute struct {
	pattern string
	pos     string
	roots   []*types.Func
}

// mutatingStaffRoutes finds every mux.Handle("<METHOD> <path>", staffRoute(deps,
// <class>, <inner>)) with a mutating METHOD in internal/httpserver, and the
// root functions of <inner>.
func (p *auditProgram) mutatingStaffRoutes() ([]mutatingStaffRoute, error) {
	pkg := p.pkgs[staffAuditModulePath+"/internal/httpserver"]
	if pkg == nil {
		return nil, fmt.Errorf("internal/httpserver not loaded")
	}
	info := p.infos[pkg]
	var routes []mutatingStaffRoute
	for _, f := range p.files[pkg] {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) != 2 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Handle" {
					return true
				}
				lit, ok := call.Args[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				h, ok := call.Args[1].(*ast.CallExpr)
				if !ok || !isIdentCall(h, "staffRoute") || len(h.Args) != 3 {
					return true
				}
				pattern := strings.Trim(lit.Value, "\"`")
				method, _, _ := strings.Cut(pattern, " ")
				if !isMutatingMethod(method) {
					return true
				}
				roots := p.rootsOf(info, fd, h.Args[2], map[types.Object]bool{})
				routes = append(routes, mutatingStaffRoute{pattern: pattern, pos: p.fset.Position(call.Pos()).String(), roots: roots})
				return true
			})
		}
	}
	return routes, nil
}

// rootsOf returns the module functions referenced by expr, following local
// variables (declared in encl) to their defining right-hand side, excluding
// staffAuditExcludedRootPkgs.
func (p *auditProgram) rootsOf(info *types.Info, encl *ast.FuncDecl, expr ast.Node, visited map[types.Object]bool) []*types.Func {
	var out []*types.Func
	seen := map[*types.Func]bool{}
	add := func(fns ...*types.Func) {
		for _, fn := range fns {
			if !seen[fn] && !staffAuditExcludedRootPkgs[fn.Pkg().Path()] {
				seen[fn] = true
				out = append(out, fn)
			}
		}
	}
	ast.Inspect(expr, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		switch obj := info.Uses[id].(type) {
		case *types.Func:
			add(p.resolve(obj)...)
		case *types.Var:
			if visited[obj] || obj.Parent() == nil || obj.Parent() == obj.Pkg().Scope() {
				return true
			}
			visited[obj] = true
			for _, rhs := range definingExprs(info, encl, obj) {
				add(p.rootsOf(info, encl, rhs, visited)...)
			}
		}
		return true
	})
	return out
}

// definingExprs returns every right-hand side assigned to the local variable v
// inside encl.
func definingExprs(info *types.Info, encl *ast.FuncDecl, v *types.Var) []ast.Expr {
	var out []ast.Expr
	match := func(id ast.Expr) bool {
		i, ok := id.(*ast.Ident)
		if !ok {
			return false
		}
		return info.Defs[i] == v || info.Uses[i] == v
	}
	ast.Inspect(encl.Body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			for i, l := range s.Lhs {
				if !match(l) {
					continue
				}
				if len(s.Rhs) == len(s.Lhs) {
					out = append(out, s.Rhs[i])
				} else if len(s.Rhs) == 1 {
					out = append(out, s.Rhs[0])
				}
			}
		case *ast.ValueSpec:
			for i, nm := range s.Names {
				if info.Defs[nm] == v && i < len(s.Values) {
					out = append(out, s.Values[i])
				}
			}
		}
		return true
	})
	return out
}

func chainString(chain []*types.Func) string {
	parts := make([]string, len(chain))
	for i, fn := range chain {
		parts[i] = strings.TrimPrefix(strings.ReplaceAll(fn.FullName(), staffAuditModulePath+"/internal/", ""), staffAuditModulePath+"/")
	}
	return strings.Join(parts, " -> ")
}

// ---- the tests ------------------------------------------------------------------

func TestStaffMutatingRoutes_ReachAuditWrite(t *testing.T) {
	prog, err := loadAuditProgram()
	if err != nil {
		t.Fatalf("load program (the audit-reachability pin must not silently switch off): %v", err)
	}
	routes, err := prog.mutatingStaffRoutes()
	if err != nil {
		t.Fatal(err)
	}

	// The enumeration is tied to the route-registration freeze: exactly the
	// mutating subset of the golden table and of scanStaffRoutes.
	regs, problems := scanStaffRoutes(packageSources(t))
	if len(problems) > 0 {
		t.Fatalf("route freeze scan has problems (see TestStaffRoutes_*): %v", problems)
	}
	want := map[string]bool{}
	for _, r := range regs {
		if m, _, _ := strings.Cut(r.pattern, " "); isMutatingMethod(m) {
			want[r.pattern] = true
		}
	}
	golden := 0
	for pat := range staffRouteGolden {
		if m, _, _ := strings.Cut(pat, " "); isMutatingMethod(m) {
			golden++
			if !want[pat] {
				t.Errorf("golden mutating route %s is not registered via staffRoute", pat)
			}
		}
	}
	got := map[string]bool{}
	for _, r := range routes {
		got[r.pattern] = true
	}
	if len(got) != len(want) || len(want) != golden {
		t.Fatalf("mutating staff route enumeration diverges: typed scan %d, freeze scan %d, golden %d", len(got), len(want), golden)
	}
	for pat := range want {
		if !got[pat] {
			t.Errorf("typed scan missed mutating staff route %s", pat)
		}
	}

	var report []string
	audited := 0
	for _, r := range routes {
		if len(r.roots) == 0 {
			t.Errorf("%s (%s): no module function found in the inner handler expression", r.pattern, r.pos)
			continue
		}
		chain := prog.auditWitness(r.roots)
		reason, allowed := staffMutationAuditAllowList[r.pattern]
		switch {
		case chain != nil && allowed:
			t.Errorf("stale allow-list entry %q (%q): the route now reaches audit.Record via %s - remove it", r.pattern, reason, chainString(chain))
		case chain == nil && !allowed:
			names := make([]string, len(r.roots))
			for i, fn := range r.roots {
				names[i] = fn.FullName()
			}
			t.Errorf("MUTATING staff route %s (%s) does not reach audit.Record within %d calls (roots %v): add the audit write (same transaction, audit.Record) or a reviewed staffMutationAuditAllowList entry with a reason",
				r.pattern, r.pos, staffMutationAuditMaxDepth, names)
		case chain != nil:
			audited++
		}
		if chain != nil {
			report = append(report, fmt.Sprintf("%s\t%d\t%s", r.pattern, len(chain)-1, chainString(chain)))
		} else {
			report = append(report, fmt.Sprintf("%s\t-\tALLOW-LISTED: %s", r.pattern, reason))
		}
	}
	for pat := range staffMutationAuditAllowList {
		if !got[pat] {
			t.Errorf("stale allow-list entry %q: no such mutating staff route", pat)
		}
	}
	sort.Strings(report)
	if path := os.Getenv("U7_AUDIT_WITNESS_REPORT"); path != "" {
		_ = os.WriteFile(path, []byte(strings.Join(report, "\n")+"\n"), 0o600)
	}
	t.Logf("%d mutating staff routes: %d reach audit.Record, %d allow-listed", len(routes), audited, len(staffMutationAuditAllowList))
}

// TestStaffMutationAudit_NegativeControls proves the analysis is not
// tautological: the shared staff gates, a read-audit sink and an unrelated
// pure helper do NOT count as reaching audit.Record, while a known auditing
// handler does.
func TestStaffMutationAudit_NegativeControls(t *testing.T) {
	prog, err := loadAuditProgram()
	if err != nil {
		t.Fatal(err)
	}
	hs := prog.pkgs[staffAuditModulePath+"/internal/httpserver"]
	lookup := func(pkg *types.Package, name string) *types.Func {
		fn, ok := pkg.Scope().Lookup(name).(*types.Func)
		if !ok {
			t.Fatalf("%s.%s not found", pkg.Path(), name)
		}
		return fn
	}
	// 1. Every root the scan discards (auth gates) is excluded by package, so
	//    even if a gate audits a denial it is never a witness.
	for _, r := range mustRoutes(t, prog) {
		for _, fn := range r.roots {
			if staffAuditExcludedRootPkgs[fn.Pkg().Path()] {
				t.Errorf("%s: excluded package function %s used as a root", r.pattern, fn.FullName())
			}
		}
	}
	method := func(pkg *types.Package, typ, name string) *types.Func {
		tn, ok := pkg.Scope().Lookup(typ).(*types.TypeName)
		if !ok {
			t.Fatalf("%s.%s not found", pkg.Path(), typ)
		}
		obj, _, _ := types.LookupFieldOrMethod(types.NewPointer(tn.Type()), true, pkg, name)
		fn, ok := obj.(*types.Func)
		if !ok {
			t.Fatalf("%s.%s.%s not found", pkg.Path(), typ, name)
		}
		return fn
	}
	pkgOf := func(rel string) *types.Package {
		p := prog.pkgs[staffAuditModulePath+"/internal/"+rel]
		if p == nil {
			t.Fatalf("package %s not loaded", rel)
		}
		return p
	}
	// 2. Functions that must NOT count as an audit witness:
	notWitness := map[string]*types.Func{
		"pure helper writeJSON": lookup(hs, "writeJSON"),
		// a denial-only helper: Entry literal held in a local variable with Outcome: audit.OutcomeDenied
		"denial helper recordAdjustmentDenied": lookup(hs, "recordAdjustmentDenied"),
		// a denial-only helper: Entry built by a call passing audit.OutcomeDenied
		"denial helper (*launchgov.Service).denied": method(pkgOf("launchgov"), "Service", "denied"),
		// a constructor that only stores audit.Record as a VALUE (cfg.Audit = audit.Record)
		"value reference rg.NewLimits": lookup(pkgOf("rg"), "NewLimits"),
		// the staff auth gates (also excluded as roots by package)
		"gate auth.RequirePermission": lookup(pkgOf("auth"), "RequirePermission"),
	}
	for label, fn := range notWitness {
		if c := prog.auditWitness([]*types.Func{fn}); c != nil {
			t.Errorf("%s reported as reaching audit.Record: %s", label, chainString(c))
		}
	}
	// Positive controls: a write through the reviewed func-field indirection,
	// a success audit in a domain service, and the step-up handler whose
	// success path passes audit.OutcomeSuccess through a parameterized helper.
	for label, fn := range map[string]*types.Func{
		"(*rg.Limits).DecideFloor via LimitsConfig.Audit": method(pkgOf("rg"), "Limits", "DecideFloor"),
		"bonus.DecideSuggestion":                          lookup(pkgOf("bonus"), "DecideSuggestion"),
		"bonus.ClaimSuggestionForReview (U-7 fix)":        lookup(pkgOf("bonus"), "ClaimSuggestionForReview"),
		"newStaffStepUpHandler":                           lookup(hs, "newStaffStepUpHandler"),
	} {
		if c := prog.auditWitness([]*types.Func{fn}); c == nil {
			t.Errorf("positive control %s does not reach audit.Record", label)
		}
	}
	// 3. The read-audit sink is blocked.
	for name := range staffAuditSinksNotCounted {
		found := false
		for fn := range prog.bodies {
			if fn.FullName() == name {
				found = true
				if c := prog.auditWitness([]*types.Func{fn}); c != nil {
					t.Errorf("blocked sink %s still reaches audit.Record: %s", name, chainString(c))
				}
			}
		}
		if !found {
			t.Errorf("stale staffAuditSinksNotCounted entry %s", name)
		}
	}
	// 4. Positive control: audit.Record itself is depth 0.
	rec := lookup(prog.pkgs[staffAuditRecordPkg], staffAuditRecordFunc)
	if c := prog.auditWitness([]*types.Func{rec}); len(c) != 1 {
		t.Errorf("positive control: audit.Record not found as its own witness")
	}
}

func mustRoutes(t *testing.T, prog *auditProgram) []mutatingStaffRoute {
	t.Helper()
	r, err := prog.mutatingStaffRoutes()
	if err != nil {
		t.Fatal(err)
	}
	return r
}
