import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import type { AmlCaseDetail, AmlCaseEvent, AmlHold, CaseStatus } from '../../api/aml'
import { ApiError } from '../../api/types'
import { AppRoutes } from '../../app/AppRoutes'
import { getNavPermissions } from '../../auth/permissions'
import { buildNavItems } from '../../layout/nav'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { PLATFORM_ADMIN_CLAIMS, signInAs } from '../../test/session'
import { makeTestJwt } from '../../test/jwt'
import { GOOD_CODE, stepUpRequiredResponse } from '../../test/stepUp'
import { describeAmlError } from './errors'
import { allowedTransitions, canFlagSar, pendingProposal, proposalKinds } from './rules'

const TENANT = '11111111-1111-4111-8111-111111111111'
const CASE = '22222222-2222-4222-8222-222222222222'
const PERSON = '33333333-3333-4333-8333-333333333333'
const HOLD = '44444444-4444-4444-8444-444444444444'
const HOLD2 = '55555555-5555-4555-8555-555555555555'
const ME = '66666666-6666-4666-8666-666666666666'
const OTHER = '77777777-7777-4777-8777-777777777777'
const HASH = 'ab'.repeat(32)
const BASE = `/v1/admin/tenants/${TENANT}/aml`
const COMPLIANCE = { role: 'compliance', tenant_id: TENANT, sub: ME }
const RAW = 'RAW SERVER MESSAGE <b>do not show</b>'
const ERR = (status: number, code: string, checks?: string[]) => HttpResponse.json({ code, message: RAW, request_id: 'req-aml', check_codes: checks }, { status })

function hold(over: Partial<AmlHold> = {}): AmlHold {
  return { id: HOLD, owner_scope: 'tenant', scope: 'withdrawal_and_payout', reason_class: 'manual', reason_code: 'fixture', placed_at: '2026-10-10T10:00:00Z', lifted: false, ...over }
}

function detail(status: CaseStatus, over: { sar?: boolean; lastSeq?: number; events?: AmlCaseEvent[] } = {}): AmlCaseDetail {
  const events: AmlCaseEvent[] = over.events ?? [{ seq: 1, type: 'opened', reason_code: 'r', actor_id: OTHER, actor_scope: 'tenant', created_at: '2026-10-10T10:00:00Z', to_status: 'open' }]
  return {
    case: { id: CASE, owner_scope: 'tenant', person_id: PERSON, kind: 'manual', status, sar_flagged: over.sar ?? false, last_seq: over.lastSeq ?? events[events.length - 1].seq, opened_at: '2026-10-10T10:00:00Z' },
    events,
    notes: [{ id: HOLD2, body: 'reviewed the pattern', lint_version: 'aml-note-lint-v1', created_by: OTHER, created_at: '2026-10-10T11:00:00Z' }],
  }
}

function proposalEvent(seq: number, actor: string, over: Partial<AmlCaseEvent> = {}): AmlCaseEvent {
  return { seq, type: 'proposal', proposal_kind: 'close_sar_not_filed', content_hash: HASH, reason_code: 'prop', actor_id: actor, actor_scope: 'tenant', created_at: '2026-10-10T12:00:00Z', ...over }
}

function useCase(d: AmlCaseDetail) {
  server.use(http.get(`${BASE}/cases/${CASE}`, () => HttpResponse.json(d)))
}

