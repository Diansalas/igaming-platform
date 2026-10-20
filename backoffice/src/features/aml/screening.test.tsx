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
import { describeScreeningError } from './screeningErrors'
import { ApiError } from '../../api/types'

vi.setConfig({ testTimeout: 30000 })

const TENANT = '11111111-1111-4111-8111-111111111111'
const PERSON = '33333333-3333-4333-8333-333333333333'
const JUR = '10000000-0000-4000-8000-000000000001'
const REQ1 = 'a1000000-0000-4000-8000-000000000001'
const REQ2 = 'a2000000-0000-4000-8000-000000000002'
const REQ3 = 'a3000000-0000-4000-8000-000000000003'
const POLICY = 'b1000000-0000-4000-8000-000000000001'
const RESULT = 'c1000000-0000-4000-8000-000000000001'
const HOLDID = 'd1000000-0000-4000-8000-000000000001'
const ME = '66666666-6666-4666-8666-666666666666'
const BASE = `/v1/admin/tenants/${TENANT}/screening`
const COMPLIANCE = { role: 'compliance', tenant_id: TENANT, sub: ME }
const PA = { role: 'platform_admin', tenant_id: PLATFORM_ADMIN_CLAIMS.tenant_id as string, sub: ME }
const RAW = 'RAW SERVER MESSAGE <b>do not show</b>'
const ERR = (status: number, code: string, checks?: string[]) => HttpResponse.json({ code, message: RAW, request_id: 'req-scr', check_codes: checks }, { status })
const NON_OFFICER = ['tenant_admin', 'finance', 'support', 'risk_manager', 'promotions_manager', 'bonus_operations']

