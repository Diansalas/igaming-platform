// AML screening (lic10 / lic18 / lic20) through the REAL stack (lic22): Back Office UI for the trigger policy and the officer resolution,
// the real MOCK screening worker that platform-api starts outside production, the real API and the real database. Synthetic only.
// What is not real: the screening provider (the MOCK, driven by SYNTH- markers) and the single owner-role fixture that sets the marker
// on a verification's vendor subject reference (lib/wd_aml.ts markVerification). The jurisdiction screening policy is authored by
// tools/aml-fixture through the real officer service because no HTTP route for it exists.
import { expect, test, type Browser } from 'playwright/test'
import { apiLogin } from '../lib/api'
import { sql } from '../lib/db'
import { loginAs } from '../lib/session'
import { ledgerTotals, moneyCounts, n, projectionDrift, runtimeSql, sqlT } from '../lib/wd_db'
import {
  authorScreeningPolicy, waitPolicyEffective, configureTenantJurisdiction, fillPolicy, fillResolve, grantOfficer, jurisdictionId, openScreening, requestRow, revokeGrant,
  screeningRequests, secondVerification, showPolicy, showRequests, staffId, submitPolicy, submitResolve, waitHeartbeat, waitRequest, withoutHeartbeat,
} from '../lib/wd_aml'
import { approveWithdrawal, call, createFundedPlayer, createTenant, requestWithdrawal, submitWithdrawal, wallet, withdrawalState, type WdPlayer, type WdTenant } from '../lib/wd_world'
import { loadWorld } from '../lib/world'

const ctx = async (browser: Browser, key: string) => loginAs(await (await browser.newContext()).newPage(), key)

/** One request row exactly as stored (status, outcome, lists, claim token). */
const reqRow = (t: WdTenant, id: string) => sqlT(t.id, `SELECT status||'|'||COALESCE(outcome,'')||'|'||COALESCE(array_to_string(match_lists,','),'')||'|'||COALESCE(claim_token::text,'') FROM aml_screening_requests WHERE id='${id}'`)
const activeHolds = (t: WdTenant, personId: string) => n(sqlT(t.id, `SELECT count(*) FROM aml_holds h WHERE h.tenant_id='${t.id}' AND h.person_id='${personId}' AND NOT EXISTS (SELECT 1 FROM aml_hold_lifts l WHERE l.hold_id = h.id)`))
/** Evidence that only an officer or a governed path can change (the background worker never touches these). */
const counts = (t: WdTenant) => ({
  lifts: n(sqlT(t.id, `SELECT count(*) FROM aml_hold_lifts WHERE tenant_id='${t.id}'`)),
  results: n(sqlT(t.id, `SELECT count(*) FROM aml_screening_results WHERE tenant_id='${t.id}'`)),
  holds: n(sqlT(t.id, `SELECT count(*) FROM aml_holds WHERE tenant_id='${t.id}'`)),
  policies: n(sqlT(t.id, `SELECT count(*) FROM aml_screening_trigger_policies WHERE tenant_id='${t.id}'`)),
})

