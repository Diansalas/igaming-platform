// Residence determination maker-checker through the REAL stack (lic21): real Back Office UI as tenant compliance staff, real
// platform-api, real PostgreSQL, plus BELOW-THE-APPLICATION-LAYER bypass attempts with the RUNTIME database role (the role
// platform-api itself uses; connection URL from the harness environment). Each attempt asserts the SQLSTATE and that nothing in
// the database changed (a fingerprint of the verification row, the three residence tables and their audit rows; every attempt also
// runs inside a transaction that is rolled back). Everything is synthetic; the countries are test inputs, not any market decision.
// Serial: one tenant, one mode lifecycle: none -> all (at once) -> determination pending -> approvals -> into_open_market (two approvals).
import { expect, test, type Browser } from 'playwright/test'
import { apiLogin } from '../lib/api'
import { lit, newUuid, ownerRead, runtimeTx } from '../lib/kyc_db'
import { determinationApproveProof, digest } from '../lib/kyc_proof'
import {
  approveDeterminationDialog, approvePolicyDialog, checks, expectRefusal, fileMode, fingerprint, modeCard, modeInForce, openResidence, policyApprovers, policyVersions, queueRow, residenceAudit,
  verification, versionRow,
} from '../lib/kyc_residence'
import {
  ensureResidenceCollection, ensureTenantStaff, kapi, marketEvidence, newPlayer, reviewVerification, staffId, staffPerson, startVerification, tenantIdOf, type Player,
} from '../lib/kyc_world'
import { loginAs, type Session } from '../lib/session'

test.describe.configure({ mode: 'serial' })

const ctx = async (browser: Browser, key: string): Promise<Session> => loginAs(await (await browser.newContext()).newPage(), key)
const pending = async (token: string) => (await kapi('GET', '/v1/admin/kyc/residence-determinations/pending', undefined, token)).json as Array<Record<string, any>>

let tid = ''
let r1: Player // determination under mode "all"
let r2: Player // determination under mode "into_open_market", market served
let r3: Player // determination under mode "into_open_market", market not served
let v1 = ''
let v2 = ''
let v3 = ''

test.beforeAll(async () => {
  test.setTimeout(480_000)
  tid = tenantIdOf()
  // Tenant compliance accounts (owner fixture inserts + real enrolment): C1B shares the Person of T_COMPLIANCE (the determiner),
  // C2 and C3 are distinct Persons. The account that shares the PLAYER's Person is created below once the player exists.
  await ensureTenantStaff('T_C1B', { role: 'compliance', person: { sameAs: 'T_COMPLIANCE' } })
  await ensureTenantStaff('T_C2', { role: 'compliance', person: 'new' })
  await ensureTenantStaff('T_C3', { role: 'compliance', person: 'new' })
  const c1 = await apiLogin('T_COMPLIANCE')
  await ensureResidenceCollection(c1) // a residence determination needs this tenant switch (Stage 4I Phase B), a precondition and not under test

  r1 = await newPlayer('res-r1')
  v1 = await startVerification(r1)
  r2 = await newPlayer('res-r2')
  v2 = await startVerification(r2)
  r3 = await newPlayer('res-r3')
  v3 = await startVerification(r3)
  // v2 and v3 stay PENDING at the vendor; their review (approval plus residence) happens in test (h)
  const playerPerson = verification(v1).person
  expect(playerPerson).toMatch(/^[0-9a-f-]{36}$/)
  await ensureTenantStaff('T_CP', { role: 'compliance', person: { uuid: playerPerson } }) // a compliance account OF THE PLAYER'S OWN Person
  expect(staffPerson('T_CP')).toBe(playerPerson)
  expect(staffPerson('T_C1B')).toBe(staffPerson('T_COMPLIANCE'))
  expect(new Set([staffPerson('T_COMPLIANCE'), staffPerson('T_C2'), staffPerson('T_C3'), playerPerson]).size).toBe(4)
})

