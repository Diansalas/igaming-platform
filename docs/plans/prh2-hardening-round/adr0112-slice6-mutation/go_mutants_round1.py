import subprocess, sys, os
ROOT=os.environ.get('REPO_ROOT') or os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '..', '..', '..'))
S=os.environ['PRIV_HARNESS_DIR']  # directory holding priv_test.sh (the private-DB harness)
env=dict(os.environ, PRIV_DB=os.environ['PRIV_DB'], PRIV_SRC=ROOT)
A='internal/launchgov/approvability.go'
R='internal/launchgov/read.go'
H='internal/httpserver/launch_authorisation_routes.go'
P='internal/auth/permission.go'
AD='internal/httpserver/admin_routes.go'
MUT=[
 ('A1 drop platform-scope check', A, 'a.Scope == ScopePlatform && a.TenantID == uuid.Nil && ', ''),
 ('A2 drop approve perm check', A, ' && auth.RoleHasPermission(a.Role, auth.PermLaunchAuthorisationApprove)', ''),
 ('A3 expiry boundary', A, '!now.Before(r.ExpiresAt)', 'now.After(r.ExpiresAt)'),
 ('A4 drop self check', A, 'if r.RequestedBy == a.StaffID {', 'if false {'),
 ('A5 drop same person', A, 'if f.MyPerson == r.RequestedByPersonID {', 'if false {'),
 ('A6 no_person fail-open', A, 'if f.MyPerson == uuid.Nil || r.RequestedByPersonID == uuid.Nil {', 'if false {'),
 ('A7 drop already decided', A, 'if f.DecidedByMyPerson {', 'if false {'),
 ('A8 prior reject ignores note', A, ' && strings.TrimSpace(r.ConditionsNote) == ""', ''),
 ('A9 prior reject always-with-note', A, 'strings.TrimSpace(r.ConditionsNote) == ""', 'strings.TrimSpace(r.ConditionsNote) != ""'),
 ('A10 not_pending missing', A, 'if r.Status != ReqPending {', 'if false {'),
 ('A11 decided query uses staff id col', A, 'AND decided_by_person_id = $2`, ids, me)', 'AND decided_by = $2`, ids, me)'),
 ('A12 prior reject window ignored', A, 'AND ap.decided_at > $2`, me, now.Add(-RequestTTL))', 'AND ap.decided_at > $2`, me, now.Add(-1000*RequestTTL))'),
 ('A13 prior reject any decision', A, "ap.decision = 'reject' AND ap.decided_by_person_id = $1", "ap.decided_by_person_id = $1"),
 ('A14 prior key ignores brand', A, 'k.brand = *brand', '_ = brand'),
 ('A15 prior key ignores action', A, 'func keyOf(r Request) subjectKey {\n\tk := subjectKey{tenant: r.TenantID, kind: r.SubjectKind, action: r.Action}', 'func keyOf(r Request) subjectKey {\n\tk := subjectKey{tenant: r.TenantID, kind: r.SubjectKind}'),
 ('R1 keyset <=', R, '< ($4::timestamptz, $5::uuid))', '<= ($4::timestamptz, $5::uuid))'),
 ('R2 keyset ts only', R, '(r.created_at, r.id) < ($4::timestamptz, $5::uuid)', 'r.created_at < $4::timestamptz'),
 ('R3 order asc', R, 'ORDER BY r.created_at DESC, r.id DESC LIMIT $2', 'ORDER BY r.created_at ASC, r.id ASC LIMIT $2'),
 ('R4 order id asc tiebreak', R, 'ORDER BY r.created_at DESC, r.id DESC LIMIT $2', 'ORDER BY r.created_at DESC, r.id ASC LIMIT $2'),
 ('R5 no limit+1', R, 'statuses, limit+1, now, afterTS, afterID', 'statuses, limit, now, afterTS, afterID'),
 ('R6 default limit', R, 'limit = QueueDefaultLimit', 'limit = 100'),
 ('R7 no cap', R, 'if limit > QueueMaxLimit {\n\t\tlimit = QueueMaxLimit\n\t}', ''),
 ('R8 next cursor from first', R, 'last := page.Items[limit-1]', 'last := page.Items[0]'),
 ('R9 tenant name join dropped', R, 'LEFT JOIN tenants t ON t.id = r.tenant_id', 'LEFT JOIN tenants t ON false'),
 ('R10 brand join tenant check', R, 'b.id = r.brand_id AND b.tenant_id = r.tenant_id', 'b.id = r.brand_id AND false'),
 ('R11 kinds oldest not newest', R, 'ORDER BY brand_id, created_at DESC, id DESC', 'ORDER BY brand_id, created_at ASC, id ASC'),
 ('R12 tenant kinds oldest', R, 'ORDER BY tenant_id, created_at DESC, id DESC', 'ORDER BY tenant_id, created_at ASC, id ASC'),
 ('R13 cursor micro->milli', R, 'c.CreatedAt.UTC().UnixMicro()', 'c.CreatedAt.UTC().UnixMilli()'),
 ('R14 queue forbids platform check', R, 'if a.Scope != ScopePlatform {\n\t\treturn QueuePage{}', 'if false {\n\t\treturn QueuePage{}'),
 ('H1 limit max 500', H, 'n > launchgov.QueueMaxLimit', 'n > 500'),
 ('H2 cursor error ignored', H, 'cok = err == nil', 'cok = true'),
 ('H3 limit min', H, 'n < 1 || n > launchgov.QueueMaxLimit', 'n < 0 || n > launchgov.QueueMaxLimit'),
 ('H4 next cursor never', H, 'if page.Next != nil {\n\t\t\ttok', 'if false {\n\t\t\ttok'),
 ('H5 can_approve not mapped', H, 'if r.Approvability != nil {', 'if false {'),
 ('H6 reason on can', H, 'd.CanApprove, d.CannotApproveReason = &ok, r.Approvability.Reason', 'd.CanApprove, d.CannotApproveReason = &ok, "x"'),
 ('H7 tenant names dropped', H, 'TenantName:  r.TenantName, BrandName: r.BrandName,', ''),
 ('P1 compliance approve', P, 'PermLaunchAuthorisationRequest, PermLaunchAuthorisationSuspend, PermLaunchAuthorisationRead,\n\t),\n\t// finance is', 'PermLaunchAuthorisationRequest, PermLaunchAuthorisationSuspend, PermLaunchAuthorisationRead, PermLaunchAuthorisationApprove,\n\t),\n\t// finance is'),
 ('P2 compliance no tenant read', P, 'PermTenantRead, PermBrandRead,\n\t\t// ADR 0112 slice 6 (b)', 'PermBrandRead,\n\t\t// ADR 0112 slice 6 (b)'),
 ('P3 compliance tenant write', P, '\t\tPermTenantRead, PermBrandRead,\n\t\t// The sole tenant-scoped grantee of PermRGRestrictionWrite', '\t\tPermTenantRead, PermBrandRead, PermTenantWrite,\n\t\t// The sole tenant-scoped grantee of PermRGRestrictionWrite'),
 ('D1 brand kinds not set', AD, 'resp[i].LaunchKind = &k', '_ = k'),
 ('D2 brand kinds forbidden swallowed->500 removed', AD, 'kerr != nil && !errors.Is(kerr, launchgov.ErrForbidden)', 'false'),
]
only=sys.argv[1:]
def run(p):
    if p=='auth': return ['go','test','-count=1','./internal/auth/']
    if p=='unit': return ['go','test','-count=1','./internal/launchgov/','-run','EvalApprovability|CannotApprove|Cursor','./internal/httpserver/','-run','OpenAPI_Launch|Slice6']
