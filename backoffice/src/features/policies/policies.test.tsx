import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it, vi } from 'vitest'
import { AppRoutes } from '../../app/AppRoutes'
import { makeTestJwt } from '../../test/jwt'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { PLATFORM_ADMIN_CLAIMS, signInAs } from '../../test/session'
import { stepUpRequiredResponse } from '../../test/stepUp'
import {
  DET_AT,
  JUR1,
  ME,
  OTHER,
  POL1,
  POL2,
  POL3,
  SENTINEL,
  TEN1,
  VER1,
  WRC1,
  kycPolicy,
  kycReads,
  pendingRow,
  refusal,
  residenceReads,
  rVersion,
  wrpChange,
  wrpConfigured,
  wrpEffective,
  wrpReads,
  wrpResponse,
} from './policyFixtures'

vi.setConfig({ testTimeout: 30000 })

const PA = { ...PLATFORM_ADMIN_CLAIMS, sub: ME }
const CO = { role: 'compliance', tenant_id: TEN1, sub: ME }
const TA = { role: 'tenant_admin', tenant_id: TEN1, sub: ME }
type User = ReturnType<typeof userEvent.setup>
const NON_ADMIN = ['tenant_admin', 'compliance', 'finance', 'support', 'risk_manager', 'promotions_manager', 'bonus_operations']

/** Records the JSON body of every request to the path (and counts them). */
function capture(method: 'post' | 'put', path: string, answer: () => Response) {
  const bodies: unknown[] = []
  server.use(
    http[method](path, async ({ request }) => {
      const t = await request.text()
      bodies.push(t ? JSON.parse(t) : null)
      return answer()
    }),
  )
  return bodies
}

/** A critical-action handler that demands a step-up first and answers only for the stepped-up token. */
function stepUpOn(method: 'post', path: string, ok: () => Response) {
  const STEPPED = makeTestJwt({ ...PA, jti: 'stepped-policy' })
  const sends: { auth: string | null; body: string }[] = []
  server.use(
    http[method](path, async ({ request }) => {
      const rec = { auth: request.headers.get('Authorization'), body: await request.text() }
      sends.push(rec)
      return rec.auth === `Bearer ${STEPPED}` ? ok() : stepUpRequiredResponse()
    }),
    http.post('/v1/staff/auth/step-up', () =>
      HttpResponse.json({ access_token: STEPPED, token_type: 'Bearer', expires_in: 900, mfa_at: '2026-10-13T10:00:00Z', step_up_expires_at: { financial: '2026-10-13T10:10:00Z', critical: '2026-10-13T10:05:00Z' } }),
    ),
  )
  return sends
}

async function doStepUp(dlgName = 'Confirm it is you') {
  const user = userEvent.setup()
  const su = await screen.findByRole('dialog', { name: dlgName })
  await user.type(within(su).getByLabelText('6-digit code'), '123456')
  await user.click(within(su).getByRole('button', { name: 'Verify' }))
}

// ---------------------------------------------------------------------------------------------------------------------------
describe('RBAC (UI hint only; the server decides): no request is sent for a role without the permission', () => {
  it.each(NON_ADMIN.map((r) => [r]))('%s sees NotAuthorized on /kyc-policies and no request is sent', async (role) => {
    signInAs({ role, tenant_id: TEN1, sub: ME })
    let calls = 0
    server.use(http.all('/v1/admin/kyc/enforcement-policies*', () => { calls++; return HttpResponse.json([]) }), http.get('/v1/admin/jurisdictions', () => { calls++; return HttpResponse.json([]) }))
    renderWithProviders(<AppRoutes />, { route: '/kyc-policies' })
    expect(await screen.findByText('Your role does not have access to this section.')).toBeInTheDocument()
    expect(calls).toBe(0)
  })

  it.each([['platform_admin'], ['finance'], ['support'], ['risk_manager'], ['promotions_manager'], ['bonus_operations']])('%s sees NotAuthorized on /residence-approval and no request is sent', async (role) => {
    signInAs(role === 'platform_admin' ? PA : { role, tenant_id: TEN1, sub: ME })
    let calls = 0
    server.use(http.all('/v1/admin/tenants/*/residence-approval-policy*', () => { calls++; return HttpResponse.json({}) }), http.all('/v1/admin/kyc/residence-determinations/pending', () => { calls++; return HttpResponse.json([]) }))
    renderWithProviders(<AppRoutes />, { route: '/residence-approval' })
    expect(await screen.findByText('Your role does not have access to this section.')).toBeInTheDocument()
    expect(calls).toBe(0)
  })

  it.each([['finance'], ['support'], ['risk_manager'], ['promotions_manager'], ['bonus_operations']])('%s sees NotAuthorized on every withdrawal-restriction route and no request is sent', async (role) => {
    signInAs({ role, tenant_id: TEN1, sub: ME })
    let calls = 0
    server.use(http.all('/v1/admin/tenants/*', () => { calls++; return HttpResponse.json({}) }), http.all('/v1/admin/withdrawal-restriction-policy-changes*', () => { calls++; return HttpResponse.json({}) }))
    for (const route of ['/withdrawal-restrictions', `/withdrawal-restrictions/tenants/${TEN1}`, '/withdrawal-restrictions/changes', `/withdrawal-restrictions/changes/${WRC1}`]) {
      const r = renderWithProviders(<AppRoutes />, { route })
      expect(await screen.findByText('Your role does not have access to this section.')).toBeInTheDocument()
      r.unmount()
    }
    expect(calls).toBe(0)
  })

  it.each([['tenant_admin'], ['compliance']])('%s may read its own tenant policy but never reaches the platform-only change screens (no request)', async (role) => {
    signInAs({ role, tenant_id: TEN1, sub: ME })
    wrpReads()
    let changeCalls = 0
    server.use(http.all('/v1/admin/withdrawal-restriction-policy-changes*', () => { changeCalls++; return HttpResponse.json({ items: [], disclaimer: 'd' }) }))
    for (const route of ['/withdrawal-restrictions/changes', `/withdrawal-restrictions/changes/${WRC1}`]) {
      const r = renderWithProviders(<AppRoutes />, { route })
      expect(await screen.findByText('Your role does not have access to this section.')).toBeInTheDocument()
      r.unmount()
    }
    renderWithProviders(<AppRoutes />, { route: '/withdrawal-restrictions' })
    expect(await screen.findByRole('note', { name: 'Hard restrictions' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'File a change' })).toBeNull()
    expect(screen.getByText(/Only platform administrators can file or decide changes/)).toBeInTheDocument()
    expect(changeCalls).toBe(0)
  })

  it('a tenant_admin can read the residence policy and queue but is offered no mode change and no approve action', async () => {
    signInAs(TA)
    residenceReads({ effective_mode: 'all', versions: [rVersion()] }, [pendingRow()])
    renderWithProviders(<AppRoutes />, { route: '/residence-approval' })
    expect(await screen.findByText('Mode in force')).toBeInTheDocument()
    expect(await screen.findByText('Not market evidence')).toBeInTheDocument()
    expect(screen.queryByText('Change the mode')).toBeNull()
    expect(screen.queryByRole('button', { name: /Approve/ })).toBeNull()
    expect(screen.getByText(/Only compliance staff of the tenant can change it or approve/)).toBeInTheDocument()
  })

  it('degrades when the API refuses a read the UI allowed: fixed wording, never the server message', async () => {
    signInAs(PA)
    kycReads()
    server.use(http.get('/v1/admin/kyc/enforcement-policies', () => refusal(403, 'forbidden')))
    renderWithProviders(<AppRoutes />, { route: '/kyc-policies' })
    expect(await screen.findByText(/does not hold the permission/)).toBeInTheDocument()
    expect(document.body).not.toHaveTextContent('do-not-render')
    expect(screen.getByText('Request ID: req-9')).toBeInTheDocument()
  })
})