test.describe.serial('10 AML screening: trigger policy, MOCK worker, officer resolution, below-the-app-layer attempts', () => {
  test.setTimeout(900_000)
  let T: WdTenant
  let Pc: WdPlayer // clean control
  let Ps: WdPlayer // MOCK sanctions marker
  let Pp: WdPlayer // MOCK PEP marker
  let wdPc = '', wdPs = '', wdPp = ''
  let grantD = ''

  test.beforeAll(async () => {
    T = await createTenant('aml1')
    await configureTenantJurisdiction(T)
    await grantOfficer(T, 'PA_C')
    grantD = await grantOfficer(T, 'PA_D')
    expect(authorScreeningPolicy(T, 'PA_C')).toMatch(/^[0-9a-f-]{36}$/)
    // players exist BEFORE any trigger policy: nothing is queued for them yet. Each has an accepted, finance-approved withdrawal.
    Ps = await createFundedPlayer(T, 10000)
    Pp = await createFundedPlayer(T, 10000)
    for (const [p, set] of [[Ps, (id: string) => (wdPs = id)], [Pp, (id: string) => (wdPp = id)]] as const) {
      const r = await requestWithdrawal(p, 1000)
      expect(r.status).toBe(201)
      set(r.json.id)
      const ap = await approveWithdrawal(T, r.json.id)
      expect(ap.a2.json.approved).toBe(true)
    }
    expect(n(sqlT(T.id, `SELECT count(*) FROM aml_screening_requests WHERE tenant_id='${T.id}'`))).toBe(0)
  })

  test('(a) trigger policy through the UI: a withdrawal gate is REFUSED with no worker heartbeat; gate off with events is accepted; with a heartbeat the gate is accepted', async ({ browser }) => {
    const C = await ctx(browser, 'PA_C')
    await openScreening(C, T)
    await showPolicy(C)
    await expect(C.page.getByText('No trigger policy is in force for this jurisdiction: nothing triggers automatically and nothing gates a withdrawal.')).toBeVisible()
    await expect(C.page.getByText('WARNING: enabling the withdrawal gate while no working screening provider exists TRAPS PLAYER FUNDS', { exact: false }).first()).toBeVisible()
    expect(counts(T).policies).toBe(0)

    // no heartbeat -> gate refused (the server's refusal is rendered, nothing is written)
    const hb = await withoutHeartbeat()
    try {
      expect(Number(sql(`SELECT count(*) FROM screening_worker_heartbeats WHERE last_seen_at > clock_timestamp() - interval '10 minutes'`))).toBe(0)
      let d = await fillPolicy(C, { events: ['Registration', 'KYC approval', 'Scheduled re-screen'], gate: true, seconds: '600', reason: 'e2e_gate_try' })
      const posted = C.page.waitForResponse((r) => r.url().includes('/screening/trigger-policy') && r.request().method() === 'POST')
      await submitPolicy(C, d, () => expect(d.getByRole('alert').filter({ hasText: 'The server rejected the policy as invalid.' })).toBeVisible())
      // FINDING (recorded): the heartbeat guard (AT004) is mapped to ErrInvalidInput, so the server answers 400 validation_error, NOT the 409
      // guard_refused that the lic20 screen's heartbeat wording (WORKER_REFUSAL) expects. The refusal is rendered, but as the generic invalid-input
      // text, and the heartbeat is not named. The control below (same body accepted once a heartbeat exists) shows the heartbeat was the cause.
      const resp = await posted
      expect(resp.status()).toBe(400)
      expect((await resp.json()).code).toBe('validation_error')
      await expect(d.getByRole('alert')).not.toContainText('screening worker has reported a recent heartbeat')
      expect(counts(T).policies).toBe(0)
      await d.getByRole('button', { name: 'Cancel' }).click()

      // gate OFF with events: accepted with no heartbeat (it gates nothing)
      d = await fillPolicy(C, { events: ['Registration', 'KYC approval'], gate: false, reason: 'e2e_events_only' })
      await submitPolicy(C, d, () => expect(C.page.getByRole('status').filter({ hasText: /Policy version .* recorded/ })).toBeVisible())
    } finally {
      await hb.release()
    }
    expect(counts(T).policies).toBe(1)
    expect(sqlT(T.id, `SELECT gate_withdrawal||'|'||(SELECT string_agg(e, ',' ORDER BY e) FROM unnest(events) e) FROM aml_screening_trigger_policies WHERE tenant_id='${T.id}'`)).toBe('false|kyc_approval,registration')
    await waitHeartbeat() // the MOCK worker platform-api started reports in again once the lock is released
    expect(sql(`SELECT provider_id||'|'||synthetic FROM screening_worker_heartbeats ORDER BY last_seen_at DESC LIMIT 1`)).toBe('mock-screening|true')

    // with a heartbeat the same gate is accepted
    await showPolicy(C)
    const d = await fillPolicy(C, { events: ['Registration', 'KYC approval', 'Scheduled re-screen'], gate: true, seconds: '600', reason: 'e2e_gate_on' })
    await submitPolicy(C, d, () => expect(C.page.getByRole('status').filter({ hasText: /Policy version .* recorded/ })).toBeVisible())
    expect(counts(T).policies).toBe(2)
    await waitPolicyEffective(T, 'PA_C', true) // takes effect at its effective time (the next full minute)
    expect(sqlT(T.id, `SELECT gate_withdrawal||'|'||gate_alert_after_seconds||'|'||(SELECT string_agg(e, ',' ORDER BY e) FROM unnest(events) e) FROM aml_screening_trigger_policies WHERE tenant_id='${T.id}' ORDER BY created_at DESC LIMIT 1`)).toBe('true|600|kyc_approval,registration,scheduled')
    await showPolicy(C)
    await expect(C.page.getByLabel('Policy in force')).toContainText('ON: gates withdrawals')
  })

  test('(a2) who may author or read: a platform admin without an officer grant, and tenant compliance, are refused (server) and nothing is written', async ({ browser, request }) => {
    const before = counts(T)
    // PA_A holds no aml_officer grant: the read is refused by the database and the refusal is rendered
    const A = await ctx(browser, 'PA_A')
    await openScreening(A, T)
    await showPolicy(A)
    await expect(A.page.getByRole('alert').filter({ hasText: 'does not hold an in-force AML officer grant' })).toBeVisible()
    const body = { jurisdiction_id: jurisdictionId(), events: ['registration'], gate_withdrawal: false, effective_from: new Date(Date.now() - 10_000).toISOString(), legal_review_reference: 'E2E-SYNTHETIC', reason_code: 'e2e_nope' }
    const a = await call('POST', `/v1/admin/tenants/${T.id}/screening/trigger-policy`, await apiLogin('PA_A'), body)
    expect(a.status).toBe(403)
    // tenant staff on the platform-only authoring path
    const comp = await apiLogin(`${T.tag}_COMPLIANCE`)
    const c1 = await call('POST', `/v1/admin/tenants/${T.id}/screening/trigger-policy`, comp, body)
    expect([403, 404]).toContain(c1.status)
    const ta = await apiLogin(`${T.tag}_TENANT_ADMIN`)
    const c2 = await call('POST', `/v1/admin/tenants/${T.id}/screening/trigger-policy`, ta, body)
    expect([403, 404]).toContain(c2.status)
    // tenant compliance sees the screen read-only: authoring is not offered
    const S = await ctx(browser, `${T.tag}_COMPLIANCE`)
    await S.page.getByRole('navigation').getByRole('link', { name: 'AML' }).click()
    await S.page.getByRole('link', { name: 'Automatic screening: trigger policy and screening requests' }).click()
    await expect(S.page.getByText('Authoring is not offered to your role. The policy is read-only here.')).toBeVisible()
    expect(counts(T)).toEqual(before)
    void request
  })

  test('(b) screening triggered by registration and KYC approval is processed by the MOCK worker; a clean person passes the gate', async () => {
    Pc = await createFundedPlayer(T, 10000) // registers (registration event) and is KYC-approved (kyc_approval event)
    const reg = await waitRequest(T, 'PA_C', Pc.personId, (r) => r.trigger === 'registration' && r.status === 'done', 'registration screening done')
    expect(reg.outcome).toBe('clear')
    const kyc = await waitRequest(T, 'PA_C', Pc.personId, (r) => r.trigger === 'kyc_approval' && r.status === 'done', 'kyc_approval screening done')
    expect(kyc.outcome).toBe('clear')
    expect(sqlT(T.id, `SELECT provider_id||'|'||outcome FROM aml_screening_requests WHERE id='${kyc.id}'`)).toBe('mock-screening|clear')
    // gate on + a fresh clear: the withdrawal passes the request stage, finance approves and the payout is submitted
    const r = await requestWithdrawal(Pc, 1000)
    expect(r.status).toBe(201)
    wdPc = r.json.id
    expect((await approveWithdrawal(T, wdPc)).a2.json.approved).toBe(true)
    const sub = await submitWithdrawal(T, wdPc)
    expect(sub.status).toBeLessThan(300)
    expect(withdrawalState(T, wdPc)).toBe('submitted')
  })

  test('(c) a MOCK potential_match lands in needs_officer; the person\'s withdrawals are held (new request refused, approved payout parked) and nothing is lost', async () => {
    const vs = await secondVerification(T, Ps, 'SYNTH-SANCTION-')
    const vp = await secondVerification(T, Pp, 'SYNTH-PEP-')
    expect(vs).not.toBe(vp)
    const rs = await waitRequest(T, 'PA_C', Ps.personId, (r) => r.trigger === 'kyc_approval' && r.status === 'needs_officer', 'sanctions match needs_officer')
    expect(rs).toMatchObject({ outcome: 'potential_match', match_lists: ['sanctions'], resolved: false })
    const rp = await waitRequest(T, 'PA_C', Pp.personId, (r) => r.trigger === 'kyc_approval' && r.status === 'needs_officer', 'pep match needs_officer')
    expect(rp).toMatchObject({ outcome: 'potential_match', match_lists: ['pep'], resolved: false })
    // no hold row exists: the system never places a hold by itself; the gate holds on the unresolved match
    expect(n(sqlT(T.id, `SELECT count(*) FROM aml_holds WHERE tenant_id='${T.id}'`))).toBe(0)

    // (1) a NEW withdrawal request of a held person is refused and writes nothing
    const before = moneyCounts(T.id)
    const w0 = await wallet(Ps)
    const key = 'e2e-aml-held-' + Date.now()
    const held = await requestWithdrawal(Ps, 500, key)
    expect(held.status).toBe(409)
    expect(held.json.message).toContain('withdrawal requires a passed identity verification')
    expect(held.text).not.toMatch(/sanction|screening|aml|match/i) // no tipping off
    expect(moneyCounts(T.id)).toEqual(before)
    expect(await wallet(Ps)).toEqual(w0)
    // (2) the already APPROVED request is PARKED at the payout claim: not sent, not released, hold intact
    const sub = await submitWithdrawal(T, wdPs)
    expect(sub.status).toBeGreaterThanOrEqual(400)
    expect(withdrawalState(T, wdPs)).toBe('approved')
    expect(n(sqlT(T.id, `SELECT count(*) FROM payment_attempts WHERE withdrawal_request_id='${wdPs}'`))).toBe(0)
    expect((await wallet(Ps)).held).toBe(1000)
    const l = ledgerTotals(T.id)
    expect(l.debits).toBe(l.credits)
    expect(projectionDrift(T.id)).toBe(0)
  })

  test('(d) officer resolution through the UI: non-officer and tenant staff refused; sanctions CLEAR refused; quorum boundary; match recorded places the hold; replay idempotent', async ({ browser }) => {
    const rs = (await screeningRequests(T, 'PA_C', Ps.personId)).find((r) => r.status === 'needs_officer')!
    const before = counts(T)
    const rowBefore = reqRow(T, rs.id)

    // non-officer platform admin: the list is refused (rendered); the resolution API is refused
    const A = await ctx(browser, 'PA_A')
    await openScreening(A, T)
    await showRequests(A, Ps.personId)
    await expect(A.page.getByRole('alert').filter({ hasText: 'does not hold an in-force AML officer grant' })).toBeVisible()
    const resBody = { outcome: 'clear', lists: ['sanctions'], reason_code: 'e2e_try' }
    const path = (pid: string, rid: string) => `/v1/admin/tenants/${T.id}/screening/persons/${pid}/requests/${rid}/resolution`
    expect((await call('POST', path(Ps.personId, rs.id), await apiLogin('PA_A'), resBody)).status).toBe(403)
    // tenant staff on the platform-owned path (owner scope = platform): refused
    for (const k of [`${T.tag}_COMPLIANCE`, `${T.tag}_TENANT_ADMIN`]) {
      const r = await call('POST', path(Ps.personId, rs.id), await apiLogin(k), resBody)
      expect([403, 404]).toContain(r.status)
    }
    expect(counts(T)).toEqual(before)
    expect(reqRow(T, rs.id)).toBe(rowBefore)

    // officer PA_C: a sanctions-class request can NOT be cleared by an officer alone
    const C = await ctx(browser, 'PA_C')
    await openScreening(C, T)
    await showRequests(C, Ps.personId)
    const row = requestRow(C, rs.id)
    await expect(row).toContainText('Needs officer')
    await expect(row).toContainText('Holding withdrawals')
    await expect(row).toContainText('Sanctions list')
    let d = await fillResolve(C, row, { outcome: 'Clear (no match)', lists: ['Sanctions list'], reason: 'e2e_clear_try' })
    await expect(d.getByText('This request records a sanctions-list match. A clear will be refused by the server.')).toBeVisible()
    await submitResolve(C, d, () => expect(d.getByRole('alert').filter({ hasText: 'The resolution was refused.' })).toBeVisible())
    expect(counts(T)).toEqual(before) // nothing changed: no result, no hold, request unresolved
    expect(reqRow(T, rs.id)).toBe(rowBefore)
    await d.getByRole('button', { name: 'Cancel' }).click()

    // quorum boundary: with only ONE live officer a match (hold) cannot be recorded
    await revokeGrant(T, grantD)
    d = await fillResolve(C, requestRow(C, rs.id), { outcome: 'Potential match (places a hold)', lists: ['Sanctions list'], matches: ['Sanctions list'], reason: 'e2e_match_noquorum' })
    await submitResolve(C, d, () => expect(d.getByRole('alert')).toBeVisible())
    await expect(d.getByRole('alert')).toContainText(/officer quorum|resolution was refused|refused/i)
    expect(counts(T)).toEqual(before)
    await d.getByRole('button', { name: 'Cancel' }).click()
    grantD = await grantOfficer(T, 'PA_D') // quorum restored (a new grant; three distinct Persons)

    // the match is recorded: places the hold and resolves the request
    d = await fillResolve(C, requestRow(C, rs.id), { outcome: 'Potential match (places a hold)', lists: ['Sanctions list'], matches: ['Sanctions list'], reason: 'e2e_match_recorded' })
    await submitResolve(C, d, () => expect(C.page.getByRole('status').filter({ hasText: /Recorded\. Result .*A hold was placed/ })).toBeVisible())
    const after = counts(T)
    expect(after.holds).toBe(before.holds + 1)
    expect(after.results).toBe(before.results + 1)
    expect(activeHolds(T, Ps.personId)).toBe(1)
    const done = (await screeningRequests(T, 'PA_C', Ps.personId)).find((r) => r.id === rs.id)!
    expect(done.resolved).toBe(true)
    // replay of the same resolution is idempotent: no second hold or result
    const rep = await call('POST', path(Ps.personId, rs.id), await apiLogin('PA_C'), { outcome: 'potential_match', lists: ['sanctions'], match_lists: ['sanctions'], reason_code: 'e2e_match_recorded' })
    expect(rep.status).toBe(409) // a resolved request cannot be resolved again
    expect(counts(T)).toEqual(after)
  })

  test('(e) the hold is real and the funds are not trapped: the approved payout is rejected back to the player; a sanctions hold is lifted only by four-eyes (same officer twice refused)', async () => {
    // the officer hold now REJECTS the approved withdrawal at the payout claim and the hold is RELEASED back to the player's cash
    // (observed: HTTP 200 with the request rejected, audit withdrawal.rejected_kyc with a release ledger transaction)
    const sub = await submitWithdrawal(T, wdPs)
    console.log(`E2E-OBSERVED Ps payout claim after the officer hold: http=${sub.status} ${JSON.stringify(sub.json)}`)
    expect(withdrawalState(T, wdPs)).toBe('rejected')
    expect(n(sqlT(T.id, `SELECT count(*) FROM payment_attempts WHERE withdrawal_request_id='${wdPs}'`))).toBe(0) // never sent to the PSP
    const w = await wallet(Ps)
    expect(w.held).toBe(0)
    expect(w.cash).toBe(10000) // the funds are back with the player
    expect(w.available).toBe(10000)
    expect(sqlT(T.id, `SELECT count(*) FROM audit_log WHERE tenant_id='${T.id}' AND action='withdrawal.rejected_kyc' AND target_id='${wdPs}' AND metadata ? 'release_ledger_transaction'`)).toBe('1')
    const l = ledgerTotals(T.id)
    expect(l.debits).toBe(l.credits)
    expect(projectionDrift(T.id)).toBe(0)

    // single-officer lift of the sanctions hold is refused; four-eyes: PA_C proposes, PA_C cannot approve, PA_D approves
    const holdId = sqlT(T.id, `SELECT h.id FROM aml_holds h WHERE h.tenant_id='${T.id}' AND h.person_id='${Ps.personId}' AND NOT EXISTS (SELECT 1 FROM aml_hold_lifts l WHERE l.hold_id = h.id)`)
    const c = await apiLogin('PA_C')
    const direct = await call('POST', `/v1/admin/tenants/${T.id}/aml/holds/${holdId}/lift`, c, { reason_code: 'e2e_lift_alone' })
    expect(direct.status).toBeGreaterThanOrEqual(400)
    expect(sqlT(T.id, `SELECT count(*) FROM aml_hold_lifts WHERE hold_id='${holdId}'`)).toBe('0')
    const kase = await call('POST', `/v1/admin/tenants/${T.id}/aml/cases`, await apiLogin('PA_C'), { person_id: Ps.personId, kind: 'manual', reason_code: 'e2e_case' })
    expect(kase.status).toBe(201)
    const detail = await call('GET', `/v1/admin/tenants/${T.id}/aml/cases/${kase.json.case_id}`, await apiLogin('PA_C'))
    const tr = await call('POST', `/v1/admin/tenants/${T.id}/aml/cases/${kase.json.case_id}/transition`, await apiLogin('PA_C'), { expected_seq: detail.json.case.last_seq, to: 'in_review', reason_code: 'e2e_review' })
    expect(tr.status).toBe(200)
    const seq = tr.json.last_seq ?? detail.json.case.last_seq + 1
    const prop = await call('POST', `/v1/admin/tenants/${T.id}/aml/cases/${kase.json.case_id}/proposals`, await apiLogin('PA_C'), { expected_seq: seq, kind: 'lift_hold', hold_id: holdId, reason_code: 'e2e_lift_prop' })
    expect(prop.status).toBe(201)
    const self = await call('POST', `/v1/admin/tenants/${T.id}/aml/cases/${kase.json.case_id}/approvals`, await apiLogin('PA_C'), { proposal_seq: prop.json.proposal_seq, content_hash: prop.json.content_hash, reason_code: 'e2e_lift_self' })
    expect(self.status).toBeGreaterThanOrEqual(400)
    expect(sqlT(T.id, `SELECT count(*) FROM aml_hold_lifts WHERE hold_id='${holdId}'`)).toBe('0')
    const ok = await call('POST', `/v1/admin/tenants/${T.id}/aml/cases/${kase.json.case_id}/approvals`, await apiLogin('PA_D'), { proposal_seq: prop.json.proposal_seq, content_hash: prop.json.content_hash, reason_code: 'e2e_lift_ok' })
    expect(ok.status).toBe(200)
    expect(sqlT(T.id, `SELECT count(*) FROM aml_hold_lifts WHERE hold_id='${holdId}'`)).toBe('1')
  })

  test('(f) a non-sanctions (PEP) match is CLEARED by an officer through the UI and the held person\'s payout then proceeds: the route works end to end', async ({ browser }) => {
    const rp = (await screeningRequests(T, 'PA_C', Pp.personId)).find((r) => r.status === 'needs_officer')!
    expect((await submitWithdrawal(T, wdPp)).status).toBeGreaterThanOrEqual(400) // parked while unresolved
    expect(withdrawalState(T, wdPp)).toBe('approved')
    const D = await ctx(browser, 'PA_D')
    await openScreening(D, T)
    await showRequests(D, Pp.personId)
    const row = requestRow(D, rp.id)
    await expect(row).toContainText('PEP list')
    const d = await fillResolve(D, row, { outcome: 'Clear (no match)', lists: ['PEP list'], reason: 'e2e_pep_cleared' })
    await submitResolve(D, d, () => expect(D.page.getByRole('status').filter({ hasText: /Recorded\. Result / })).toBeVisible())
    const cleared = (await screeningRequests(T, 'PA_C', Pp.personId)).find((r) => r.id === rp.id)!
    expect(cleared.resolved).toBe(true)
    // the synthetic marker row is itself an approved verification, so it enqueued its own kyc_approval request (the same marker): clear it too, by API
    for (const other of (await screeningRequests(T, 'PA_C', Pp.personId)).filter((r) => r.status === 'needs_officer' && !r.resolved)) {
      const rr = await call('POST', `/v1/admin/tenants/${T.id}/screening/persons/${Pp.personId}/requests/${other.id}/resolution`, await apiLogin('PA_D'), { outcome: 'clear', lists: ['pep'], reason_code: 'e2e_pep_cleared' })
      expect(rr.status).toBe(201)
    }
    const sub = await submitWithdrawal(T, wdPp)
    expect(sub.status).toBeLessThan(300)
    expect(withdrawalState(T, wdPp)).toBe('submitted')
    // and a NEW request of the cleared person passes the request stage
    const r2 = await requestWithdrawal(Pp, 500)
    expect(r2.status).toBe(201)
    const l = ledgerTotals(T.id)
    expect(l.debits).toBe(l.credits)
    expect(projectionDrift(T.id)).toBe(0)
  })

  test('(g) below the application layer, as the RUNTIME role: cross-tenant claim/finish refused, direct writes refused, a forged resolution for another person refused', async () => {
    const other = sql(`SELECT id FROM tenants WHERE slug = '${loadWorld().tenantSlug}'`)
    const snapshot = () => ({ ...counts(T), terminal: sqlT(T.id, `SELECT string_agg(id||':'||status||':'||COALESCE(outcome,''), ',' ORDER BY id) FROM aml_screening_requests WHERE tenant_id='${T.id}' AND status IN ('done','needs_officer','failed_terminal')`) })
    const s0 = snapshot()
    const reqId = sqlT(T.id, `SELECT id FROM aml_screening_requests WHERE tenant_id='${T.id}' ORDER BY created_at LIMIT 1`)
    const bogus = '00000000-0000-4000-8000-000000000001'

    // claim / finish for T's queue with NO session tenant, and with ANOTHER tenant's session: refused by the tenant bind (AT002)
    for (const setup of ['', `SELECT set_config('app.tenant_id', '${other}', false);`]) {
      const claim = runtimeSql(`${setup}\nSELECT * FROM aml_screening_request_claim('${T.id}', 4, 30);`)
      expect(claim.ok).toBe(false)
      expect(claim.sqlstate).toBe('AT002')
      const fin = runtimeSql(`${setup}\nSELECT * FROM aml_screening_request_finish('${T.id}', '${reqId}', '${bogus}', 'clear', NULL, NULL, 'mock-screening', 'ref', 'subj', now(), NULL, NULL);`)
      expect(fin.ok).toBe(false)
      expect(fin.sqlstate).toBe('AT002')
    }
    // with the right tenant session but no claim token, finish applies nothing
    const own = runtimeSql(`SELECT set_config('app.tenant_id', '${T.id}', false);\nSELECT o_applied FROM aml_screening_request_finish('${T.id}', '${reqId}', '${bogus}', 'clear', NULL, NULL, 'mock-screening', 'ref', 'subj', now(), NULL, NULL);`)
    expect(own.ok).toBe(true)
    expect(own.out.split('\n').pop()).toBe('f')

    // direct INSERT / UPDATE / DELETE on the evidence tables: no privilege (42501), even WITH the tenant session
    const sess = `SELECT set_config('app.tenant_id', '${T.id}', false);`
    for (const stmt of [
      `INSERT INTO aml_holds (tenant_id) VALUES ('${T.id}');`,
      `INSERT INTO aml_hold_lifts (tenant_id) VALUES ('${T.id}');`,
      `UPDATE aml_holds SET reason_code = 'x' WHERE tenant_id = '${T.id}';`,
      `INSERT INTO aml_screening_results (tenant_id) VALUES ('${T.id}');`,
      `UPDATE aml_screening_results SET outcome = 'clear' WHERE tenant_id = '${T.id}';`,
      `UPDATE aml_screening_requests SET status = 'done', outcome = 'clear' WHERE tenant_id = '${T.id}';`,
      `DELETE FROM aml_screening_results WHERE tenant_id = '${T.id}';`,
    ]) {
      const r = runtimeSql(`${sess}\n${stmt}`)
      expect(r.ok, stmt).toBe(false)
      expect(r.sqlstate, stmt).toBe('42501')
    }
    expect(snapshot()).toEqual(s0)

    // forged resolution through the API: a request id that belongs to ANOTHER person under this person's path (S1 binding)
    const psReq = (await screeningRequests(T, 'PA_C', Ps.personId)).find((r) => r.status === 'needs_officer' || r.resolved)!
    const c = await apiLogin('PA_C')
    const forged = await call('POST', `/v1/admin/tenants/${T.id}/screening/persons/${Pc.personId}/requests/${psReq.id}/resolution`, c, { outcome: 'clear', lists: ['pep'], reason_code: 'e2e_forged' })
    expect(forged.status).toBe(404)
    const forged2 = await call('POST', `/v1/admin/tenants/${T.id}/screening/persons/${Ps.personId}/requests/${(await screeningRequests(T, 'PA_C', Pc.personId))[0].id}/resolution`, await apiLogin('PA_C'), { outcome: 'clear', lists: ['pep'], reason_code: 'e2e_forged' })
    expect(forged2.status).toBe(404)
    expect(snapshot()).toEqual(s0)
    console.log(`E2E-OBSERVED forged statuses: ${forged.status} ${JSON.stringify(forged.json)} / ${forged2.status} ${JSON.stringify(forged2.json)}`)
    void staffId
  })
})