res=[]
for name,f,old,new in MUT:
    if only and name.split()[0] not in only: continue
    path=os.path.join(ROOT,f)
    src=open(path).read()
    if old not in src:
        res.append((name,'NOAPPLY')); print(name,'NOAPPLY',flush=True); continue
    open(path,'w').write(src.replace(old,new,1))
    try:
        killed=None
        # 1: auth + unit, DB-free
        r=subprocess.run(['go','test','-count=1','./internal/auth/','./internal/launchgov/'],cwd=ROOT,capture_output=True,text=True)
        if r.returncode!=0: killed='unit'
        if killed is None:
            r=subprocess.run(['bash',S+'/priv_test.sh','-p','1','-tags','integration','-count=1','./internal/launchgov/','-run','TestQueuePage'],cwd=ROOT,capture_output=True,text=True,env=env)
            if r.returncode!=0: killed='launchgov-int'
        if killed is None:
            r=subprocess.run(['bash',S+'/priv_test.sh','-p','1','-tags','integration','-count=1','./internal/httpserver/','-run','TestLaunchHTTP6|TestLaunchHTTP_AuthzMatrix|TestLaunchHTTP_ReadsListQueueHistory|OpenAPI_Launch'],cwd=ROOT,capture_output=True,text=True,env=env)
            if r.returncode!=0: killed='http-int'
        if killed is not None and 'build failed' in (r.stdout+r.stderr) : killed+=' (BUILD)'
    finally:
        open(path,'w').write(src)
    res.append((name,killed or 'SURVIVED')); print(name,killed or 'SURVIVED',flush=True)
