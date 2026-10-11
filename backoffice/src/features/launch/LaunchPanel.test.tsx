import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { Route, Routes } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { PLATFORM_ADMIN_CLAIMS, signInAs } from '../../test/session'
import { BrandDetailPage } from '../tenants/BrandDetailPage'
import { TenantDetailPage } from '../tenants/TenantDetailPage'
import { TenantListPage } from '../tenants/TenantListPage'
import { LAUNCH_DISCLAIMER } from './labels'
import {
  API_DISCLAIMER,
  BRAND_ID,
  JURIS_ID,
  LICENCE_ID,
  TENANT_ID,
  launchReadHandlers,
  launchRequest,
  readiness,
  transition,
} from './launchFixtures'

const TENANT_ADMIN = { role: 'tenant_admin', tenant_id: TENANT_ID, sub: 'tenant-admin-1' }

function renderBrand(query = '') {
  return renderWithProviders(
    <Routes>
      <Route path="/tenants/:tenantId/brands/:brandId" element={<BrandDetailPage />} />
    </Routes>,
    { route: `/tenants/${TENANT_ID}/brands/${BRAND_ID}${query}` },
  )
}
function renderTenant(query = '') {
  return renderWithProviders(
    <Routes>
      <Route path="/tenants/:tenantId" element={<TenantDetailPage />} />
    </Routes>,
    { route: `/tenants/${TENANT_ID}${query}` },
  )
}

describe('status badges and filter', () => {
  it('tenant list: renders all four statuses as labelled badges and offers a pending_launch filter that is sent to the API', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    const seen: (string | null)[] = []
    server.use(
      http.get('/v1/admin/tenants', ({ request }) => {
        seen.push(new URL(request.url).searchParams.get('status'))
        return HttpResponse.json({
          items: ['pending_launch', 'active', 'suspended', 'closed'].map((s) => ({ id: `id-${s}`, name: `T ${s}`, slug: s, licensing_model: 'own_licence', status: s })),
          limit: 20,
          offset: 0,
          total: 4,
        })
      }),
    )
    renderWithProviders(<TenantListPage />, { route: '/tenants' })
    await screen.findByText('T pending_launch')
    for (const label of ['Pending launch', 'Active', 'Suspended', 'Closed']) {
      expect(screen.getAllByText(label).length).toBeGreaterThan(0)
    }
    const user = userEvent.setup()
    await user.selectOptions(screen.getAllByRole('combobox')[0], 'pending_launch')
    await waitFor(() => expect(seen).toContain('pending_launch'))
  })

  it('brand detail: shows the legacy-baseline badge when the newest transition is the legacy baseline, and the disclaimer', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    launchReadHandlers({
      brandStatus: 'active',
      history: [transition({ kind: 'legacy_baseline', from_status: undefined, to_status: 'active' })],
    })
    renderBrand()
    const statusTitle = await screen.findByLabelText('Launch status')
    await waitFor(() => expect(within(statusTitle).getByText('Legacy baseline - no launch decision recorded through ADR 0112')).toBeInTheDocument())
    const note = (await screen.findAllByRole('note', { name: 'Disclaimer' }))[0]
    expect(note).toHaveTextContent(LAUNCH_DISCLAIMER)
    expect(note).toHaveTextContent(API_DISCLAIMER)
  })

  it('no legacy badge once a governed decision is the newest', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    launchReadHandlers({
      brandStatus: 'active',
      history: [
        transition({ kind: 'legacy_baseline', from_status: undefined, id: 'l', created_at: '2026-01-01T00:00:00Z' }),
        transition({ id: 'g', created_at: '2026-10-01T00:00:00Z' }),
      ],
    })
    renderBrand()
    await screen.findByRole('list', { name: 'Status history' })
    expect(within(screen.getByLabelText('Launch status')).queryByText(/Legacy baseline/)).not.toBeInTheDocument()
  })
})