test('(a) default state: mode None, no policy history, nothing waiting', async ({ browser }) => {
  const C1 = await ctx(browser, 'T_COMPLIANCE')
  await openResidence(C1)
  await expect(C1.page.getByText('None: a single reviewer')).toBeVisible()
  await expect(C1.page.getByText('No mode has ever been set for this tenant.')).toBeVisible()
  await expect(C1.page.getByText('No determination is waiting for a second Person.')).toBeVisible()
  expect(policyVersions()).toEqual([])
  expect(modeInForce()).toBe('none')
})

test('(b) a STRICTER mode (none -> all) is one actor and applies at once', async ({ browser }) => {
  const C1 = await ctx(browser, 'T_COMPLIANCE')
  await openResidence(C1)
  await fileMode(C1, 'All', 'e2e_mode_all', /stricter mode/, async () =>
    expect(modeCard(C1).getByRole('status').filter({ hasText: 'Applied at once: the mode in force is now "All"' })).toBeVisible({ timeout: 20000 }),
  )
  const v = policyVersions()
  expect(v).toHaveLength(1)
  expect(v[0]).toMatchObject({ mode: 'all', previous: 'none', status: 'active', required: 0, approvals: 0, requestedBy: staffId('T_COMPLIANCE') })
  expect(modeInForce()).toBe('all')
  expect(residenceAudit('residence_policy.changed')).toBe(1)
})

test('(c) a determination under mode "all" is PENDING: shown as "Not market evidence" and NOT returned by the market evidence read', async ({ browser }) => {
  // the reviewer (C1) approves the verification and records residence "MT" through the real review route
  const c1 = await apiLogin('T_COMPLIANCE')
  const rv = await reviewVerification(c1, v1, 'approved', 'MT')
  expect(rv.status).toBe(200)
  expect(verification(v1)).toMatchObject({ country: 'MT', status: 'approved', setBy: staffId('T_COMPLIANCE') })
  const c = checks(v1)
  expect(c).toHaveLength(1)
  expect(c[0]).toMatchObject({ approvalRequired: true, mode: 'all', approvedBy: '', approvedAt: '', determinedBy: staffId('T_COMPLIANCE'), determinedByPerson: staffPerson('T_COMPLIANCE'), country: 'MT' })

  // the REAL evidence read (kyc.ReadMarketResidenceEvidence, runtime role) excludes it
  expect(marketEvidence(tid, r1.id)).toEqual({ ok: false, country: '' })

  // the list API carries ids and times only: never the country
  const list = await pending(c1)
  expect(list.map((x) => x.verification_id)).toContain(v1)
  expect(JSON.stringify(list)).not.toContain('"MT"')
  expect(Object.keys(list.find((x) => x.verification_id === v1)!).sort()).toEqual(['brand_id', 'determined_at', 'determined_by', 'player_account_id', 'policy_mode', 'verification_id'])

  const C1 = await ctx(browser, 'T_COMPLIANCE')
  await openResidence(C1)
  const row = queueRow(C1, v1)
  await expect(row).toContainText('Not market evidence')
  await expect(row).toContainText('All') // the mode that required approval
  await expect(C1.page.getByText('A pending determination is NOT market evidence.')).toBeVisible()
})

