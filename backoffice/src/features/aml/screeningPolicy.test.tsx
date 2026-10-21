import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it, vi } from 'vitest'
import { AppRoutes } from '../../app/AppRoutes'
import { makeTestJwt } from '../../test/jwt'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { PLATFORM_ADMIN_CLAIMS, signInAs } from '../../test/session'
import { stepUpRequiredResponse } from '../../test/stepUp'

vi.setConfig({ testTimeout: 30000 })

const TENANT = '11111111-1111-4111-8111-111111111111'
const JUR = '10000000-0000-4000-8000-000000000001'
const POLICY = 'b1000000-0000-4000-8000-000000000001'
const ME = '66666666-6666-4666-8666-666666666666'
const BASE = `/v1/admin/tenants/${TENANT}/screening`
const COMPLIANCE = { role: 'compliance', tenant_id: TENANT, sub: ME }
const PA = { role: 'platform_admin', tenant_id: PLATFORM_ADMIN_CLAIMS.tenant_id as string, sub: ME }
const RAW = 'RAW SERVER MESSAGE <b>do not show</b>'
const ERR = (status: number, code: string, checks?: string[]) => HttpResponse.json({ code, message: RAW, request_id: 'req-pol', check_codes: checks }, { status })
const NONE = { configured: false, lists: [], rescreen_interval_seconds: null, backfill_grace_seconds: null, effective_from: null }
const NO_TRIGGER = { configured: false, events: [], gate_withdrawal: false, gate_alert_after_seconds: null, effective_from: null }
const GATING = { configured: true, events: ['scheduled'], gate_withdrawal: true, gate_alert_after_seconds: 3600, effective_from: '2026-10-01T10:00:00Z' }
type User = ReturnType<typeof userEvent.setup>