// ---------------------------------------------------------------------------------------------------------------------------
describe('KYC enforcement policies', () => {
  /** The table row holding a status badge (the same words also appear in the filter's options). */
  const rowOf = async (label: string) => {
    await waitFor(() => expect(screen.getAllByText(label).some((e) => e.closest('tr'))).toBe(true))
    return screen.getAllByText(label).find((e) => e.closest('tr'))!.closest('tr') as HTMLElement
  }
  const open = async (user: User) => {
    await user.click(await screen.findByRole('button', { name: 'Author a policy' }))
    return screen.findByRole('dialog', { name: 'Author a KYC enforcement policy (draft)' })
  }

  it('lists per jurisdiction, states that values come from compliance, and offers no way to switch an active policy off', async () => {
    signInAs(PA)
    kycReads([
      kycPolicy(),
      kycPolicy({ id: POL2, status: 'active', created_by: ME, legal_review_reference: '<img src=x onerror=alert(1)>' }),
      kycPolicy({ id: POL3, status: 'withdrawn', successor_policy_id: POL2, licensing_jurisdiction_id: '10000000-0000-4000-8000-000000000002', trigger_type: 'play', play_operation: 'casino_play', threshold_minor_units: null, asset_code: null }),
    ])
    renderWithProviders(<AppRoutes />, { route: '/kyc-policies' })
    expect(await screen.findByRole('note', { name: 'Source of values' })).toHaveTextContent('ships no policy and suggests no value')
    expect(screen.getByRole('note', { name: 'Dormant by default' })).toHaveTextContent('dormant')
    expect(screen.getByRole('note', { name: 'Four-eyes' })).toHaveTextContent('An active policy cannot be switched off')
    const draft = await rowOf('Draft (governs nothing)')
    expect(within(draft).getByRole('button', { name: 'Activate' })).toBeInTheDocument()
    expect(within(draft).getByRole('button', { name: 'Withdraw draft' })).toBeInTheDocument()
    const active = await rowOf('Active')
    expect(within(active).getByRole('button', { name: 'Supersede' })).toBeInTheDocument()
    expect(within(active).queryByRole('button', { name: /Withdraw|Deactivate|Disable|Switch off/i })).toBeNull()
    // XSS: stored text is rendered as text, not markup
    expect(within(active).getByText('<img src=x onerror=alert(1)>')).toBeInTheDocument()
    expect(document.querySelector('img')).toBeNull()
    const wd = await rowOf('Withdrawn')
    expect(within(wd).queryAllByRole('button')).toHaveLength(0)
    expect(within(wd).getByText('Casino play')).toBeInTheDocument()
    expect(within(draft).getByText(/1000000/)).toBeInTheDocument()
  })

  it('filters on the server by jurisdiction and status', async () => {
    signInAs(PA)
    kycReads()
    const seen: URL[] = []
    server.use(http.get('/v1/admin/kyc/enforcement-policies', ({ request }) => { seen.push(new URL(request.url)); return HttpResponse.json([]) }))
    renderWithProviders(<AppRoutes />, { route: '/kyc-policies' })
    const user = userEvent.setup()
    await screen.findByText(/No KYC enforcement policy is recorded/)
    await user.selectOptions(await screen.findByLabelText('Licence jurisdiction'), 'XX - Testland')
    await user.selectOptions(screen.getByLabelText('Status'), 'draft')
    await waitFor(() => expect(seen.some((u) => u.searchParams.get('licensing_jurisdiction_id') === JUR1 && u.searchParams.get('status') === 'draft')).toBe(true))
  })

  it('the author form preselects nothing and offers only the two evaluator-wired trigger types', async () => {
    signInAs(PA)
    kycReads([])
    renderWithProviders(<AppRoutes />, { route: '/kyc-policies' })
    const user = userEvent.setup()
    const dlg = await open(user)
    for (const l of ['Licence jurisdiction', 'Trigger type', 'Legal review reference', 'Reason code']) expect(within(dlg).getByLabelText(l)).toHaveValue('')
    expect(within(dlg).queryByLabelText('Threshold in minor units')).toBeNull()
    expect(within(dlg).queryByLabelText('Asset code')).toBeNull()
    expect(within(dlg).queryByLabelText('Play operation')).toBeNull()
    const trig = within(dlg).getByLabelText('Trigger type')
    expect(within(trig).getAllByRole('option').map((o) => (o as HTMLOptionElement).value)).toEqual(['', 'cumulative_deposit', 'play'])
    expect(within(dlg).getByRole('button', { name: 'Create draft' })).toBeDisabled()
    expect(within(dlg).getByRole('note', { name: 'Source of values' })).toBeInTheDocument()
  })

  it('creates a cumulative_deposit draft with the exact payload; the 38-digit threshold stays a string; the draft is not shown as active', async () => {
    signInAs(PA)
    kycReads([])
    const big = '9'.repeat(38)
    const bodies = capture('post', '/v1/admin/kyc/enforcement-policies', () => HttpResponse.json(kycPolicy({ threshold_minor_units: big }), { status: 201 }))
    renderWithProviders(<AppRoutes />, { route: '/kyc-policies' })
    const user = userEvent.setup()
    const dlg = await open(user)
    await user.selectOptions(within(dlg).getByLabelText('Licence jurisdiction'), 'XX - Testland')
    await user.selectOptions(within(dlg).getByLabelText('Trigger type'), 'cumulative_deposit')
    await user.type(within(dlg).getByLabelText('Threshold in minor units'), big)
    await user.type(within(dlg).getByLabelText('Asset code'), 'EUR')
    await user.type(within(dlg).getByLabelText('Legal review reference'), 'LEGAL-REF-1')
    await user.type(within(dlg).getByLabelText('Reason code'), 'compliance_decision')
    await user.click(within(dlg).getByRole('button', { name: 'Create draft' }))
    expect(await screen.findByText(/Draft created/)).toHaveTextContent('governs nothing until a different platform administrator activates it')
    expect(bodies).toEqual([{ licensing_jurisdiction_id: JUR1, trigger_type: 'cumulative_deposit', threshold_minor_units: big, asset_code: 'EUR', legal_review_reference: 'LEGAL-REF-1', reason_code: 'compliance_decision' }])
    expect(typeof (bodies[0] as { threshold_minor_units: unknown }).threshold_minor_units).toBe('string')
  })

  it('creates a play draft without threshold or asset keys', async () => {
    signInAs(PA)
    kycReads([])
    const bodies = capture('post', '/v1/admin/kyc/enforcement-policies', () => HttpResponse.json(kycPolicy({ trigger_type: 'play', play_operation: 'sportsbook_play', threshold_minor_units: null, asset_code: null }), { status: 201 }))
    renderWithProviders(<AppRoutes />, { route: '/kyc-policies' })
    const user = userEvent.setup()
    const dlg = await open(user)
    await user.selectOptions(within(dlg).getByLabelText('Licence jurisdiction'), 'XX - Testland')
    await user.selectOptions(within(dlg).getByLabelText('Trigger type'), 'play')
    expect(within(dlg).getByLabelText('Play operation')).toHaveValue('')
    await user.selectOptions(within(dlg).getByLabelText('Play operation'), 'sportsbook_play')
    await user.type(within(dlg).getByLabelText('Legal review reference'), 'LEGAL-REF-2')
    await user.type(within(dlg).getByLabelText('Reason code'), 'compliance_decision')
    await user.click(within(dlg).getByRole('button', { name: 'Create draft' }))
    await screen.findByText(/Draft created/)
    expect(bodies).toEqual([{ licensing_jurisdiction_id: JUR1, trigger_type: 'play', play_operation: 'sportsbook_play', legal_review_reference: 'LEGAL-REF-2', reason_code: 'compliance_decision' }])
  })

  it('validates the threshold (digits only, positive), the legal reference and the reason code before enabling submit', async () => {
    signInAs(PA)
    kycReads([])
    let posts = 0
    server.use(http.post('/v1/admin/kyc/enforcement-policies', () => { posts++; return HttpResponse.json(kycPolicy(), { status: 201 }) }))
    renderWithProviders(<AppRoutes />, { route: '/kyc-policies' })
    const user = userEvent.setup()
    const dlg = await open(user)
    await user.selectOptions(within(dlg).getByLabelText('Licence jurisdiction'), 'XX - Testland')
    await user.selectOptions(within(dlg).getByLabelText('Trigger type'), 'cumulative_deposit')
    await user.type(within(dlg).getByLabelText('Asset code'), 'EUR')
    await user.type(within(dlg).getByLabelText('Legal review reference'), 'LEGAL-REF-1')
    await user.type(within(dlg).getByLabelText('Reason code'), 'ok_reason')
    const submit = within(dlg).getByRole('button', { name: 'Create draft' })
    for (const bad of ['0', '1.5', '1e3', '-5', '01']) {
      await user.clear(within(dlg).getByLabelText('Threshold in minor units'))
      await user.type(within(dlg).getByLabelText('Threshold in minor units'), bad)
      expect(submit, bad).toBeDisabled()
    }
    await user.clear(within(dlg).getByLabelText('Threshold in minor units'))
    await user.type(within(dlg).getByLabelText('Threshold in minor units'), '5000')
    expect(submit).toBeEnabled()
    await user.clear(within(dlg).getByLabelText('Legal review reference'))
    await user.type(within(dlg).getByLabelText('Legal review reference'), ' padded')
    expect(submit).toBeDisabled()
    await user.clear(within(dlg).getByLabelText('Legal review reference'))
    await user.type(within(dlg).getByLabelText('Legal review reference'), 'LEGAL-REF-1')
    await user.clear(within(dlg).getByLabelText('Reason code'))
    await user.type(within(dlg).getByLabelText('Reason code'), '_bad')
    expect(submit).toBeDisabled()
    expect(posts).toBe(0)
  })

  it('a 201 whose body is not a draft is "not confirmed", never success', async () => {
    signInAs(PA)
    kycReads([])
    capture('post', '/v1/admin/kyc/enforcement-policies', () => HttpResponse.json({ ok: true }, { status: 201 }))
    renderWithProviders(<AppRoutes />, { route: '/kyc-policies' })
    const user = userEvent.setup()
    const dlg = await open(user)
    await user.selectOptions(within(dlg).getByLabelText('Licence jurisdiction'), 'XX - Testland')
    await user.selectOptions(within(dlg).getByLabelText('Trigger type'), 'play')
    await user.selectOptions(within(dlg).getByLabelText('Play operation'), 'casino_play')
    await user.type(within(dlg).getByLabelText('Legal review reference'), 'R')
    await user.type(within(dlg).getByLabelText('Reason code'), 'r1')
    await user.click(within(dlg).getByRole('button', { name: 'Create draft' }))
    expect(await screen.findByText(/could not be recognised/)).toBeInTheDocument()
    expect(screen.queryByText(/Draft created/)).toBeNull()
  })

  it('activation sends only the reason code; success is shown only for an active body of the same policy', async () => {
    signInAs(PA)
    kycReads([kycPolicy()])
    const bodies = capture('post', `/v1/admin/kyc/enforcement-policies/${POL1}/activate`, () => HttpResponse.json(kycPolicy({ status: 'active' })))
    renderWithProviders(<AppRoutes />, { route: '/kyc-policies' })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Activate' }))
    const dlg = await screen.findByRole('dialog', { name: 'Activate KYC enforcement policy' })
    expect(within(dlg).getByRole('button', { name: 'Activate' })).toBeDisabled()
    await user.type(within(dlg).getByLabelText('Reason code'), 'second_admin_review')
    await user.click(within(dlg).getByRole('button', { name: 'Activate' }))
    expect(await screen.findByText(/Activated\. This policy now enforces KYC/)).toBeInTheDocument()
    expect(bodies).toEqual([{ reason_code: 'second_admin_review' }])
  })

  it('shows the server four-eyes refusal in fixed words (author = activator), not the server message', async () => {
    signInAs(PA)
    kycReads([kycPolicy({ created_by: OTHER })])
    capture('post', `/v1/admin/kyc/enforcement-policies/${POL1}/activate`, () => refusal(403, 'forbidden'))
    renderWithProviders(<AppRoutes />, { route: '/kyc-policies' })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Activate' }))
    const dlg = await screen.findByRole('dialog', { name: 'Activate KYC enforcement policy' })
    await user.type(within(dlg).getByLabelText('Reason code'), 'x_reason')
    await user.click(within(dlg).getByRole('button', { name: 'Activate' }))
    const alert = await within(dlg).findByRole('alert')
    expect(alert).toHaveTextContent('DIFFERENT platform administrator')
    expect(alert).toHaveTextContent('Nothing was changed')
    expect(document.body).not.toHaveTextContent('do-not-render')
    expect(screen.queryByText(/Activated\./)).toBeNull()
  })

  it('warns when the signed-in administrator authored the draft (a hint; the server still decides)', async () => {
    signInAs(PA)
    kycReads([kycPolicy({ created_by: ME })])
    renderWithProviders(<AppRoutes />, { route: '/kyc-policies' })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Activate' }))
    const dlg = await screen.findByRole('dialog', { name: 'Activate KYC enforcement policy' })
    expect(within(dlg).getByText(/You authored this draft/)).toBeInTheDocument()
  })

  it('maps a 409 on activation to the fixed conflict wording', async () => {
    signInAs(PA)
    kycReads([kycPolicy()])
    capture('post', `/v1/admin/kyc/enforcement-policies/${POL1}/activate`, () => refusal(409, 'conflict'))
    renderWithProviders(<AppRoutes />, { route: '/kyc-policies' })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Activate' }))
    const dlg = await screen.findByRole('dialog', { name: 'Activate KYC enforcement policy' })
    await user.type(within(dlg).getByLabelText('Reason code'), 'x_reason')
    await user.click(within(dlg).getByRole('button', { name: 'Activate' }))
    expect(await within(dlg).findByRole('alert')).toHaveTextContent('an active policy already exists for the same enforcement key (supersede that one instead)')
  })

  it('withdraws a draft with the reason code only', async () => {
    signInAs(PA)
    kycReads([kycPolicy()])
    const bodies = capture('post', `/v1/admin/kyc/enforcement-policies/${POL1}/withdraw`, () => HttpResponse.json(kycPolicy({ status: 'withdrawn' })))
    renderWithProviders(<AppRoutes />, { route: '/kyc-policies' })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Withdraw draft' }))
    const dlg = await screen.findByRole('dialog', { name: 'Withdraw draft KYC enforcement policy' })
    await user.type(within(dlg).getByLabelText('Reason code'), 'no_longer_needed')
    await user.click(within(dlg).getByRole('button', { name: 'Withdraw draft' }))
    expect(await screen.findByText('Draft withdrawn.')).toBeInTheDocument()
    expect(bodies).toEqual([{ reason_code: 'no_longer_needed' }])
  })

  it('supersedes an active policy: only drafts of the same enforcement key are offered; the payload names the successor', async () => {
    signInAs(PA)
    const active = kycPolicy({ id: POL1, status: 'active' })
    const sameKey = kycPolicy({ id: POL2, status: 'draft', threshold_minor_units: '2000000' })
    const otherAsset = kycPolicy({ id: POL3, status: 'draft', asset_code: 'GBP', threshold_minor_units: '777' })
    kycReads([active, sameKey, otherAsset])
    const bodies = capture('post', `/v1/admin/kyc/enforcement-policies/${POL1}/supersede`, () => HttpResponse.json(kycPolicy({ id: POL2, status: 'active', threshold_minor_units: '2000000' })))
    renderWithProviders(<AppRoutes />, { route: '/kyc-policies' })
    const user = userEvent.setup()
    const row = await rowOf('Active')
    await user.click(within(row).getByRole('button', { name: 'Supersede' }))
    const dlg = await screen.findByRole('dialog', { name: 'Supersede active KYC enforcement policy' })
    expect(within(dlg).getByRole('note', { name: 'Supersede notice' })).toHaveTextContent('BOTH policies')
    const sel = await within(dlg).findByLabelText('Successor draft')
    await waitFor(() => expect(within(sel).getAllByRole('option')).toHaveLength(2))
    expect(within(sel).queryByText(/777/)).toBeNull()
    await user.selectOptions(sel, within(sel).getByRole('option', { name: /2000000/ }))
    await user.type(within(dlg).getByLabelText('Reason code'), 'replace_threshold')
    await user.click(within(dlg).getByRole('button', { name: 'Supersede' }))
    expect(await screen.findByText(/Superseded\. The successor is now the active policy/)).toBeInTheDocument()
    expect(bodies).toEqual([{ successor_policy_id: POL2, reason_code: 'replace_threshold' }])
  })

  it('a supersede answered with a body that is not the successor is not confirmed', async () => {
    signInAs(PA)
    kycReads([kycPolicy({ id: POL1, status: 'active' }), kycPolicy({ id: POL2, status: 'draft' })])
    capture('post', `/v1/admin/kyc/enforcement-policies/${POL1}/supersede`, () => HttpResponse.json(kycPolicy({ id: POL1, status: 'active' })))
    renderWithProviders(<AppRoutes />, { route: '/kyc-policies' })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Supersede' }))
    const dlg = await screen.findByRole('dialog', { name: 'Supersede active KYC enforcement policy' })
    const sel = await within(dlg).findByLabelText('Successor draft')
    await waitFor(() => expect(within(sel).getAllByRole('option')).toHaveLength(2))
    await user.selectOptions(sel, POL2)
    await user.type(within(dlg).getByLabelText('Reason code'), 'replace_it')
    await user.click(within(dlg).getByRole('button', { name: 'Supersede' }))
    expect(await screen.findByText(/could not be recognised/)).toBeInTheDocument()
    expect(screen.queryByText(/Superseded\./)).toBeNull()
  })

  it('runs the step-up flow for a write: refused once, code entered once, the SAME body re-sent exactly once', async () => {
    signInAs(PA)
    kycReads([kycPolicy()])
    const sends = stepUpOn('post', `/v1/admin/kyc/enforcement-policies/${POL1}/activate`, () => HttpResponse.json(kycPolicy({ status: 'active' })))
    renderWithProviders(<AppRoutes />, { route: '/kyc-policies' })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Activate' }))
    const dlg = await screen.findByRole('dialog', { name: 'Activate KYC enforcement policy' })
    await user.type(within(dlg).getByLabelText('Reason code'), 'second_admin_review')
    await user.click(within(dlg).getByRole('button', { name: 'Activate' }))
    await doStepUp()
    expect(await screen.findByText(/Activated\./)).toBeInTheDocument()
    expect(sends).toHaveLength(2)
    expect(sends[1].body).toBe(sends[0].body)
  })
})

