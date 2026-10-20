// Withdrawal restriction policy (lic11 / lic17) through the REAL stack (lic22): Back Office UI for the policy, the real API and
// the real database for the enforcement. Synthetic tenants and players only (cmd/seed-staging-world, the MOCK PSP and MOCK KYC).
// Ground truth is read from the database as the table owner with a tenant scope (lib/wd_db.ts).
import { expect, test, type Browser } from 'playwright/test'
import { apiLogin } from '../lib/api'
import { createLicence, licenceStatus, sql } from '../lib/db'
import { loginAs } from '../lib/session'
import { ledgerTotals, moneyCounts, n, projectionDrift, sqlT } from '../lib/wd_db'
import { decideChange, fillFiling, openFiledChange, openPolicy, openPolicyViaNav, submitFilingDialog } from '../lib/wd_ui'
import {
  approveAndSubmit, auditRows, bindOwnLicence, call, createFundedPlayer, createTenant, playerLogin, requestWithdrawal, unbindTenantLicence,
  wallet, wrpApprove, wrpChangeStatus, wrpFile, wrpPolicy, wrpVersions, type WdPlayer, type WdTenant,
} from '../lib/wd_world'
import { loadWorld } from '../lib/world'

const ctx = async (browser: Browser, key: string) => loginAs(await (await browser.newContext()).newPage(), key)
const paA = () => sql(`SELECT id FROM staff_users WHERE email = 'pa_a@e2e.invalid'`)

/** SUM(debits) = SUM(credits) over the whole tenant, and every balance projection equals the sum of its ledger entries. */
function expectLedgerSound(t: WdTenant) {
  const l = ledgerTotals(t.id)
  expect(l.entries).toBeGreaterThan(0)
  expect(l.debits).toBe(l.credits)
  expect(projectionDrift(t.id)).toBe(0)
}

