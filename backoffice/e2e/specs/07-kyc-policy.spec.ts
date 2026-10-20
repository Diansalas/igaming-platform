// KYC enforcement policy authoring through the REAL stack (lic21): real Back Office UI, real platform-api, real PostgreSQL.
// Ground truth is read from the database as the table owner; the below-the-application-layer attempts use the RUNTIME role (the
// role platform-api itself uses) inside transactions that are always rolled back. Everything is synthetic: the threshold values
// below are test inputs for a throwaway database, not recommendations and not any jurisdiction's rule.
// Serial: the tests share one policy lifecycle (draft -> activation refusals -> activation -> enforcement -> supersede).
import { expect, test, type Browser } from 'playwright/test'
import { apiLogin, jurisdictionId } from '../lib/api'
import { lit, ownerRead, runtimeTx, runtimeTxMessage } from '../lib/kyc_db'
import {
  allPolicies, expectRefusalRendered, fillAuthor, openPolicies, policy, policyAudit, policyAuditActor, policyFingerprint, rowAction, rowFor, submitAction, submitAuthor,
} from '../lib/kyc_policy'
import { digest, signProof } from '../lib/kyc_proof'
import { deposit, ensureMockPayments, ensureUnlinkedPlatformAdmin, kapi, newPlayer, reviewVerification, staffId, startVerification, tenantIdOf, type Player } from '../lib/kyc_world'
import { loginAs, spaGoto, type Session } from '../lib/session'

test.describe.configure({ mode: 'serial' })

const LEGAL = 'E2E-SYNTHETIC-LEGAL-REF-NOT-A-REAL-REVIEW'
const ctx = async (browser: Browser, key: string): Promise<Session> => loginAs(await (await browser.newContext()).newPage(), key)
const pol = (id: string, op: string) => `/v1/admin/kyc/enforcement-policies/${id}/${op}`

let p1: Player
let p2: Player
let d1 = '' // first draft, authored by PA_A (Person P1)
let d2 = '' // successor draft, authored by PA_D
// rows are located by their (unique) reason code: the table prints no ids
const R1 = 'e2e_kyc_threshold'
const R2 = 'e2e_kyc_successor'
const R3 = 'e2e_play_policy'

const tenantRead = (q: string) => ownerRead({ tenant: tenantIdOf() }, q)
/** Deposit attempts of a player that the KYC gate denied (audit trail written in the deposit transaction). */
const denials = (p: Player) =>
  Number(tenantRead(`SELECT count(*) FROM audit_log WHERE action = 'payments.deposit_denied_by_kyc' AND metadata->>'deposit_intent_id' IN (SELECT id::text FROM deposit_intents WHERE player_account_id = ${lit(p.id)})`))
const intentStatus = (id: string) => tenantRead(`SELECT status FROM deposit_intents WHERE id = ${lit(id)}`)
const declineReason = (id: string) => tenantRead(`SELECT metadata->>'decline_reason' FROM audit_log WHERE action = 'deposit.declined' AND target_id = ${lit(id)}`)
const depositsOf = (p: Player) => Number(tenantRead(`SELECT count(*) FROM deposit_intents WHERE player_account_id = ${lit(p.id)}`))

test.beforeAll(async () => {
  test.setTimeout(240_000)
  await ensureUnlinkedPlatformAdmin('PA_U') // legacy platform admin with NO Person link (sanctioned fixture path, see lib/kyc_world.ts)
  await ensureMockPayments()
  expect(allPolicies()).toEqual([]) // the database is fresh: the platform ships no policy
})

test('(a) PA_A authors a draft through the UI with explicit fields; the draft governs nothing', async ({ browser }) => {
  const A = await ctx(browser, 'PA_A')
  await openPolicies(A)
  await expect(A.page.getByText('No KYC enforcement policy is recorded for this filter.')).toBeVisible()
  await expect(A.page.getByRole('note', { name: 'Dormant by default' })).toBeVisible()
  const d = await fillAuthor(A, { trigger: 'cumulative_deposit', threshold: '5000', asset: 'EUR', legal: LEGAL, reason: R1 })
  await submitAuthor(A, d)

  const rows = allPolicies()
  expect(rows).toHaveLength(1)
  d1 = rows[0].id
  expect(rows[0]).toMatchObject({ status: 'draft', trigger: 'cumulative_deposit', threshold: '5000', asset: 'EUR', legal: LEGAL, createdBy: staffId('PA_A') })
  expect(policyAudit(d1, 'created')).toBe(1)
  expect(policyAuditActor(d1, 'created')).toBe(staffId('PA_A'))
  const row = rowFor(A, R1)
  await expect(row).toContainText('Draft (governs nothing)')
  await expect(row).toContainText('Threshold 5000 minor units of EUR')
  await expect(row).toContainText('(you)')

  // A draft enforces nothing: an UNVERIFIED player's deposit far over the draft threshold is accepted by the real deposit path.
  p1 = await newPlayer('kyc-p1')
  const r = await deposit(p1, 10000, 'e2e-draft-1')
  expect(r.status).toBe(201)
  expect(r.json.status).toBe('pending')
  expect(denials(p1)).toBe(0)
})

