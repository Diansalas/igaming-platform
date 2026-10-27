import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it, vi } from 'vitest'
import { AppRoutes } from '../../app/AppRoutes'
import { server } from '../../test/mswServer'
import { makeTestJwt } from '../../test/jwt'
import { renderWithProviders } from '../../test/renderWithProviders'
import { PLATFORM_ADMIN_CLAIMS, signInAs } from '../../test/session'
import { stepUpRequiredResponse } from '../../test/stepUp'
import {
  CHG1,
  DISCLAIMER,
  LIC1,
  LIC2,
  ME,
  OTHER,
  SENTINEL,
  TEN1,
  THIRD,
  change,
  changeResponse,
  licence,
  licensingReads,
  page,
} from './licensingFixtures'

vi.setConfig({ testTimeout: 30000 })

const PA = { ...PLATFORM_ADMIN_CLAIMS, sub: ME }
const refusal = (status: number, code: string, extra: Record<string, unknown> = {}) => HttpResponse.json({ code, message: SENTINEL, request_id: 'req-9', ...extra }, { status })

describe('RBAC: the section is platform_admin only (UI hint; the server decides)', () => {
  it.each([['tenant_admin'], ['compliance'], ['finance'], ['support'], ['risk_manager'], ['promotions_manager'], ['bonus_operations']])(
    '%s sees NotAuthorized and no licensing request is sent',
    async (role) => {
      signInAs({ role, tenant_id: TEN1, sub: ME })
      let calls = 0
      server.use(http.all('/v1/admin/licen*', () => { calls++; return HttpResponse.json([]) }), http.all('/v1/admin/jurisdictions*', () => { calls++; return HttpResponse.json([]) }))
      for (const route of ['/licensing/licences', '/licensing/changes', '/licensing/jurisdictions', `/licensing/tenants/${TEN1}`]) {
        const r = renderWithProviders(<AppRoutes />, { route })
        expect(await screen.findByText('Your role does not have access to this section.')).toBeInTheDocument()
        r.unmount()
      }
      expect(calls).toBe(0)
    },
  )

  it('platform_admin gets the Licensing nav entry and the section', async () => {
    signInAs(PA)
    licensingReads()
    renderWithProviders(<AppRoutes />, { route: '/licensing' })
    expect(await screen.findByRole('link', { name: 'Licensing' })).toBeInTheDocument()
    expect(await screen.findByRole('table')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'XX' })).toHaveAttribute('href', expect.stringContaining('/licensing/jurisdictions/'))
  })

  it('degrades when the API refuses what the UI allowed: a 403 is shown as a refusal, the server message is not', async () => {
    signInAs(PA)
    licensingReads()
    server.use(http.get('/v1/admin/licences', () => refusal(403, 'forbidden')))
    renderWithProviders(<AppRoutes />, { route: '/licensing/licences' })
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(/does not hold the permission/)
    expect(document.body).not.toHaveTextContent('do-not-render')
  })
})

describe('jurisdictions', () => {
  it('lists, filters locally and opens a detail that says it is not an approval', async () => {
    signInAs(PA)
    licensingReads()
    renderWithProviders(<AppRoutes />, { route: '/licensing/jurisdictions' })
    const user = userEvent.setup()
    await screen.findByRole('link', { name: 'XX' })
    await user.type(screen.getByLabelText('Filter jurisdictions'), 'nomatch')
    expect(await screen.findByText('No jurisdictions are recorded.')).toBeInTheDocument()
    await user.clear(screen.getByLabelText('Filter jurisdictions'))
    await user.click(await screen.findByRole('link', { name: 'XX' }))
    expect(await screen.findByText(/it is not an approval of the jurisdiction/)).toBeInTheDocument()
    expect(screen.getByText('SYN-0001')).toBeInTheDocument()
  })
})