test('(d) approval by the DETERMINER, by another account of the determiner\'s Person, and by an account of the PLAYER\'s Person are refused (UI + server + DB)', async ({ browser }) => {
  const before = fingerprint(v1)

  // the determiner (C1): the UI disables the button; the server refuses the call anyway
  const C1 = await ctx(browser, 'T_COMPLIANCE')
  await openResidence(C1)
  await expect(queueRow(C1, v1).getByRole('button', { name: 'Approve as second Person' })).toBeDisabled()
  const item = (await pending(C1.accessToken())).find((x) => x.verification_id === v1)!
  const own = await kapi('POST', `/v1/admin/kyc/verifications/${v1}/residence-approval`, { determined_at: item.determined_at, reason_code: 'e2e_self_approve' }, await apiLogin('T_COMPLIANCE'))
  expect(own.status).toBe(403)
  expect(own.json.message).toContain('four-eyes')

  // another ACCOUNT of the determiner's Person (C1B): the UI cannot know, so the button is enabled and the server/DB refuse
  const C1B = await ctx(browser, 'T_C1B')
  await openResidence(C1B)
  await expect(queueRow(C1B, v1).getByRole('button', { name: 'Approve as second Person' })).toBeEnabled()
  let dlg = await approveDeterminationDialog(C1B, v1, 'e2e_same_person', async (d) => expectRefusal(d, 403))
  await dlg.getByRole('button', { name: 'Close', exact: true }).last().click()

  // an account of the PLAYER's own Person
  const CP = await ctx(browser, 'T_CP')
  await openResidence(CP)
  dlg = await approveDeterminationDialog(CP, v1, 'e2e_player_person', async (d) => expectRefusal(d, 403))
  const viaApi = await kapi('POST', `/v1/admin/kyc/verifications/${v1}/residence-approval`, { determined_at: item.determined_at, reason_code: 'e2e_player_person' }, await apiLogin('T_CP'))
  expect(viaApi.status).toBe(403)

  // nothing changed anywhere, the determination is still pending and still not evidence
  expect(fingerprint(v1)).toBe(before)
  expect(checks(v1)[0].approvedAt).toBe('')
  expect(marketEvidence(tid, r1.id).ok).toBe(false)
  expect(residenceAudit('kyc.residence_determination_approved')).toBe(0)
})