describe('role gating (UI hint) and degradation when the API refuses', () => {
  it('a foreign-tenant 404 on the launch reads shows a generic not-found, with no existence hint or raw text', async () => {
    signInAs(TENANT_ADMIN)
    launchReadHandlers({ brandStatus: 'active' })
    server.use(
      http.get('/v1/admin/tenants/:tid/launch-authorisations', () => HttpResponse.json({ code: 'not_found', message: 'RAW tenant 999 exists elsewhere' }, { status: 404 })),
      http.get('/v1/admin/tenants/:tid/launch-status-history', () => HttpResponse.json({ code: 'not_found', message: 'not found' }, { status: 404 })),
    )
    renderBrand()
    expect((await screen.findAllByText(/Not found\. It may not exist, or it may belong to another tenant/)).length).toBe(2)
    expect(document.body.textContent).not.toContain('RAW tenant 999')
  })

  it('finance sees no launch panel at all and makes no launch API call', async () => {
    signInAs({ role: 'finance', tenant_id: TENANT_ID })
    let called = false
    launchReadHandlers({ brandStatus: 'active' })
    server.use(
      http.get('/v1/admin/tenants/:tid/launch-authorisations', () => {
        called = true
        return HttpResponse.json({ items: [] })
      }),
    )
    renderBrand()
    await screen.findByText('Brand details')
    expect(screen.queryByText('Launch authorisation')).not.toBeInTheDocument()
    expect(called).toBe(false)
  })

  it('tenant_admin on the TENANT subject sees the panel read-only (no request or suspend buttons)', async () => {
    signInAs(TENANT_ADMIN)
    launchReadHandlers({ tenantStatus: 'active' })
    renderTenant()
    await screen.findByText('Launch authorisation')
    expect(screen.queryByRole('button', { name: /Request/ })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Suspend' })).not.toBeInTheDocument()
  })

  it('tenant_admin on a BRAND can request and suspend, but never sees approve or reject', async () => {
    signInAs(TENANT_ADMIN)
    const pending = launchRequest({ requested_by: undefined, requested_by_scope: 'platform' })
    launchReadHandlers({ brandStatus: 'active', requests: [pending] })
    server.use(
      http.get('/v1/admin/tenants/:tid/launch-authorisations/:rid', () => HttpResponse.json({ disclaimer: API_DISCLAIMER, request: pending, readiness: readiness() })),
    )
    renderBrand(`?request=${pending.id}`)
    await screen.findByRole('button', { name: 'Request closure' })
    expect(screen.getByRole('button', { name: 'Suspend' })).toBeInTheDocument()
    await screen.findByLabelText('Launch request detail')
    expect(screen.queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Reject' })).not.toBeInTheDocument()
  })

  it('when the server refuses an action the UI allowed, the fixed message shows and the raw server text never does', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    launchReadHandlers({ brandStatus: 'active' })
    server.use(
      http.get('/v1/admin/tenants/:tid/launch-readiness', () => HttpResponse.json({ disclaimer: API_DISCLAIMER, readiness: readiness({ checks: [], non_passing_check_codes: [] }) })),
      http.post('/v1/admin/tenants/:tid/launch-authorisations', () =>
        HttpResponse.json({ code: 'forbidden', message: 'RAW internal: role lacks grant xyz' }, { status: 403 }),
      ),
    )
    renderBrand()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Request closure' }))
    const dialog = await screen.findByRole('dialog')
    await user.selectOptions(within(dialog).getByLabelText('Reason code'), 'offboarding')
    const submit = within(dialog).getByRole('button', { name: 'File close request' })
    await waitFor(() => expect(submit).toBeEnabled())
    await user.click(submit)
    const alert = await within(dialog).findByText(/You are not allowed to do this/)
    expect(alert).toBeInTheDocument()
    expect(document.body.textContent).not.toContain('RAW internal')
  })
})

describe('request dialog', () => {
  it('activate: licensing fields incl. all four models, conditions-note WARNING, acknowledgement by check code, exact ack set submitted, no tenant_id', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    launchReadHandlers({ brandStatus: 'pending_launch' })
    const readinessQueries: URLSearchParams[] = []
    const lastQuery = () => readinessQueries[readinessQueries.length - 1]
    let body: Record<string, unknown> = {}
    server.use(
      http.get('/v1/admin/tenants/:tid/launch-readiness', ({ request }) => {
        readinessQueries.push(new URL(request.url).searchParams)
        return HttpResponse.json({ disclaimer: API_DISCLAIMER, readiness: readiness() })
      }),
      http.post('/v1/admin/tenants/:tid/launch-authorisations', async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>
        return HttpResponse.json(
          { disclaimer: API_DISCLAIMER, request: launchRequest(), readiness: readiness(), executed: false },
          { status: 201 },
        )
      }),
    )
    renderBrand()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Request activate' }))
    const dialog = await screen.findByRole('dialog', { name: 'Activate request' })

    // the four licensing models are all selectable
    const model = within(dialog).getByLabelText('Licensing model (recorded basis)') as HTMLSelectElement
    expect(Array.from(model.options).map((o) => o.value)).toEqual([
      '',
      'platform_licence',
      'tenant_licence',
      'other_manually_approved',
      'not_applicable_recorded_determination',
    ])
    // PII warning is visible
    expect(within(dialog).getAllByText(/not a PII store/i).length).toBeGreaterThan(0)
    expect(within(dialog).getAllByText(/Do not enter names, ID numbers, card or account numbers/).length).toBeGreaterThan(0)

    await user.selectOptions(within(dialog).getByLabelText('Reason code'), 'initial_launch')
    await user.selectOptions(model, 'other_manually_approved')
    await user.selectOptions(within(dialog).getByLabelText('Licensing status (recorded basis)'), 'conditional')
    await user.type(within(dialog).getByLabelText('Determination reference'), 'DOC-77')
    await user.type(within(dialog).getByLabelText('Determination by (responsible function)'), 'external counsel')
    await user.type(within(dialog).getByLabelText('Responsible operator (legal entity)'), 'Operator Ltd')
    // registry picker (platform admin) fills the jurisdiction ids
    await user.click(await within(dialog).findByRole('checkbox', { name: /AJ - Anjouan/ }))

    // readiness appears, submit stays disabled until every non-passing check is acknowledged
    await within(dialog).findByLabelText('Readiness checklist')
    const submit = within(dialog).getByRole('button', { name: 'File activate request' })
    expect(submit).toBeDisabled()
    expect(within(dialog).getAllByText('Pass').length).toBe(2)
    expect(within(dialog).getAllByText('Fail').length).toBe(2)
    expect(within(dialog).getByText('Info: not evaluable')).toBeInTheDocument()
    await waitFor(() => expect(lastQuery()?.get('jurisdiction_ids')).toBe(JURIS_ID))
    expect(lastQuery()?.get('action')).toBe('activate')
    expect(lastQuery()?.get('brand_id')).toBe(BRAND_ID)
    expect(lastQuery()?.get('licensing_model')).toBe('other_manually_approved')

    for (const code of ['R-PROVIDER-TIER', 'R-ALERTS']) await user.click(within(dialog).getByRole('checkbox', { name: `Acknowledge ${code}` }))
    expect(submit).toBeDisabled()
    await user.click(within(dialog).getByRole('checkbox', { name: 'Acknowledge R-PLAYER-FUNDS' }))
    expect(submit).toBeEnabled()
    await user.click(submit)

    await waitFor(() => expect(body.action).toBe('activate'))
    expect(body).toMatchObject({
      brand_id: BRAND_ID,
      reason_code: 'initial_launch',
      acknowledged_check_codes: ['R-PROVIDER-TIER', 'R-ALERTS', 'R-PLAYER-FUNDS'],
      licensing: { model: 'other_manually_approved', status: 'conditional', determination_reference: 'DOC-77', determination_by_role: 'external counsel' },
      responsible_operator_name: 'Operator Ltd',
      jurisdiction_ids: [JURIS_ID],
    })
    expect('tenant_id' in body).toBe(false)
    expect(await screen.findByText(/request filed\. It changes nothing until 1 distinct platform approver approves it/)).toBeInTheDocument()
  })

  it('licence-backed model needs a licence id; a not-applicable determination offers no licence field', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    launchReadHandlers({ brandStatus: 'pending_launch' })
    renderBrand()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Request activate' }))
    const dialog = await screen.findByRole('dialog')
    const model = within(dialog).getByLabelText('Licensing model (recorded basis)')
    await user.selectOptions(model, 'platform_licence')
    expect(await within(dialog).findByLabelText('Licence')).toBeInTheDocument()
    expect(within(dialog).getByRole('option', { name: /LIC-1/ })).toBeInTheDocument()
    expect(within(dialog).queryByLabelText('Determination reference')).not.toBeInTheDocument()
    await user.selectOptions(model, 'not_applicable_recorded_determination')
    expect(within(dialog).queryByLabelText('Licence')).not.toBeInTheDocument()
    expect(within(dialog).getByLabelText('Determination reference')).toBeInTheDocument()
    expect(within(dialog).getByText(/requires the status "Not required per determination"/)).toBeInTheDocument()
  })

  it('a tenant caller (no registry access) types ids instead and the registry is never fetched', async () => {
    signInAs(TENANT_ADMIN)
    launchReadHandlers({ brandStatus: 'pending_launch' })
    let registryCalls = 0
    server.use(
      http.get('/v1/admin/jurisdictions', () => {
        registryCalls++
        return HttpResponse.json({ code: 'forbidden', message: 'x' }, { status: 403 })
      }),
    )
    renderBrand()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Request activate' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByLabelText('Jurisdiction ids')).toBeInTheDocument()
    expect(registryCalls).toBe(0)
  })

  it('close: shows R-PLAYER-FUNDS per-asset sums as exact strings (1.8e19) and the stranded-until-HD-CTF-4 notice', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    launchReadHandlers({ brandStatus: 'active' })
    let body: Record<string, unknown> = {}
    server.use(
      http.get('/v1/admin/tenants/:tid/launch-readiness', () =>
        HttpResponse.json({
          disclaimer: API_DISCLAIMER,
          readiness: readiness({
            checks: [{ code: 'R-PLAYER-FUNDS', result: 'fail', detail_code: 'non_zero_balances' }],
            non_passing_check_codes: ['R-PLAYER-FUNDS'],
            counts: { players_with_balance: 3, open_sportsbook_bets: 0, hold_bearing_withdrawals: 0 },
            balances: { ETH: '18000000000000000000', EUR: '123456789012345678901234567890' },
            notices: ['Server notice text.'],
          }),
        }),
      ),
      http.post('/v1/admin/tenants/:tid/launch-authorisations', async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>
        return HttpResponse.json({ disclaimer: API_DISCLAIMER, request: launchRequest({ action: 'close', required_approvals: 2 }), readiness: readiness(), executed: false }, { status: 201 })
      }),
    )
    renderBrand()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Request closure' }))
    const dialog = await screen.findByRole('dialog', { name: 'Close request' })
    expect(await within(dialog).findByTestId('balance-ETH')).toHaveTextContent('18,000,000,000,000,000,000')
    expect(within(dialog).getByTestId('balance-ETH')).toHaveAttribute('title', '18000000000000000000')
    expect(within(dialog).getByTestId('balance-EUR')).toHaveTextContent('123,456,789,012,345,678,901,234,567,890')
    expect(dialog.textContent).not.toMatch(/e\+\d|1\.8e/i)
    expect(within(dialog).getAllByText(/balances stay stranded until HD-CTF-4/i).length).toBeGreaterThan(0)
    expect(within(dialog).getByText('Server notice text.')).toBeInTheDocument()

    await user.selectOptions(within(dialog).getByLabelText('Reason code'), 'offboarding')
    const submit = within(dialog).getByRole('button', { name: 'File close request' })
    expect(submit).toBeDisabled()
    await user.click(within(dialog).getByRole('checkbox', { name: 'Acknowledge R-PLAYER-FUNDS' }))
    await user.click(submit)
    await waitFor(() => expect(body.action).toBe('close'))
    expect(body.acknowledged_check_codes).toEqual(['R-PLAYER-FUNDS'])
    expect('licensing' in body).toBe(false)
  })

  it('maps LAUNCH_PRECONDITION_FAILED to a fixed sentence plus the closed check codes', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    launchReadHandlers({ brandStatus: 'active' })
    server.use(
      http.get('/v1/admin/tenants/:tid/launch-readiness', () => HttpResponse.json({ disclaimer: API_DISCLAIMER, readiness: readiness({ checks: [], non_passing_check_codes: [] }) })),
      http.post('/v1/admin/tenants/:tid/launch-authorisations', () =>
        HttpResponse.json({ code: 'LAUNCH_WITHDRAWALS_HOLDING_FUNDS', message: 'RAW db text', check_codes: ['LA023'] }, { status: 409 }),
      ),
    )
    renderBrand()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Request closure' }))
    const dialog = await screen.findByRole('dialog')
    await user.selectOptions(within(dialog).getByLabelText('Reason code'), 'offboarding')
    const submit = within(dialog).getByRole('button', { name: 'File close request' })
    await waitFor(() => expect(submit).toBeEnabled())
    await user.click(submit)
    expect(await within(dialog).findByText(/withdrawal requests still hold player funds\. Resolve them first/)).toBeInTheDocument()
    expect(within(dialog).getByText('LA023')).toBeInTheDocument()
    expect(document.body.textContent).not.toContain('RAW db text')
  })
})