describe('licences', () => {
  it('lists with filter and shows the recorded-status caveat', async () => {
    signInAs(PA)
    licensingReads({ licences: [licence(), licence({ id: LIC2, licence_number: 'SYN-0002', status: 'active', tenant_ids: [] })] })
    renderWithProviders(<AppRoutes />, { route: '/licensing/licences' })
    const user = userEvent.setup()
    await screen.findByRole('link', { name: 'SYN-0001' })
    expect(screen.getByRole('link', { name: 'SYN-0002' })).toBeInTheDocument()
    expect(screen.getByText(/not a statement from any regulator and is not verified by the platform/)).toBeInTheDocument()
    await user.selectOptions(screen.getByLabelText('Status'), 'active')
    expect(screen.queryByRole('link', { name: 'SYN-0001' })).toBeNull()
    expect(screen.getByRole('link', { name: 'SYN-0002' })).toBeInTheDocument()
  })

  it('detail: lifecycle, evidence note, tenants bound, version history and read-only ceilings (licence_id, current=true)', async () => {
    signInAs(PA)
    licensingReads()
    const seen: URL[] = []
    server.use(
      http.get('/v1/admin/licence-country-ceilings', ({ request }) => {
        seen.push(new URL(request.url))
        return HttpResponse.json(page([{ id: 'c1', licence_id: LIC1, country_code: 'ZZ', state: 'enabled', status: 'active', authorization_reference: 'REF-1', reason_code: 'initial', policy_version: 'v1', effective_from: '2026-01-01T00:00:00Z', effective_to: null, created_by_actor_type: 'staff', created_by: null, created_at: '2026-01-01T00:00:00Z' }]))
      }),
    )
    renderWithProviders(<AppRoutes />, { route: `/licensing/licences/${LIC1}` })
    expect(await screen.findByText('Licence SYN-0001')).toBeInTheDocument()
    expect(screen.getByText('Suspended (recorded)')).toBeInTheDocument()
    expect(screen.getByText('TICKET-123')).toBeInTheDocument()
    expect(screen.getAllByRole('note', { name: 'Evidence reference note' })[0]).toHaveTextContent('supplied by the operator')
    expect(screen.getAllByRole('note', { name: 'Evidence reference note' })[0]).toHaveTextContent('does not verify it')
    expect(screen.getByRole('link', { name: TEN1 })).toHaveAttribute('href', `/licensing/tenants/${TEN1}`)
    expect(await screen.findByText('REF-1')).toBeInTheDocument()
    expect(seen[0].searchParams.get('licence_id')).toBe(LIC1)
    expect(seen[0].searchParams.get('current')).toBe('true')
    expect(await screen.findByText('Baseline')).toBeInTheDocument()
    // lic31: the ceilings are no longer read-only; the ONLY ceiling control is the explicit filing entry (it files a change for
    // approval, it does not edit a row), plus one per-row entry for the current version
    expect(screen.getAllByRole('button', { name: /ceiling/i })).toHaveLength(1)
    expect(screen.getByRole('button', { name: 'File a country ceiling change' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'File a change for ZZ' })).toBeInTheDocument()
  })
})

async function openLifecycle(user: ReturnType<typeof userEvent.setup>, name: string) {
  await screen.findByText('Licence SYN-0001')
  await user.click(screen.getByRole('button', { name }))
  return screen.findByRole('dialog', { name: 'File a licensing change' })
}