test('(b) activation by the AUTHOR is refused: server/DB refusal rendered, nothing changes', async ({ browser }) => {
  const A = await ctx(browser, 'PA_A')
  await openPolicies(A)
  const fp = policyFingerprint()
  const d = await rowAction(A, R1, 'Activate', 'e2e_activate_by_author')
  await expect(d.getByText('You authored this draft. The server will refuse your activation.')).toBeVisible() // a hint only
  await submitAction(A, d, 'Activate', async () => expectRefusalRendered(d, 403, 'forbidden'))
  expect(policy(d1).status).toBe('draft')
  expect(policyFingerprint()).toBe(fp)
  // the hint is not the control: the same refusal comes from the API with a fresh token
  const api = await kapi('POST', pol(d1, 'activate'), { reason_code: 'e2e_activate_by_author' }, await apiLogin('PA_A'))
  expect(api.status).toBe(403)
  expect(api.json.code).toBe('forbidden')
  expect(api.json.message).toContain('four-eyes')
  expect(policyFingerprint()).toBe(fp)
})

test('(c) activation by a SECOND ACCOUNT OF THE SAME PERSON (PA_A2, Person P1) is refused (KP003): no UI hint, the database refuses', async ({ browser }) => {
  const A2 = await ctx(browser, 'PA_A2')
  await openPolicies(A2)
  const fp = policyFingerprint()
  expect(staffId('PA_A2')).not.toBe(staffId('PA_A'))
  const d = await rowAction(A2, R1, 'Activate', 'e2e_activate_same_person')
  // the UI cannot know about Persons: it shows no authorship hint for this account, so only the database can refuse
  await expect(d.getByText('You authored this draft. The server will refuse your activation.')).toHaveCount(0)
  await submitAction(A2, d, 'Activate', async () => expectRefusalRendered(d, 403, 'forbidden'))
  expect(policy(d1).status).toBe('draft')
  expect(policyFingerprint()).toBe(fp)
  const api = await kapi('POST', pol(d1, 'activate'), { reason_code: 'e2e_activate_same_person' }, await apiLogin('PA_A2'))
  expect(api.status).toBe(403)
  expect(api.json.message).toContain('four-eyes')
  expect(policyFingerprint()).toBe(fp)
})

test('(d) an UNLINKED-Person platform admin (PA_U) is refused for authoring and activation (KP002)', async ({ browser }) => {
  const U = await ctx(browser, 'PA_U')
  await openPolicies(U)
  const fp = policyFingerprint()
  // activation
  const d = await rowAction(U, R1, 'Activate', 'e2e_activate_unlinked')
  await submitAction(U, d, 'Activate', async () => expectRefusalRendered(d, 403, 'forbidden'))
  await d.getByRole('button', { name: 'Close', exact: true }).last().click()
  // authoring
  const a = await fillAuthor(U, { trigger: 'cumulative_deposit', threshold: '7000', asset: 'EUR', legal: LEGAL, reason: 'e2e_unlinked_author' })
  await a.getByRole('button', { name: 'Create draft', exact: true }).click()
  await expectRefusalRendered(a, 403, 'forbidden')
  expect(allPolicies()).toHaveLength(1)
  expect(policyFingerprint()).toBe(fp)
  // the API names the cause
  const tok = await apiLogin('PA_U')
  const act = await kapi('POST', pol(d1, 'activate'), { reason_code: 'e2e_activate_unlinked' }, tok)
  expect(act.status).toBe(403)
  expect(act.json.message).toContain('not linked to a Person')
  expect(policyFingerprint()).toBe(fp)
})