// ---------------------------------------------------------------------------------------------------------------------------
describe('residence determination maker-checker', () => {
  it('shows the default mode "None" with no preselected choice and a disabled submit', async () => {
    signInAs(CO)
    residenceReads()
    renderWithProviders(<AppRoutes />, { route: '/residence-approval' })
    const card = (await screen.findByText('Mode in force')).closest('div') as HTMLElement
    expect(card.parentElement).toHaveTextContent('None')
    expect(screen.getByText(/default state of every tenant/)).toBeInTheDocument()
    expect(screen.getByLabelText('New mode')).toHaveValue('')
    expect(screen.getByLabelText('Reason code')).toHaveValue('')
    expect(screen.getByRole('button', { name: 'File the change' })).toBeDisabled()
    expect(screen.getByRole('note', { name: 'Applies to future determinations' })).toHaveTextContent('FUTURE determinations only')
  })

  it('a stricter mode is applied at once; the payload is exactly {mode, reason_code}; no step-up is needed', async () => {
    signInAs(CO)
    residenceReads()
    const bodies = capture('post', `/v1/admin/tenants/${TEN1}/residence-approval-policy`, () =>
      HttpResponse.json({ policy_id: VER1, status: 'active', required_approvals: 0, previous_mode: 'none', requested_mode: 'all' }),
    )
    renderWithProviders(<AppRoutes />, { route: '/residence-approval' })
    const user = userEvent.setup()
    await user.selectOptions(await screen.findByLabelText('New mode'), 'all')
    expect(screen.getByRole('note', { name: 'Class hint' })).toHaveTextContent('stricter mode')
    await user.type(screen.getByLabelText('Reason code'), 'tighten_review')
    await user.click(screen.getByRole('button', { name: 'File the change' }))
    expect(await screen.findByText(/Applied at once: the mode in force is now "All" \(it was "None"\)/)).toBeInTheDocument()
    expect(bodies).toEqual([{ mode: 'all', reason_code: 'tighten_review' }])
  })

  it('a looser mode (202 pending) is shown as pending with the mode in force unchanged, never as applied', async () => {
    signInAs(CO)
    residenceReads({ effective_mode: 'all', versions: [] })
    const bodies = capture('post', `/v1/admin/tenants/${TEN1}/residence-approval-policy`, () =>
      HttpResponse.json({ policy_id: VER1, status: 'pending', required_approvals: 2, previous_mode: 'all', requested_mode: 'none' }, { status: 202 }),
    )
    renderWithProviders(<AppRoutes />, { route: '/residence-approval' })
    const user = userEvent.setup()
    await user.selectOptions(await screen.findByLabelText('New mode'), 'none')
    expect(screen.getByRole('note', { name: 'Class hint' })).toHaveTextContent('filed PENDING')
    await user.type(screen.getByLabelText('Reason code'), 'relax_review')
    await user.click(screen.getByRole('button', { name: 'File the change' }))
    const out = await screen.findByText(/Pending approval\. Nothing has changed yet: the mode in force is still "All"/)
    expect(out).toHaveTextContent('needs 2 approvals from 2 different people, neither the requester')
    expect(screen.queryByText(/Applied at once/)).toBeNull()
    expect(bodies).toEqual([{ mode: 'none', reason_code: 'relax_review' }])
  })

  it('a 200 that does not echo the requested mode is not confirmed', async () => {
    signInAs(CO)
    residenceReads()
    capture('post', `/v1/admin/tenants/${TEN1}/residence-approval-policy`, () =>
      HttpResponse.json({ policy_id: VER1, status: 'active', required_approvals: 0, previous_mode: 'none', requested_mode: 'into_open_market' }),
    )
    renderWithProviders(<AppRoutes />, { route: '/residence-approval' })
    const user = userEvent.setup()
    await user.selectOptions(await screen.findByLabelText('New mode'), 'all')
    await user.type(screen.getByLabelText('Reason code'), 'tighten_review')
    await user.click(screen.getByRole('button', { name: 'File the change' }))
    expect(await screen.findByText(/could not be recognised/)).toBeInTheDocument()
    expect(screen.queryByText(/Applied at once/)).toBeNull()
  })

  it('refuses to file the mode already in force (no request) and shows the server 409 wording otherwise', async () => {
    signInAs(CO)
    residenceReads({ effective_mode: 'into_open_market', versions: [] })
    capture('post', `/v1/admin/tenants/${TEN1}/residence-approval-policy`, () => refusal(409, 'conflict'))
    renderWithProviders(<AppRoutes />, { route: '/residence-approval' })
    const user = userEvent.setup()
    await user.selectOptions(await screen.findByLabelText('New mode'), 'into_open_market')
    expect(screen.getByText('That is already the mode in force.')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'File the change' })).toBeDisabled()
    await user.selectOptions(screen.getByLabelText('New mode'), 'all')
    await user.type(screen.getByLabelText('Reason code'), 'tighten_review')
    await user.click(screen.getByRole('button', { name: 'File the change' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('already open')
    expect(document.body).not.toHaveTextContent('do-not-render')
  })

  it('lists the history and approves a pending request: payload is the reason only; a second approval activates it', async () => {
    signInAs(CO)
    residenceReads({ effective_mode: 'all', versions: [rVersion({ approvals: 1 })] })
    const bodies = capture('post', `/v1/admin/tenants/${TEN1}/residence-approval-policy/${VER1}/approvals`, () => HttpResponse.json({ status: 'active', approvals: 2, required_approvals: 2 }))
    renderWithProviders(<AppRoutes />, { route: '/residence-approval' })
    const user = userEvent.setup()
    const row = (await screen.findByText('Pending approval')).closest('tr') as HTMLElement
    expect(within(row).getByText('1 of 2')).toBeInTheDocument()
    await user.click(within(row).getByRole('button', { name: 'Approve' }))
    const dlg = await screen.findByRole('dialog', { name: 'Approve residence mode change' })
    await user.type(within(dlg).getByLabelText('Reason code'), 'second_person_ok')
    await user.click(within(dlg).getByRole('button', { name: 'Approve' }))
    expect(await screen.findByText(/Approval recorded \(2 of 2\) and the mode "None" is now in force/)).toBeInTheDocument()
    expect(bodies).toEqual([{ reason_code: 'second_person_ok' }])
  })

  it('a first approval is shown as still pending; the requester\'s own request is disabled', async () => {
    signInAs(CO)
    residenceReads({ effective_mode: 'all', versions: [rVersion({ requested_by: OTHER })] })
    capture('post', `/v1/admin/tenants/${TEN1}/residence-approval-policy/${VER1}/approvals`, () => HttpResponse.json({ status: 'pending', approvals: 1, required_approvals: 2 }))
    renderWithProviders(<AppRoutes />, { route: '/residence-approval' })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Approve' }))
    const dlg = await screen.findByRole('dialog', { name: 'Approve residence mode change' })
    await user.type(within(dlg).getByLabelText('Reason code'), 'first_ok')
    await user.click(within(dlg).getByRole('button', { name: 'Approve' }))
    expect(await screen.findByText(/Approval recorded: 1 of 2\. Still pending - the mode in force has not changed/)).toBeInTheDocument()
  })

  it('own pending request has a disabled approve button; the server four-eyes 403 is shown in fixed words', async () => {
    signInAs(CO)
    residenceReads({ effective_mode: 'all', versions: [rVersion({ requested_by: ME }), rVersion({ id: '71000000-0000-4000-8000-000000000002', requested_by: OTHER })] })
    capture('post', `/v1/admin/tenants/${TEN1}/residence-approval-policy/71000000-0000-4000-8000-000000000002/approvals`, () => refusal(403, 'forbidden'))
    renderWithProviders(<AppRoutes />, { route: '/residence-approval' })
    const user = userEvent.setup()
    const buttons = await screen.findAllByRole('button', { name: 'Approve' })
    expect(buttons).toHaveLength(2)
    expect(buttons.filter((b) => (b as HTMLButtonElement).disabled)).toHaveLength(1)
    await user.click(buttons.find((b) => !(b as HTMLButtonElement).disabled)!)
    const dlg = await screen.findByRole('dialog', { name: 'Approve residence mode change' })
    await user.type(within(dlg).getByLabelText('Reason code'), 'try_it')
    await user.click(within(dlg).getByRole('button', { name: 'Approve' }))
    expect(await within(dlg).findByRole('alert')).toHaveTextContent('a different, linked Person must approve')
    expect(document.body).not.toHaveTextContent('do-not-render')
  })

  it('the queue states that a pending determination is NOT market evidence and shows no country', async () => {
    signInAs(CO)
    residenceReads({ effective_mode: 'all', versions: [] }, [pendingRow()])
    renderWithProviders(<AppRoutes />, { route: '/residence-approval' })
    const note = await screen.findByRole('note', { name: 'Pending determinations are not evidence' })
    expect(note).toHaveTextContent('NOT market evidence')
    expect(note).toHaveTextContent('The country is not shown here')
    expect(await screen.findByText('Not market evidence')).toBeInTheDocument()
    expect(screen.queryByText(/country_code|verified_residence_country/)).toBeNull()
    expect(screen.getByRole('button', { name: 'Approve as second Person' })).toBeEnabled()
  })

  it('approves a determination passing determined_at back byte-for-byte; success only for an approved body of that verification', async () => {
    signInAs(CO)
    const row = pendingRow({ determined_by: OTHER })
    residenceReads({ effective_mode: 'all', versions: [] }, [row])
    const bodies = capture('post', `/v1/admin/kyc/verifications/${row.verification_id}/residence-approval`, () => HttpResponse.json({ verification_id: row.verification_id, status: 'approved' }))
    renderWithProviders(<AppRoutes />, { route: '/residence-approval' })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Approve as second Person' }))
    const dlg = await screen.findByRole('dialog', { name: 'Approve residence determination' })
    expect(within(dlg).getByRole('note', { name: 'Second Person' })).toHaveTextContent('different staff account AND a different, linked Person')
    await user.type(within(dlg).getByLabelText('Reason code'), 'second_person_checked')
    await user.click(within(dlg).getByRole('button', { name: 'Approve determination' }))
    expect(await screen.findByText('Approved. The determination is now market evidence.')).toBeInTheDocument()
    expect(bodies).toEqual([{ determined_at: DET_AT, reason_code: 'second_person_checked' }])
  })

  it('an approve answered with another verification is not confirmed', async () => {
    signInAs(CO)
    const row = pendingRow()
    residenceReads({ effective_mode: 'all', versions: [] }, [row])
    capture('post', `/v1/admin/kyc/verifications/${row.verification_id}/residence-approval`, () => HttpResponse.json({ verification_id: '00000000-0000-4000-8000-000000000000', status: 'approved' }))
    renderWithProviders(<AppRoutes />, { route: '/residence-approval' })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Approve as second Person' }))
    const dlg = await screen.findByRole('dialog', { name: 'Approve residence determination' })
    await user.type(within(dlg).getByLabelText('Reason code'), 'second_person_checked')
    await user.click(within(dlg).getByRole('button', { name: 'Approve determination' }))
    expect(await screen.findByText(/could not be recognised/)).toBeInTheDocument()
    expect(screen.queryByText(/Approved\. The determination/)).toBeNull()
  })

  it('the determiner\'s own row is disabled; a 404 (replaced determination) and a 403 are shown in fixed words', async () => {
    signInAs(CO)
    const own = pendingRow({ determined_by: ME })
    const other = pendingRow({ verification_id: '74000000-0000-4000-8000-000000000002', determined_by: OTHER })
    residenceReads({ effective_mode: 'all', versions: [] }, [own, other])
    capture('post', `/v1/admin/kyc/verifications/${other.verification_id}/residence-approval`, () => refusal(404, 'not_found'))
    renderWithProviders(<AppRoutes />, { route: '/residence-approval' })
    const user = userEvent.setup()
    const buttons = await screen.findAllByRole('button', { name: 'Approve as second Person' })
    expect(buttons.filter((b) => (b as HTMLButtonElement).disabled)).toHaveLength(1)
    await user.click(buttons.find((b) => !(b as HTMLButtonElement).disabled)!)
    const dlg = await screen.findByRole('dialog', { name: 'Approve residence determination' })
    await user.type(within(dlg).getByLabelText('Reason code'), 'second_person_checked')
    await user.click(within(dlg).getByRole('button', { name: 'Approve determination' }))
    expect(await within(dlg).findByRole('alert')).toHaveTextContent('A newer determination may have replaced it')
    expect(document.body).not.toHaveTextContent('do-not-render')
  })

  it('determination approval runs the step-up flow and re-sends the same body exactly once', async () => {
    signInAs({ ...CO, sub: ME })
    const row = pendingRow()
    residenceReads({ effective_mode: 'all', versions: [] }, [row])
    const STEPPED = makeTestJwt({ ...CO, jti: 'stepped-res' })
    const sends: { auth: string | null; body: string }[] = []
    server.use(
      http.post(`/v1/admin/kyc/verifications/${row.verification_id}/residence-approval`, async ({ request }) => {
        const rec = { auth: request.headers.get('Authorization'), body: await request.text() }
        sends.push(rec)
        return rec.auth === `Bearer ${STEPPED}` ? HttpResponse.json({ verification_id: row.verification_id, status: 'approved' }) : stepUpRequiredResponse()
      }),
      http.post('/v1/staff/auth/step-up', () => HttpResponse.json({ access_token: STEPPED, token_type: 'Bearer', expires_in: 900, mfa_at: '2026-10-13T10:00:00Z', step_up_expires_at: { financial: '2026-10-13T10:10:00Z', critical: '2026-10-13T10:05:00Z' } })),
    )
    renderWithProviders(<AppRoutes />, { route: '/residence-approval' })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Approve as second Person' }))
    const dlg = await screen.findByRole('dialog', { name: 'Approve residence determination' })
    await user.type(within(dlg).getByLabelText('Reason code'), 'second_person_checked')
    await user.click(within(dlg).getByRole('button', { name: 'Approve determination' }))
    await doStepUp()
    expect(await screen.findByText(/Approved\. The determination/)).toBeInTheDocument()
    expect(sends).toHaveLength(2)
    expect(sends[1].body).toBe(sends[0].body)
  })
})

// ---------------------------------------------------------------------------------------------------------------------------
describe('withdrawal restriction policy', () => {
  it('a tenant role lands on its own tenant: UNCONFIGURED notice per cause, hard restrictions, status causes informational only', async () => {
    signInAs(TA)
    wrpReads()
    renderWithProviders(<AppRoutes />, { route: '/withdrawal-restrictions' })
    expect(await screen.findByRole('note', { name: 'Hard restrictions' })).toHaveTextContent('AML or sanctions holds')
    expect(screen.getByRole('note', { name: 'Unconfigured' })).toHaveTextContent('UNCONFIGURED')
    expect(screen.getByRole('note', { name: 'Unconfigured licence_suspended' })).toHaveTextContent('interim engineering default')
    expect(screen.getAllByText('Default, unconfigured').length).toBe(12)
    for (const stage of ['Request', 'Dispatch', 'Re-dispatch']) expect(screen.getAllByText(stage).length).toBeGreaterThan(0)
    const info = screen.getByText('Not configurable here (information only)').closest('div') as HTMLElement
    const infoCard = info.parentElement as HTMLElement
    expect(within(infoCard).getByText('Tenant suspended')).toBeInTheDocument()
    expect(within(infoCard).getByText('Brand closed')).toBeInTheDocument()
    expect(within(infoCard).getByText('Market closed')).toBeInTheDocument()
    expect(within(infoCard).queryAllByRole('button')).toHaveLength(0)
    expect(within(infoCard).getByRole('note', { name: 'Status causes' })).toHaveTextContent('NOT configurable here')
  })

  it('a configured cause shows the configured action and set version, with no UNCONFIGURED notice for it', async () => {
    signInAs(TA)
    wrpReads(wrpConfigured())
    renderWithProviders(<AppRoutes />, { route: `/withdrawal-restrictions/tenants/${TEN1}` })
    expect(await screen.findAllByText('Configured (set version 3)')).toHaveLength(3)
    expect(screen.queryByRole('note', { name: 'Unconfigured' })).toBeNull()
    expect(screen.queryByRole('note', { name: 'Unconfigured licence_suspended' })).toBeNull()
    expect(screen.getByRole('note', { name: 'Unconfigured licence_expired' })).toBeInTheDocument()
    expect(screen.getAllByText('Block').length).toBeGreaterThan(0)
  })

  it('a platform administrator chooses the tenant explicitly (no default) and then sees the policy', async () => {
    signInAs(PA)
    wrpReads()
    renderWithProviders(<AppRoutes />, { route: '/withdrawal-restrictions' })
    const user = userEvent.setup()
    const sel = await screen.findByLabelText('Tenant')
    expect(sel).toHaveValue('')
    await user.selectOptions(sel, await screen.findByRole('option', { name: 'Acme (acme)' }))
    expect(await screen.findByText('Withdrawal restriction policy of Acme')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'File a change' })).toBeInTheDocument()
  })

  const openFile = async (user: User) => {
    await user.click(await screen.findByRole('button', { name: 'File a change' }))
    return screen.findByRole('dialog', { name: 'File a withdrawal restriction change' })
  }
  const fill = async (user: User, dlg: HTMLElement, cause: string, action: string) => {
    await user.selectOptions(within(dlg).getByLabelText('Cause'), cause)
    await user.selectOptions(within(dlg).getByLabelText('Action'), action)
    await user.type(within(dlg).getByLabelText('Reason code'), 'owner_decision_q12')
  }

  it('the filing dialog preselects nothing and offers only the four licence causes and three actions', async () => {
    signInAs(PA)
    wrpReads(wrpConfigured())
    renderWithProviders(<AppRoutes />, { route: `/withdrawal-restrictions/tenants/${TEN1}` })
    const user = userEvent.setup()
    const dlg = await openFile(user)
    for (const l of ['Cause', 'Action', 'Reason code']) expect(within(dlg).getByLabelText(l)).toHaveValue('')
    expect(within(dlg).getByLabelText('Note (optional)')).toHaveValue('')
    expect(within(within(dlg).getByLabelText('Cause')).getAllByRole('option').map((o) => (o as HTMLOptionElement).value)).toEqual(['', 'licence_suspended', 'licence_expired', 'licence_not_yet_issued', 'licence_missing'])
    expect(within(within(dlg).getByLabelText('Action')).getAllByRole('option').map((o) => (o as HTMLOptionElement).value)).toEqual(['', 'allow', 'hold_for_review', 'block'])
    expect(within(dlg).getByRole('button', { name: 'File change' })).toBeDisabled()
    expect(within(dlg).getByRole('note', { name: 'Semantics' })).toHaveTextContent('loosening change')
  })

  it('a tightening change (201 executed:true) is shown as applied; exact payload; the loosening hint appears for a looser choice', async () => {
    signInAs(PA)
    wrpReads()
    const bodies = capture('post', '/v1/admin/withdrawal-restriction-policy-changes', () =>
      HttpResponse.json(wrpResponse(wrpChange({ status: 'executed', widening: false, required_approvals: 1, action: 'block' }), true), { status: 201 }),
    )
    renderWithProviders(<AppRoutes />, { route: `/withdrawal-restrictions/tenants/${TEN1}` })
    const user = userEvent.setup()
    const dlg = await openFile(user)
    await fill(user, dlg, 'licence_suspended', 'block')
    expect(within(dlg).getByRole('note', { name: 'Class hint' })).toHaveTextContent('tightening or equal')
    await user.type(within(dlg).getByLabelText('Note (optional)'), 'ticket 42')
    await user.click(within(dlg).getByRole('button', { name: 'File change' }))
    expect(await screen.findByText(/Applied at once\. Licence suspended is now "Block"/)).toBeInTheDocument()
    expect(bodies).toEqual([{ tenant_id: TEN1, cause: 'licence_suspended', action: 'block', reason_code: 'owner_decision_q12', note: 'ticket 42' }])
  })

  it('omits the note key when empty; a loosening change (201 executed:false) is PENDING, never applied', async () => {
    signInAs(PA)
    wrpReads(wrpConfigured())
    const bodies = capture('post', '/v1/admin/withdrawal-restriction-policy-changes', () =>
      HttpResponse.json(wrpResponse(wrpChange({ status: 'requested', widening: true, action: 'allow' }), false), { status: 201 }),
    )
    renderWithProviders(<AppRoutes />, { route: `/withdrawal-restrictions/tenants/${TEN1}` })
    const user = userEvent.setup()
    const dlg = await openFile(user)
    await fill(user, dlg, 'licence_suspended', 'allow')
    expect(within(dlg).getByRole('note', { name: 'Class hint' })).toHaveTextContent('LOOSENING')
    await user.click(within(dlg).getByRole('button', { name: 'File change' }))
    const out = await screen.findByText(/Pending approval\. Nothing has changed yet\. This is a loosening change: it needs 2 approvals/)
    expect(out).toBeInTheDocument()
    expect(screen.queryByText(/Applied at once/)).toBeNull()
    expect(bodies).toEqual([{ tenant_id: TEN1, cause: 'licence_suspended', action: 'allow', reason_code: 'owner_decision_q12' }])
    expect(Object.keys(bodies[0] as object)).not.toContain('note')
  })

  it('a 201 whose body is neither shape is not confirmed', async () => {
    signInAs(PA)
    wrpReads()
    capture('post', '/v1/admin/withdrawal-restriction-policy-changes', () => HttpResponse.json({ change: { id: WRC1, status: 'requested' }, executed: true, disclaimer: 'd' }, { status: 201 }))
    renderWithProviders(<AppRoutes />, { route: `/withdrawal-restrictions/tenants/${TEN1}` })
    const user = userEvent.setup()
    const dlg = await openFile(user)
    await fill(user, dlg, 'licence_expired', 'hold_for_review')
    await user.click(within(dlg).getByRole('button', { name: 'File change' }))
    expect(await screen.findByText(/could not be recognised/)).toBeInTheDocument()
    expect(screen.queryByText(/Applied at once/)).toBeNull()
    expect(screen.queryByText(/Pending approval\. Nothing has changed yet/)).toBeNull()
  })

  it.each([
    ['WITHDRAWAL_RESTRICTION_POLICY_NOT_CONFIGURABLE', 409, 'governed by recorded owner decisions'],
    ['WITHDRAWAL_RESTRICTION_POLICY_DUPLICATE_PENDING', 409, 'An open change already exists'],
    ['WITHDRAWAL_RESTRICTION_POLICY_RETRYABLE_LOCK_TIMEOUT', 409, 'Try again in a moment'],
    ['WITHDRAWAL_RESTRICTION_POLICY_PROOF_REFUSED', 403, 'signed proof'],
    ['WITHDRAWAL_RESTRICTION_POLICY_NO_PERSON', 403, 'not linked to a person'],
    ['forbidden', 403, 'only platform administrators may file'],
  ])('filing refusal %s is shown in fixed words and never the server message', async (code, status, text) => {
    signInAs(PA)
    wrpReads()
    capture('post', '/v1/admin/withdrawal-restriction-policy-changes', () => refusal(status, code))
    renderWithProviders(<AppRoutes />, { route: `/withdrawal-restrictions/tenants/${TEN1}` })
    const user = userEvent.setup()
    const dlg = await openFile(user)
    await fill(user, dlg, 'licence_missing', 'block')
    await user.click(within(dlg).getByRole('button', { name: 'File change' }))
    const alert = await within(dlg).findByRole('alert')
    expect(alert).toHaveTextContent(text)
    expect(alert).toHaveTextContent('Nothing was changed')
    expect(document.body).not.toHaveTextContent('do-not-render')
  })

  it('filing runs the step-up flow and re-sends the same body exactly once', async () => {
    signInAs(PA)
    wrpReads()
    const sends = stepUpOn('post', '/v1/admin/withdrawal-restriction-policy-changes', () => HttpResponse.json(wrpResponse(wrpChange({ status: 'executed', widening: false, action: 'block' }), true), { status: 201 }))
    renderWithProviders(<AppRoutes />, { route: `/withdrawal-restrictions/tenants/${TEN1}` })
    const user = userEvent.setup()
    const dlg = await openFile(user)
    await fill(user, dlg, 'licence_suspended', 'block')
    await user.click(within(dlg).getByRole('button', { name: 'File change' }))
    await doStepUp()
    await screen.findByText(/Applied at once/)
    expect(sends).toHaveLength(2)
    expect(sends[1].body).toBe(sends[0].body)
  })

  it('lists change requests with a server status filter and links to the detail', async () => {
    signInAs(PA)
    const seen: URL[] = []
    wrpReads(wrpEffective(), [wrpChange()])
    server.use(http.get('/v1/admin/withdrawal-restriction-policy-changes', ({ request }) => { seen.push(new URL(request.url)); return HttpResponse.json({ items: [wrpChange()], disclaimer: 'd' }) }))
    renderWithProviders(<AppRoutes />, { route: '/withdrawal-restrictions/changes' })
    const user = userEvent.setup()
    expect(await screen.findByRole('link', { name: WRC1.slice(0, 8) })).toHaveAttribute('href', `/withdrawal-restrictions/changes/${WRC1}`)
    expect(screen.getByText('Loosening')).toBeInTheDocument()
    await user.selectOptions(screen.getByLabelText('Status'), 'executed')
    await waitFor(() => expect(seen.some((u) => u.searchParams.get('status') === 'executed')).toBe(true))
    expect(seen[0].searchParams.get('limit')).toBe('200')
  })

  describe('change detail', () => {
    it('approves with the reason code only; "executed" is shown only when the server says executed', async () => {
      signInAs(PA)
      wrpReads(wrpEffective(), [wrpChange({ approvals: [{ approver_id: OTHER, reason_code: 'ok1', decided_at: '2026-10-18T11:00:00Z' }] })])
      const bodies = capture('post', `/v1/admin/withdrawal-restriction-policy-changes/${WRC1}/approve`, () => HttpResponse.json(wrpResponse(wrpChange({ status: 'executed' }), true)))
      renderWithProviders(<AppRoutes />, { route: `/withdrawal-restrictions/changes/${WRC1}` })
      const user = userEvent.setup()
      expect(await screen.findByLabelText('Approval rules')).toHaveTextContent('2 approvals from 2 different people')
      expect(screen.getByRole('note', { name: 'Hard restrictions' })).toBeInTheDocument()
      await user.click(screen.getByRole('button', { name: 'Approve' }))
      const dlg = await screen.findByRole('dialog', { name: 'Approve withdrawal restriction change' })
      await user.type(within(dlg).getByLabelText('Reason code'), 'second_ok')
      await user.click(within(dlg).getByRole('button', { name: 'Approve' }))
      expect(await screen.findByText('Approval recorded and the change was executed.')).toBeInTheDocument()
      expect(bodies).toEqual([{ reason_code: 'second_ok' }])
    })

    it('a first approval is shown as still pending, not executed', async () => {
      signInAs(PA)
      wrpReads(wrpEffective(), [wrpChange()])
      capture('post', `/v1/admin/withdrawal-restriction-policy-changes/${WRC1}/approve`, () => HttpResponse.json(wrpResponse(wrpChange({ approvals: [{ approver_id: ME, reason_code: 'ok1', decided_at: '2026-10-19T11:00:00Z' }] }), false)))
      renderWithProviders(<AppRoutes />, { route: `/withdrawal-restrictions/changes/${WRC1}` })
      const user = userEvent.setup()
      await user.click(await screen.findByRole('button', { name: 'Approve' }))
      const dlg = await screen.findByRole('dialog', { name: 'Approve withdrawal restriction change' })
      await user.type(within(dlg).getByLabelText('Reason code'), 'first_ok')
      await user.click(within(dlg).getByRole('button', { name: 'Approve' }))
      expect(await screen.findByText(/Approval recorded: 1 of 2\. Still pending approval - the change has not taken effect/)).toBeInTheDocument()
      expect(screen.queryByText(/was executed/)).toBeNull()
    })

    it('the requester cannot approve or reject (disabled hint) but can cancel, and cancel sends no body and needs no reason', async () => {
      signInAs(PA)
      wrpReads(wrpEffective(), [wrpChange({ requested_by: ME })])
      const bodies = capture('post', `/v1/admin/withdrawal-restriction-policy-changes/${WRC1}/cancel`, () => HttpResponse.json(wrpResponse(wrpChange({ status: 'cancelled', requested_by: ME }), false)))
      renderWithProviders(<AppRoutes />, { route: `/withdrawal-restrictions/changes/${WRC1}` })
      const user = userEvent.setup()
      expect(await screen.findByRole('button', { name: 'Approve' })).toBeDisabled()
      expect(screen.getByRole('button', { name: 'Reject' })).toBeDisabled()
      await user.click(screen.getByRole('button', { name: 'Cancel request' }))
      const dlg = await screen.findByRole('dialog', { name: 'Cancel withdrawal restriction change' })
      expect(within(dlg).queryByLabelText('Reason code')).toBeNull()
      await user.click(within(dlg).getByRole('button', { name: 'Cancel request' }))
      expect(await screen.findByText('Cancelled. The request is closed and nothing was changed.')).toBeInTheDocument()
      expect(bodies).toEqual([null])
    })

    it('rejects with the reason code only (no step-up)', async () => {
      signInAs(PA)
      wrpReads(wrpEffective(), [wrpChange()])
      const bodies = capture('post', `/v1/admin/withdrawal-restriction-policy-changes/${WRC1}/reject`, () => HttpResponse.json(wrpResponse(wrpChange({ status: 'rejected' }), false)))
      renderWithProviders(<AppRoutes />, { route: `/withdrawal-restrictions/changes/${WRC1}` })
      const user = userEvent.setup()
      await user.click(await screen.findByRole('button', { name: 'Reject' }))
      const dlg = await screen.findByRole('dialog', { name: 'Reject withdrawal restriction change' })
      await user.type(within(dlg).getByLabelText('Reason code'), 'not_justified')
      await user.click(within(dlg).getByRole('button', { name: 'Reject' }))
      expect(await screen.findByText('Rejected. The request is closed and nothing was changed.')).toBeInTheDocument()
      expect(bodies).toEqual([{ reason_code: 'not_justified' }])
    })

    it.each([
      ['WITHDRAWAL_RESTRICTION_POLICY_SELF_APPROVAL', 'You cannot decide a request you filed'],
      ['WITHDRAWAL_RESTRICTION_POLICY_ALREADY_DECIDED', 'already approved this request'],
      ['WITHDRAWAL_RESTRICTION_POLICY_APPROVALS_STALE', 'older than 24 hours'],
      ['WITHDRAWAL_RESTRICTION_POLICY_NOT_PENDING', 'no longer open'],
    ])('the server refusal %s is shown in fixed words', async (code, text) => {
      signInAs(PA)
      wrpReads(wrpEffective(), [wrpChange()])
      capture('post', `/v1/admin/withdrawal-restriction-policy-changes/${WRC1}/approve`, () => refusal(409, code))
      renderWithProviders(<AppRoutes />, { route: `/withdrawal-restrictions/changes/${WRC1}` })
      const user = userEvent.setup()
      await user.click(await screen.findByRole('button', { name: 'Approve' }))
      const dlg = await screen.findByRole('dialog', { name: 'Approve withdrawal restriction change' })
      await user.type(within(dlg).getByLabelText('Reason code'), 'some_reason')
      await user.click(within(dlg).getByRole('button', { name: 'Approve' }))
      const alert = await within(dlg).findByRole('alert')
      expect(alert).toHaveTextContent(text)
      expect(document.body).not.toHaveTextContent('do-not-render')
    })

    it('approval runs the step-up flow and re-sends the same body exactly once', async () => {
      signInAs(PA)
      wrpReads(wrpEffective(), [wrpChange()])
      const sends = stepUpOn('post', `/v1/admin/withdrawal-restriction-policy-changes/${WRC1}/approve`, () => HttpResponse.json(wrpResponse(wrpChange({ status: 'executed' }), true)))
      renderWithProviders(<AppRoutes />, { route: `/withdrawal-restrictions/changes/${WRC1}` })
      const user = userEvent.setup()
      await user.click(await screen.findByRole('button', { name: 'Approve' }))
      const dlg = await screen.findByRole('dialog', { name: 'Approve withdrawal restriction change' })
      await user.type(within(dlg).getByLabelText('Reason code'), 'second_ok')
      await user.click(within(dlg).getByRole('button', { name: 'Approve' }))
      await doStepUp()
      expect(await screen.findByText('Approval recorded and the change was executed.')).toBeInTheDocument()
      expect(sends).toHaveLength(2)
      expect(sends[1].body).toBe(sends[0].body)
    })

    it('renders a note containing markup as text', async () => {
      signInAs(PA)
      wrpReads(wrpEffective(), [wrpChange({ note: '<img src=x onerror=alert(1)>' })])
      renderWithProviders(<AppRoutes />, { route: `/withdrawal-restrictions/changes/${WRC1}` })
      expect(await screen.findByText(/<img src=x onerror=alert\(1\)>/)).toBeInTheDocument()
      expect(document.querySelector('img')).toBeNull()
    })
  })
})