describe('filing a lifecycle change', () => {
  it('a widening status change answers 202: shown as pending approval, never as success; nothing is preselected', async () => {
    signInAs(PA)
    licensingReads()
    let body: unknown
    server.use(http.put(`/v1/admin/licences/${LIC1}/status`, async ({ request }) => { body = await request.json(); return HttpResponse.json(changeResponse(change()), { status: 202 }) }))
    renderWithProviders(<AppRoutes />, { route: `/licensing/licences/${LIC1}` })
    const user = userEvent.setup()
    const dlg = await openLifecycle(user, 'Change licence status')
    expect(within(dlg).getByLabelText('New status')).toHaveValue('')
    expect(within(dlg).getByLabelText('Reason code')).toHaveValue('')
    expect(within(dlg).getByRole('button', { name: 'File change' })).toBeDisabled()
    // approval rules are on screen before filing
    expect(within(dlg).getByRole('region', { name: 'Approval rules' })).toHaveTextContent(/2 approvals from 2 different people/)
    expect(within(dlg).getByRole('region', { name: 'Approval rules' })).toHaveTextContent(/cannot approve or reject their own request/)
    expect(within(dlg).getByRole('region', { name: 'Approval rules' })).toHaveTextContent(/24 hours/)
    expect(within(dlg).getByRole('note', { name: 'Disclaimer' })).toHaveTextContent(DISCLAIMER)
    await user.selectOptions(within(dlg).getByLabelText('New status'), 'active')
    await user.type(within(dlg).getByLabelText('Reason code'), 'bad code!')
    expect(within(dlg).getByRole('button', { name: 'File change' })).toBeDisabled()
    await user.clear(within(dlg).getByLabelText('Reason code'))
    await user.type(within(dlg).getByLabelText('Reason code'), 'reinstated.2026')
    await user.click(within(dlg).getByRole('button', { name: 'File change' }))
    const out = await within(dlg).findByText(/Pending approval\. Nothing has changed yet/)
    expect(out).toHaveTextContent('needs 2 approvals from different people')
    expect(within(dlg).queryByText(/Applied at once/)).toBeNull()
    expect(body).toEqual({ status: 'active', reason_code: 'reinstated.2026' })
    expect(within(dlg).queryByRole('button', { name: 'File change' })).toBeNull()
  })

  it('a narrowing change answers 200 and is shown as applied at once', async () => {
    signInAs(PA)
    licensingReads({ licences: [licence({ status: 'active' })] })
    server.use(http.put(`/v1/admin/licences/${LIC1}/status`, () => HttpResponse.json({ licence: licence({ status: 'suspended' }), change_kind: 'narrowing', change_request_id: CHG1 })))
    renderWithProviders(<AppRoutes />, { route: `/licensing/licences/${LIC1}` })
    const user = userEvent.setup()
    const dlg = await openLifecycle(user, 'Change licence status')
    await user.selectOptions(within(dlg).getByLabelText('New status'), 'suspended')
    await user.type(within(dlg).getByLabelText('Reason code'), 'regulator_notice')
    await user.click(within(dlg).getByRole('button', { name: 'File change' }))
    expect(await within(dlg).findByText(/Applied at once\. This was a narrowing change/)).toBeInTheDocument()
  })

  it('an answer that matches neither shape is "not confirmed", never success', async () => {
    signInAs(PA)
    licensingReads()
    server.use(http.put(`/v1/admin/licences/${LIC1}/review-due`, () => HttpResponse.json({ ok: true })))
    renderWithProviders(<AppRoutes />, { route: `/licensing/licences/${LIC1}` })
    const user = userEvent.setup()
    const dlg = await openLifecycle(user, 'Change licence review-due date')
    await user.type(within(dlg).getByLabelText('Reason code'), 'postponed')
    await user.click(within(dlg).getByRole('button', { name: 'File change' }))
    expect(await within(dlg).findByText(/could not be recognised\. Nothing is confirmed/)).toBeInTheDocument()
    expect(within(dlg).queryByText(/Applied at once/)).toBeNull()
  })

  it('validity dates send both required keys (null clears), prefilled from the current licence', async () => {
    signInAs(PA)
    licensingReads()
    let body: unknown
    server.use(http.put(`/v1/admin/licences/${LIC1}/validity-dates`, async ({ request }) => { body = await request.json(); return HttpResponse.json(changeResponse(change({ kind: 'licence_validity_dates_change' })), { status: 202 }) }))
    renderWithProviders(<AppRoutes />, { route: `/licensing/licences/${LIC1}` })
    const user = userEvent.setup()
    const dlg = await openLifecycle(user, 'Change licence validity dates')
    expect(within(dlg).getByLabelText('Issued on')).toHaveValue('2026-01-02')
    await user.clear(within(dlg).getByLabelText('Expires on'))
    await user.type(within(dlg).getByLabelText('Reason code'), 'cleared')
    await user.click(within(dlg).getByRole('button', { name: 'File change' }))
    await within(dlg).findByText(/Pending approval/)
    expect(body).toEqual({ issued_at: '2026-01-02', expires_at: null, reason_code: 'cleared' })
  })

  it('evidence reference: rejects an email-like value client-side, labels it operator-supplied and unverified, sends the key', async () => {
    signInAs(PA)
    licensingReads()
    let body: unknown
    server.use(http.put(`/v1/admin/licences/${LIC1}/evidence-reference`, async ({ request }) => { body = await request.json(); return HttpResponse.json({ licence: licence(), change_kind: 'neutral' }) }))
    renderWithProviders(<AppRoutes />, { route: `/licensing/licences/${LIC1}` })
    const user = userEvent.setup()
    const dlg = await openLifecycle(user, 'Change licence evidence reference')
    expect(within(dlg).getByRole('note', { name: 'Evidence reference note' })).toHaveTextContent(/supplied by the operator.*does not verify it/)
    const ev = within(dlg).getByLabelText('Evidence reference')
    await user.clear(ev)
    await user.type(ev, 'a@b.example')
    await user.type(within(dlg).getByLabelText('Reason code'), 'ticket_update')
    expect(within(dlg).getByRole('button', { name: 'File change' })).toBeDisabled()
    await user.clear(ev)
    await user.type(ev, 'TICKET-456')
    await user.click(within(dlg).getByRole('button', { name: 'File change' }))
    await within(dlg).findByText(/Applied at once/)
    expect(body).toEqual({ licence_evidence_reference: 'TICKET-456', reason_code: 'ticket_update' })
  })

  it('creates a licence: jurisdiction and licensee are explicit choices; a 202 is pending', async () => {
    signInAs(PA)
    licensingReads()
    let body: unknown
    server.use(http.post('/v1/admin/licences', async ({ request }) => { body = await request.json(); return HttpResponse.json(changeResponse(change({ kind: 'licence_create' })), { status: 202 }) }))
    renderWithProviders(<AppRoutes />, { route: '/licensing/licences' })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'File a licence change' }))
    const dlg = await screen.findByRole('dialog', { name: 'File a licensing change' })
    await user.selectOptions(within(dlg).getByLabelText('Kind of change'), 'create_licence')
    expect(within(dlg).getByLabelText('Jurisdiction')).toHaveValue('')
    expect(within(dlg).getByLabelText('Licensee')).toHaveValue('')
    await user.selectOptions(await within(dlg).findByRole('combobox', { name: 'Jurisdiction' }), 'XX - Testland')
    await user.selectOptions(within(dlg).getByLabelText('Licensee'), 'tenant')
    await user.type(within(dlg).getByLabelText('Licence number'), 'SYN-9')
    await user.type(within(dlg).getByLabelText('Permitted products (optional)'), 'casino, sportsbook')
    await user.type(within(dlg).getByLabelText('Reason code'), 'new_licence')
    await user.click(within(dlg).getByRole('button', { name: 'File change' }))
    await within(dlg).findByText(/Pending approval/)
    expect(body).toEqual({ jurisdiction_id: '10000000-0000-4000-8000-000000000001', licensee: 'tenant', licence_number: 'SYN-9', permitted_products: ['casino', 'sportsbook'], permitted_markets: [], reason_code: 'new_licence' })
  })

  it('a refused filing shows the readable code and never the server message; a duplicate is explained', async () => {
    signInAs(PA)
    licensingReads()
    server.use(http.put(`/v1/admin/licences/${LIC1}/status`, () => refusal(409, 'LICENSING_CHANGE_DUPLICATE_PENDING')))
    renderWithProviders(<AppRoutes />, { route: `/licensing/licences/${LIC1}` })
    const user = userEvent.setup()
    const dlg = await openLifecycle(user, 'Change licence status')
    await user.selectOptions(within(dlg).getByLabelText('New status'), 'active')
    await user.type(within(dlg).getByLabelText('Reason code'), 'again')
    await user.click(within(dlg).getByRole('button', { name: 'File change' }))
    const alert = await within(dlg).findByRole('alert')
    expect(alert).toHaveTextContent('An identical request is already open')
    expect(alert).toHaveTextContent('LICENSING_CHANGE_DUPLICATE_PENDING')
    expect(document.body).not.toHaveTextContent('do-not-render')
  })

  it('runs the step-up flow: first send refused step_up_required, code entered once, the SAME body re-sent', async () => {
    signInAs(PA)
    licensingReads()
    const STEPPED = makeTestJwt({ ...PA, jti: 'stepped-pa' })
    const sends: Array<{ auth: string | null; body: string }> = []
    server.use(
      http.put(`/v1/admin/licences/${LIC1}/status`, async ({ request }) => {
        const rec = { auth: request.headers.get('Authorization'), body: await request.text() }
        sends.push(rec)
        return rec.auth === `Bearer ${STEPPED}` ? HttpResponse.json(changeResponse(change()), { status: 202 }) : stepUpRequiredResponse()
      }),
      http.post('/v1/staff/auth/step-up', () =>
        HttpResponse.json({ access_token: STEPPED, token_type: 'Bearer', expires_in: 900, mfa_at: '2026-10-13T10:00:00Z', step_up_expires_at: { financial: '2026-10-13T10:10:00Z', critical: '2026-10-13T10:05:00Z' } }),
      ),
    )
    renderWithProviders(<AppRoutes />, { route: `/licensing/licences/${LIC1}` })
    const user = userEvent.setup()
    const dlg = await openLifecycle(user, 'Change licence status')
    await user.selectOptions(within(dlg).getByLabelText('New status'), 'active')
    await user.type(within(dlg).getByLabelText('Reason code'), 'reinstated')
    await user.click(within(dlg).getByRole('button', { name: 'File change' }))
    const su = await screen.findByRole('dialog', { name: 'Confirm it is you' })
    await user.type(within(su).getByLabelText('6-digit code'), '123456')
    await user.click(within(su).getByRole('button', { name: 'Verify' }))
    await within(dlg).findByText(/Pending approval/)
    expect(sends).toHaveLength(2)
    expect(sends[1].body).toBe(sends[0].body)
  })
})