test('(e) BELOW the API (runtime role): the residence guards, the privileges and the signed-proof verifier refuse every bypass', async () => {
  const v = verification(v1)
  const at = v.setAtMicro
  const reason = 'e2e_forged'
  const before = fingerprint(v1)
  const tenantOnly = { 'app.tenant_id': tid }
  const asStaff = (key: string, extra: Record<string, string> = {}) => ({ 'app.tenant_id': tid, 'app.principal_id': staffId(key), ...extra })
  const approve = (verification = v1, when = at, rc = reason, tenant = tid) =>
    `SELECT o_check_id FROM residence_determination_approve(${lit(tenant)}::uuid, ${lit(verification)}::uuid, ${lit(when)}::timestamptz, ${lit(rc)})`
  const proof = (key: string, o: Parameters<typeof determinationApproveProof>[1] = {}, over: Parameters<typeof determinationApproveProof>[2] = {}) =>
    determinationApproveProof({ tenant: tid, actor: staffId(key), verification: v1, determinedAtMicro: at, reason }, o, over)
  const attempts: Array<{ name: string; settings: Record<string, string>; sql: string; want: string }> = [
    // 1. direct writes to the verification's residence columns without a new determination (S3 guard)
    { name: 'UPDATE country only', settings: tenantOnly, sql: `UPDATE kyc_verifications SET verified_residence_country = 'DE' WHERE id = ${lit(v1)}`, want: 'RD005' },
    { name: 'UPDATE country to NULL (refused by a table CHECK before the trigger)', settings: tenantOnly, sql: `UPDATE kyc_verifications SET verified_residence_country = NULL WHERE id = ${lit(v1)}`, want: '23514' },
    { name: 'UPDATE country + source only', settings: tenantOnly, sql: `UPDATE kyc_verifications SET verified_residence_country = 'DE', verified_residence_source = 'reviewer_determination' WHERE id = ${lit(v1)}`, want: 'RD005' },
    { name: 'UPDATE country with a BACKDATED determination time', settings: tenantOnly, sql: `UPDATE kyc_verifications SET verified_residence_country = 'DE', verified_residence_set_at = verified_residence_set_at - interval '1 day' WHERE id = ${lit(v1)}`, want: 'RD005' },
    { name: 'UPDATE country with a FUTURE-dated determination time (+1 day)', settings: tenantOnly, sql: `UPDATE kyc_verifications SET verified_residence_country = 'DE', verified_residence_source = 'reviewer_determination', verified_residence_set_by = ${lit(staffId('T_COMPLIANCE'))}, verified_residence_set_at = clock_timestamp() + interval '1 day' WHERE id = ${lit(v1)}`, want: 'RD005' },
    // 1b. lic23 SN-6 / migration 0186: rewriting the verification so the evidence read ignores it (orphaning the newest verification) is refused
    { name: 'UPDATE status approved -> pending (terminal status is final)', settings: tenantOnly, sql: `UPDATE kyc_verifications SET status = 'pending' WHERE id = ${lit(v1)}`, want: 'KV001' },
    { name: 'UPDATE status approved -> review_required', settings: tenantOnly, sql: `UPDATE kyc_verifications SET status = 'review_required' WHERE id = ${lit(v1)}`, want: 'KV001' },
    { name: 'UPDATE provider_reference to NULL (orphan the newest verification)', settings: tenantOnly, sql: `UPDATE kyc_verifications SET provider_reference = NULL WHERE id = ${lit(v1)}`, want: 'KV002' },
    { name: 'UPDATE provider_reference to another value', settings: tenantOnly, sql: `UPDATE kyc_verifications SET provider_reference = 'forged-ref' WHERE id = ${lit(v1)}`, want: 'KV002' },
    { name: 'UPDATE created_at (reorder the newest verification)', settings: tenantOnly, sql: `UPDATE kyc_verifications SET created_at = created_at - interval '1 day' WHERE id = ${lit(v1)}`, want: 'KV002' },
    // 2. direct writes to the residence tables: no privilege at all
    { name: 'INSERT residence_approval_policies', settings: tenantOnly, sql: `INSERT INTO residence_approval_policies (tenant_id) VALUES (${lit(tid)})`, want: '42501' },
    { name: 'UPDATE residence_approval_policies', settings: tenantOnly, sql: `UPDATE residence_approval_policies SET status = 'active'`, want: '42501' },
    { name: 'SELECT residence_approval_policies', settings: tenantOnly, sql: `SELECT count(*) FROM residence_approval_policies`, want: '42501' },
    { name: 'DELETE residence_approval_policies', settings: tenantOnly, sql: `DELETE FROM residence_approval_policies`, want: '42501' },
    { name: 'INSERT residence_approval_policy_approvals', settings: tenantOnly, sql: `INSERT INTO residence_approval_policy_approvals (tenant_id) VALUES (${lit(tid)})`, want: '42501' },
    { name: 'UPDATE residence_approval_policy_approvals', settings: tenantOnly, sql: `UPDATE residence_approval_policy_approvals SET reason_code = 'x'`, want: '42501' },
    { name: 'INSERT residence_determination_checks', settings: tenantOnly, sql: `INSERT INTO residence_determination_checks (tenant_id) VALUES (${lit(tid)})`, want: '42501' },
    { name: 'UPDATE residence_determination_checks (forge an approval)', settings: tenantOnly, sql: `UPDATE residence_determination_checks SET approved_by = ${lit(staffId('T_C2'))}, approved_at = now() WHERE verification_id = ${lit(v1)}`, want: '42501' },
    { name: 'DELETE residence_determination_checks', settings: tenantOnly, sql: `DELETE FROM residence_determination_checks WHERE verification_id = ${lit(v1)}`, want: '42501' },
    { name: 'TRUNCATE residence_determination_checks', settings: tenantOnly, sql: `TRUNCATE residence_determination_checks`, want: '42501' },
    // 3. the definer function with no valid proof, from a session that otherwise looks right (a compliance officer of the tenant, C2)
    { name: 'approval function, NO proof', settings: asStaff('T_C2'), sql: approve(), want: 'AP001' },
    { name: 'approval function, forged MAC', settings: asStaff('T_C2', { 'app.actor_proof': proof('T_C2', { badMac: true }) }), sql: approve(), want: 'AP002' },
    { name: 'approval function, unknown key id', settings: asStaff('T_C2', { 'app.actor_proof': proof('T_C2', { kid: 'nokey1' }) }), sql: approve(), want: 'AP002' },
    { name: 'approval function, expired proof', settings: asStaff('T_C2', { 'app.actor_proof': proof('T_C2', { iatOffset: -120 }) }), sql: approve(), want: 'AP003' },
    { name: 'approval function, proof for ANOTHER verification', settings: asStaff('T_C2', { 'app.actor_proof': proof('T_C2', {}, { target: v2 }) }), sql: approve(), want: 'AP004' },
    { name: 'approval function, proof bound to ANOTHER reason', settings: asStaff('T_C2', { 'app.actor_proof': proof('T_C2', {}, { payloadHash: determinationApproveProofHash(tid, v1, at, 'another_reason') }) }), sql: approve(), want: 'AP004' },
    { name: 'approval function, proof signed for ANOTHER actor (C3)', settings: asStaff('T_C2', { 'app.actor_proof': proof('T_C3') }), sql: approve(), want: 'AP004' },
    // 4. a VALID proof, but the person rules (the proof does not authorise a person the database forbids)
    { name: 'valid proof, the DETERMINER (C1)', settings: asStaff('T_COMPLIANCE', { 'app.actor_proof': proof('T_COMPLIANCE') }), sql: approve(), want: 'RD004' },
    { name: 'valid proof, another account of the DETERMINER\'s Person (C1B)', settings: asStaff('T_C1B', { 'app.actor_proof': proof('T_C1B') }), sql: approve(), want: 'RD004' },
    { name: 'valid proof, an account of the PLAYER\'s own Person (CP)', settings: asStaff('T_CP', { 'app.actor_proof': proof('T_CP') }), sql: approve(), want: 'RD004' },
    { name: 'valid proof, wrong ROLE (finance)', settings: asStaff('T_FINANCE1', { 'app.actor_proof': proof('T_FINANCE1') }), sql: approve(), want: 'RD001' },
    { name: 'valid proof, session tenant is not the requested tenant', settings: asStaff('T_C2', { 'app.actor_proof': proof('T_C2') }), sql: approve(v1, at, reason, newUuid()), want: 'RD001' },
    { name: 'valid proof, a STALE determination time (one microsecond off)', settings: asStaff('T_C2', { 'app.actor_proof': determinationApproveProof({ tenant: tid, actor: staffId('T_C2'), verification: v1, determinedAtMicro: bump(at), reason }) }), sql: approve(v1, bump(at)), want: 'RD040' },
  ]
  const outcomes: string[] = []
  const wrong: string[] = []
  for (const a of attempts) {
    const r = runtimeTx(a.settings, a.sql)
    outcomes.push(`${a.name} -> ${r.sqlstate || 'NO ERROR'}`)
    if (r.sqlstate !== a.want) wrong.push(`${a.name}: want ${a.want}, got ${r.sqlstate || 'NO ERROR'}`)
    if (fingerprint(v1) !== before) wrong.push(`${a.name}: the database changed`)
  }
  expect(wrong).toEqual([])

  // CONTROL (rolled back): the identical call with a valid proof from a Person-distinct compliance officer succeeds. The refusals
  // above are therefore caused by the specific control each one targets, not by a broken probe.
  const ok = runtimeTx(asStaff('T_C2', { 'app.actor_proof': proof('T_C2') }), `${approve()};\nSELECT approved_by IS NOT NULL FROM residence_determination_checks WHERE verification_id = ${lit(v1)}`)
  expect(ok.sqlstate).toBe('')
  expect(ok.out.split('\n').pop()).toBe('t')
  expect(fingerprint(v1)).toBe(before) // rolled back

  // CHARACTERISATION (documented residual, lic10 section 3 / lic18 section 7; not a claim of protection): a holder of the runtime
  // credential can write a new determination row directly, since the database cannot prove the writer is a reviewer. Under mode
  // "all" it lands PENDING with approval required, so it is NOT market evidence until a second Person approves it.
  const forged = runtimeTx(tenantOnly, `UPDATE kyc_verifications SET verified_residence_country = 'DE', verified_residence_source = 'reviewer_determination', verified_residence_set_by = ${lit(staffId('T_COMPLIANCE'))}, verified_residence_set_at = clock_timestamp() WHERE id = ${lit(v1)};
    SELECT approval_required::text || '|' || policy_mode || '|' || (approved_at IS NULL)::text FROM residence_determination_checks WHERE verification_id = ${lit(v1)} AND country = 'DE'`)
  expect(forged.sqlstate).toBe('')
  expect(forged.out.split('\n').pop()).toBe('true|all|true')
  expect(fingerprint(v1)).toBe(before)
  expect(marketEvidence(tid, r1.id).ok).toBe(false)
  test.info().annotations.push({ type: 'below-app attempts', description: outcomes.join('; ') })
})