test('(e) BELOW the API: the runtime role cannot activate the draft except as a Person-distinct admin with a valid, matching proof', async () => {
  const fp = policyFingerprint()
  const id = d1
  const upd = `UPDATE kyc_enforcement_policies SET status = 'active' WHERE id = ${lit(id)}`
  const plat = (key: string, extra: Record<string, string> = {}) => ({ 'app.platform_admin_principal_id': staffId(key), ...extra })
  const hashActivate = digest([id, 'draft', 'active', null, 'e2e_db_probe'])
  const claims = (key: string, over: Partial<Parameters<typeof signProof>[0]> = {}) => ({
    actor: staffId(key), scope: 'platform' as const, tenant: '', operation: 'kyc_enforcement_policy:activate', target: id, payloadHash: hashActivate, ...over,
  })

  // the author's own account: the older principal rule (migration 0100), a plain exception
  const own = runtimeTxMessage(plat('PA_A', { 'app.kyc_policy_reason_code': 'e2e_db_probe' }), upd)
  expect(own.sqlstate).toBe('P0001')
  expect(own.message).toContain('four-eyes required')
  // another account of the author's Person: KP003
  expect(runtimeTx(plat('PA_A2', { 'app.kyc_policy_reason_code': 'e2e_db_probe' }), upd).sqlstate).toBe('KP003')
  // an admin with no Person link: KP002
  expect(runtimeTx(plat('PA_U', { 'app.kyc_policy_reason_code': 'e2e_db_probe' }), upd).sqlstate).toBe('KP002')
  // a Person-distinct admin but no reason code: KP004
  expect(runtimeTx(plat('PA_B'), upd).sqlstate).toBe('KP004')
  // reason code present, no proof at all: AP001
  expect(runtimeTx(plat('PA_B', { 'app.kyc_policy_reason_code': 'e2e_db_probe' }), upd).sqlstate).toBe('AP001')
  // a proof whose MAC is wrong (forged): AP002
  expect(runtimeTx(plat('PA_B', { 'app.kyc_policy_reason_code': 'e2e_db_probe', 'app.actor_proof': signProof(claims('PA_B'), { badMac: true }) }), upd).sqlstate).toBe('AP002')
  // a proof signed with an unknown key id: AP002
  expect(runtimeTx(plat('PA_B', { 'app.kyc_policy_reason_code': 'e2e_db_probe', 'app.actor_proof': signProof(claims('PA_B'), { kid: 'nokey1' }) }), upd).sqlstate).toBe('AP002')
  // an already-expired proof: AP003
  expect(runtimeTx(plat('PA_B', { 'app.kyc_policy_reason_code': 'e2e_db_probe', 'app.actor_proof': signProof(claims('PA_B'), { iatOffset: -120 }) }), upd).sqlstate).toBe('AP003')
  // a VALID proof for a different transition (withdraw) or another reason code, or another admin's proof: AP004
  const wrongHash = digest([id, 'draft', 'withdrawn', null, 'e2e_db_probe'])
  expect(runtimeTx(plat('PA_B', { 'app.kyc_policy_reason_code': 'e2e_db_probe', 'app.actor_proof': signProof(claims('PA_B', { payloadHash: wrongHash })) }), upd).sqlstate).toBe('AP004')
  expect(runtimeTx(plat('PA_B', { 'app.kyc_policy_reason_code': 'another_reason', 'app.actor_proof': signProof(claims('PA_B')) }), upd).sqlstate).toBe('AP004')
  expect(runtimeTx(plat('PA_B', { 'app.kyc_policy_reason_code': 'e2e_db_probe', 'app.actor_proof': signProof(claims('PA_C')) }), upd).sqlstate).toBe('AP004')
  // a tenant-shaped session cannot reach the platform table at all (row-level security): zero rows, nothing changes
  const tenantShaped = runtimeTx({ 'app.tenant_id': tenantIdOf(), 'app.platform_admin_principal_id': staffId('PA_B') }, `${upd} RETURNING id`)
  expect(tenantShaped.sqlstate).toBe('')
  expect(tenantShaped.out).toBe('')
  // the control: the same statement WITH a valid matching proof from a Person-distinct admin passes the verifier (rolled back)
  const ok = runtimeTx(plat('PA_B', { 'app.kyc_policy_reason_code': 'e2e_db_probe', 'app.actor_proof': signProof(claims('PA_B')) }), `${upd} RETURNING status`)
  expect(ok.sqlstate).toBe('')
  expect(ok.out).toBe('active')
  // and nothing above persisted
  expect(policy(d1).status).toBe('draft')
  expect(policyFingerprint()).toBe(fp)
})