test.describe.serial('09 withdrawal restriction policy: UI, enforcement through the API and the database', () => {
  test.setTimeout(600_000)
  let T: WdTenant
  let P: WdPlayer // player 1: licence-state scenarios
  let P2: WdPlayer // player 2: tenant suspension
  let firstWd = ''
  let parkedWd = ''

  test.beforeAll(async () => {
    T = await createTenant('wd1')
    P = await createFundedPlayer(T, 10000)
    P2 = await createFundedPlayer(T, 10000)
    // fund first, THEN make the licence suspended: the market gate needs the seeded licence while players are set up
    bindOwnLicence(T, 'suspended')
    expect(licenceStatus(T.licenceId)).toBe('suspended')
    expectLedgerSound(T)
  })

  test('(a) platform admin: effective policy is UNCONFIGURED (default allow); (i) default: withdrawal accepted as before, provenance default_unconfigured', async ({ browser, request }) => {
    const A = await ctx(browser, 'PA_A')
    await openPolicyViaNav(A, T)
    await expect(A.page.getByText('configured set version 0')).toBeVisible()
    expect(await A.page.getByText(/^UNCONFIGURED: no policy is recorded for this cause/).count()).toBeGreaterThanOrEqual(5) // page + 4 licence causes
    await expect(A.page.getByText('Default, unconfigured')).toHaveCount(12) // 4 causes x 3 stages
    await expect(A.page.getByText('Configured (set version')).toHaveCount(0)
    await expect(A.page.getByText('A policy can only ADD a refusal; "allow" never skips another gate.', { exact: false })).toBeVisible()
    // the same read through the API: every configurable cause is allow / default_unconfigured, set version 0
    const pol = await request.get(`${loadWorld().apiUrl}/v1/admin/tenants/${T.id}/withdrawal-restriction-policy`, { headers: { Authorization: `Bearer ${A.accessToken()}` } })
    const body = await pol.json()
    expect(pol.status()).toBe(200)
    expect(body.set_version).toBe(0)
    expect(body.configured).toBe(false)
    for (const c of body.causes.filter((x: any) => x.configurable)) {
      expect(c.unconfigured).toBe(true)
      for (const s of c.stages) { expect(s.action).toBe('allow'); expect(s.source).toBe('default_unconfigured') }
    }
    expect(wrpVersions(T.id)).toHaveLength(0)

    // (i) enforcement with the licence suspended and NO policy: accepted as before 0182
    const w0 = await wallet(P)
    const r = await requestWithdrawal(P, 1000)
    expect(r.status).toBe(201)
    expect(r.json.state).toBe('requested')
    firstWd = r.json.id
    const [req] = auditRows(T.id, 'withdrawal.requested', 1)
    expect(req.md.restriction_policy_source).toBe('default_unconfigured')
    expect(req.md.restriction_policy_set_version).toBe(0)
    expect(req.md.restriction_cause).toBe('licence_suspended')
    expect(req.md.restriction_licence_state).toBe('suspended')
    expect(auditRows(T.id, 'withdrawal.denied_by_restriction_policy')).toHaveLength(0)
    const w1 = await wallet(P)
    expect(w1.held).toBe(w0.held + 1000)
    expect(w1.available).toBe(w0.available - 1000)
    expectLedgerSound(T)
  })

  test('(b) TIGHTENING (block on licence_suspended) applies at once with one actor; DB shows the version with provenance; (ii) block refuses with WD056 and leaves no hold or ledger row', async ({ browser }) => {
    const A = await ctx(browser, 'PA_A')
    await openPolicy(A, T)
    const d = await fillFiling(A, 'Licence suspended', 'Block', 'e2e_block', 'e2e tightening, single actor')
    await expect(d.getByText(/Expected: a tightening or equal change/)).toBeVisible()
    await submitFilingDialog(A, d, /Applied at once\./)
    await expect.poll(() => wrpVersions(T.id).length, { timeout: 15000 }).toBe(1)
    const rows = wrpVersions(T.id)
    expect(rows).toHaveLength(1)
    expect(rows[0]).toMatchObject({ setVersion: 1, cause: 'licence_suspended', action: 'block', createdBy: paA() })
    expect(wrpChangeStatus(T.id, rows[0].changeId)).toBe('executed')
    // the change record: single actor, no approvals needed, requested = executed by PA_A
    expect(sql(`SELECT widening||'|'||required_approvals||'|'||requested_by||'|'||executed_by FROM withdrawal_restriction_policy_changes WHERE id='${rows[0].changeId}'`)).toBe(`false|0|${paA()}|${paA()}`)
    await expect(A.page.getByText('Configured (set version 1)').first()).toBeVisible()
    await expect(A.page.getByRole('cell', { name: 'e2e_block' })).toBeVisible() // version history shows the reason code

    // (ii) enforcement: refused, nothing written
    const before = moneyCounts(T.id)
    const ledgerBefore = ledgerTotals(T.id)
    const w0 = await wallet(P)
    const key = 'e2e-blocked-' + Date.now()
    const r = await requestWithdrawal(P, 1000, key)
    expect(r.status).toBe(403)
    expect(r.json.code).toBe('forbidden')
    expect(r.text).not.toMatch(/WD056|licence|restriction/i) // the player-facing answer is generic (no oracle)
    expect(moneyCounts(T.id)).toEqual(before)
    expect(ledgerTotals(T.id)).toEqual(ledgerBefore)
    expect(n(sqlT(T.id, `SELECT count(*) FROM withdrawal_requests WHERE idempotency_key='${key}'`))).toBe(0) // idempotency key not consumed
    expect(await wallet(P)).toEqual(w0)
    const [den] = auditRows(T.id, 'withdrawal.denied_by_restriction_policy', 1)
    expect(den.outcome).toBe('denied')
    expect(den.md).toMatchObject({ gate_code: 'WD056', restriction_cause: 'licence_suspended', restriction_action: 'block', restriction_policy_source: 'configured', restriction_policy_set_version: 1, restriction_licence_state: 'suspended', restriction_stage: 'request' })
    expect(den.md.restriction_policy_id).toBeTruthy()
    expectLedgerSound(T)
  })

  test('(c) LOOSENING (block -> hold_for_review) is pending; requester and same-Person account are refused; two distinct Persons approve -> executed; (iii) hold_for_review accepts and parks', async ({ browser, request }) => {
    const A = await ctx(browser, 'PA_A')
    await openPolicy(A, T)
    const d = await fillFiling(A, 'Licence suspended', 'Hold for review', 'e2e_loosen')
    await expect(d.getByText(/Expected: a LOOSENING change/)).toBeVisible()
    await submitFilingDialog(A, d, /Pending approval\. Nothing has changed yet/)
    const id = await openFiledChange(A)
    expect(wrpChangeStatus(T.id, id)).toBe('requested')
    expect(wrpVersions(T.id)).toHaveLength(1) // nothing changed yet
    expect(sql(`SELECT widening||'|'||required_approvals FROM withdrawal_restriction_policy_changes WHERE id='${id}'`)).toBe('true|2')

    // requester: UI hint AND server refusal
    await expect(A.page.locator('[aria-label="Decisions"]').getByRole('button', { name: 'Approve', exact: true })).toBeDisabled()
    const self = await request.post(`${loadWorld().apiUrl}/v1/admin/withdrawal-restriction-policy-changes/${id}/approve`, {
      headers: { Authorization: `Bearer ${A.accessToken()}` }, data: { reason_code: 'e2e_self' },
    })
    expect(self.status()).toBe(409)
    expect((await self.json()).code).toBe('WITHDRAWAL_RESTRICTION_POLICY_SELF_APPROVAL')
    // a second account of the SAME Person is refused too
    const A2 = await ctx(browser, 'PA_A2')
    await decideChange(A2, id, 'approve', 'e2e_sameperson', () => expect(A2.page.getByRole('alert').filter({ hasText: 'another account of the same person cannot either' })).toBeVisible())
    expect(wrpChangeStatus(T.id, id)).toBe('requested')
    // first distinct Person: recorded, still pending
    const B = await ctx(browser, 'PA_B')
    await decideChange(B, id, 'approve', 'e2e_ok_one', () => expect(B.page.getByText(/Approval recorded: 1 of 2\. Still pending approval/)).toBeVisible())
    expect(wrpChangeStatus(T.id, id)).toBe('requested')
    expect(wrpVersions(T.id)).toHaveLength(1)
    // the same Person's other account cannot give the second approval
    const B2 = await ctx(browser, 'PA_B2')
    await decideChange(B2, id, 'approve', 'e2e_dup_person', () => expect(B2.page.getByRole('alert').filter({ hasText: 'You (this person) already approved' })).toBeVisible())
    expect(wrpChangeStatus(T.id, id)).toBe('requested')
    // second distinct Person executes it
    const C = await ctx(browser, 'PA_C')
    await decideChange(C, id, 'approve', 'e2e_ok_two', () => expect(C.page.getByText('Approval recorded and the change was executed.')).toBeVisible())
    expect(wrpChangeStatus(T.id, id)).toBe('executed')
    const rows = wrpVersions(T.id)
    expect(rows).toHaveLength(2)
    expect(rows[1]).toMatchObject({ setVersion: 2, action: 'hold_for_review', changeId: id })
    expect(sql(`SELECT count(DISTINCT approver_person_id) FROM withdrawal_restriction_policy_approvals WHERE change_id='${id}'`)).toBe('2')

    // (iii) hold_for_review at the REQUEST stage: accepted with a hold, audit says so
    const w0 = await wallet(P)
    const r = await requestWithdrawal(P, 1000)
    expect(r.status).toBe(201)
    parkedWd = r.json.id
    const [held] = auditRows(T.id, 'withdrawal.held_for_restriction_review', 1)
    expect(held.md).toMatchObject({ withdrawal_request_id: parkedWd, restriction_action: 'hold_for_review', restriction_policy_source: 'configured', restriction_policy_set_version: 2, restriction_cause: 'licence_suspended' })
    expect((await wallet(P)).held).toBe(w0.held + 1000)
    expectLedgerSound(T)

    // dispatch: two distinct finance staff approve, the submit (T1p) is PARKED by the policy
    const { a1, a2, submit } = await approveAndSubmit(T, parkedWd)
    expect(a1.status).toBe(200); expect(a1.json.approved).toBe(false)
    expect(a2.status).toBe(200); expect(a2.json.approved).toBe(true)
    expect(submit.status).toBe(409)
    expect(submit.json.message).toContain("held under the tenant's withdrawal restriction policy")
    expect(sqlT(T.id, `SELECT state FROM withdrawal_requests WHERE id='${parkedWd}'`)).toBe('approved')
    expect(sqlT(T.id, `SELECT stage||'|'||allowed||'|'||gate_code||'|'||reason_code||'|'||restriction_policy_source||'|'||restriction_policy_set_version||'|'||restriction_cause||'|'||restriction_action FROM withdrawal_gate_decisions WHERE withdrawal_request_id='${parkedWd}' ORDER BY decided_at DESC LIMIT 1`))
      .toBe('dispatch|false|WD056|licence_suspended|configured|2|licence_suspended|hold_for_review')
    expect(n(sqlT(T.id, `SELECT count(*) FROM payment_attempts WHERE withdrawal_request_id='${parkedWd}'`))).toBe(0) // nothing was sent to the PSP
    // the hold is intact (not released, not paid): held_for_withdrawal covers both open requests
    expect((await wallet(P)).held).toBe(2000)
    expectLedgerSound(T)

    // the read API states what parked means and reports the parked count / age
    const ta = await apiLogin(`${T.tag}_TENANT_ADMIN`)
    const tp = await wrpPolicy(ta, T.id)
    expect(tp.status).toBe(200)
    expect(tp.json.parked).toMatchObject({ visible: true, count: 1 })
    expect(typeof tp.json.parked.oldest_parked_since).toBe('string')
    expect(tp.json.parked.oldest_age_seconds).toBeGreaterThanOrEqual(0)
    const hold = tp.json.action_semantics.find((x: any) => x.action === 'hold_for_review').meaning as string
    expect(hold).toContain('PARK, not a review queue')
    expect(hold).toContain('There is NO per-withdrawal release in this policy')
    expect(hold).toContain('not forfeited and not paid')
    const pa = await wrpPolicy(A.accessToken(), T.id)
    expect(pa.json.parked.visible).toBe(false) // decision rows are tenant-scoped: a platform caller is pointed at the alerts
  })

  test('(c2) NO release action exists for a parked withdrawal (NOT IMPLEMENTED by design, owner decision pending)', async ({ browser }) => {
    const A = await ctx(browser, 'PA_A')
    await openPolicy(A, T)
    // UI: no release / unpark control anywhere on the policy screen, and the dialog states what hold_for_review means
    await expect(A.page.getByRole('button', { name: /release|unpark|resume/i })).toHaveCount(0)
    const d = await fillFiling(A, 'Licence expired', 'Hold for review', 'e2e_probe')
    await expect(d.getByText('Request: accepted with a hold placed. Dispatch and re-dispatch: the payout is parked or escalated for human review.')).toBeVisible()
    await d.getByRole('button', { name: 'Close', exact: true }).last().click()
    // API: no release route exists (404), a staff reject of an APPROVED request is refused, the request and hold are unchanged
    const f1 = await apiLogin(`${T.tag}_FINANCE1`)
    for (const path of [`/v1/admin/withdrawals/${parkedWd}/release`, `/v1/admin/withdrawals/${parkedWd}/unpark`, `/v1/admin/withdrawal-restriction-policy/parked/${parkedWd}/release`]) {
      expect((await call('POST', path, f1, {})).status).toBe(404)
    }
    const rej = await call('POST', `/v1/admin/withdrawals/${parkedWd}/reject`, f1, { reason_code: 'e2e_probe' })
    expect(rej.status).toBe(409)
    expect(sqlT(T.id, `SELECT state FROM withdrawal_requests WHERE id='${parkedWd}'`)).toBe('approved')
    expect((await wallet(P)).held).toBe(2000)
    expectLedgerSound(T)
  })

  test('(d) tenant staff read their own effective policy and the parked summary but cannot change it (UI and server), and not another tenant', async ({ browser, request }) => {
    for (const key of [`${T.tag}_TENANT_ADMIN`, `${T.tag}_COMPLIANCE`]) {
      const S = await ctx(browser, key)
      await S.page.getByRole('navigation').getByRole('link', { name: 'Withdrawal Restrictions' }).click()
      await expect(S.page).toHaveURL(new RegExp(`/withdrawal-restrictions/tenants/${T.id}$`)) // straight to the OWN tenant
      await expect(S.page.getByText('Configured (set version 2)').first()).toBeVisible()
      await expect(S.page.getByText('Your role can read this policy. Only platform administrators can file or decide changes.')).toBeVisible()
      await expect(S.page.getByRole('button', { name: 'File a change' })).toHaveCount(0)
      // server: own policy readable incl. the parked summary, a filing is refused, another tenant's policy is not visible, nothing written
      const tok = S.accessToken()
      const own = await request.get(`${loadWorld().apiUrl}/v1/admin/tenants/${T.id}/withdrawal-restriction-policy`, { headers: { Authorization: `Bearer ${tok}` } })
      expect(own.status()).toBe(200)
      expect((await own.json()).parked).toMatchObject({ visible: true, count: 1 })
      const before = n(sql(`SELECT count(*) FROM withdrawal_restriction_policy_changes`))
      const file = await request.post(`${loadWorld().apiUrl}/v1/admin/withdrawal-restriction-policy-changes`, { headers: { Authorization: `Bearer ${tok}` }, data: { tenant_id: T.id, cause: 'licence_suspended', action: 'allow', reason_code: 'e2e_tenant_try' } })
      expect(file.status()).toBe(403)
      expect(n(sql(`SELECT count(*) FROM withdrawal_restriction_policy_changes`))).toBe(before)
      const other = await request.get(`${loadWorld().apiUrl}/v1/admin/tenants/${sql(`SELECT id FROM tenants WHERE slug='${loadWorld().tenantSlug}'`)}/withdrawal-restriction-policy`, { headers: { Authorization: `Bearer ${tok}` } })
      expect([403, 404]).toContain(other.status())
      const list = await request.get(`${loadWorld().apiUrl}/v1/admin/withdrawal-restriction-policy-changes`, { headers: { Authorization: `Bearer ${tok}` } })
      expect(list.status()).toBe(403) // the change list is platform-only
    }
    expect(wrpVersions(T.id)).toHaveLength(2)
  })

  test('(e) reject by another Person and cancel by the requester: policy unchanged, requests closed', async ({ browser }) => {
    const A = await ctx(browser, 'PA_A')
    await openPolicy(A, T)
    let d = await fillFiling(A, 'Licence suspended', 'Allow', 'e2e_reject_me')
    await submitFilingDialog(A, d, /Pending approval/)
    const rejId = await openFiledChange(A)
    const B = await ctx(browser, 'PA_B')
    await decideChange(B, rejId, 'reject', 'e2e_not_ok', () => expect(B.page.getByText('Rejected. The request is closed and nothing was changed.')).toBeVisible())
    expect(wrpChangeStatus(T.id, rejId)).toBe('rejected')

    await openPolicy(A, T)
    d = await fillFiling(A, 'Licence suspended', 'Allow', 'e2e_cancel_me')
    await submitFilingDialog(A, d, /Pending approval/)
    const canId = await openFiledChange(A)
    await decideChange(A, canId, 'cancel', '', () => expect(A.page.getByText('Cancelled. The request is closed and nothing was changed.')).toBeVisible())
    expect(wrpChangeStatus(T.id, canId)).toBe('cancelled')
    // nothing was changed: still two versions, hold_for_review in force; the parked withdrawal is untouched
    expect(wrpVersions(T.id).map((r) => r.action)).toEqual(['block', 'hold_for_review'])
    expect(sqlT(T.id, `SELECT state FROM withdrawal_requests WHERE id='${parkedWd}'`)).toBe('approved')
  })

  test('(v) a configured ALLOW never overrides a hard restriction (suspended player account), (iv) tenant suspension refuses whatever the policy says', async ({ browser }) => {
    // configure ALLOW (loosening: two distinct Persons, through the API: the UI flow is covered above)
    const a = await apiLogin('PA_A')
    const f = await wrpFile(a, T.id, 'licence_suspended', 'allow', 'e2e_allow')
    expect(f.status).toBe(201)
    expect(f.json.executed).toBe(false)
    const id = f.json.change.id
    expect((await wrpApprove(await apiLogin('PA_B'), id)).status).toBe(200)
    expect((await wrpApprove(await apiLogin('PA_C'), id)).status).toBe(200)
    expect(wrpVersions(T.id).map((r) => r.action)).toEqual(['block', 'hold_for_review', 'allow'])

    // licence still suspended + configured allow -> accepted
    await playerLogin(T, P)
    const ok = await requestWithdrawal(P, 500)
    expect(ok.status).toBe(201)
    const [rq] = auditRows(T.id, 'withdrawal.requested', 1)
    expect(rq.md).toMatchObject({ restriction_policy_source: 'configured', restriction_policy_set_version: 3, restriction_action: 'allow', restriction_cause: 'licence_suspended' })

    // hard restriction: the tenant suspends the player account; ALLOW must not skip that gate
    const ta = await apiLogin(`${T.tag}_TENANT_ADMIN`)
    const sus = await call('POST', `/v1/admin/players/${P.accountId}/suspend`, ta, { reason: 'e2e_hard_restriction' })
    expect(sus.status).toBeLessThan(300)
    expect(sqlT(T.id, `SELECT status FROM player_accounts WHERE id='${P.accountId}'`)).not.toBe('active')
    const before = moneyCounts(T.id)
    const ledgerBefore = ledgerTotals(T.id)
    const key = 'e2e-hard-' + Date.now()
    const bad = await requestWithdrawal(P, 500, key)
    expect(bad.status).toBe(403)
    // refused by the HARD gate (WD050, account not active), not by the policy: the configured ALLOW did not skip it
    const [rg] = auditRows(T.id, 'withdrawal.denied_by_rg', 1)
    expect(rg.md).toMatchObject({ gate_code: 'WD050', reason_code: 'player_account_not_active', amount: 500 })
    expect(auditRows(T.id, 'withdrawal.denied_by_restriction_policy', 10).every((r) => r.md.restriction_policy_set_version === 1)).toBe(true) // no new WD056 denial
    expect(n(sqlT(T.id, `SELECT count(*) FROM withdrawal_requests WHERE idempotency_key='${key}'`))).toBe(0)
    expect(moneyCounts(T.id).requests).toBe(before.requests)
    expect(ledgerTotals(T.id)).toEqual(ledgerBefore)
    expectLedgerSound(T)

    // (iv) tenant suspension (owner-decided, not configurable): refused even with ALLOW in force. Player 2 logs in while active.
    await playerLogin(T, P2)
    const sup = await call('POST', `/v1/admin/tenants/${T.id}/suspend`, a, { reason_code: 'compliance_hold' })
    expect(sup.status).toBe(200)
    expect(sql(`SELECT status FROM tenants WHERE id='${T.id}'`)).toBe('suspended')
    const cb = moneyCounts(T.id)
    const key2 = 'e2e-tenant-susp-' + Date.now()
    const ts = await requestWithdrawal(P2, 500, key2)
    expect(ts.status).toBeGreaterThanOrEqual(400)
    expect(ts.json?.code).toBe('TENANT_OR_BRAND_NOT_ACTIVE')
    expect(n(sqlT(T.id, `SELECT count(*) FROM withdrawal_requests WHERE idempotency_key='${key2}'`))).toBe(0)
    expect(moneyCounts(T.id).requests).toBe(cb.requests)
    // the policy itself is unaffected by the refusal; no WD056 denial was written for this request
    expect(wrpVersions(T.id).at(-1)?.action).toBe('allow')
    expectLedgerSound(T)
      })
})