describe('request detail: approvals, rejects, approve / reject / cancel', () => {
  const pendingByOther = launchRequest({
    required_approvals: 2,
    approvals: [{ id: 'a1', decision: 'approve', decided_by: 'cccccccc-cccc-4ccc-8ccc-cccccccccccc', reason_code: 'reviewed_ok', readiness_hash_at_decision: 'a'.repeat(64), decided_at: '2026-10-11T09:10:00Z' }],
    prior_rejects: [{ request_id: '66666666-6666-4666-8666-666666666666', decided_at: '2026-10-10T09:00:00Z', reason_code: 'readiness_concerns', same_payload: true }],
  })

  function detailHandler(req = pendingByOther, r = readiness({ readiness_hash: 'f'.repeat(64) })) {
    server.use(
      http.get('/v1/admin/tenants/:tid/launch-authorisations/:rid', () => HttpResponse.json({ disclaimer: API_DISCLAIMER, request: req, readiness: r })),
    )
  }

  it('shows licensing, jurisdictions, conditions note, approvals and prior rejects within the TTL', async () => {
    signInAs({ ...PLATFORM_ADMIN_CLAIMS, sub: 'approver-1' })
    launchReadHandlers({ brandStatus: 'pending_launch', requests: [pendingByOther] })
    detailHandler()
    renderBrand(`?request=${pendingByOther.id}`)
    const detail = await screen.findByLabelText('Launch request detail')
    expect(within(detail).getByText('Other (manually approved)')).toBeInTheDocument()
    expect(within(detail).getByText('Conditional')).toBeInTheDocument()
    expect(within(detail).getByText('DOC-77')).toBeInTheDocument()
    expect(within(detail).getByText('Operator Ltd')).toBeInTheDocument()
    expect(within(detail).getByText(JURIS_ID)).toBeInTheDocument()
    expect(within(detail).getByText('Launch only after the soft-launch review.')).toBeInTheDocument()
    expect(within(detail).getByText('1 of 2 required')).toBeInTheDocument()
    const approvals = within(detail).getByRole('list', { name: 'Approvals' })
    expect(within(approvals).getByText('Approved')).toBeInTheDocument()
    const rejects = within(detail).getByRole('list', { name: 'Prior rejects' })
    expect(within(rejects).getByText('identical request')).toBeInTheDocument()
    expect(within(detail).getByRole('note', { name: 'Disclaimer' })).toHaveTextContent(LAUNCH_DISCLAIMER)
  })

  it('approve sends reason_code and the seen_readiness_hash from the pending GET, and reports execution', async () => {
    signInAs({ ...PLATFORM_ADMIN_CLAIMS, sub: 'approver-1' })
    launchReadHandlers({ brandStatus: 'pending_launch', requests: [pendingByOther] })
    detailHandler()
    let body: Record<string, unknown> = {}
    server.use(
      http.post('/v1/admin/tenants/:tid/launch-authorisations/:rid/approve', async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>
        return HttpResponse.json({
          disclaimer: API_DISCLAIMER,
          request: { ...pendingByOther, status: 'executed' },
          decision: 'approve',
          executed: true,
          readiness: readiness(),
          prior_rejects: [],
        })
      }),
    )
    renderBrand(`?request=${pendingByOther.id}`)
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Approve' }))
    const dialog = await screen.findByRole('dialog', { name: 'Approve launch request' })
    expect(within(dialog).getByLabelText('Readiness checklist')).toBeInTheDocument()
    await user.selectOptions(within(dialog).getByLabelText('Reason code'), 'reviewed_ok')
    await user.click(within(dialog).getByRole('button', { name: 'Approve request' }))
    await waitFor(() => expect(body).toEqual({ reason_code: 'reviewed_ok', seen_readiness_hash: 'f'.repeat(64) }))
    expect(await screen.findByText(/the request executed: the status is now Active/)).toBeInTheDocument()
  })

  it('approve is not offered to the requester; the cancel is', async () => {
    signInAs({ ...PLATFORM_ADMIN_CLAIMS, sub: pendingByOther.requested_by })
    launchReadHandlers({ brandStatus: 'pending_launch', requests: [pendingByOther] })
    detailHandler()
    let cancelBody: unknown
    server.use(
      http.post('/v1/admin/tenants/:tid/launch-authorisations/:rid/cancel', async ({ request }) => {
        cancelBody = await request.json()
        return HttpResponse.json({ disclaimer: API_DISCLAIMER, request: { ...pendingByOther, status: 'cancelled' } })
      }),
    )
    renderBrand(`?request=${pendingByOther.id}`)
    await screen.findByLabelText('Launch request detail')
    expect(screen.queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Reject' })).not.toBeInTheDocument()
    expect(screen.getByText(/You filed this request\. A different platform approver must decide it/)).toBeInTheDocument()
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Cancel request' }))
    const dialog = await screen.findByRole('dialog', { name: 'Cancel launch request' })
    const confirm = within(dialog).getByRole('button', { name: 'Cancel request' })
    expect(confirm).toBeDisabled()
    await user.selectOptions(within(dialog).getByLabelText('Reason code'), 'requester_withdrew')
    await user.click(confirm)
    await waitFor(() => expect(cancelBody).toEqual({ reason_code: 'requester_withdrew' }))
    expect(await screen.findByText('Request cancelled.')).toBeInTheDocument()
  })

  it('a stale readiness refusal records nothing, shows the fixed message and re-reads the request', async () => {
    signInAs({ ...PLATFORM_ADMIN_CLAIMS, sub: 'approver-1' })
    launchReadHandlers({ brandStatus: 'pending_launch', requests: [pendingByOther] })
    let gets = 0
    server.use(
      http.get('/v1/admin/tenants/:tid/launch-authorisations/:rid', () => {
        gets++
        return HttpResponse.json({ disclaimer: API_DISCLAIMER, request: pendingByOther, readiness: readiness({ readiness_hash: gets > 1 ? 'e'.repeat(64) : 'f'.repeat(64) }) })
      }),
      http.post('/v1/admin/tenants/:tid/launch-authorisations/:rid/approve', () =>
        HttpResponse.json({ code: 'LAUNCH_STALE_READINESS', message: 'RAW stale' }, { status: 409 }),
      ),
    )
    renderBrand(`?request=${pendingByOther.id}`)
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Approve' }))
    const dialog = await screen.findByRole('dialog')
    await user.selectOptions(within(dialog).getByLabelText('Reason code'), 'reviewed_ok')
    await user.click(within(dialog).getByRole('button', { name: 'Approve request' }))
    expect(await within(dialog).findByText(/The readiness changed since you opened this request/)).toBeInTheDocument()
    await waitFor(() => expect(gets).toBeGreaterThan(1))
    expect(document.body.textContent).not.toContain('RAW stale')
  })

  it('reject requires a reason code and posts it without a readiness hash', async () => {
    signInAs({ ...PLATFORM_ADMIN_CLAIMS, sub: 'approver-1' })
    launchReadHandlers({ brandStatus: 'pending_launch', requests: [pendingByOther] })
    detailHandler()
    let body: unknown
    server.use(
      http.post('/v1/admin/tenants/:tid/launch-authorisations/:rid/reject', async ({ request }) => {
        body = await request.json()
        return HttpResponse.json({ disclaimer: API_DISCLAIMER, request: { ...pendingByOther, status: 'rejected' }, decision: 'reject', executed: false, readiness: readiness(), prior_rejects: [] })
      }),
    )
    renderBrand(`?request=${pendingByOther.id}`)
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Reject' }))
    const dialog = await screen.findByRole('dialog', { name: 'Reject launch request' })
    const confirm = within(dialog).getByRole('button', { name: 'Reject request' })
    expect(confirm).toBeDisabled()
    await user.selectOptions(within(dialog).getByLabelText('Reason code'), 'policy_concern')
    await user.click(confirm)
    await waitFor(() => expect(body).toEqual({ reason_code: 'policy_concern' }))
    expect(await screen.findByText('Rejection recorded. The request has ended.')).toBeInTheDocument()
  })

  it('shows a superseded request explicitly, not as a silent disappearance', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    const sup = launchRequest({ status: 'superseded' })
    launchReadHandlers({ brandStatus: 'suspended', requests: [sup] })
    server.use(http.get('/v1/admin/tenants/:tid/launch-authorisations/:rid', () => HttpResponse.json({ disclaimer: API_DISCLAIMER, request: sup })))
    renderBrand(`?request=${sup.id}`)
    expect(await screen.findByText(/superseded by a suspension of the same subject/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument()
  })

  it('tenant-scope redaction: no staff ids, scope only, environment and process detail marked platform-only', async () => {
    signInAs(TENANT_ADMIN)
    const redacted = launchRequest({
      requested_by: undefined,
      requested_by_scope: 'platform',
      approvals: [{ id: 'a1', decision: 'approve', reason_code: 'reviewed_ok', readiness_hash_at_decision: 'a'.repeat(64), decided_at: '2026-10-11T09:10:00Z' }],
    })
    launchReadHandlers({ brandStatus: 'pending_launch', requests: [redacted], history: [transition({ actor_type: 'platform', executed_by: undefined })] })
    server.use(
      http.get('/v1/admin/tenants/:tid/launch-authorisations/:rid', () =>
        HttpResponse.json({
          disclaimer: API_DISCLAIMER,
          request: redacted,
          readiness: readiness({
            environment: 'redacted',
            checks: [
              { code: 'R-PROVIDER-TIER', result: 'fail', detail_code: 'see_platform' },
              { code: 'R-ALERTS', result: 'fail', detail_code: 'see_platform' },
            ],
            non_passing_check_codes: ['R-PROVIDER-TIER', 'R-ALERTS'],
          }),
        }),
      ),
    )
    renderBrand(`?request=${redacted.id}`)
    const detail = await screen.findByLabelText('Launch request detail')
    expect(within(detail).getByText('Platform staff (platform)')).toBeInTheDocument()
    expect(within(detail).getByText(/by Platform staff on/)).toBeInTheDocument()
    expect(within(detail).getAllByText('Visible to platform staff only').length).toBe(2)
    expect(within(detail).getByText(/Not shown to tenant staff/)).toBeInTheDocument()
    expect(detail.textContent).not.toMatch(/Staff [0-9a-f]{8}/)
    const history = screen.getByRole('list', { name: 'Status history' })
    expect(history.textContent).toContain('executed by Platform staff')
    expect(history.textContent).not.toMatch(/Staff [0-9a-f]{8}/)
  })
})

describe('suspend dialog', () => {
  it('single-actor emergency: reason required, funds inaccessible-not-stranded notice, reports superseded requests', async () => {
    signInAs({ role: 'compliance', tenant_id: TENANT_ID })
    launchReadHandlers({ brandStatus: 'active' })
    let body: unknown
    server.use(
      http.get('/v1/admin/tenants/:tid/launch-readiness', ({ request }) => {
        expect(new URL(request.url).searchParams.get('action')).toBe('suspend')
        return HttpResponse.json({
          disclaimer: API_DISCLAIMER,
          readiness: readiness({ checks: [], non_passing_check_codes: [], balances: { EUR: '500' }, counts: { players_with_balance: 1 } }),
        })
      }),
      http.post('/v1/admin/tenants/:tid/brands/:bid/suspend', async ({ request }) => {
        body = await request.json()
        return HttpResponse.json({
          disclaimer: API_DISCLAIMER,
          request: launchRequest({ action: 'suspend', from_status: 'active', to_status: 'suspended', status: 'executed' }),
          readiness: readiness(),
          executed: true,
          superseded_request_ids: ['x'],
        })
      }),
    )
    renderBrand()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Suspend' }))
    const dialog = await screen.findByRole('dialog', { name: 'Suspend Brand One' })
    expect(within(dialog).getByText(/funds are inaccessible, not stranded/)).toBeInTheDocument()
    expect(dialog.textContent).not.toMatch(/stranded until HD-CTF-4/)
    expect(await within(dialog).findByTestId('balance-EUR')).toHaveTextContent('500')
    expect(within(dialog).getAllByText(/not a PII store/i).length).toBeGreaterThan(0)
    const go = within(dialog).getByRole('button', { name: 'Suspend now' })
    expect(go).toBeDisabled()
    await user.selectOptions(within(dialog).getByLabelText('Reason code'), 'other_documented')
    expect(go).toBeDisabled() // other_documented needs a note
    await user.type(within(dialog).getByLabelText('Conditions note'), 'incident review')
    await user.click(go)
    await waitFor(() => expect(body).toEqual({ reason_code: 'other_documented', conditions_note: 'incident review' }))
    expect(await screen.findByText(/Suspended\. The status is now Suspended\. 1 pending request was superseded\./)).toBeInTheDocument()
  })

  it('tenant suspend uses the tenant route and shows a lock-timeout refusal with the retry message', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    launchReadHandlers({ tenantStatus: 'active' })
    server.use(
      http.get('/v1/admin/tenants/:tid/launch-readiness', () => HttpResponse.json({ disclaimer: API_DISCLAIMER, readiness: readiness({ checks: [], non_passing_check_codes: [] }) })),
      http.post('/v1/admin/tenants/:tid/suspend', () => HttpResponse.json({ code: 'LAUNCH_RETRYABLE_LOCK_TIMEOUT', message: 'RAW lock' }, { status: 409 })),
    )
    renderTenant()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Suspend' }))
    const dialog = await screen.findByRole('dialog')
    await user.selectOptions(within(dialog).getByLabelText('Reason code'), 'security_incident')
    await user.click(within(dialog).getByRole('button', { name: 'Suspend now' }))
    expect(await within(dialog).findByText(/Nothing was recorded\. Try again in a moment/)).toBeInTheDocument()
  })
})