test('(f) a Person-distinct admin (PA_B, Person P2) activates through the UI; the DB shows the policy active; there is no switch-off action', async ({ browser }) => {
  const B = await ctx(browser, 'PA_B')
  await openPolicies(B)
  const d = await rowAction(B, R1, 'Activate', 'e2e_activate_ok')
  await submitAction(B, d, 'Activate', async () => expect(B.page.getByRole('status').filter({ hasText: 'Activated. This policy now enforces KYC for its jurisdiction.' })).toBeVisible({ timeout: 20000 }))
  expect(policy(d1).status).toBe('active')
  expect(policyAudit(d1, 'activated')).toBe(1)
  expect(policyAuditActor(d1, 'activated')).toBe(staffId('PA_B'))

  // the row of an ACTIVE policy offers exactly one action, Supersede: no withdraw, deactivate, disable or switch-off
  const row = rowFor(B, R1)
  await expect(row).toContainText('Active')
  await expect(row.getByRole('button')).toHaveCount(1)
  await expect(row.getByRole('button', { name: 'Supersede', exact: true })).toBeVisible()
  await expect(row.getByRole('button', { name: /withdraw|deactivat|disable|switch off|turn off/i })).toHaveCount(0)

  const fp = policyFingerprint()
  // the server refuses a switch-off through the API (409) ...
  const w = await kapi('POST', pol(d1, 'withdraw'), { reason_code: 'e2e_switch_off' }, await apiLogin('PA_C'))
  expect(w.status).toBe(409)
  expect(w.json.message).toContain('not in the state this action needs') // withdraw is for never-activated drafts only
  // ... and the database refuses it below the API, for a Person-distinct admin, even with the reason code set
  const sw = runtimeTxMessage({ 'app.platform_admin_principal_id': staffId('PA_C'), 'app.kyc_policy_reason_code': 'e2e_switch_off' }, `UPDATE kyc_enforcement_policies SET status = 'withdrawn' WHERE id = ${lit(d1)}`)
  expect(sw.sqlstate).toBe('P0001')
  expect(sw.message).toContain('requires supersession')
  // append-only: a DELETE removes nothing (row-level security offers no DELETE policy, so zero rows are visible to it) ...
  const del = runtimeTx({ 'app.platform_admin_principal_id': staffId('PA_C') }, `DELETE FROM kyc_enforcement_policies WHERE id = ${lit(d1)} RETURNING id`)
  expect(del.sqlstate).toBe('')
  expect(del.out).toBe('')
  // ... and a TRUNCATE is refused
  const trunc = runtimeTxMessage({ 'app.platform_admin_principal_id': staffId('PA_C') }, `TRUNCATE kyc_enforcement_policies`)
  expect(['42501', 'P0001']).toContain(trunc.sqlstate)
  expect(policy(d1).status).toBe('active')
  expect(policyFingerprint()).toBe(fp)
})

test('(g) ENFORCEMENT on the real deposit path: over the threshold an unverified player is refused; after KYC verification the same deposit is allowed', async () => {
  // below the threshold: allowed. The threshold is reached at 5000 (total >= threshold), so 4999 passes and 5000 does not.
  const small = await deposit(p1, 4999, 'e2e-enf-4999')
  expect(small.status).toBe(201)
  expect(small.json.status).toBe('pending')
  expect(denials(p1)).toBe(0)

  const over = await deposit(p1, 10000, 'e2e-enf-10000')
  expect(over.status).toBe(201)
  expect(over.json.status).toBe('declined')
  expect(intentStatus(over.json.id)).toBe('declined')
  expect(declineReason(over.json.id)).toMatch(/^kyc_required:kyc_deposit:/)
  expect(denials(p1)).toBe(1)
  expect((await deposit(p1, 5000, 'e2e-enf-5000')).json.status).toBe('declined') // exactly the threshold
  expect(denials(p1)).toBe(2)

  // a verification that is only PENDING at the vendor does not admit the player either
  const verId = await startVerification(p1)
  const pending = await deposit(p1, 10000, 'e2e-enf-10000-pending-kyc')
  expect(pending.json.status).toBe('declined')
  expect(denials(p1)).toBe(3)

  // KYC verification: a compliance reviewer approves the verification (real review endpoint)
  const comp = await apiLogin('T_COMPLIANCE')
  const rv = await reviewVerification(comp, verId, 'approved')
  expect(rv.status).toBe(200)
  expect(rv.json.status).toBe('approved')

  const after = await deposit(p1, 10000, 'e2e-enf-10000-verified')
  expect(after.status).toBe(201)
  expect(after.json.status).toBe('pending')
  expect(after.json.provider_id).toBe('mock-payments')
  expect(intentStatus(after.json.id)).toBe('pending')
  expect(denials(p1)).toBe(3) // unchanged: the verified deposit was not denied
  // the same request replayed with the same idempotency key returns the same intent (retry safety on the real path)
  const replay = await deposit(p1, 10000, 'e2e-enf-10000-verified')
  expect(replay.json.id).toBe(after.json.id)
  expect(depositsOf(p1)).toBe(6) // draft-phase, 4999, 10000, 5000, pending-kyc, verified (the replay added none)
})