test('(f) a second, distinct Person (C2) approves through the UI; the evidence read now returns the determination', async ({ browser }) => {
  const C2 = await ctx(browser, 'T_C2')
  await openResidence(C2)
  await expect(queueRow(C2, v1)).toContainText('Not market evidence')
  await expect(queueRow(C2, v1).getByRole('button', { name: 'Approve as second Person' })).toBeEnabled()
  await approveDeterminationDialog(C2, v1, 'e2e_second_person', async () =>
    expect(C2.page.getByRole('status').filter({ hasText: 'Approved. The determination is now market evidence.' })).toBeVisible({ timeout: 20000 }),
  )
  const c = checks(v1)[0]
  expect(c).toMatchObject({ approvalRequired: true, approvedBy: staffId('T_C2'), approvedByPerson: staffPerson('T_C2'), reason: 'e2e_second_person', determinedBy: staffId('T_COMPLIANCE') })
  expect(c.approvedAt).not.toBe('')
  expect(residenceAudit('kyc.residence_determination_approved', v1)).toBe(1)
  expect(marketEvidence(tid, r1.id)).toEqual({ ok: true, country: 'MT' })
  await expect(C2.page.getByText('No determination is waiting for a second Person.')).toBeVisible({ timeout: 15000 })
  // an approval happens once: a third officer is told there is nothing to approve, and nothing changes
  const fp = fingerprint(v1)
  const again = await kapi('POST', `/v1/admin/kyc/verifications/${v1}/residence-approval`, { determined_at: verification(v1).setAtMicro, reason_code: 'e2e_again' }, await apiLogin('T_C3'))
  expect([404, 409]).toContain(again.status)
  expect(fingerprint(v1)).toBe(fp)
})

