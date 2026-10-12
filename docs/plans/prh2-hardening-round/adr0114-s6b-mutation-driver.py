import subprocess, sys, os
W = '/home/user/igaming-platform/.claude/worktrees/agent-a079050f80085fb7c'
S = '/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad'
H = 'internal/httpserver/payments_kill_switch_handlers.go'
P = 'internal/payments/killswitch.go'
BO = 'backoffice/src/features/killswitch/'

GO_TEST = ['env', 'PRIV_DB=s6b_ks_fix', 'PRIV_SRC=' + W, 'bash', S + '/priv_test.sh', '-tags', 'integration', '-p', '1', '-count=1',
           './internal/httpserver', '-run', 'KillSwitchOpenRequest']
UI_TEST = ['npx', 'vitest', 'run', 'src/features/killswitch']

mutants = [
  ('B1 open ids always empty', P, "\t\tout[ksID] = reqID\n", "\t\t_ = reqID\n\t\t_ = ksID\n", GO_TEST, W),
  ('B2 drop status=open (list)', P, "WHERE tenant_id=$1 AND status='open'`, tenantID)", "WHERE tenant_id=$1`, tenantID)", GO_TEST, W),
  ('B2b drop status=open (single)', P, "AND kill_switch_id=$2 AND status='open'`", "AND kill_switch_id=$2`", GO_TEST, W),
  ('B3 23505 not mapped', H, 'pgErr.Code == "23505" && pgErr.ConstraintName == killSwitchOneOpenIndex', 'false', GO_TEST, W),
  ('B4 no denied audit on duplicate', H, '\t\trecordKillSwitchRefusalAudit(ctx, deps, c, op, class, targetType, targetID)\n\t\tapierror.Write(w, c.requestID, apierror.CodeKillSwitchReleaseRequestAlreadyOpen', '\t\tapierror.Write(w, c.requestID, apierror.CodeKillSwitchReleaseRequestAlreadyOpen', GO_TEST, W),
  ('B4b duplicate answers generic conflict code', H, 'apierror.Write(w, c.requestID, apierror.CodeKillSwitchReleaseRequestAlreadyOpen,', 'apierror.Write(w, c.requestID, apierror.CodeConflict,', GO_TEST, W),
  ('B5 rune count instead of bytes', H, 'func killSwitchReasonCodeValid(reason string) bool { return len(reason) >= 1 && len(reason) <= 64 }', 'func killSwitchReasonCodeValid(reason string) bool { n := len([]rune(reason)); return n >= 1 && n <= 64 }', GO_TEST, W),
  ('B5b release handler skips byte check', H, '\t\tif !killSwitchReasonCodeValid(body.ReasonCode) {\n\t\t\tapierror.Write(w, c.requestID, apierror.CodeValidation, "reason_code must be 1 to 64 bytes")\n\t\t\treturn\n\t\t}\n\t\tvar req payments.KillSwitchReleaseRequest', '\t\tvar req payments.KillSwitchReleaseRequest', GO_TEST, W),
  ('B5c engage handler skips byte check', H, '\t\tif !killSwitchReasonCodeValid(body.ReasonCode) {\n\t\t\tapierror.Write(w, c.requestID, apierror.CodeValidation, "reason_code must be 1 to 64 bytes")\n\t\t\treturn\n\t\t}\n\t\tswitch body.OperationScope', '\t\tswitch body.OperationScope', GO_TEST, W),
  ('B6 engage response never reports open request', H, 'if openReq, err = payments.OpenReleaseRequestID(ctx, tx, c.target, ks.ID); err != nil {\n\t\t\t\treturn err', 'if _, err = payments.OpenReleaseRequestID(ctx, tx, c.target, ks.ID); err != nil {\n\t\t\t\treturn err', GO_TEST, W),
  ('B7 GET switch never reports open request', H, 'openReq, err = payments.OpenReleaseRequestID(ctx, tx, c.target, ks.ID)\n\t\t\treturn err\n\t\t})\n\t\tif err != nil {\n\t\t\twriteKillSwitchError(r.Context(), deps, w, c, "get"', '_, err = payments.OpenReleaseRequestID(ctx, tx, c.target, ks.ID)\n\t\t\treturn err\n\t\t})\n\t\tif err != nil {\n\t\t\twriteKillSwitchError(r.Context(), deps, w, c, "get"', GO_TEST, W),
  ('B7b list never reports open request', H, 'if id, ok := open[ks.ID]; ok {', 'if id, ok := open[ks.ID]; ok && false {', GO_TEST, W),
  ('B8 request DTO leaks platform requester to tenant', H, 'RequestedBy: v.ref(string(req.RequestedByScope), req.RequestedBy)', 'RequestedBy: platformStaffViewer().ref(string(req.RequestedByScope), req.RequestedBy)', GO_TEST, W),
  ('B9 switch DTO leaks platform changer to tenant', H, 'ChangedBy: v.ref(string(ks.ChangedByScope), ks.ChangedBy)', 'ChangedBy: platformStaffViewer().ref(string(ks.ChangedByScope), ks.ChangedBy)', GO_TEST, W),
  ('B10 open id from another tenant (no tenant predicate, single)', P, "WHERE tenant_id=$1 AND kill_switch_id=$2 AND status='open'`,\n\t\ttenantID, killSwitchID)", "WHERE $1::uuid IS NOT NULL AND kill_switch_id=$2 AND status='open'`,\n\t\ttenantID, killSwitchID)", GO_TEST, W),
  ('B11 control chars/NUL accepted', H, 'if unicode.IsControl(r) || r == utf8.RuneError {', 'if (unicode.IsControl(r) && false) || r == utf8.RuneError {', GO_TEST, W),
  ('B11b whitespace-only accepted', H, ' || strings.TrimSpace(reason) == "" {', ' {', GO_TEST, W),
  ('B11c U+FFFD (lone surrogate) accepted', H, 'if unicode.IsControl(r) || r == utf8.RuneError {', 'if unicode.IsControl(r) {', GO_TEST, W),
  # UI
  ('U9 stale 201 fallback never dropped', BO + 'ReleasePanel.tsx', "const noReq = reqId === null || (ks.open_release_request_id == null && req !== undefined && req.status !== 'open')", 'const noReq = reqId === null', UI_TEST, W + '/backoffice'),
  ('U10 platform-filed Approve enabled for tenant', BO + 'ReleasePanel.tsx', "<Button disabled={!viewerIsPlatform && req.requested_by_scope === 'platform'} onClick", '<Button onClick', UI_TEST, W + '/backoffice'),
  ('U1 gate removed from AppRoutes', 'backoffice/src/app/AppRoutes.tsx', '''            <RequireNavPermission permission="killSwitch">
              <KillSwitchPage />
            </RequireNavPermission>''', '''            <KillSwitchPage />''', UI_TEST, W + '/backoffice'),
  ('U2 panel uses release_request_id', BO + 'ReleasePanel.tsx', 'ks.open_release_request_id ?? (filed', 'ks.release_request_id ?? (filed', UI_TEST, W + '/backoffice'),
  ('U3 201 id not kept', BO + 'ReleasePanel.tsx', 'ks.open_release_request_id ?? (filed && filed.ksId === ks.id ? filed.requestId : null)', 'ks.open_release_request_id ?? null', UI_TEST, W + '/backoffice'),
  ('U4 characters not bytes', 'backoffice/src/api/killSwitch.ts', 'return new TextEncoder().encode(s).length', 'return s.length', UI_TEST, W + '/backoffice'),
  ('U5 table badge uses release_request_id', BO + 'KillSwitchPage.tsx', 'r.engaged && r.open_release_request_id ?', 'r.engaged && r.release_request_id ?', UI_TEST, W + '/backoffice'),
  ('U6 no stale refresh on refusal', BO + 'ReleasePanel.tsx', "if (refusalIsStale(e, 'request')) onRefused()", '', UI_TEST, W + '/backoffice'),
  ('U7 notices removed', BO + 'EngageDialog.tsx', '<p>{NOTICE_REENGAGE}</p>', '', UI_TEST, W + '/backoffice'),
  ('U8 platform notice shown to tenant', BO + 'EngageDialog.tsx', '{viewerIsPlatform ? NOTICE_TAKEOVER_PLATFORM : NOTICE_TAKEOVER_TENANT}', '{NOTICE_TAKEOVER_PLATFORM}', UI_TEST, W + '/backoffice'),
]

only = sys.argv[1:]
for name, f, old, new, cmd, cwd in mutants:
    if only and not any(name.startswith(o) for o in only):
        continue
    path = os.path.join(W, f)
    src = open(path).read()
    if src.count(old) != 1:
        print('SKIP (anchor count %d): %s' % (src.count(old), name), flush=True)
        continue
    open(path, 'w').write(src.replace(old, new))
    try:
        r = subprocess.run(cmd, cwd=cwd if cwd != W else W, capture_output=True, text=True, timeout=900)
        out = r.stdout + r.stderr
        verdict = 'KILLED' if r.returncode != 0 else 'SURVIVED'
        if r.returncode != 0 and ('build failed' in out or 'cannot' in out and 'FAIL' not in out):
            verdict = 'KILLED(build?)'
    finally:
        open(path, 'w').write(src)
    print('%s: %s' % (verdict, name), flush=True)