test('(h) supersede: a successor is drafted by PA_D, the authors cannot supersede, PA_C does; the old policy is superseded and enforcement follows the successor', async ({ browser }) => {
  // PA_D (Person P4) authors the successor draft of the SAME enforcement key (new threshold)
  const D = await ctx(browser, 'PA_D')
  await openPolicies(D)
  const a = await fillAuthor(D, { trigger: 'cumulative_deposit', threshold: '8000', asset: 'EUR', legal: LEGAL, reason: R2 })
  await submitAuthor(D, a)
  d2 = allPolicies().find((p) => p.id !== d1)!.id
  expect(policy(d2)).toMatchObject({ status: 'draft', threshold: '8000', createdBy: staffId('PA_D') })
  expect(policy(d1).status).toBe('active')

  // the author of the ACTIVE policy (PA_A) cannot supersede it
  const fp = policyFingerprint()
  const A = await ctx(browser, 'PA_A')
  await openPolicies(A)
  const da = await rowAction(A, R1, 'Supersede', 'e2e_supersede_by_author', { successor: d2 })
  await submitAction(A, da, 'Supersede', async () => expectRefusalRendered(da, 403, 'forbidden'))
  // and neither can the author of the SUCCESSOR
  const byD = await kapi('POST', pol(d1, 'supersede'), { successor_policy_id: d2, reason_code: 'e2e_supersede_by_successor_author' }, await apiLogin('PA_D'))
  expect(byD.status).toBe(403)
  expect(policy(d1).status).toBe('active')
  expect(policy(d2).status).toBe('draft')
  expect(policyFingerprint()).toBe(fp)

  // PA_C (Person P3, author of neither) supersedes
  const C = await ctx(browser, 'PA_C')
  await openPolicies(C)
  const dc = await rowAction(C, R1, 'Supersede', 'e2e_supersede_ok', { successor: d2 })
  await submitAction(C, dc, 'Supersede', async () =>
    expect(C.page.getByRole('status').filter({ hasText: 'Superseded. The successor is now the active policy and the previous one is withdrawn.' })).toBeVisible({ timeout: 20000 }),
  )
  expect(policy(d1)).toMatchObject({ status: 'withdrawn', successor: d2 })
  expect(policy(d2).status).toBe('active')
  expect(allPolicies().filter((p) => p.status === 'active')).toHaveLength(1)
  expect(policyAuditActor(d2, 'activated')).toBe(staffId('PA_C'))
  await expect(rowFor(C, R1)).toContainText('Withdrawn')
  await expect(rowFor(C, R1)).toContainText(d2.slice(0, 8)) // "Superseded by"
  await expect(rowFor(C, R1).getByRole('button')).toHaveCount(0) // a withdrawn policy offers nothing
  await expect(rowFor(C, R2)).toContainText('Active')

  // enforcement follows the successor: a NEW unverified player is allowed 6000 (it would have been refused under 5000) and refused at 8000
  p2 = await newPlayer('kyc-p2')
  expect((await deposit(p2, 6000, 'e2e-succ-6000')).json.status).toBe('pending')
  expect((await deposit(p2, 8000, 'e2e-succ-8000')).json.status).toBe('declined')
  expect(denials(p2)).toBe(1)
})