describe('role gating (UI hint only; the server decides)', () => {
  it('mirrors aml_case:* and aml_hold:manage: compliance and platform_admin only', () => {
    for (const r of ['compliance', 'platform_admin']) {
      const p = getNavPermissions(r)
      expect([p.amlRead, p.amlWork, p.amlApprove, p.amlHolds]).toEqual([true, true, true, true])
    }
    for (const r of ['tenant_admin', 'support', 'finance', 'risk_manager', 'promotions_manager', 'bonus_operations', 'nobody', undefined]) {
      const p = getNavPermissions(r as string | undefined)
      expect([p.amlRead, p.amlWork, p.amlApprove, p.amlHolds]).toEqual([false, false, false, false])
    }
  })

  it('shows the AML nav item to the two roles and nobody else', () => {
    const base = { sub: 's', tenant_id: TENANT, principal_type: 'staff', exp: 0, iat: 0 }
    expect(buildNavItems({ ...base, role: 'compliance' }).map((i) => i.label)).toContain('AML')
    expect(buildNavItems({ ...base, role: 'platform_admin', tenant_id: '00000000-0000-0000-0000-000000000000' }).map((i) => i.label)).toContain('AML')
    for (const role of ['tenant_admin', 'support', 'finance']) expect(buildNavItems({ ...base, role }).map((i) => i.label)).not.toContain('AML')
  })

  // Rendered through the real AppRoutes, so removing the RequireNavPermission gate from AppRoutes.tsx fails these.
  it('shows NotAuthorized, and makes no AML request, to a tenant_admin or support user who typed the URL', async () => {
    for (const role of ['tenant_admin', 'support']) {
      signInAs({ role, tenant_id: TENANT, sub: 'x-1' })
      let calls = 0
      server.use(http.all(/\/aml\//, () => { calls++; return HttpResponse.json({ items: [] }) }))
      const { unmount } = renderWithProviders(<AppRoutes />, { route: '/aml' })
      expect(await screen.findByText('Your role does not have access to this section.')).toBeInTheDocument()
      expect(screen.queryByRole('heading', { name: 'AML' })).toBeNull()
      expect(calls).toBe(0)
      unmount()
    }
  })

  it('does not reach the case detail route either', async () => {
    signInAs({ role: 'tenant_admin', tenant_id: TENANT, sub: 'x-1' })
    renderWithProviders(<AppRoutes />, { route: `/aml/cases/${CASE}` })
    expect(await screen.findByText('Your role does not have access to this section.')).toBeInTheDocument()
  })
})

describe('queue and tenant scoping', () => {
  it('a tenant-scoped officer acts in its OWN tenant: the queue is requested under the claims tenant, with the confidentiality banner', async () => {
    signInAs(COMPLIANCE)
    let path = ''
    server.use(
      http.get(`${BASE}/cases`, ({ request }) => {
        path = new URL(request.url).pathname
        return HttpResponse.json({ items: [{ case_id: CASE, owner_scope: 'tenant', person_id: PERSON, status: 'in_review', sar_flagged: true, last_seq: 4, opened_at: '2026-10-10T10:00:00Z' }] })
      }),
    )
    renderWithProviders(<AppRoutes />, { route: '/aml' })
    expect(await screen.findByRole('heading', { name: 'AML' })).toBeInTheDocument()
    expect(screen.getByRole('note')).toHaveTextContent('tipping-off')
    const row = (await screen.findByRole('link', { name: '22222222…' })).closest('tr') as HTMLElement
    expect(within(row).getByText('In review')).toBeInTheDocument()
    expect(within(row).getByText('Flagged')).toBeInTheDocument()
    expect(path).toBe(`${BASE}/cases`)
    expect(screen.queryByLabelText('Tenant')).toBeNull() // no tenant picker for a tenant-scoped officer
  })

  it('a platform officer must choose a tenant first, and nothing is requested before it', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    let calls = 0
    server.use(
      http.get('/v1/admin/tenants', () => HttpResponse.json({ items: [{ id: TENANT, name: 'Brand Co', slug: 'brand', status: 'active' }], limit: 50, offset: 0, total: 1 })),
      http.get(`${BASE}/cases`, () => { calls++; return HttpResponse.json({ items: [] }) }),
    )
    const user = userEvent.setup()
    renderWithProviders(<AppRoutes />, { route: '/aml' })
    expect(await screen.findByText('Choose a tenant to work its AML queue.')).toBeInTheDocument()
    expect(calls).toBe(0)
    const select = await screen.findByLabelText('Tenant')
    await waitFor(() => expect(within(select).getByRole('option', { name: 'Brand Co' })).toBeInTheDocument())
    await user.selectOptions(select, TENANT)
    expect(await screen.findByText('No cases in your scope.')).toBeInTheDocument()
    expect(calls).toBe(1)
  })

  it('renders a refusal from the closed map and never the server message; the request id is shown', async () => {
    signInAs(COMPLIANCE)
    server.use(http.get(`${BASE}/cases`, () => ERR(403, 'forbidden')))
    renderWithProviders(<AppRoutes />, { route: '/aml' })
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('does not hold an in-force AML officer grant')
    expect(alert).toHaveTextContent('Request ID: req-aml')
    expect(document.body).not.toHaveTextContent('RAW SERVER MESSAGE')
  })
})

describe('holds', () => {
  async function openHolds(rows: AmlHold[]) {
    signInAs(COMPLIANCE)
    server.use(
      http.get(`${BASE}/cases`, () => HttpResponse.json({ items: [] })),
      http.get(`${BASE}/persons/${PERSON}/holds`, () => HttpResponse.json({ items: rows })),
    )
    const user = userEvent.setup()
    renderWithProviders(<AppRoutes />, { route: '/aml' })
    await user.click(await screen.findByRole('tab', { name: 'Holds' }))
    await user.type(screen.getByLabelText('Person id'), PERSON)
    await user.click(screen.getByRole('button', { name: 'Show holds' }))
    await screen.findByRole('region', { name: 'Holds' })
    return user
  }

  it('only a syntactically valid person id can be looked up; the list shows scope, class and state', async () => {
    signInAs(COMPLIANCE)
    server.use(http.get(`${BASE}/cases`, () => HttpResponse.json({ items: [] })))
    const user = userEvent.setup()
    renderWithProviders(<AppRoutes />, { route: '/aml' })
    await user.click(await screen.findByRole('tab', { name: 'Holds' }))
    await user.type(screen.getByLabelText('Person id'), 'not-a-uuid')
    expect(screen.getByRole('button', { name: 'Show holds' })).toBeDisabled()
  })

  it('lists holds; a manual hold can be lifted, a sanctions-list hold points to the four-eyes route and has no Lift button', async () => {
    await openHolds([hold(), hold({ id: HOLD2, scope: 'all_value_movement', reason_class: 'confirmed_sanctions' }), hold({ id: '88888888-8888-4888-8888-888888888888', lifted: true, lifted_at: '2026-10-11T10:00:00Z' })])
    expect((await screen.findAllByText('Withdrawal and payout')).length).toBeGreaterThan(0)
    expect(screen.getByText('Lift via a case proposal and approval')).toBeInTheDocument()
    expect(screen.getAllByRole('button', { name: 'Lift' })).toHaveLength(1)
    expect(screen.getByText('Lifted')).toBeInTheDocument()
  })

  it('placing a hold posts exactly the documented body (no tenant, no actor) and refreshes', async () => {
    const user = await openHolds([])
    let body: Record<string, unknown> | null = null
    server.use(http.post(`${BASE}/holds`, async ({ request }) => { body = (await request.json()) as Record<string, unknown>; return HttpResponse.json({ hold_id: HOLD }, { status: 201 }) }))
    await user.click(screen.getByRole('button', { name: 'Place a hold' }))
    const dlg = await screen.findByRole('dialog', { name: 'Place an AML hold' })
    const confirm = within(dlg).getByRole('button', { name: 'Place hold' })
    expect(confirm).toBeDisabled()
    await user.type(within(dlg).getByLabelText('Reason code'), 'Bad Code!')
    expect(confirm).toBeDisabled()
    await user.clear(within(dlg).getByLabelText('Reason code'))
    await user.type(within(dlg).getByLabelText('Reason code'), 'manual_review')
    await user.click(confirm)
    expect(await screen.findByText('Hold placed.')).toBeInTheDocument()
    expect(body).toEqual({ person_id: PERSON, scope: 'withdrawal_and_payout', reason_class: 'manual', reason_code: 'manual_review' })
  })

  it('a sanctions-class hold can only be placed with the all-value scope', async () => {
    const user = await openHolds([])
    await user.click(screen.getByRole('button', { name: 'Place a hold' }))
    const dlg = await screen.findByRole('dialog', { name: 'Place an AML hold' })
    expect(within(dlg).getByLabelText('Class')).toBeDisabled()
    await user.selectOptions(within(dlg).getByLabelText('What it gates'), 'all_value_movement')
    expect(within(dlg).getByLabelText('Class')).toBeEnabled()
  })

  it('shows the quorum refusal in the dialog from the closed map', async () => {
    const user = await openHolds([])
    server.use(http.post(`${BASE}/holds`, () => ERR(409, 'conflict', ['quorum_not_met'])))
    await user.click(screen.getByRole('button', { name: 'Place a hold' }))
    const dlg = await screen.findByRole('dialog', { name: 'Place an AML hold' })
    await user.type(within(dlg).getByLabelText('Reason code'), 'manual_review')
    await user.click(within(dlg).getByRole('button', { name: 'Place hold' }))
    expect(await within(dlg).findByRole('alert')).toHaveTextContent('officer quorum')
    expect(document.body).not.toHaveTextContent('RAW SERVER MESSAGE')
  })

  it('lifting needs a step-up: the 403 opens the modal, the code is verified and the lift is re-sent ONCE with the stepped-up token', async () => {
    const user = await openHolds([hold()])
    // The stepped-up token carries the SAME claims as the session (a real step-up re-mints the same principal).
    const STEPPED = makeTestJwt({ ...COMPLIANCE, jti: 'aml-stepped-up' })
    const stepUps: string[] = []
    const sent: Array<{ auth: string | null; body: unknown }> = []
    server.use(
      http.post('/v1/staff/auth/step-up', async ({ request }) => {
        const { code } = (await request.json()) as { code: string }
        stepUps.push(code)
        return HttpResponse.json({
          access_token: STEPPED, token_type: 'Bearer', expires_in: 900, mfa_at: '2026-10-13T10:00:00Z',
          step_up_expires_at: { financial: '2026-10-13T10:10:00Z', critical: '2026-10-13T10:05:00Z' },
        })
      }),
      http.post(`${BASE}/holds/${HOLD}/lift`, async ({ request }) => {
        const auth = request.headers.get('Authorization')
        sent.push({ auth, body: await request.json() })
        if (auth === `Bearer ${STEPPED}`) return HttpResponse.json({ lift_id: HOLD2 })
        return stepUpRequiredResponse(300)
      }),
    )
    await user.click(screen.getByRole('button', { name: 'Lift' }))
    const dlg = await screen.findByRole('dialog', { name: 'Lift an AML hold' })
    await user.type(within(dlg).getByLabelText('Reason code'), 'false_positive')
    await user.click(within(dlg).getByRole('button', { name: 'Lift hold' }))
    const su = await screen.findByRole('dialog', { name: 'Confirm it is you' })
    await user.type(within(su).getByLabelText('6-digit code'), GOOD_CODE)
    await user.click(within(su).getByRole('button', { name: 'Verify' }))
    expect(await screen.findByText('Hold lifted.')).toBeInTheDocument()
    expect(stepUps).toEqual([GOOD_CODE])
    expect(sent).toHaveLength(2)
    expect(sent[1].auth).toBe(`Bearer ${STEPPED}`)
    expect(sent[1].body).toEqual(sent[0].body)
    expect(sent[0].body).toEqual({ reason_code: 'false_positive' })
  })
})

describe('case detail actions follow the case state (the server still decides)', () => {
  async function openCase(d: AmlCaseDetail, claims = COMPLIANCE) {
    signInAs(claims)
    useCase(d)
    renderWithProviders(<AppRoutes />, { route: `/aml/cases/${CASE}` })
    await screen.findByRole('region', { name: 'Case summary' })
    return userEvent.setup()
  }
  const buttons = () => screen.queryAllByRole('button').map((b) => b.textContent)

  it('open: only the move to in review; no SAR flag, no proposal, no approval', async () => {
    await openCase(detail('open'))
    expect(buttons()).toContain('Move to: In review')
    expect(buttons()).not.toContain('Flag as SAR-related')
    expect(buttons()).not.toContain('Propose a four-eyes action')
    expect(screen.queryByRole('region', { name: 'Pending proposal' })).toBeNull()
  })

  it('in review: escalate or close as no action; a SAR-flagged case cannot close as no action', async () => {
    await openCase(detail('in_review'))
    expect(buttons()).toEqual(expect.arrayContaining(['Move to: Escalated', 'Move to: Closed, no action', 'Flag as SAR-related']))
  })

  it('a SAR-flagged in-review case offers no no-action closure and no second flag', async () => {
    await openCase(detail('in_review', { sar: true }))
    expect(buttons()).toContain('Move to: Escalated')
    expect(buttons()).not.toContain('Move to: Closed, no action')
    expect(buttons()).not.toContain('Flag as SAR-related')
  })

  it('sar_decision_pending offers no plain transition; the SAR closures are proposals only', async () => {
    const user = await openCase(detail('sar_decision_pending', { sar: true }))
    expect(buttons().filter((b) => b?.startsWith('Move to'))).toEqual([])
    await user.click(screen.getByRole('button', { name: 'Propose a four-eyes action' }))
    const dlg = await screen.findByRole('dialog', { name: 'Propose a four-eyes action' })
    const opts = within(within(dlg).getByLabelText('Action')).getAllByRole('option').map((o) => o.textContent)
    expect(opts).toEqual(expect.arrayContaining(['Close the case, SAR not filed', 'Close the case, SAR filed (external filing reference)', 'Lift a sanctions hold']))
  })

  it('there is no reject operation anywhere (the server has none: NOT IMPLEMENTED)', async () => {
    await openCase(detail('sar_decision_pending', { events: [{ seq: 1, type: 'opened', reason_code: 'r', actor_id: OTHER, actor_scope: 'tenant', created_at: '2026-10-10T10:00:00Z' }, proposalEvent(2, OTHER)] }))
    expect(buttons().join('|')).not.toMatch(/reject/i)
  })

  it('a transition presents the case last_seq (compare-and-set) and the closed reason code', async () => {
    const user = await openCase(detail('open', { lastSeq: 1 }))
    let body: unknown
    server.use(http.post(`${BASE}/cases/${CASE}/transition`, async ({ request }) => { body = await request.json(); return HttpResponse.json({ last_seq: 2 }) }))
    await user.click(screen.getByRole('button', { name: 'Move to: In review' }))
    const dlg = await screen.findByRole('dialog', { name: 'Move case to: In review' })
    await user.type(within(dlg).getByLabelText('Reason code'), 'start_review')
    await user.click(within(dlg).getByRole('button', { name: 'Move case' }))
    expect(await screen.findByText('Case updated.')).toBeInTheDocument()
    expect(body).toEqual({ expected_seq: 1, to: 'in_review', reason_code: 'start_review' })
  })

  it('a stale case is explained and reloaded, never retried silently', async () => {
    const user = await openCase(detail('open', { lastSeq: 1 }))
    let reads = 0
    server.use(
      http.get(`${BASE}/cases/${CASE}`, () => { reads++; return HttpResponse.json(detail('in_review', { lastSeq: 3, events: [{ seq: 3, type: 'status_changed', reason_code: 'r', actor_id: OTHER, actor_scope: 'tenant', created_at: '2026-10-10T10:00:00Z' }] })) }),
      http.post(`${BASE}/cases/${CASE}/transition`, () => ERR(409, 'conflict', ['stale'])),
    )
    await user.click(screen.getByRole('button', { name: 'Move to: In review' }))
    const dlg = await screen.findByRole('dialog', { name: 'Move case to: In review' })
    await user.type(within(dlg).getByLabelText('Reason code'), 'start_review')
    await user.click(within(dlg).getByRole('button', { name: 'Move case' }))
    expect(await within(dlg).findByRole('alert')).toHaveTextContent('The case changed since you opened it')
    await waitFor(() => expect(reads).toBeGreaterThan(0))
    expect(document.body).not.toHaveTextContent('RAW SERVER MESSAGE')
  })

  it('a refused note shows the guard explanation and stores nothing', async () => {
    const user = await openCase(detail('in_review'))
    server.use(http.post(`${BASE}/cases/${CASE}/notes`, () => ERR(400, 'validation_error', ['note_refused'])))
    await user.click(screen.getByRole('button', { name: 'Add a note' }))
    const dlg = await screen.findByRole('dialog', { name: 'Add a note' })
    await user.type(within(dlg).getByLabelText('Note'), 'call someone@example.com')
    await user.click(within(dlg).getByRole('button', { name: 'Add note' }))
    expect(await within(dlg).findByRole('alert')).toHaveTextContent('must not contain card numbers')
  })

  it('shows the pending proposal to an approver, binds the server content hash (not typed), and posts proposal_seq + hash', async () => {
    const events = [{ seq: 1, type: 'opened', reason_code: 'r', actor_id: OTHER, actor_scope: 'tenant' as const, created_at: '2026-10-10T10:00:00Z' }, proposalEvent(2, OTHER)]
    const user = await openCase(detail('sar_decision_pending', { sar: true, events }))
    const section = await screen.findByRole('region', { name: 'Pending proposal' })
    expect(section).toHaveTextContent('Close the case, SAR not filed')
    let body: unknown
    server.use(http.post(`${BASE}/cases/${CASE}/approvals`, async ({ request }) => { body = await request.json(); return HttpResponse.json({ last_seq: 3 }) }))
    await user.click(within(section).getByRole('button', { name: 'Approve this proposal' }))
    const dlg = await screen.findByRole('dialog', { name: 'Approve the proposal' })
    await user.type(within(dlg).getByLabelText('Reason code'), 'approved_close')
    await user.click(within(dlg).getByRole('button', { name: 'Approve' }))
    expect(await screen.findByText('Approved.')).toBeInTheDocument()
    expect(body).toEqual({ proposal_seq: 2, content_hash: HASH, reason_code: 'approved_close' })
  })

  it('you cannot approve your own proposal: the control is disabled and says why', async () => {
    const events = [{ seq: 1, type: 'opened', reason_code: 'r', actor_id: OTHER, actor_scope: 'tenant' as const, created_at: '2026-10-10T10:00:00Z' }, proposalEvent(2, ME)]
    await openCase(detail('sar_decision_pending', { sar: true, events }))
    const section = await screen.findByRole('region', { name: 'Pending proposal' })
    expect(within(section).getByRole('button', { name: 'Approve this proposal' })).toBeDisabled()
    expect(section).toHaveTextContent('you cannot approve it')
  })

  it('only the LAST event can be a pending proposal: a later event withdraws it', async () => {
    const events = [proposalEvent(2, OTHER), { seq: 3, type: 'note_added', reason_code: 'n', actor_id: OTHER, actor_scope: 'tenant' as const, created_at: '2026-10-10T13:00:00Z' }]
    await openCase(detail('sar_decision_pending', { events }))
    expect(screen.queryByRole('region', { name: 'Pending proposal' })).toBeNull()
  })

  it('a four-eyes refusal (same person / subject) is explained from the closed map', async () => {
    const events = [{ seq: 1, type: 'opened', reason_code: 'r', actor_id: OTHER, actor_scope: 'tenant' as const, created_at: '2026-10-10T10:00:00Z' }, proposalEvent(2, OTHER)]
    const user = await openCase(detail('sar_decision_pending', { events }))
    server.use(http.post(`${BASE}/cases/${CASE}/approvals`, () => ERR(409, 'conflict', ['four_eyes_refused'])))
    await user.click(await screen.findByRole('button', { name: 'Approve this proposal' }))
    const dlg = await screen.findByRole('dialog', { name: 'Approve the proposal' })
    await user.type(within(dlg).getByLabelText('Reason code'), 'approved_close')
    await user.click(within(dlg).getByRole('button', { name: 'Approve' }))
    expect(await within(dlg).findByRole('alert')).toHaveTextContent('the same person as the proposer, or the subject of the case')
  })

  it('a platform officer needs the tenant in the link: without it nothing is requested', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    let calls = 0
    server.use(http.get(`${BASE}/cases/${CASE}`, () => { calls++; return HttpResponse.json(detail('open')) }))
    renderWithProviders(<AppRoutes />, { route: `/aml/cases/${CASE}` })
    expect(await screen.findByText(/does not name a tenant and a case/)).toBeInTheDocument()
    expect(calls).toBe(0)
  })

  it('a platform officer with ?tenant= acts under that tenant path', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    let path = ''
    server.use(http.get(`${BASE}/cases/${CASE}`, ({ request }) => { path = new URL(request.url).pathname; return HttpResponse.json(detail('open')) }))
    renderWithProviders(<AppRoutes />, { route: `/aml/cases/${CASE}?tenant=${TENANT}` })
    await screen.findByRole('region', { name: 'Case summary' })
    expect(path).toBe(`${BASE}/cases/${CASE}`)
  })

  it('a redacted (null) actor or author id is shown as a fixed word and never guessed or compared with the viewer', async () => {
    const d = detail('in_review', { events: [{ seq: 1, type: 'opened', reason_code: 'r', actor_id: null, actor_scope: 'platform_acting', created_at: '2026-10-10T10:00:00Z' }] })
    d.notes[0].created_by = null
    await openCase(d)
    const events = screen.getByRole('region', { name: 'Events' })
    expect(events).toHaveTextContent('by hidden (platform_acting)')
    expect(screen.getByRole('region', { name: 'Notes' })).toHaveTextContent('hidden')
    expect(document.body).not.toHaveTextContent('null')
  })

  it('renders note bodies as text, never as markup', async () => {
    const d = detail('in_review')
    d.notes[0].body = '<img src=x onerror=alert(1)> <b>bold</b>'
    await openCase(d)
    expect(document.querySelector('img')).toBeNull()
    expect(screen.getByText('<img src=x onerror=alert(1)> <b>bold</b>')).toBeInTheDocument()
  })
})