test('(g) a LESS strict mode (all -> into_open_market) is filed PENDING and needs two approvals by two distinct Persons, neither the requester', async ({ browser }) => {
  const C1 = await ctx(browser, 'T_COMPLIANCE')
  await openResidence(C1)
  await fileMode(C1, 'Into open market', 'e2e_mode_loosen', /less strict mode/, async () =>
    expect(modeCard(C1).getByRole('status').filter({ hasText: 'Pending approval. Nothing has changed yet' })).toBeVisible({ timeout: 20000 }),
  )
  let v = policyVersions()
  expect(v).toHaveLength(2)
  const p = v[1]
  expect(p).toMatchObject({ mode: 'into_open_market', previous: 'all', status: 'pending', required: 2, approvals: 0, requestedBy: staffId('T_COMPLIANCE'), activatedAt: '' })
  expect(modeInForce()).toBe('all') // nothing changed yet

  // the requester cannot approve: the UI disables it and the server refuses
  await expect(versionRow(C1, 'Into open market').getByRole('button', { name: 'Approve', exact: true })).toBeDisabled()
  const self = await kapi('POST', `/v1/admin/tenants/${tid}/residence-approval-policy/${p.id}/approvals`, { reason_code: 'e2e_self' }, await apiLogin('T_COMPLIANCE'))
  expect(self.status).toBe(403)

  // another ACCOUNT of the requester's Person
  const C1B = await ctx(browser, 'T_C1B')
  await openResidence(C1B)
  const d = await approvePolicyDialog(C1B, versionRow(C1B, 'Into open market'), 'e2e_same_person', async (dd) => expectRefusal(dd, 403))
  await d.getByRole('button', { name: 'Close', exact: true }).last().click()
  expect(policyVersions()[1]).toMatchObject({ approvals: 0, status: 'pending' })

  // first distinct Person: recorded, still pending
  const C2 = await ctx(browser, 'T_C2')
  await openResidence(C2)
  await approvePolicyDialog(C2, versionRow(C2, 'Into open market'), 'e2e_loosen_one', async () =>
    expect(C2.page.getByRole('status').filter({ hasText: 'Approval recorded: 1 of 2. Still pending - the mode in force has not changed.' })).toBeVisible({ timeout: 20000 }),
  )
  expect(policyVersions()[1]).toMatchObject({ approvals: 1, status: 'pending', activatedAt: '' })
  expect(modeInForce()).toBe('all')
  // the same Person cannot approve twice
  const twice = await kapi('POST', `/v1/admin/tenants/${tid}/residence-approval-policy/${p.id}/approvals`, { reason_code: 'e2e_twice' }, await apiLogin('T_C2'))
  expect([403, 409]).toContain(twice.status)
  expect(policyVersions()[1].approvals).toBe(1)

  // second distinct Person: executed
  const C3 = await ctx(browser, 'T_C3')
  await openResidence(C3)
  await approvePolicyDialog(C3, versionRow(C3, 'Into open market'), 'e2e_loosen_two', async () =>
    expect(C3.page.getByRole('status').filter({ hasText: 'Approval recorded (2 of 2) and the mode "Into open market" is now in force.' })).toBeVisible({ timeout: 20000 }),
  )
  v = policyVersions()
  expect(v[1]).toMatchObject({ status: 'active', approvals: 2 })
  expect(v[1].activatedAt).not.toBe('')
  expect(new Set(policyApprovers(p.id)).size).toBe(2)
  expect(policyApprovers(p.id)).not.toContain(staffPerson('T_COMPLIANCE'))
  expect(modeInForce()).toBe('into_open_market')
  await expect(C3.page.getByText('Into open market: a determination into a market the brand serves')).toBeVisible()
})