test.describe.serial('09b operating-basis reliance (F-1): the block fires when the tenant relies ONLY on a platform-licence operating basis', () => {
  test.setTimeout(600_000)
  test('licence_id NULL + platform_licence basis whose licence is suspended: default allows (cause read through the basis), configured block refuses with WD056', async ({ request }) => {
    const T2 = await createTenant('wd2')
    const P = await createFundedPlayer(T2, 10000)
    // the licence the tenant will rely on ONLY through an approved operating basis
    const lic = createLicence('E2E-WD-BASIS', 'active')
    unbindTenantLicence(T2)
    expect(sql(`SELECT COALESCE(licence_id::text,'NULL') FROM tenants WHERE id='${T2.id}'`)).toBe('NULL')
    // the basis is filed and approved through the real governed route (two distinct Persons)
    const a = await apiLogin('PA_A')
    const payload = {
      tenant_id: T2.id, brand_id: null, country_code: 'MT', operation_code: 'wagering', product_code: null, basis_kind: 'platform_licence', licence_id: lic.id,
      jurisdiction_id: null, determination_reference: 'E2E-DET-MT-wagering-wd2', determined_by_role: 'head_of_compliance', basis_evidence_reference: null,
      restrictions_note: null, restricted_product_codes: [], review_due_at: null,
    }
    const f = await call('POST', '/v1/admin/licensing-changes', a, { kind: 'operating_basis_set', reason_code: 'e2e_setup', payload })
    expect(f.status).toBe(201)
    const cid = f.json.change_request.id
    expect((await call('POST', `/v1/admin/licensing-changes/${cid}/approve`, await apiLogin('PA_B'), { reason_code: 'e2e_ok' })).status).toBe(200)
    expect((await call('POST', `/v1/admin/licensing-changes/${cid}/approve`, await apiLogin('PA_C'), { reason_code: 'e2e_ok' })).status).toBe(200)
    expect(sqlT(T2.id, `SELECT status||'|'||basis_kind||'|'||licence_id FROM operating_bases WHERE tenant_id='${T2.id}' AND effective_to IS NULL`)).toBe(`active|platform_licence|${lic.id}`)
    // the relied licence is suspended afterwards (a narrowing change: single actor, applied at once)
    const s = await call('POST', '/v1/admin/licensing-changes', a, { kind: 'licence_status_change', reason_code: 'e2e_suspend', payload: { licence_id: lic.id, status: 'suspended' } })
    expect(s.status).toBe(201)
    expect(licenceStatus(lic.id)).toBe('suspended')

    // no policy: allowed as before, but the provenance shows the cause was found THROUGH THE BASIS (tenants.licence_id is NULL)
    const w = await requestWithdrawal(P, 1000)
    expect(w.status).toBe(201)
    const [req] = auditRows(T2.id, 'withdrawal.requested', 1)
    expect(req.md).toMatchObject({ restriction_policy_source: 'default_unconfigured', restriction_policy_set_version: 0, restriction_cause: 'licence_suspended', restriction_licence_state: 'suspended' })

    // configured block (tightening: single actor)
    const b = await wrpFile(a, T2.id, 'licence_suspended', 'block', 'e2e_block_basis')
    expect(b.status).toBe(201)
    expect(b.json.executed).toBe(true)
    const before = moneyCounts(T2.id)
    const key = 'e2e-basis-block-' + Date.now()
    const r = await requestWithdrawal(P, 1000, key)
    expect(r.status).toBe(403)
    expect(moneyCounts(T2.id)).toEqual(before)
    expect(n(sqlT(T2.id, `SELECT count(*) FROM withdrawal_requests WHERE idempotency_key='${key}'`))).toBe(0)
    const [den] = auditRows(T2.id, 'withdrawal.denied_by_restriction_policy', 1)
    expect(den.md).toMatchObject({ gate_code: 'WD056', restriction_cause: 'licence_suspended', restriction_action: 'block', restriction_policy_source: 'configured', restriction_licence_state: 'suspended' })
    expect(sql(`SELECT COALESCE(licence_id::text,'NULL') FROM tenants WHERE id='${T2.id}'`)).toBe('NULL')
    const l = ledgerTotals(T2.id)
    expect(l.debits).toBe(l.credits)
    expect(projectionDrift(T2.id)).toBe(0)
    void request
  })
})