describe('rules mirror and error map (pure)', () => {
  it('the transition table mirrors migration 0142', () => {
    const t = (status: CaseStatus, sar = false) => allowedTransitions({ status, sar_flagged: sar })
    expect(t('open')).toEqual(['in_review'])
    expect(t('in_review')).toEqual(['escalated', 'closed_no_action'])
    expect(t('in_review', true)).toEqual(['escalated'])
    expect(t('escalated')).toEqual(['sar_decision_pending'])
    expect(t('sar_decision_pending')).toEqual([])
    for (const s of ['closed_no_action', 'closed_sar_not_filed', 'closed_sar_filed'] as CaseStatus[]) expect(t(s)).toEqual(['in_review'])
    expect(canFlagSar({ status: 'open', sar_flagged: false })).toBe(false)
    expect(canFlagSar({ status: 'escalated', sar_flagged: false })).toBe(true)
    expect(canFlagSar({ status: 'escalated', sar_flagged: true })).toBe(false)
    expect(proposalKinds({ status: 'open' })).toEqual([])
    expect(proposalKinds({ status: 'escalated' })).toEqual(['lift_hold'])
    expect(proposalKinds({ status: 'sar_decision_pending' })).toEqual(['close_sar_not_filed', 'close_sar_filed', 'lift_hold'])
    expect(pendingProposal({ last_seq: 2 }, [proposalEvent(2, OTHER)])?.seq).toBe(2)
    expect(pendingProposal({ last_seq: 3 }, [proposalEvent(2, OTHER)])).toBeNull()
  })

  it('every refusal is a fixed sentence: the server message and unknown codes never surface', () => {
    const cases: Array<[ApiError, string]> = [
      [new ApiError('forbidden', RAW, 403, 'r1'), 'in-force AML officer grant'],
      [new ApiError('not_found', RAW, 404, 'r1'), 'outside your AML scope'],
      [new ApiError('conflict', RAW, 409, 'r1', ['quorum_not_met']), 'officer quorum'],
      [new ApiError('conflict', RAW, 409, 'r1', ['hold_refused']), 'four-eyes proposal'],
      [new ApiError('conflict', RAW, 409, 'r1', ['illegal_transition']), 'not allowed'],
      [new ApiError('conflict', RAW, 409, 'r1', ['something_new']), 'conflict'],
      [new ApiError('validation_error', RAW, 400, 'r1'), 'invalid'],
      [new ApiError('service_unavailable', RAW, 503, 'r1'), 'not available'],
      [new ApiError('internal_error', RAW, 500, 'r1'), 'could not complete'],
      [new ApiError('step_up_cancelled', RAW, 0), 'cancelled'],
      [new ApiError('network_error', RAW, 0), 'Could not reach'],
    ]
    for (const [err, want] of cases) {
      const v = describeAmlError(err, 'propose')
      expect(v.message).toContain(want)
      expect(v.message).not.toContain('RAW')
    }
    expect(describeAmlError(new Error('boom'), 'case').message).toContain('Nothing was confirmed')
    expect(describeAmlError(new ApiError('conflict', RAW, 409, 'r', ['stale']), 'propose').stale).toBe(true)
    expect(describeAmlError(new ApiError('forbidden', RAW, 403, 'r'), 'propose').stale).toBe(false)
  })
})