test('(h) mode "into_open_market": the open-market declaration is fail-closed below the API; through the real route, a served market needs a second Person and an unserved one does not', async () => {
  const tenantOnly = { 'app.tenant_id': tid }
  const det = (country: string, v: string) =>
    `UPDATE kyc_verifications SET verified_residence_country = ${lit(country)}, verified_residence_source = 'reviewer_determination', verified_residence_set_by = ${lit(staffId('T_COMPLIANCE'))}, verified_residence_set_at = clock_timestamp() WHERE id = ${lit(v)}`
  const readCheck = (v: string) => `SELECT approval_required::text || '|' || policy_mode FROM residence_determination_checks WHERE verification_id = ${lit(v)}`
  const before = fingerprint(v2)
  expect(verification(v2).country).toBe('')

  // the declaration OMITTED, empty or malformed: refused (RD010), nothing written
  expect(runtimeTx(tenantOnly, det('MT', v2)).sqlstate).toBe('RD010')
  expect(runtimeTx({ ...tenantOnly, 'app.residence_into_open_market': '' }, det('MT', v2)).sqlstate).toBe('RD010')
  expect(runtimeTx({ ...tenantOnly, 'app.residence_into_open_market': 'maybe' }, det('MT', v2)).sqlstate).toBe('RD010')
  expect(runtimeTx({ ...tenantOnly, 'app.residence_into_open_market': 'TRUE' }, det('MT', v2)).sqlstate).toBe('RD010')
  expect(fingerprint(v2)).toBe(before)
  expect(verification(v2).country).toBe('')
  expect(checks(v2)).toEqual([])

  // a declaration "true" is honoured: approval required
  const t = runtimeTx({ ...tenantOnly, 'app.residence_into_open_market': 'true' }, `${det('MT', v2)};\n${readCheck(v2)}`)
  expect(t.sqlstate).toBe('')
  expect(t.out.split('\n').pop()).toBe('true|into_open_market')
  // KNOWN RESIDUAL (decision 4 of the lic10 record; NOT a claim of protection): the database cannot evaluate the operating-market
  // ladder, so it TRUSTS the application's declaration. A holder of the runtime credential who declares "false" gets a determination
  // that needs no second Person under this mode. The answer to this residual is mode "all", not this control.
  const f = runtimeTx({ ...tenantOnly, 'app.residence_into_open_market': 'false' }, `${det('MT', v2)};\n${readCheck(v2)}`)
  expect(f.sqlstate).toBe('')
  expect(f.out.split('\n').pop()).toBe('false|into_open_market')
  expect(fingerprint(v2)).toBe(before) // both rolled back

  // through the real reviewer route (the application declares the market evaluation itself): MT is served -> pending
  const c1 = await apiLogin('T_COMPLIANCE')
  expect((await reviewVerification(c1, v2, 'approved', 'MT')).status).toBe(200)
  expect(checks(v2)[0]).toMatchObject({ approvalRequired: true, mode: 'into_open_market', approvedAt: '' })
  expect(marketEvidence(tid, r2.id).ok).toBe(false)
  // ... and DE is not served by the brand -> effective at once
  expect((await reviewVerification(await apiLogin('T_COMPLIANCE'), v3, 'approved', 'DE')).status).toBe(200)
  expect(checks(v3)[0]).toMatchObject({ approvalRequired: false, mode: 'into_open_market' })
  expect(marketEvidence(tid, r3.id)).toEqual({ ok: true, country: 'DE' })
})