test('(i) withdrawal of a DRAFT: the author cannot, another Person-distinct admin can; a withdrawn policy stays withdrawn', async ({ browser }) => {
  const A = await ctx(browser, 'PA_A')
  await openPolicies(A)
  const a = await fillAuthor(A, { trigger: 'play', playOp: 'casino_play', legal: LEGAL, reason: R3 })
  await submitAuthor(A, a)
  const d3 = allPolicies().find((p) => p.trigger === 'play')!.id
  expect(policy(d3)).toMatchObject({ status: 'draft', playOp: 'casino_play', createdBy: staffId('PA_A') })

  const fp = policyFingerprint()
  const own = await rowAction(A, R3, 'Withdraw draft', 'e2e_withdraw_by_author')
  await submitAction(A, own, 'Withdraw draft', async () => expectRefusalRendered(own, 403, 'forbidden'))
  expect(policy(d3).status).toBe('draft')
  expect(policyFingerprint()).toBe(fp)

  const B = await ctx(browser, 'PA_B')
  await openPolicies(B)
  const d = await rowAction(B, R3, 'Withdraw draft', 'e2e_withdraw_ok')
  await submitAction(B, d, 'Withdraw draft', async () => expect(B.page.getByRole('status').filter({ hasText: 'Draft withdrawn.' })).toBeVisible({ timeout: 20000 }))
  expect(policy(d3).status).toBe('withdrawn')
  expect(policyAudit(d3, 'withdrawn')).toBe(1)
  await expect(rowFor(B, R3).getByRole('button')).toHaveCount(0)

  // it cannot be brought back: the API says 409 and the database refuses the transition
  const fp2 = policyFingerprint()
  const act = await kapi('POST', pol(d3, 'activate'), { reason_code: 'e2e_resurrect' }, await apiLogin('PA_C'))
  expect(act.status).toBe(409)
  const back = runtimeTxMessage({ 'app.platform_admin_principal_id': staffId('PA_C'), 'app.kyc_policy_reason_code': 'e2e_resurrect' }, `UPDATE kyc_enforcement_policies SET status = 'active' WHERE id = ${lit(d3)}`)
  expect(back.sqlstate).toBe('P0001')
  expect(back.message).toContain('illegal status transition')
  expect(policy(d3).status).toBe('withdrawn')
  expect(policyFingerprint()).toBe(fp2)
})

test('(j) tenant staff are denied: no nav entry, UI denial, the server refuses reads and writes, nothing changes', async ({ browser }) => {
  const T = await ctx(browser, 'T_COMPLIANCE')
  await expect(T.page.getByRole('navigation').getByRole('link', { name: 'KYC Policies' })).toHaveCount(0)
  await spaGoto(T.page, '/kyc-policies')
  await expect(T.page.getByText('Your role does not have access to this section.')).toBeVisible()
  const fp = policyFingerprint()
  const tok = await apiLogin('T_COMPLIANCE') // fresh: the critical-step-up window is 60 s
  expect((await kapi('GET', '/v1/admin/kyc/enforcement-policies', undefined, tok)).status).toBe(403)
  const body = { licensing_jurisdiction_id: jurisdictionId(), trigger_type: 'play', play_operation: 'casino_play', legal_review_reference: LEGAL, reason_code: 'e2e_tenant_try' }
  expect((await kapi('POST', '/v1/admin/kyc/enforcement-policies', body, tok)).status).toBe(403)
  for (const op of ['activate', 'withdraw']) expect((await kapi('POST', pol(d2, op), { reason_code: 'e2e_tenant_try' }, tok)).status).toBe(403)
  expect((await kapi('POST', pol(d2, 'supersede'), { successor_policy_id: d1, reason_code: 'e2e_tenant_try' }, tok)).status).toBe(403)
  expect((await kapi('GET', '/v1/admin/kyc/enforcement-policies')).status).toBe(401)
  expect(policyFingerprint()).toBe(fp)
})

test('(k) housekeeping: replace the test policy by a non-binding successor so later specs sharing this database are unaffected', async () => {
  // Policies cannot be switched off (the point of this spec). The last active policy would make unverified players' EUR deposits
  // fail in any spec that runs after this one; it is therefore superseded, through the same real routes, by a successor whose
  // threshold no test deposit can reach. This is a fixture action, not a behaviour under test.
  const a = await kapi('POST', '/v1/admin/kyc/enforcement-policies', {
    licensing_jurisdiction_id: jurisdictionId(),
    trigger_type: 'cumulative_deposit', threshold_minor_units: '99999999999999999999', asset_code: 'EUR', legal_review_reference: LEGAL, reason_code: 'e2e_housekeeping',
  }, await apiLogin('PA_A'))
  expect(a.status).toBe(201)
  const s = await kapi('POST', pol(d2, 'supersede'), { successor_policy_id: a.json.id, reason_code: 'e2e_housekeeping' }, await apiLogin('PA_C'))
  expect(s.status).toBe(200)
  expect(policy(a.json.id).status).toBe('active')
  expect(policy(d2)).toMatchObject({ status: 'withdrawn', successor: a.json.id })
})
