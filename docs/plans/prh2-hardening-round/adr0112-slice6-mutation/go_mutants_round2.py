import subprocess, os
ROOT=os.environ.get('REPO_ROOT') or os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '..', '..', '..'))
S=os.environ['PRIV_HARNESS_DIR']  # directory holding priv_test.sh (the private-DB harness)
env=dict(os.environ, PRIV_DB=os.environ['PRIV_DB'], PRIV_SRC=ROOT)
A='internal/launchgov/approvability.go'
R='internal/launchgov/read.go'
H='internal/httpserver/launch_authorisation_routes.go'
P='internal/auth/permission.go'
PRM='\t\tPermTenantRead, PermBrandRead,\n\t\t// The sole tenant-scoped grantee of PermRGRestrictionWrite'
MUT=[
 ('B1 approve perm check neutered', A, 'auth.RoleHasPermission(a.Role, auth.PermLaunchAuthorisationApprove)', '(auth.RoleHasPermission(a.Role, auth.PermLaunchAuthorisationApprove) || true)'),
 ('B2 prior reject ignores note', A, 'strings.TrimSpace(r.ConditionsNote) == ""', '(strings.TrimSpace(r.ConditionsNote) == "" || true)'),
 ('B3 cursor error ignored', H, 'cok = err == nil', 'cok = err == nil || true'),
 ('B4 window ignored', A, 'now.Add(-RequestTTL))', 'now.Add(-1000*RequestTTL))'),
 ('B5 any decision counts as prior reject', A, "ap.decision = 'reject' AND ap.decided_by_person_id = $1", "ap.decided_by_person_id = $1"),
 ('B8 compliance no tenant read', P, PRM, '\t\tPermBrandRead,\n\t\t// The sole tenant-scoped grantee of PermRGRestrictionWrite'),
 ('B9 compliance no brand read', P, PRM, '\t\tPermTenantRead,\n\t\t// The sole tenant-scoped grantee of PermRGRestrictionWrite'),
 ('B10 expiry by status only', A, 'r.Status == ReqExpired || (r.Status == ReqPending && !now.Before(r.ExpiresAt))', 'r.Status == ReqExpired'),
 ('B11 tenant kinds subject filter', R, "WHERE subject_kind = 'tenant' AND tenant_id = ANY($1)", "WHERE tenant_id = ANY($1)"),
 ('B12 brand kinds subject filter', R, "WHERE subject_kind = 'brand' AND tenant_id = $1 AND brand_id = ANY($2)", "WHERE tenant_id = $1 AND brand_id = ANY($2)"),
 ('B13 nonNilIDs guard', R, 'func nonNilIDs(ids []uuid.UUID) []uuid.UUID {\n\tif ids == nil {', 'func nonNilIDs(ids []uuid.UUID) []uuid.UUID {\n\tif false {'),
]
for name,f,old,new in MUT:
    path=os.path.join(ROOT,f)
    src=open(path).read()
    if old not in src:
        print(name,'NOAPPLY',flush=True); continue
    open(path,'w').write(src.replace(old,new,1))
    try:
        killed=None
        r=subprocess.run(['go','test','-count=1','./internal/auth/','./internal/launchgov/'],cwd=ROOT,capture_output=True,text=True)
        if r.returncode!=0: killed='unit'+(' (BUILD)' if 'build failed' in r.stdout+r.stderr else '')
        if killed is None:
            r=subprocess.run(['bash',S+'/priv_test.sh','-p','1','-tags','integration','-count=1','./internal/launchgov/','-run','TestQueuePage|TestApprovability'],cwd=ROOT,capture_output=True,text=True,env=env)
            if r.returncode!=0: killed='launchgov-int'
        if killed is None:
            r=subprocess.run(['bash',S+'/priv_test.sh','-p','1','-tags','integration','-count=1','./internal/httpserver/','-run','TestLaunchHTTP6|TestLaunchHTTP_AuthzMatrix|TestLaunchHTTP_ReadsListQueueHistory|OpenAPI_Launch'],cwd=ROOT,capture_output=True,text=True,env=env)
            if r.returncode!=0: killed='http-int'+(' (BUILD)' if 'build failed' in r.stdout+r.stderr else '')
    finally:
        open(path,'w').write(src)
    print(name,killed or 'SURVIVED',flush=True)
