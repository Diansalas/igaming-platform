import subprocess, os
_R=os.environ.get('REPO_ROOT') or os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '..', '..', '..'))
ROOT=os.path.join(_R, 'backoffice')
M=[
 ('U1 absent answer hides', 'src/features/launch/approvability.ts', 'r.can_approve !== false', 'r.can_approve === true'),
 ('U2 reject always', 'src/features/launch/approvability.ts', "const reject = r.cannot_approve_reason === 'prior_reject_requires_change_note'", 'const reject = true'),
 ('U3 role hint ignored', 'src/features/launch/approvability.ts', 'if (!roleMayApprove) return', 'if (false) return'),
 ('U4 next page never', 'src/features/launch/LaunchQueuePage.tsx', 'last.next_cursor ?? undefined', 'undefined'),
 ('U5 page size', 'src/features/launch/LaunchQueuePage.tsx', 'const PAGE_SIZE = 50', 'const PAGE_SIZE = 100'),
 ('U6 names ignored', 'src/features/launch/LaunchQueuePage.tsx', '{r.tenant_name ??', '{undefined ??'),
 ('U7 legacy badge tenant list', 'src/features/tenants/TenantListPage.tsx', "t.launch_kind === 'legacy_baseline'", 'true'),
 ('U8 legacy badge brand table', 'src/features/tenants/TenantDetailPage.tsx', "b.launch_kind === 'legacy_baseline'", "b.launch_kind !== 'governed'"),
 ('U9 compliance ownTenant', 'src/auth/permissions.ts', 'compliance: { ...NONE, ownTenant: true,', 'compliance: { ...NONE,'),
 ('U10 cursor not sent', 'src/features/launch/LaunchQueuePage.tsx', 'limit: PAGE_SIZE, cursor: pageParam', 'limit: PAGE_SIZE'),
 ('U11 detail still guesses requester', 'src/features/launch/RequestDetail.tsx', 'const controls = approvalControls(r, perms.launchApprove)', 'const controls = approvalControls(r, perms.launchApprove && !isRequester)'),
]
for name,f,old,new in M:
    path=os.path.join(ROOT,f)
    src=open(path).read()
    if old not in src:
        print(name,'NOAPPLY',flush=True); continue
    open(path,'w').write(src.replace(old,new,1))
    try:
        r=subprocess.run(['npx','vitest','run'],cwd=ROOT,capture_output=True,text=True)
        print(name,'KILLED' if r.returncode!=0 else 'SURVIVED',flush=True)
    finally:
        open(path,'w').write(src)