test('(i) housekeeping: restore the default mode "none" through the same two-approver route so later specs sharing this database are unaffected', async () => {
  const f = await kapi('POST', `/v1/admin/tenants/${tid}/residence-approval-policy`, { mode: 'none', reason_code: 'e2e_restore_default' }, await apiLogin('T_COMPLIANCE'))
  expect(f.status).toBe(202)
  const id = f.json.policy_id as string
  for (const k of ['T_C2', 'T_C3']) {
    const r = await kapi('POST', `/v1/admin/tenants/${tid}/residence-approval-policy/${id}/approvals`, { reason_code: 'e2e_restore_default' }, await apiLogin(k))
    expect(r.status).toBe(200)
  }
  expect(modeInForce()).toBe('none')
  expect(ownerRead({ tenant: tid }, `SELECT count(*) FROM residence_approval_policies WHERE status = 'active'`)).toBe('3')
})

// ---- helpers local to this spec ------------------------------------------------------------------------------------------

/** One microsecond later, in the canonical text the function takes. */
function bump(microIso: string): string {
  const m = /^(.*\.)(\d{6})Z$/.exec(microIso)!
  return `${m[1]}${String(Number(m[2]) + 1).padStart(6, '0')}Z`
}
function determinationApproveProofHash(tenant: string, verificationId: string, atMicro: string, reason: string): string {
  return digest([tenant, verificationId, atMicro, reason])
}