describe('tenant licensing page', () => {
  it('shows the binding and read-only configs and policies; binding needs an explicit licence and a 202 is pending', async () => {
    signInAs(PA)
    licensingReads({ licences: [licence({ status: 'active' }), licence({ id: LIC2, licence_number: 'SYN-0002', tenant_ids: [] })] })
    const pol: URL[] = []
    server.use(
      http.get('/v1/admin/operating-country-policies', ({ request }) => { pol.push(new URL(request.url)); return HttpResponse.json(page([])) }),
      http.get(`/v1/admin/tenants/${TEN1}/jurisdiction-configs`, () => HttpResponse.json(page([{ id: 'cfg1', tenant_id: TEN1, jurisdiction_id: '10000000-0000-4000-8000-000000000001', kyc_ruleset_id: 'kyc-a', aml_ruleset_id: null, rg_ruleset_id: null, reporting_ruleset_id: null, allowed_currencies: ['EUR'], allowed_payment_methods: [], geo_block_list: ['ZZ'], effective_from: '2026-01-01T00:00:00Z', effective_to: null }]))),
    )
    let body: unknown
    server.use(http.put(`/v1/admin/tenants/${TEN1}/licence`, async ({ request }) => { body = await request.json(); return HttpResponse.json(changeResponse(change({ kind: 'tenant_licence_bind' })), { status: 202 }) }))
    renderWithProviders(<AppRoutes />, { route: `/licensing/tenants/${TEN1}` })
    const user = userEvent.setup()
    expect(await screen.findByText('Under the platform licence')).toBeInTheDocument()
    expect(await screen.findByText(/Bound to licence/)).toBeInTheDocument()
    expect(await screen.findByText('kyc-a')).toBeInTheDocument()
    expect(pol[0].searchParams.get('tenant_id')).toBe(TEN1)
    // the licensing model is not selectable here
    expect(screen.queryByLabelText(/licensing model/i)).toBeNull()
    await user.click(screen.getByRole('button', { name: 'File a rebind to a licence' }))
    const dlg = await screen.findByRole('dialog', { name: 'File a licensing change' })
    expect(within(dlg).getByLabelText('Licence')).toHaveValue('')
    expect(within(dlg).getByRole('button', { name: 'File change' })).toBeDisabled()
    await user.selectOptions(await within(dlg).findByRole('combobox', { name: 'Licence' }), 'SYN-0002 (platform, suspended)')
    await user.type(within(dlg).getByLabelText('Reason code'), 'bind_ticket')
    await user.click(within(dlg).getByRole('button', { name: 'File change' }))
    await within(dlg).findByText(/Pending approval/)
    expect(body).toEqual({ licence_id: LIC2, reason_code: 'bind_ticket' })
  })

  it('unbind sends licence_id null and an unbind answer is shown as done only for a 200 assignment body', async () => {
    signInAs(PA)
    licensingReads({ licences: [licence({ status: 'active' })] })
    let body: unknown
    server.use(http.put(`/v1/admin/tenants/${TEN1}/licence`, async ({ request }) => { body = await request.json(); return HttpResponse.json({ tenant_id: TEN1, licence_id: null, change_request_id: CHG1 }) }))
    renderWithProviders(<AppRoutes />, { route: `/licensing/tenants/${TEN1}` })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Unbind licence' }))
    const dlg = await screen.findByRole('dialog', { name: 'File a licensing change' })
    await user.type(within(dlg).getByLabelText('Reason code'), 'stop')
    await user.click(within(dlg).getByRole('button', { name: 'File change' }))
    await within(dlg).findByText(/no licence bound/)
    expect(body).toEqual({ licence_id: null, reason_code: 'stop' })
  })

  it('an unbound tenant is shown as unbound and nothing is preselected', async () => {
    signInAs(PA)
    licensingReads({ licences: [licence({ tenant_ids: [] })] })
    renderWithProviders(<AppRoutes />, { route: `/licensing/tenants/${TEN1}` })
    expect(await screen.findByText(/No licence is bound to this tenant/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Unbind licence' })).toBeNull()
  })
})

describe('licensing changes', () => {
  it('lists pending requests by default (status=requested) with approvals x of y', async () => {
    signInAs(PA)
    licensingReads()
    const urls: URL[] = []
    server.use(http.get('/v1/admin/licensing-changes', ({ request }) => { urls.push(new URL(request.url)); return HttpResponse.json({ items: [change()], disclaimer: DISCLAIMER }) }))
    renderWithProviders(<AppRoutes />, { route: '/licensing/changes' })
    const table = await screen.findByRole('table')
    expect(within(table).getByText('Licence status change')).toBeInTheDocument()
    expect(within(table).getByText('Pending approval')).toBeInTheDocument()
    expect(within(table).getByText('0 of 2')).toBeInTheDocument()
    expect(urls[0].searchParams.get('status')).toBe('requested')
    expect(urls[0].searchParams.get('limit')).toBe('200')
    expect(screen.getByRole('region', { name: 'Approval rules' })).toHaveTextContent(/72 hours/)
  })

  it('detail by another requester: approve is allowed, runs step-up, and a non-final approval says still pending', async () => {
    signInAs(PA)
    licensingReads()
    const STEPPED = makeTestJwt({ ...PA, jti: 'stepped-2' })
    const sends: Array<{ auth: string | null; body: string }> = []
    server.use(
      http.post(`/v1/admin/licensing-changes/${CHG1}/approve`, async ({ request }) => {
        const rec = { auth: request.headers.get('Authorization'), body: await request.text() }
        sends.push(rec)
        return rec.auth === `Bearer ${STEPPED}`
          ? HttpResponse.json(changeResponse(change({ approvals: [{ approver_id: ME, reason_code: 'reviewed_ok', decided_at: new Date().toISOString() }] })))
          : stepUpRequiredResponse()
      }),
      http.post('/v1/staff/auth/step-up', () =>
        HttpResponse.json({ access_token: STEPPED, token_type: 'Bearer', expires_in: 900, mfa_at: '2026-10-13T10:00:00Z', step_up_expires_at: { financial: '2026-10-13T10:10:00Z', critical: '2026-10-13T10:05:00Z' } }),
      ),
    )
    renderWithProviders(<AppRoutes />, { route: `/licensing/changes/${CHG1}` })
    const user = userEvent.setup()
    expect(await screen.findByRole('button', { name: 'Approve' })).toBeEnabled()
    expect(screen.getByRole('region', { name: 'Approval rules' })).toBeInTheDocument()
    expect(screen.getByLabelText('Request payload')).toHaveTextContent('"status": "active"')
    await user.click(screen.getByRole('button', { name: 'Approve' }))
    const dlg = await screen.findByRole('dialog', { name: 'Approve licensing change' })
    expect(within(dlg).getByLabelText('Reason code')).toHaveValue('')
    expect(within(dlg).getByRole('button', { name: 'Approve' })).toBeDisabled()
    await user.type(within(dlg).getByLabelText('Reason code'), 'reviewed_ok')
    await user.click(within(dlg).getByRole('button', { name: 'Approve' }))
    const su = await screen.findByRole('dialog', { name: 'Confirm it is you' })
    await user.type(within(su).getByLabelText('6-digit code'), '123456')
    await user.click(within(su).getByRole('button', { name: 'Verify' }))
    expect(await screen.findByText(/Approval recorded: 1 of 2\. Still pending approval/)).toBeInTheDocument()
    expect(sends).toHaveLength(2)
    expect(sends[1].body).toBe(sends[0].body)
    expect(JSON.parse(sends[0].body)).toEqual({ reason_code: 'reviewed_ok' })
  })

  it('a final approval that executed is shown as executed', async () => {
    signInAs(PA)
    licensingReads({ changes: [change({ approvals: [{ approver_id: THIRD, reason_code: 'reviewed_ok', decided_at: new Date().toISOString() }] })] })
    server.use(http.post(`/v1/admin/licensing-changes/${CHG1}/approve`, () => HttpResponse.json(changeResponse(change({ status: 'executed' }), true))))
    renderWithProviders(<AppRoutes />, { route: `/licensing/changes/${CHG1}` })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Approve' }))
    const dlg = await screen.findByRole('dialog', { name: 'Approve licensing change' })
    await user.type(within(dlg).getByLabelText('Reason code'), 'reviewed_ok')
    await user.click(within(dlg).getByRole('button', { name: 'Approve' }))
    expect(await screen.findByText(/Approval recorded and the change was executed/)).toBeInTheDocument()
  })

  it('the requester cannot approve or reject their own request (buttons disabled) and may cancel, with no body', async () => {
    signInAs(PA)
    licensingReads({ changes: [change({ requested_by: ME })] })
    let cancelBody: string | undefined
    let approveCalls = 0
    server.use(
      http.post(`/v1/admin/licensing-changes/${CHG1}/approve`, () => { approveCalls++; return refusal(409, 'LICENSING_CHANGE_SELF_APPROVAL') }),
      http.post(`/v1/admin/licensing-changes/${CHG1}/cancel`, async ({ request }) => { cancelBody = await request.text(); return HttpResponse.json(changeResponse(change({ status: 'cancelled', requested_by: ME }))) }),
    )
    renderWithProviders(<AppRoutes />, { route: `/licensing/changes/${CHG1}` })
    const user = userEvent.setup()
    expect(await screen.findByRole('button', { name: 'Approve' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Reject' })).toBeDisabled()
    expect(screen.getByText(/You filed this request\. A different person must approve it/)).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Cancel request' }))
    const dlg = await screen.findByRole('dialog', { name: 'Cancel licensing change' })
    expect(within(dlg).queryByLabelText('Reason code')).toBeNull()
    await user.click(within(dlg).getByRole('button', { name: 'Cancel request' }))
    expect(await screen.findByText(/Cancelled\. The request is closed/)).toBeInTheDocument()
    expect(cancelBody).toBe('')
    expect(approveCalls).toBe(0)
  })

  it('a person who already approved cannot approve again (hint) and sees why', async () => {
    signInAs(PA)
    licensingReads({ changes: [change({ approvals: [{ approver_id: ME, reason_code: 'reviewed_ok', decided_at: new Date().toISOString() }] })] })
    renderWithProviders(<AppRoutes />, { route: `/licensing/changes/${CHG1}` })
    expect(await screen.findByRole('button', { name: 'Approve' })).toBeDisabled()
    expect(screen.getByText(/You already approved this request\. A second approval must come from a different person/)).toBeInTheDocument()
  })

  it('flags an approval older than 24 hours as not counting', async () => {
    signInAs(PA)
    const old = new Date(Date.now() - 25 * 3600 * 1000).toISOString()
    licensingReads({ changes: [change({ approvals: [{ approver_id: THIRD, reason_code: 'reviewed_ok', decided_at: old }] })] })
    renderWithProviders(<AppRoutes />, { route: `/licensing/changes/${CHG1}` })
    expect(await screen.findByText('older than 24 hours')).toBeInTheDocument()
    expect(screen.getByText(/does not count at execution/)).toBeInTheDocument()
  })

  it.each([
    ['LICENSING_CHANGE_SELF_APPROVAL', 409, /cannot decide a request you filed/],
    ['LICENSING_CHANGE_ALREADY_DECIDED', 409, /already approved this request/],
    ['LICENSING_CHANGE_NOT_PENDING', 409, /no longer open/],
    ['LICENSING_CHANGE_APPROVALS_STALE', 409, /older than 24 hours/],
    ['LICENSING_CHANGE_REFUSED', 409, /refused by the domain rules/],
    ['LICENSING_CHANGE_PROOF_REFUSED', 403, /signed proof/],
    ['LICENSING_CHANGE_NO_PERSON', 403, /not linked to a person record/],
    ['LICENSING_CHANGE_UNAVAILABLE', 503, /not available in this environment/],
  ])('approve refused with %s is explained in fixed words (server message never shown)', async (code, status, text) => {
    signInAs(PA)
    licensingReads()
    server.use(http.post(`/v1/admin/licensing-changes/${CHG1}/approve`, () => refusal(status, code)))
    renderWithProviders(<AppRoutes />, { route: `/licensing/changes/${CHG1}` })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Approve' }))
    const dlg = await screen.findByRole('dialog', { name: 'Approve licensing change' })
    await user.type(within(dlg).getByLabelText('Reason code'), 'reviewed_ok')
    await user.click(within(dlg).getByRole('button', { name: 'Approve' }))
    const alert = await within(dlg).findByRole('alert')
    expect(alert).toHaveTextContent(text)
    expect(alert).toHaveTextContent(code)
    expect(alert).toHaveTextContent('Request ID: req-9')
    expect(document.body).not.toHaveTextContent('do-not-render')
  })

  it('a guard refusal shows the LC0xx codes the server sends, readably', async () => {
    signInAs(PA)
    licensingReads()
    server.use(http.post(`/v1/admin/licensing-changes/${CHG1}/approve`, () => refusal(409, 'LICENSING_CHANGE_GUARD_REFUSED', { check_codes: ['LC024', 'not<b>safe'] })))
    renderWithProviders(<AppRoutes />, { route: `/licensing/changes/${CHG1}` })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Approve' }))
    const dlg = await screen.findByRole('dialog', { name: 'Approve licensing change' })
    await user.type(within(dlg).getByLabelText('Reason code'), 'reviewed_ok')
    await user.click(within(dlg).getByRole('button', { name: 'Approve' }))
    const alert = await within(dlg).findByRole('alert')
    expect(alert).toHaveTextContent('LC024: The change would now widen: file a new request')
    expect(alert).not.toHaveTextContent('not<b>safe')
  })

  it('reject sends the reason code and needs no step-up', async () => {
    signInAs(PA)
    licensingReads()
    let body: unknown
    server.use(http.post(`/v1/admin/licensing-changes/${CHG1}/reject`, async ({ request }) => { body = await request.json(); return HttpResponse.json(changeResponse(change({ status: 'rejected' }))) }))
    renderWithProviders(<AppRoutes />, { route: `/licensing/changes/${CHG1}` })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Reject' }))
    const dlg = await screen.findByRole('dialog', { name: 'Reject licensing change' })
    await user.type(within(dlg).getByLabelText('Reason code'), 'ticket_mismatch')
    await user.click(within(dlg).getByRole('button', { name: 'Reject' }))
    expect(await screen.findByText(/Rejected\. The request is closed/)).toBeInTheDocument()
    expect(body).toEqual({ reason_code: 'ticket_mismatch' })
  })

  it('a decided request offers no decision buttons', async () => {
    signInAs(PA)
    licensingReads({ changes: [change({ status: 'executed', executed_at: '2026-10-19T10:00:00Z' })] })
    renderWithProviders(<AppRoutes />, { route: `/licensing/changes/${CHG1}` })
    await screen.findByText('Executed', { selector: 'span' })
    expect(screen.queryByRole('button', { name: 'Approve' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Reject' })).toBeNull()
  })

  it('the file-a-change dialog on the changes page starts with no kind chosen', async () => {
    signInAs(PA)
    licensingReads()
    renderWithProviders(<AppRoutes />, { route: '/licensing/changes' })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'File a change' }))
    const dlg = await screen.findByRole('dialog', { name: 'File a licensing change' })
    expect(within(dlg).getByLabelText('Kind of change')).toHaveValue('')
    await waitFor(() => expect(within(dlg).queryByLabelText('Reason code')).toBeNull())
  })
})