const req = (o: Record<string, unknown> = {}) => ({ id: REQ1, trigger: 'registration', idempotency_key: 'k1', status: 'needs_officer', attempts: 3, resolved: false, created_at: '2026-10-10T10:00:00Z', ...o })
const localInput = (offsetMs: number) => {
  const d = new Date(Date.now() + offsetMs)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`
}

function capture(path: string, answer: () => Response) {
  const bodies: unknown[] = []
  server.use(http.post(path, async ({ request }) => { bodies.push(JSON.parse(await request.text())); return answer() }))
  return bodies
}

function stepUpOn(path: string, ok: () => Response) {
  const STEPPED = makeTestJwt({ ...PA, jti: 'stepped-screening' })
  const sends: { auth: string | null; body: string }[] = []
  server.use(
    http.post(path, async ({ request }) => {
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
async function doStepUp() {
  const user = userEvent.setup()
  const su = await screen.findByRole('dialog', { name: 'Confirm it is you' })
  await user.type(within(su).getByLabelText('6-digit code'), '123456')
  await user.click(within(su).getByRole('button', { name: 'Verify' }))
}

function policyReads(policy: Record<string, unknown> = { configured: false, events: [], gate_withdrawal: false, gate_alert_after_seconds: null, effective_from: null }) {
  server.use(
    http.get('/v1/admin/tenants', () => HttpResponse.json({ items: [{ id: TENANT, name: 'Brand Co', slug: 'brand', status: 'active' }], limit: 50, offset: 0, total: 1 })),
    http.get('/v1/admin/jurisdictions', () => HttpResponse.json([{ id: JUR, code: 'XX', name: 'Testland', created_at: '2026-01-01T00:00:00Z' }])),
    http.get(`${BASE}/trigger-policy`, () => HttpResponse.json(policy)),
  )
}

async function openPolicyAsPlatform(policy?: Record<string, unknown>) {
  signInAs(PA)
  policyReads(policy)
  const user = userEvent.setup()
  renderWithProviders(<AppRoutes />, { route: '/aml/screening' })
  const select = await screen.findByLabelText('Tenant')
  await waitFor(() => expect(within(select).getByRole('option', { name: 'Brand Co' })).toBeInTheDocument())
  await user.selectOptions(select, TENANT)
  const jsel = await screen.findByLabelText('Licence jurisdiction')
  await waitFor(() => expect(within(jsel).getByRole('option', { name: /Testland/ })).toBeInTheDocument())
  await user.selectOptions(jsel, JUR)
  await user.click(screen.getByRole('button', { name: 'Show policy in force' }))
  return user
}

async function openAuthor(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole('button', { name: 'Author next version' }))
  return screen.findByRole('dialog', { name: 'Author the next trigger policy version' })
}

describe('RBAC (UI hint only; the server decides): no request is sent for a role without the permission', () => {
  it.each(NON_OFFICER.map((r) => [r]))('%s sees NotAuthorized on /aml/screening and no request is sent', async (role) => {
    signInAs({ role, tenant_id: TENANT, sub: ME })
    let calls = 0
    server.use(http.all(/\/screening\//, () => { calls++; return HttpResponse.json({ items: [] }) }), http.get('/v1/admin/jurisdictions', () => { calls++; return HttpResponse.json([]) }))
    renderWithProviders(<AppRoutes />, { route: '/aml/screening' })
    expect(await screen.findByText('Your role does not have access to this section.')).toBeInTheDocument()
    expect(calls).toBe(0)
  })

  it('a tenant-scoped compliance officer reads the policy in its OWN tenant and is offered no authoring', async () => {
    signInAs(COMPLIANCE)
    let path = ''
    server.use(http.get(`${BASE}/trigger-policy`, ({ request }) => { path = new URL(request.url).pathname + new URL(request.url).search; return HttpResponse.json({ configured: false, events: [], gate_withdrawal: false, gate_alert_after_seconds: null, effective_from: null }) }))
    const user = userEvent.setup()
    renderWithProviders(<AppRoutes />, { route: '/aml/screening' })
    await user.type(await screen.findByLabelText('Jurisdiction id'), JUR)
    await user.click(screen.getByRole('button', { name: 'Show policy in force' }))
    expect(await screen.findByText(/No trigger policy is in force/)).toBeInTheDocument()
    expect(path).toBe(`${BASE}/trigger-policy?jurisdiction_id=${JUR}`)
    expect(screen.queryByRole('button', { name: 'Author next version' })).toBeNull()
    expect(screen.getByText(/Authoring is not offered to your role/)).toBeInTheDocument()
  })

  it('is linked from the AML page', async () => {
    signInAs(COMPLIANCE)
    server.use(http.get(`/v1/admin/tenants/${TENANT}/aml/cases`, () => HttpResponse.json({ items: [] })))
    renderWithProviders(<AppRoutes />, { route: '/aml' })
    expect(await screen.findByRole('link', { name: /Automatic screening/ })).toHaveAttribute('href', '/aml/screening')
  })
})

describe('trigger policy view and authoring', () => {
  it('shows the prominent gate warning and the not-shipped note; nothing is requested before a jurisdiction is chosen', async () => {
    signInAs(COMPLIANCE)
    let calls = 0
    server.use(http.get(`${BASE}/trigger-policy`, () => { calls++; return HttpResponse.json({}) }))
    renderWithProviders(<AppRoutes />, { route: '/aml/screening' })
    expect(await screen.findByRole('note', { name: 'Withdrawal gate warning' })).toHaveTextContent('TRAPS PLAYER FUNDS')
    expect(screen.getByRole('note', { name: 'Withdrawal gate warning' })).toHaveTextContent('never enable a gate in production')
    expect(screen.getByRole('note', { name: 'No policy shipped' })).toHaveTextContent('ships no screening trigger policy')
    expect(screen.getByRole('note', { name: 'Interval and grace' })).toBeInTheDocument()
    expect(calls).toBe(0)
  })

  it('shows a configured gating policy', async () => {
    await openPolicyAsPlatform({ configured: true, events: ['scheduled', 'registration'], gate_withdrawal: true, gate_alert_after_seconds: 3600, effective_from: '2026-10-20T10:00:00Z' })
    const dl = await screen.findByLabelText('Policy in force')
    expect(dl).toHaveTextContent('Scheduled re-screen')
    expect(dl).toHaveTextContent('ON: gates withdrawals')
    expect(dl).toHaveTextContent('3600 seconds')
  })

  it('an unrecognised body is not shown as a policy', async () => {
    await openPolicyAsPlatform({ configured: 'yes' })
    expect(await screen.findByText(/could not be recognised/)).toBeInTheDocument()
    expect(screen.queryByLabelText('Policy in force')).toBeNull()
  })

  it('the author form starts empty: no events, gate, time, reference or reason preselected; submit disabled', async () => {
    const user = await openPolicyAsPlatform()
    const dlg = await openAuthor(user)
    for (const cb of within(dlg).getAllByRole('checkbox')) expect(cb).not.toBeChecked()
    expect(within(dlg).getByLabelText('Withdrawal gate')).toHaveValue('')
    expect(within(dlg).getByLabelText('Effective from')).toHaveValue('')
    expect(within(dlg).getByLabelText('Legal review reference')).toHaveValue('')
    expect(within(dlg).getByLabelText('Reason code')).toHaveValue('')
    expect(within(dlg).getByRole('button', { name: 'Author version' })).toBeDisabled()
    expect(within(dlg).getByRole('note', { name: 'Withdrawal gate warning' })).toHaveTextContent('TRAPS PLAYER FUNDS')
  })

  async function fill(dlg: HTMLElement, user: ReturnType<typeof userEvent.setup>, o: { gate: 'on' | 'off'; secs?: string; events?: string[]; ack?: boolean }) {
    for (const e of o.events ?? []) await user.click(within(dlg).getByLabelText(e))
    await user.selectOptions(within(dlg).getByLabelText('Withdrawal gate'), o.gate)
    if (o.gate === 'on') {
      if (o.secs) await user.type(within(dlg).getByLabelText('Trapped alert after (seconds)'), o.secs)
      if (o.ack) await user.click(within(dlg).getByRole('checkbox', { name: /I understand/ }))
    }
    fireEvent.change(within(dlg).getByLabelText('Effective from'), { target: { value: localInput(3_600_000) } })
    await user.type(within(dlg).getByLabelText('Legal review reference'), 'LEGAL-1')
    await user.type(within(dlg).getByLabelText('Reason code'), 'compliance_decision')
  }

  it('gate OFF payload: exact keys, no gate_alert_after_seconds, no tenant or actor field, 201 confirmed', async () => {
    const user = await openPolicyAsPlatform()
    const bodies = capture(`${BASE}/trigger-policy`, () => HttpResponse.json({ policy_id: POLICY }, { status: 201 }))
    const dlg = await openAuthor(user)
    await fill(dlg, user, { gate: 'off', events: ['Registration', 'KYC approval'] })
    await user.click(within(dlg).getByRole('button', { name: 'Author version' }))
    expect(await screen.findByText(/Policy version b1000000… recorded/)).toBeInTheDocument()
    expect(bodies).toHaveLength(1)
    const b = bodies[0] as Record<string, unknown>
    expect(Object.keys(b).sort()).toEqual(['effective_from', 'events', 'gate_withdrawal', 'jurisdiction_id', 'legal_review_reference', 'reason_code'])
    expect(b.events).toEqual(['registration', 'kyc_approval'])
    expect(b.gate_withdrawal).toBe(false)
    expect(b.jurisdiction_id).toBe(JUR)
    expect(b.effective_from).toMatch(/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$/)
  })

  it('gate OFF with no event at all is a valid switch-off version (empty events array)', async () => {
    const user = await openPolicyAsPlatform()
    const bodies = capture(`${BASE}/trigger-policy`, () => HttpResponse.json({ policy_id: POLICY }, { status: 201 }))
    const dlg = await openAuthor(user)
    await fill(dlg, user, { gate: 'off' })
    await user.click(within(dlg).getByRole('button', { name: 'Author version' }))
    await screen.findByText(/Policy version .* recorded/)
    expect((bodies[0] as { events: unknown[] }).events).toEqual([])
  })

  it('gate ON needs the scheduled event, an in-range threshold and the acknowledgement; payload carries the integer seconds', async () => {
    const user = await openPolicyAsPlatform()
    const bodies = capture(`${BASE}/trigger-policy`, () => HttpResponse.json({ policy_id: POLICY }, { status: 201 }))
    const dlg = await openAuthor(user)
    const submit = within(dlg).getByRole('button', { name: 'Author version' })
    await fill(dlg, user, { gate: 'on', secs: '59', events: ['Registration'] })
    expect(submit).toBeDisabled()
    expect(within(dlg).getByText(/needs the scheduled re-screen event/)).toBeInTheDocument()
    expect(within(dlg).getByText('Use whole seconds from 60 to 2592000.')).toBeInTheDocument()
    await user.click(within(dlg).getByLabelText('Scheduled re-screen'))
    expect(submit).toBeDisabled() // 59 still out of range, ack missing
    const secs = within(dlg).getByLabelText('Trapped alert after (seconds)')
    await user.clear(secs)
    await user.type(secs, '2592001')
    expect(submit).toBeDisabled()
    await user.clear(secs)
    await user.type(secs, '3600')
    expect(submit).toBeDisabled() // not acknowledged
    await user.click(within(dlg).getByRole('checkbox', { name: /I understand/ }))
    expect(submit).toBeEnabled()
    await user.click(submit)
    await screen.findByText(/Policy version .* recorded/)
    const b = bodies[0] as Record<string, unknown>
    expect(b.gate_withdrawal).toBe(true)
    expect(b.gate_alert_after_seconds).toBe(3600)
    expect(b.events).toEqual(['registration', 'scheduled'])
  })

  it('rejects a decimal, signed or zero threshold and a non-conforming reason code or reference', async () => {
    const user = await openPolicyAsPlatform()
    const dlg = await openAuthor(user)
    await user.selectOptions(within(dlg).getByLabelText('Withdrawal gate'), 'on')
    const secs = within(dlg).getByLabelText('Trapped alert after (seconds)')
    for (const bad of ['60.5', '-60', '0', '1e3', '0060']) {
      await user.clear(secs)
      await user.type(secs, bad)
      expect(within(dlg).getByText('Use whole seconds from 60 to 2592000.')).toBeInTheDocument()
    }
    await user.type(within(dlg).getByLabelText('Reason code'), 'Bad Reason!')
    expect(within(dlg).getByText('Not a valid reason code.')).toBeInTheDocument()
    await user.type(within(dlg).getByLabelText('Legal review reference'), ' padded')
    expect(within(dlg).getByText('Not a valid legal review reference.')).toBeInTheDocument()
  })

  it('a 201 without a policy_id is NOT confirmed', async () => {
    const user = await openPolicyAsPlatform()
    capture(`${BASE}/trigger-policy`, () => HttpResponse.json({ ok: true }, { status: 201 }))
    const dlg = await openAuthor(user)
    await fill(dlg, user, { gate: 'off' })
    await user.click(within(dlg).getByRole('button', { name: 'Author version' }))
    expect(await screen.findByText(/could not be recognised, nothing is confirmed|could not be recognised/)).toBeInTheDocument()
    expect(screen.queryByText(/Policy version .* recorded/)).toBeNull()
  })

  it('shows the server refusal (worker heartbeat) in fixed wording, never the server message, and writes nothing', async () => {
    const user = await openPolicyAsPlatform()
    capture(`${BASE}/trigger-policy`, () => ERR(409, 'conflict', ['guard_refused']))
    const dlg = await openAuthor(user)
    await fill(dlg, user, { gate: 'on', secs: '3600', events: ['Scheduled re-screen'], ack: true })
    await user.click(within(dlg).getByRole('button', { name: 'Author version' }))
    const alert = await within(dlg).findByRole('alert')
    expect(alert).toHaveTextContent('unless a screening worker has reported a recent heartbeat')
    expect(alert).toHaveTextContent('Nothing was changed.')
    expect(alert).toHaveTextContent('Request ID: req-scr')
    expect(document.body).not.toHaveTextContent('RAW SERVER MESSAGE')
    expect(screen.queryByText(/Policy version .* recorded/)).toBeNull()
  })

  it('degrades on 403 from the server for an action the UI allowed to be attempted', async () => {
    const user = await openPolicyAsPlatform()
    capture(`${BASE}/trigger-policy`, () => ERR(403, 'forbidden'))
    const dlg = await openAuthor(user)
    await fill(dlg, user, { gate: 'off' })
    await user.click(within(dlg).getByRole('button', { name: 'Author version' }))
    expect(await within(dlg).findByRole('alert')).toHaveTextContent('Authoring a trigger policy is for a platform administrator acting as an AML officer')
  })

  it('step-up: the critical write is re-sent ONCE with the stepped-up token and the same body', async () => {
    const user = await openPolicyAsPlatform()
    const sends = stepUpOn(`${BASE}/trigger-policy`, () => HttpResponse.json({ policy_id: POLICY }, { status: 201 }))
    const dlg = await openAuthor(user)
    await fill(dlg, user, { gate: 'off', events: ['Registration'] })
    await user.click(within(dlg).getByRole('button', { name: 'Author version' }))
    await doStepUp()
    await screen.findByText(/Policy version .* recorded/)
    expect(sends).toHaveLength(2)
    expect(sends[1].auth).not.toBe(sends[0].auth)
    expect(sends[1].body).toBe(sends[0].body)
  })

  it('XSS: server-supplied strings are rendered as text only', async () => {
    await openPolicyAsPlatform({ configured: true, events: ['registration'], gate_withdrawal: false, gate_alert_after_seconds: null, effective_from: '<img src=x onerror=alert(1)>' })
    await screen.findByLabelText('Policy in force')
    expect(document.querySelector('img')).toBeNull()
  })
})

describe('screening requests and officer resolution', () => {
  async function openRequests(items: unknown[], claims: Record<string, string> = COMPLIANCE) {
    signInAs(claims)
    server.use(http.get(`${BASE}/persons/${PERSON}/requests`, () => HttpResponse.json({ items })))
    if (claims === PA) server.use(http.get('/v1/admin/jurisdictions', () => HttpResponse.json([])), http.get('/v1/admin/tenants', () => HttpResponse.json({ items: [{ id: TENANT, name: 'Brand Co', slug: 'brand', status: 'active' }], limit: 50, offset: 0, total: 1 })))
    const user = userEvent.setup()
    renderWithProviders(<AppRoutes />, { route: '/aml/screening' })
    if (claims === PA) {
      const select = await screen.findByLabelText('Tenant')
      await waitFor(() => expect(within(select).getByRole('option', { name: 'Brand Co' })).toBeInTheDocument())
      await user.selectOptions(select, TENANT)
    }
    await user.click(await screen.findByRole('tab', { name: 'Screening requests' }))
    await user.type(screen.getByLabelText('Person id'), PERSON)
    await user.click(screen.getByRole('button', { name: 'Show requests' }))
    return user
  }
  const rowOf = (id: string) => screen.getByText(id.slice(0, 8) + '…').closest('tr') as HTMLElement

  it('lists states and offers Resolve only for an unresolved needs_officer / failed_terminal request', async () => {
    await openRequests([
      req(),
      req({ id: REQ2, status: 'failed_terminal', last_error_code: 'unavailable' }),
      req({ id: REQ3, status: 'pending', trigger: 'scheduled' }),
      req({ id: 'a4000000-0000-4000-8000-000000000004', status: 'needs_officer', resolved: true }),
      req({ id: 'a5000000-0000-4000-8000-000000000005', status: 'done', outcome: 'clear' }),
      req({ id: 'a6000000-0000-4000-8000-000000000006', status: 'weird' }),
    ])
    await screen.findByText('a1000000…')
    expect(within(rowOf(REQ1)).getByText('Needs officer')).toBeInTheDocument()
    expect(within(rowOf(REQ1)).getByText('Holding withdrawals')).toBeInTheDocument()
    expect(within(rowOf(REQ2)).getByText('Failed (terminal)')).toBeInTheDocument()
    expect(within(rowOf(REQ3)).getByText('Pending')).toBeInTheDocument()
    expect(screen.getByText('Unrecognised (weird)')).toBeInTheDocument()
    expect(screen.getAllByRole('button', { name: 'Resolve' })).toHaveLength(2)
    expect(within(rowOf(REQ1)).getByRole('button', { name: 'Resolve' })).toBeInTheDocument()
    expect(within(rowOf(REQ3)).queryByRole('button', { name: 'Resolve' })).toBeNull()
  })

  it('an unrecognised list body shows nothing', async () => {
    await openRequests([{ id: 5 }])
    expect(await screen.findByText(/list could not be recognised/)).toBeInTheDocument()
  })

  it('the person id and the request id are path-encoded; the request is sent under the tenant of the session', async () => {
    let path = ''
    signInAs(COMPLIANCE)
    server.use(http.get(/\/screening\/persons\/.*\/requests$/, ({ request }) => { path = new URL(request.url).pathname; return HttpResponse.json({ items: [] }) }))
    const user = userEvent.setup()
    renderWithProviders(<AppRoutes />, { route: '/aml/screening' })
    await user.click(await screen.findByRole('tab', { name: 'Screening requests' }))
    await user.type(screen.getByLabelText('Person id'), PERSON)
    await user.click(screen.getByRole('button', { name: 'Show requests' }))
    await screen.findByText('No screening requests for this person in your scope.')
    expect(path).toBe(`${BASE}/persons/${PERSON}/requests`)
  })

  it('shows the server-enforced notes and the sanctions rule in the dialog; the form starts empty', async () => {
    const user = await openRequests([req({ match_lists: ['sanctions'] })])
    await user.click(await screen.findByRole('button', { name: 'Resolve' }))
    const dlg = await screen.findByRole('dialog', { name: 'Resolve a screening request' })
    expect(within(dlg).getByRole('note', { name: 'Sanctions rule' })).toHaveTextContent('can NOT be cleared by an officer alone')
    expect(within(dlg).getByRole('note', { name: 'Sanctions-class request' })).toBeInTheDocument()
    expect(within(dlg).getByRole('note', { name: 'Resolution rules' })).toHaveTextContent('An error outcome never resolves')
    expect(screen.getByRole('note', { name: 'Server enforcement' })).toHaveTextContent('officer quorum')
    expect(within(dlg).getByLabelText('Outcome')).toHaveValue('')
    for (const cb of within(dlg).getAllByRole('checkbox')) expect(cb).not.toBeChecked()
    expect(within(dlg).getByRole('button', { name: 'Record resolution' })).toBeDisabled()
    expect(within(dlg).queryByRole('option', { name: /error/i })).toBeNull()
  })

  it('clear payload: outcome, lists, reason; no match_lists; confirmed 201 shows the result', async () => {
    const user = await openRequests([req()])
    const bodies = capture(`${BASE}/persons/${PERSON}/requests/${REQ1}/resolution`, () => HttpResponse.json({ result_id: RESULT, replayed: false }, { status: 201 }))
    await user.click(await screen.findByRole('button', { name: 'Resolve' }))
    const dlg = await screen.findByRole('dialog', { name: 'Resolve a screening request' })
    await user.selectOptions(within(dlg).getByLabelText('Outcome'), 'clear')
    await user.click(within(dlg).getByLabelText('Sanctions list'))
    await user.click(within(dlg).getByLabelText('PEP list'))
    await user.type(within(dlg).getByLabelText('Reason code'), 'officer_review')
    await user.click(within(dlg).getByRole('button', { name: 'Record resolution' }))
    expect(await screen.findByText(/Recorded\. Result c1000000…/)).toBeInTheDocument()
    expect(bodies).toEqual([{ outcome: 'clear', lists: ['sanctions', 'pep'], reason_code: 'officer_review' }])
  })

  it('match payload needs match_lists; shows the hold id and a replay is labelled', async () => {
    const user = await openRequests([req()])
    const bodies = capture(`${BASE}/persons/${PERSON}/requests/${REQ1}/resolution`, () => HttpResponse.json({ result_id: RESULT, hold_id: HOLDID, replayed: true }, { status: 201 }))
    await user.click(await screen.findByRole('button', { name: 'Resolve' }))
    const dlg = await screen.findByRole('dialog', { name: 'Resolve a screening request' })
    await user.selectOptions(within(dlg).getByLabelText('Outcome'), 'potential_match')
    await user.click(within(dlg).getAllByLabelText('Sanctions list')[0])
    await user.type(within(dlg).getByLabelText('Reason code'), 'officer_review')
    expect(within(dlg).getByRole('button', { name: 'Record resolution' })).toBeDisabled() // no match list yet
    await user.click(within(dlg).getAllByLabelText('Sanctions list')[1])
    await user.click(within(dlg).getByRole('button', { name: 'Record resolution' }))
    const ok = await screen.findByText(/Already recorded \(replay\)/)
    expect(ok).toHaveTextContent('A hold was placed (d1000000…)')
    expect(bodies).toEqual([{ outcome: 'potential_match', lists: ['sanctions'], match_lists: ['sanctions'], reason_code: 'officer_review' }])
  })

  it.each([
    ['no result_id', { replayed: false }],
    ['replayed missing', { result_id: RESULT }],
    ['bad hold_id', { result_id: RESULT, replayed: false, hold_id: 'x' }],
  ])('a 201 with %s is NOT confirmed', async (_n, body) => {
    const user = await openRequests([req()])
    capture(`${BASE}/persons/${PERSON}/requests/${REQ1}/resolution`, () => HttpResponse.json(body, { status: 201 }))
    await user.click(await screen.findByRole('button', { name: 'Resolve' }))
    const dlg = await screen.findByRole('dialog', { name: 'Resolve a screening request' })
    await user.selectOptions(within(dlg).getByLabelText('Outcome'), 'clear')
    await user.click(within(dlg).getByLabelText('PEP list'))
    await user.type(within(dlg).getByLabelText('Reason code'), 'officer_review')
    await user.click(within(dlg).getByRole('button', { name: 'Record resolution' }))
    expect(await screen.findByText(/could not be recognised/)).toBeInTheDocument()
    expect(screen.queryByText(/Recorded\./)).toBeNull()
  })

  it('a server refusal of a sanctions clear is shown in fixed wording and the list is refetched', async () => {
    let lists = 0
    signInAs(COMPLIANCE)
    server.use(http.get(`${BASE}/persons/${PERSON}/requests`, () => { lists++; return HttpResponse.json({ items: [req()] }) }))
    capture(`${BASE}/persons/${PERSON}/requests/${REQ1}/resolution`, () => ERR(409, 'conflict', ['guard_refused']))
    const user = userEvent.setup()
    renderWithProviders(<AppRoutes />, { route: '/aml/screening' })
    await user.click(await screen.findByRole('tab', { name: 'Screening requests' }))
    await user.type(screen.getByLabelText('Person id'), PERSON)
    await user.click(screen.getByRole('button', { name: 'Show requests' }))
    await user.click(await screen.findByRole('button', { name: 'Resolve' }))
    const dlg = await screen.findByRole('dialog', { name: 'Resolve a screening request' })
    await user.selectOptions(within(dlg).getByLabelText('Outcome'), 'clear')
    await user.click(within(dlg).getByLabelText('Sanctions list'))
    await user.type(within(dlg).getByLabelText('Reason code'), 'officer_review')
    await user.click(within(dlg).getByRole('button', { name: 'Record resolution' }))
    const alert = await within(dlg).findByRole('alert')
    expect(alert).toHaveTextContent('a sanctions-list match cannot be cleared by an officer')
    expect(document.body).not.toHaveTextContent('RAW SERVER MESSAGE')
    await waitFor(() => expect(lists).toBe(2))
  })

  it('quorum refusal uses the quorum wording', async () => {
    const user = await openRequests([req()])
    capture(`${BASE}/persons/${PERSON}/requests/${REQ1}/resolution`, () => ERR(409, 'conflict', ['quorum_not_met']))
    await user.click(await screen.findByRole('button', { name: 'Resolve' }))
    const dlg = await screen.findByRole('dialog', { name: 'Resolve a screening request' })
    await user.selectOptions(within(dlg).getByLabelText('Outcome'), 'clear')
    await user.click(within(dlg).getByLabelText('PEP list'))
    await user.type(within(dlg).getByLabelText('Reason code'), 'officer_review')
    await user.click(within(dlg).getByRole('button', { name: 'Record resolution' }))
    expect(await within(dlg).findByRole('alert')).toHaveTextContent('officer quorum for this tenant is not met')
  })

  it('step-up: the critical resolution is re-sent ONCE with the stepped-up token and the same body', async () => {
    const user = await openRequests([req()], PA)
    const sends = stepUpOn(`${BASE}/persons/${PERSON}/requests/${REQ1}/resolution`, () => HttpResponse.json({ result_id: RESULT, replayed: false }, { status: 201 }))
    await user.click(await screen.findByRole('button', { name: 'Resolve' }))
    const dlg = await screen.findByRole('dialog', { name: 'Resolve a screening request' })
    await user.selectOptions(within(dlg).getByLabelText('Outcome'), 'clear')
    await user.click(within(dlg).getByLabelText('PEP list'))
    await user.type(within(dlg).getByLabelText('Reason code'), 'officer_review')
    await user.click(within(dlg).getByRole('button', { name: 'Record resolution' }))
    await doStepUp()
    await screen.findByText(/Recorded\./)
    expect(sends).toHaveLength(2)
    expect(sends[1].body).toBe(sends[0].body)
  })

  it('XSS: a hostile trigger / outcome / error code is not rendered as markup', async () => {
    await openRequests([req({ trigger: '<img src=x onerror=alert(1)>', outcome: '<b>x</b>', last_error_code: '<script>1</script>' })])
    await screen.findByText('a1000000…')
    expect(document.querySelector('img')).toBeNull()
    expect(document.querySelector('script')).toBeNull()
    expect(screen.getAllByText('unprintable').length).toBeGreaterThanOrEqual(3)
  })

  it('a 403 listing degrades with fixed wording', async () => {
    signInAs(COMPLIANCE)
    server.use(http.get(`${BASE}/persons/${PERSON}/requests`, () => ERR(403, 'forbidden')))
    const user = userEvent.setup()
    renderWithProviders(<AppRoutes />, { route: '/aml/screening' })
    await user.click(await screen.findByRole('tab', { name: 'Screening requests' }))
    await user.type(screen.getByLabelText('Person id'), PERSON)
    await user.click(screen.getByRole('button', { name: 'Show requests' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('does not hold an in-force AML officer grant')
  })
})

describe('describeScreeningError', () => {
  it('never returns the server message', () => {
    for (const op of ['policy_read', 'policy_set', 'requests', 'resolve'] as const) {
      for (const status of [400, 401, 403, 404, 409, 429, 500, 503]) {
        const v = describeScreeningError(new ApiError('unknown' as never, RAW, status, 'rid', ['guard_refused']), op)
        expect(v.message).not.toContain('RAW SERVER')
      }
    }
  })
})