const localInput = (offsetMs: number) => {
  const d = new Date(Date.now() + offsetMs)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`
}

function reads(policy: Record<string, unknown> = NONE, trigger: Record<string, unknown> = NO_TRIGGER) {
  server.use(
    http.get('/v1/admin/tenants', () => HttpResponse.json({ items: [{ id: TENANT, name: 'Brand Co', slug: 'brand', status: 'active' }], limit: 50, offset: 0, total: 1 })),
    http.get('/v1/admin/jurisdictions', () => HttpResponse.json([{ id: JUR, code: 'XX', name: 'Testland', created_at: '2026-01-01T00:00:00Z' }])),
    http.get(`${BASE}/policy`, () => HttpResponse.json(policy)),
    http.get(`${BASE}/trigger-policy`, () => HttpResponse.json(trigger)),
  )
}

async function openTab(policy?: Record<string, unknown>, trigger?: Record<string, unknown>) {
  signInAs(PA)
  reads(policy, trigger)
  const user = userEvent.setup()
  renderWithProviders(<AppRoutes />, { route: '/aml/screening' })
  const select = await screen.findByLabelText('Tenant')
  await waitFor(() => expect(within(select).getByRole('option', { name: 'Brand Co' })).toBeInTheDocument())
  await user.selectOptions(select, TENANT)
  await user.click(await screen.findByRole('tab', { name: 'Screening policy' }))
  const jsel = await screen.findByLabelText('Licence jurisdiction')
  await waitFor(() => expect(within(jsel).getByRole('option', { name: /Testland/ })).toBeInTheDocument())
  await user.selectOptions(jsel, JUR)
  await user.click(screen.getByRole('button', { name: 'Show screening policy in force' }))
  return user
}

async function openAuthor(user: User) {
  await user.click(await screen.findByRole('button', { name: 'Author next screening policy version' }))
  return screen.findByRole('dialog', { name: 'Author the next screening policy version' })
}

function capture(answer: () => Response) {
  const bodies: unknown[] = []
  server.use(http.post(`${BASE}/policy`, async ({ request }) => { bodies.push(JSON.parse(await request.text())); return answer() }))
  return bodies
}

async function fill(dlg: HTMLElement, user: User, o: { lists?: string[]; interval?: string; grace?: string; offset?: number } = {}) {
  for (const l of o.lists ?? ['Sanctions list']) await user.click(within(dlg).getByLabelText(l))
  await user.type(within(dlg).getByLabelText('Re-screen interval (seconds)'), o.interval ?? '86400')
  await user.type(within(dlg).getByLabelText('Backfill grace (seconds)'), o.grace ?? '0')
  fireEvent.change(within(dlg).getByLabelText('Effective from'), { target: { value: localInput(o.offset ?? 3_600_000) } })
  await user.type(within(dlg).getByLabelText('Legal review reference'), 'LEGAL-7')
  await user.type(within(dlg).getByLabelText('Reason code'), 'policy_decision')
}

describe('RBAC (UI hint only; the server decides): no request is sent for a role without the permission', () => {
  it.each([['tenant_admin'], ['finance'], ['support'], ['risk_manager'], ['promotions_manager'], ['bonus_operations']])('%s sees NotAuthorized and no request is sent', async (role) => {
    signInAs({ role, tenant_id: TENANT, sub: ME })
    let calls = 0
    server.use(http.all(/\/screening\//, () => { calls++; return HttpResponse.json({}) }))
    renderWithProviders(<AppRoutes />, { route: '/aml/screening' })
    expect(await screen.findByText('Your role does not have access to this section.')).toBeInTheDocument()
    expect(calls).toBe(0)
  })

  it('a tenant-scoped compliance officer reads the policy in its OWN tenant and is offered no authoring', async () => {
    signInAs(COMPLIANCE)
    let path = ''
    server.use(
      http.get(`${BASE}/policy`, ({ request }) => { path = new URL(request.url).pathname + new URL(request.url).search; return HttpResponse.json(NONE) }),
      http.get(`${BASE}/trigger-policy`, () => HttpResponse.json(NO_TRIGGER)),
    )
    const user = userEvent.setup()
    renderWithProviders(<AppRoutes />, { route: '/aml/screening' })
    await user.click(await screen.findByRole('tab', { name: 'Screening policy' }))
    await user.type(await screen.findByLabelText('Jurisdiction id'), JUR)
    await user.click(screen.getByRole('button', { name: 'Show screening policy in force' }))
    expect(await screen.findByText(/No screening policy is in force/)).toBeInTheDocument()
    expect(path).toBe(`${BASE}/policy?jurisdiction_id=${JUR}`)
    expect(screen.queryByRole('button', { name: 'Author next screening policy version' })).toBeNull()
    expect(screen.getByText(/Authoring is not offered to your role/)).toBeInTheDocument()
  })
})

describe('screening policy view', () => {
  it('shows the safety rules in words and the not-shipped note; nothing is requested before a jurisdiction is chosen', async () => {
    signInAs(COMPLIANCE)
    let calls = 0
    server.use(http.get(`${BASE}/policy`, () => { calls++; return HttpResponse.json({}) }))
    renderWithProviders(<AppRoutes />, { route: '/aml/screening' })
    await userEvent.setup().click(await screen.findByRole('tab', { name: 'Screening policy' }))
    const rules = await screen.findByRole('region', { name: 'Screening policy safety rules' })
    expect(rules).toHaveTextContent('at least 3600 seconds')
    expect(rules).toHaveTextContent('twice the gate alert threshold')
    expect(rules).toHaveTextContent('between now (2 minutes of clock skew are tolerated) and 30 days ahead')
    expect(rules).toHaveTextContent('Tighten only')
    expect(rules).toHaveTextContent('A list cannot be removed')
    expect(screen.getByRole('note', { name: 'Screening policy note' })).toHaveTextContent('ships NO value')
    expect(calls).toBe(0)
  })

  it('shows a configured policy and the gate threshold rule with its number', async () => {
    await openTab({ configured: true, lists: ['sanctions', 'pep'], rescreen_interval_seconds: 86400, backfill_grace_seconds: 0, effective_from: '2026-10-20T10:00:00Z' }, GATING)
    const dl = await screen.findByLabelText('Screening policy in force')
    expect(dl).toHaveTextContent('Sanctions list, PEP list')
    expect(dl).toHaveTextContent('86400 seconds')
    expect(dl).toHaveTextContent('0 seconds')
    expect(await screen.findByLabelText('Gate threshold of the jurisdiction')).toHaveTextContent('so the interval must be at least 7200 seconds')
  })

  it('an unrecognised body is not shown as a policy', async () => {
    await openTab({ configured: true, lists: ['bogus'] })
    expect(await screen.findByText(/could not be recognised/)).toBeInTheDocument()
    expect(screen.queryByLabelText('Screening policy in force')).toBeNull()
  })

  it('XSS: server strings are text only', async () => {
    await openTab({ configured: true, lists: ['pep'], rescreen_interval_seconds: 3600, backfill_grace_seconds: 1, effective_from: '<img src=x onerror=alert(1)>' })
    await screen.findByLabelText('Screening policy in force')
    expect(document.querySelector('img')).toBeNull()
  })

  it('a read refusal is shown in fixed words', async () => {
    signInAs(PA)
    reads()
    server.use(http.get(`${BASE}/policy`, () => ERR(403, 'forbidden')))
    const user = userEvent.setup()
    renderWithProviders(<AppRoutes />, { route: '/aml/screening' })
    const select = await screen.findByLabelText('Tenant')
    await waitFor(() => expect(within(select).getByRole('option', { name: 'Brand Co' })).toBeInTheDocument())
    await user.selectOptions(select, TENANT)
    await user.click(await screen.findByRole('tab', { name: 'Screening policy' }))
    const jsel = await screen.findByLabelText('Licence jurisdiction')
    await waitFor(() => expect(within(jsel).getByRole('option', { name: /Testland/ })).toBeInTheDocument())
    await user.selectOptions(jsel, JUR)
    await user.click(screen.getByRole('button', { name: 'Show screening policy in force' }))
    expect(await screen.findByText(/does not hold an in-force AML officer grant/)).toBeInTheDocument()
    expect(document.body).not.toHaveTextContent('RAW SERVER MESSAGE')
  })
})

describe('screening policy authoring', () => {
  it('starts empty: no list, interval, grace, time, reference or reason preselected; submit disabled', async () => {
    const user = await openTab()
    const dlg = await openAuthor(user)
    for (const cb of within(dlg).getAllByRole('checkbox')) expect(cb).not.toBeChecked()
    for (const l of ['Re-screen interval (seconds)', 'Backfill grace (seconds)', 'Effective from', 'Legal review reference', 'Reason code']) expect(within(dlg).getByLabelText(l)).toHaveValue('')
    expect(within(dlg).getByRole('button', { name: 'Author screening policy version' })).toBeDisabled()
    expect(within(dlg).getByRole('region', { name: 'Screening policy safety rules' })).toHaveTextContent('never be looser')
  })

  it('payload: exact keys, integer seconds, an explicit 0 grace, no tenant, actor or proof; 201 with a policy_id is confirmed', async () => {
    const user = await openTab()
    const bodies = capture(() => HttpResponse.json({ policy_id: POLICY }, { status: 201 }))
    const dlg = await openAuthor(user)
    await fill(dlg, user, { lists: ['Sanctions list', 'PEP list'], interval: '86400', grace: '0' })
    await user.click(within(dlg).getByRole('button', { name: 'Author screening policy version' }))
    expect(await screen.findByText(/Screening policy version b1000000… recorded/)).toBeInTheDocument()
    const b = bodies[0] as Record<string, unknown>
    expect(Object.keys(b).sort()).toEqual(['backfill_grace_seconds', 'effective_from', 'jurisdiction_id', 'legal_review_reference', 'lists', 'reason_code', 'rescreen_interval_seconds'])
    expect(b.lists).toEqual(['sanctions', 'pep'])
    expect(b.rescreen_interval_seconds).toBe(86400)
    expect(b.backfill_grace_seconds).toBe(0)
    expect(b.jurisdiction_id).toBe(JUR)
    expect(b.effective_from).toMatch(/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$/)
  })

  it('the interval floor is enforced in the form: below 3600, decimals, signs and leading zeros send nothing', async () => {
    const user = await openTab()
    const bodies = capture(() => HttpResponse.json({ policy_id: POLICY }, { status: 201 }))
    const dlg = await openAuthor(user)
    await fill(dlg, user, { interval: '3599' })
    const submit = within(dlg).getByRole('button', { name: 'Author screening policy version' })
    expect(within(dlg).getByText(/below the engineering safety floor of 3600 seconds/)).toBeInTheDocument()
    expect(submit).toBeDisabled()
    const iv = within(dlg).getByLabelText('Re-screen interval (seconds)')
    for (const bad of ['3600.5', '-3600', '03600', '1e5', '31536001']) {
      await user.clear(iv)
      await user.type(iv, bad)
      expect(submit).toBeDisabled()
    }
    await user.clear(iv)
    await user.type(iv, '3600')
    expect(submit).toBeEnabled()
    const gr = within(dlg).getByLabelText('Backfill grace (seconds)')
    await user.clear(gr)
    expect(submit).toBeDisabled() // grace is mandatory, 0 is a stated value
    expect(bodies).toHaveLength(0)
  })

  it('needs at least one list', async () => {
    const user = await openTab()
    const dlg = await openAuthor(user)
    await fill(dlg, user, { lists: [] })
    expect(within(dlg).getByRole('button', { name: 'Author screening policy version' })).toBeDisabled()
    await user.click(within(dlg).getByLabelText('PEP list'))
    expect(within(dlg).getByRole('button', { name: 'Author screening policy version' })).toBeEnabled()
  })

  it('warns (does not decide) when the interval is below twice the gate alert threshold of the jurisdiction', async () => {
    const user = await openTab(NONE, GATING)
    await screen.findByLabelText('Gate threshold of the jurisdiction')
    const dlg = await openAuthor(user)
    await fill(dlg, user, { interval: '3600' })
    expect(within(dlg).getByText(/will refuse an interval below 7200 seconds/)).toBeInTheDocument()
    expect(within(dlg).getByRole('button', { name: 'Author screening policy version' })).toBeEnabled()
  })

  it('warns when the effective time is outside now .. +30 days (the server decides)', async () => {
    const user = await openTab()
    const dlg = await openAuthor(user)
    await fill(dlg, user, { offset: 40 * 86_400_000 })
    expect(within(dlg).getByText(/outside the window the server accepts/)).toBeInTheDocument()
  })

  it('refuses locally a version that is certainly looser than the one in force (list removed, longer interval or grace) and sends nothing', async () => {
    const user = await openTab({ configured: true, lists: ['sanctions', 'pep'], rescreen_interval_seconds: 7200, backfill_grace_seconds: 100, effective_from: '2026-10-01T10:00:00Z' })
    const bodies = capture(() => HttpResponse.json({ policy_id: POLICY }, { status: 201 }))
    const dlg = await openAuthor(user)
    await fill(dlg, user, { lists: ['Sanctions list'], interval: '86400', grace: '200' })
    const alert = within(dlg).getByRole('alert', { name: 'Loosening refused' })
    expect(alert).toHaveTextContent('a list screened by the policy in force would be removed')
    expect(alert).toHaveTextContent('the re-screen interval would be longer')
    expect(alert).toHaveTextContent('the backfill grace would be longer')
    expect(within(dlg).getByRole('button', { name: 'Author screening policy version' })).toBeDisabled()
    // tightening is allowed
    await user.click(within(dlg).getByLabelText('PEP list'))
    const iv = within(dlg).getByLabelText('Re-screen interval (seconds)')
    await user.clear(iv)
    await user.type(iv, '3600')
    const gr = within(dlg).getByLabelText('Backfill grace (seconds)')
    await user.clear(gr)
    await user.type(gr, '50')
    expect(within(dlg).queryByRole('alert', { name: 'Loosening refused' })).toBeNull()
    expect(within(dlg).getByRole('button', { name: 'Author screening policy version' })).toBeEnabled()
    expect(bodies).toHaveLength(0)
  })

  it('a 201 without a policy_id is NOT confirmed', async () => {
    const user = await openTab()
    capture(() => HttpResponse.json({ ok: true }, { status: 201 }))
    const dlg = await openAuthor(user)
    await fill(dlg, user)
    await user.click(within(dlg).getByRole('button', { name: 'Author screening policy version' }))
    expect(await screen.findByText(/could not be recognised/)).toBeInTheDocument()
    expect(screen.queryByText(/Screening policy version .* recorded/)).toBeNull()
  })

  it.each([
    [409, 'policy_loosening_refused', /less strict than the latest one/],
    [400, 'rescreen_interval_below_floor', /below the engineering safety floor of 3600 seconds/],
    [409, 'rescreen_interval_below_alert_threshold', /below twice the withdrawal gate alert threshold/],
    [400, 'policy_effective_from_out_of_range', /between now \(2 minutes of clock skew are tolerated\) and 30 days ahead/],
    [403, 'platform_officer_required', /platform administrator acting as an AML officer/],
    [409, 'tenant_not_in_jurisdiction', /not configured to operate in this jurisdiction/],
    [409, 'jurisdiction_not_in_scope', /not the one of the tenant/],
  ])('refusal %s %s is shown in exact fixed wording, never the server message', async (status, code, text) => {
    const user = await openTab()
    capture(() => ERR(status as number, 'conflict', [code as string]))
    const dlg = await openAuthor(user)
    await fill(dlg, user)
    await user.click(within(dlg).getByRole('button', { name: 'Author screening policy version' }))
    const alert = await within(dlg).findByRole('alert', { name: '' })
    expect(alert).toHaveTextContent(text as RegExp)
    expect(alert).toHaveTextContent('Nothing was changed.')
    expect(alert).toHaveTextContent('Request ID: req-pol')
    expect(document.body).not.toHaveTextContent('RAW SERVER MESSAGE')
    expect(screen.queryByText(/Screening policy version .* recorded/)).toBeNull()
  })

  it('a 403 the UI allowed is a refusal in fixed words', async () => {
    const user = await openTab()
    capture(() => ERR(403, 'forbidden'))
    const dlg = await openAuthor(user)
    await fill(dlg, user)
    await user.click(within(dlg).getByRole('button', { name: 'Author screening policy version' }))
    expect(await within(dlg).findByRole('alert', { name: '' })).toHaveTextContent('Authoring a screening policy is for a platform administrator acting as an AML officer')
  })

  it('step-up: the critical write is re-sent ONCE with the stepped-up token and the same body', async () => {
    const user = await openTab()
    const STEPPED = makeTestJwt({ ...PA, jti: 'stepped-screening-policy' })
    const sends: { auth: string | null; body: string }[] = []
    server.use(
      http.post(`${BASE}/policy`, async ({ request }) => {
        const rec = { auth: request.headers.get('Authorization'), body: await request.text() }
        sends.push(rec)
        return rec.auth === `Bearer ${STEPPED}` ? HttpResponse.json({ policy_id: POLICY }, { status: 201 }) : stepUpRequiredResponse()
      }),
      http.post('/v1/staff/auth/step-up', () =>
        HttpResponse.json({ access_token: STEPPED, token_type: 'Bearer', expires_in: 900, mfa_at: '2026-10-13T10:00:00Z', step_up_expires_at: { financial: '2026-10-13T10:10:00Z', critical: '2026-10-13T10:05:00Z' } }),
      ),
    )
    const dlg = await openAuthor(user)
    await fill(dlg, user)
    await user.click(within(dlg).getByRole('button', { name: 'Author screening policy version' }))
    const su = await screen.findByRole('dialog', { name: 'Confirm it is you' })
    await user.type(within(su).getByLabelText('6-digit code'), '123456')
    await user.click(within(su).getByRole('button', { name: 'Verify' }))
    await screen.findByText(/Screening policy version .* recorded/)
    expect(sends).toHaveLength(2)
    expect(sends[1].auth).not.toBe(sends[0].auth)
    expect(sends[1].body).toBe(sends[0].body)
  })
})

describe('the existing screening screens show the exact refusal (check_codes) instead of the generic wording', () => {
  it('trigger policy: gate_needs_scheduled_event is exact; a bare guard_refused keeps the existing worker wording', async () => {
    signInAs(PA)
    reads()
    const user = userEvent.setup()
    renderWithProviders(<AppRoutes />, { route: '/aml/screening' })
    const select = await screen.findByLabelText('Tenant')
    await waitFor(() => expect(within(select).getByRole('option', { name: 'Brand Co' })).toBeInTheDocument())
    await user.selectOptions(select, TENANT)
    const jsel = await screen.findByLabelText('Licence jurisdiction')
    await waitFor(() => expect(within(jsel).getByRole('option', { name: /Testland/ })).toBeInTheDocument())
    await user.selectOptions(jsel, JUR)
    await user.click(screen.getByRole('button', { name: 'Show policy in force' }))
    let answer = () => ERR(409, 'conflict', ['gate_needs_scheduled_event'])
    server.use(http.post(`${BASE}/trigger-policy`, () => answer()))
    for (const [mk, text] of [
      [() => ERR(409, 'conflict', ['gate_needs_scheduled_event']), /A withdrawal gate needs the scheduled re-screen event, otherwise a clear goes stale/],
      [() => ERR(409, 'conflict', ['screening_worker_heartbeat_missing']), /No screening worker has reported in/],
      [() => ERR(409, 'conflict', ['screening_policy_missing']), /Set the screening policy .* for this jurisdiction first/],
      [() => ERR(409, 'conflict', ['guard_refused']), /unless a screening worker has reported a recent heartbeat/],
    ] as const) {
      answer = mk
      await user.click(await screen.findByRole('button', { name: 'Author next version' }))
      const dlg = await screen.findByRole('dialog', { name: 'Author the next trigger policy version' })
      await user.selectOptions(within(dlg).getByLabelText('Withdrawal gate'), 'off')
      fireEvent.change(within(dlg).getByLabelText('Effective from'), { target: { value: localInput(3_600_000) } })
      await user.type(within(dlg).getByLabelText('Legal review reference'), 'LEGAL-1')
      await user.type(within(dlg).getByLabelText('Reason code'), 'compliance_decision')
      await user.click(within(dlg).getByRole('button', { name: 'Author version' }))
      expect(await within(dlg).findByRole('alert')).toHaveTextContent(text)
      await user.click(within(dlg).getByRole('button', { name: 'Cancel' }))
    }
    expect(document.body).not.toHaveTextContent('RAW SERVER MESSAGE')
  })
})
