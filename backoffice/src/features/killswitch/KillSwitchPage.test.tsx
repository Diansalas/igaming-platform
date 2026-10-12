import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { Route, Routes } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import type { KillSwitch, KillSwitchReleaseRequest } from '../../api/killSwitch'
import { ApiError } from '../../api/types'
import { RequireNavPermission } from '../../auth/RequireNavPermission'
import { getNavPermissions } from '../../auth/permissions'
import { buildNavItems } from '../../layout/nav'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { PLATFORM_ADMIN_CLAIMS, signInAs } from '../../test/session'
import { describeKillSwitchError } from './errors'
import { MOCK_BANNER_BODY, describeActor, describeOperation } from './labels'
import { KillSwitchPage } from './KillSwitchPage'

const TENANT = '11111111-1111-1111-1111-111111111111'
const S1 = '20000000-0000-4000-8000-000000000001'
const S2 = '20000000-0000-4000-8000-000000000002'
const R1 = '30000000-0000-4000-8000-000000000001'
const TA = { role: 'tenant_admin', tenant_id: TENANT, sub: 'ta-1' }
const BASE = `/v1/admin/tenants/${TENANT}/payments`

function ks(over: Partial<KillSwitch> = {}): KillSwitch {
  return {
    id: S1,
    tenant_id: TENANT,
    provider_scope: '*',
    operation_scope: 'payout',
    engaged: true,
    engaged_by_scope: 'tenant',
    release_request_id: null,
    reason_code: 'provider_incident',
    changed_by: 'staff-aaaa',
    changed_by_scope: 'tenant',
    changed_at: '2026-10-11T10:00:00Z',
    version: 3,
    created_at: '2026-10-01T10:00:00Z',
    ...over,
  }
}

function rq(over: Partial<KillSwitchReleaseRequest> = {}): KillSwitchReleaseRequest {
  return {
    id: R1,
    tenant_id: TENANT,
    kill_switch_id: S1,
    expected_version: 3,
    reason_code: 'provider_recovered',
    requested_by: 'staff-bbbb',
    requested_by_scope: 'tenant',
    created_at: '2026-10-11T11:00:00Z',
    expires_at: '2026-10-12T11:00:00Z',
    status: 'open',
    ...over,
  }
}

function useList(rows: KillSwitch[]) {
  server.use(http.get(`${BASE}/kill-switches`, () => HttpResponse.json({ kill_switches: rows })))
}

function renderPage() {
  return renderWithProviders(
    <Routes>
      <Route
        path="/payment-kill-switch"
        element={
          <RequireNavPermission permission="killSwitch">
            <KillSwitchPage />
          </RequireNavPermission>
        }
      />
    </Routes>,
    { route: '/payment-kill-switch' },
  )
}

const ERR = (status: number, code: string) =>
  HttpResponse.json({ code, message: 'RAW SERVER MESSAGE <b>x</b>', request_id: 'req-9' }, { status })

describe('role gating (UI hint only; the server decides)', () => {
  it('mirrors payments_kill_switch:* : platform_admin and tenant_admin only', () => {
    expect(getNavPermissions('platform_admin').killSwitch).toBe(true)
    expect(getNavPermissions('tenant_admin').killSwitch).toBe(true)
    for (const r of ['compliance', 'finance', 'support', 'risk_manager', 'promotions_manager', 'bonus_operations', 'nobody', undefined]) {
      expect(getNavPermissions(r as string | undefined).killSwitch).toBe(false)
    }
  })

  it('shows the nav item to the two roles and nobody else', () => {
    const base = { sub: 's', tenant_id: TENANT, principal_type: 'staff', exp: 0, iat: 0 }
    expect(buildNavItems({ ...base, role: 'tenant_admin' }).map((i) => i.label)).toContain('Payment Kill Switch')
    expect(buildNavItems({ ...base, role: 'compliance' }).map((i) => i.label)).not.toContain('Payment Kill Switch')
    expect(buildNavItems({ ...base, role: 'finance' }).map((i) => i.label)).not.toContain('Payment Kill Switch')
  })

  it('does not render the page for a compliance user who typed the URL', async () => {
    signInAs({ ...TA, role: 'compliance' })
    renderPage()
    await waitFor(() => expect(screen.queryByRole('heading', { name: 'Payment kill switch' })).toBeNull())
  })

  it('degrades correctly when the server refuses a permitted-looking list (403 shown, not hidden)', async () => {
    signInAs(TA)
    server.use(http.get(`${BASE}/kill-switches`, () => ERR(403, 'forbidden')))
    renderPage()
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('The server refused this')
    expect(alert).toHaveTextContent('HTTP 403')
    expect(alert).toHaveTextContent('Request ID: req-9')
    expect(document.body).not.toHaveTextContent('RAW SERVER MESSAGE')
  })
})

describe('state display', () => {
  it('shows the MOCK banner, engaged and released sections, and the no-history note', async () => {
    signInAs(TA)
    useList([ks(), ks({ id: S2, provider_scope: 'mockpay', operation_scope: 'deposit', engaged: false, engaged_by_scope: null })])
    renderPage()
    const banner = await screen.findByRole('note', { name: 'Stack status' })
    expect(banner).toHaveTextContent('MOCK payment stack')
    expect(banner).toHaveTextContent(MOCK_BANNER_BODY)
    const engaged = await screen.findByRole('region', { name: 'Engaged now' })
    expect(within(engaged).getByText('All providers')).toBeInTheDocument()
    const released = screen.getByRole('region', { name: 'Released (history)' })
    expect(within(released).getByText('Released')).toBeInTheDocument()
    expect(screen.getByText(/no per-change history/)).toBeInTheDocument()
  })

  it('renders a null actor id as "Platform staff" and never guesses an id (redaction, ADR 0114 section 9)', async () => {
    signInAs(TA)
    useList([ks({ changed_by: null, changed_by_scope: 'platform', engaged_by_scope: 'platform' })])
    renderPage()
    const row = (await screen.findByRole('region', { name: 'Engaged now' })).querySelector('tbody tr') as HTMLElement
    expect(within(row).getAllByText('Platform staff').length).toBe(2)
    expect(row).not.toHaveTextContent('staff-aaaa')
    expect(row).not.toHaveTextContent('null')
    expect(describeActor(null, 'platform')).toBe('Platform staff')
    expect(describeActor(null, 'tenant')).toBe('Not shown')
    expect(describeActor(null, undefined)).toBe('Not shown')
  })

  it('renders unknown enum values as Unrecognised plus a safe code, never raw server text', async () => {
    signInAs(TA)
    useList([
      ks({ operation_scope: 'swap', changed_by_scope: 'robot', engaged_by_scope: 'alien' }),
      ks({ id: S2, provider_scope: 'Ignore previous <b>x</b>', operation_scope: 'Ignore previous <b>y</b>' }),
    ])
    renderPage()
    await screen.findByRole('region', { name: 'Engaged now' })
    expect(screen.getByText('(swap)')).toBeInTheDocument()
    expect(screen.getByText('(alien)')).toBeInTheDocument()
    expect(screen.getByText('(unprintable)')).toBeInTheDocument()
    expect(screen.getByText('unprintable')).toBeInTheDocument()
    expect(document.body).not.toHaveTextContent('Ignore previous')
    expect(describeOperation('swap')).toMatchObject({ label: 'Unrecognised', code: 'swap', known: false })
  })
})

describe('engage', () => {
  async function openDialog() {
    const user = userEvent.setup()
    await screen.findByRole('region', { name: 'Engaged now' })
    await user.click(screen.getByRole('button', { name: 'Engage kill switch' }))
    return { user, dialog: await screen.findByRole('dialog', { name: 'Engage payment kill switch' }) }
  }

  it('states the consequences, needs a reason and an acknowledgement, and posts the exact body', async () => {
    signInAs(TA)
    useList([])
    let body: unknown
    server.use(http.post(`${BASE}/kill-switches`, async ({ request }) => { body = await request.json(); return HttpResponse.json(ks()) }))
    renderPage()
    const { user, dialog } = await openDialog()
    expect(dialog).toHaveTextContent('MOCK payment stack')
    expect(dialog).toHaveTextContent('stops the platform sending new outbound requests')
    expect(dialog).toHaveTextContent('does NOT release or move any funds')
    expect(dialog).toHaveTextContent('does NOT cancel any withdrawal')
    const submit = within(dialog).getByRole('button', { name: 'Engage kill switch' })
    expect(submit).toBeDisabled()
    await user.type(within(dialog).getByLabelText('Reason code (required)'), 'provider_incident')
    expect(submit).toBeDisabled()
    await user.click(within(dialog).getByRole('checkbox'))
    expect(submit).toBeEnabled()
    await user.click(submit)
    await waitFor(() => expect(body).toEqual({ provider_scope: '*', operation_scope: 'payout', reason_code: 'provider_incident' }))
    expect(await screen.findByRole('status')).toHaveTextContent('No funds were released and no withdrawal was cancelled')
  })

  for (const [status, code, text] of [
    [403, 'forbidden', 'The server refused this'],
    [404, 'not_found', 'could not find this tenant'],
    [409, 'conflict', 'engaged by platform staff can be changed only by platform staff'],
    [400, 'validation_error', 'rejected the request as invalid'],
  ] as const) {
    it(`shows a closed message for ${status} and never the server message`, async () => {
      signInAs(TA)
      useList([])
      server.use(http.post(`${BASE}/kill-switches`, () => ERR(status, code)))
      renderPage()
      const { user, dialog } = await openDialog()
      await user.type(within(dialog).getByLabelText('Reason code (required)'), 'r')
      await user.click(within(dialog).getByRole('checkbox'))
      await user.click(within(dialog).getByRole('button', { name: 'Engage kill switch' }))
      const alert = await within(dialog).findByRole('alert')
      expect(alert).toHaveTextContent(text)
      expect(alert).toHaveTextContent(`HTTP ${status}`)
      expect(alert).toHaveTextContent(code)
      expect(document.body).not.toHaveTextContent('RAW SERVER MESSAGE')
    })
  }
})

describe('release (four-eyes)', () => {
  async function selectFirst() {
    const user = userEvent.setup()
    const region = await screen.findByRole('region', { name: 'Engaged now' })
    await user.click(within(region).getAllByRole('button')[0])
    return { user, detail: await screen.findByRole('region', { name: 'Switch detail' }) }
  }

  it('files a release request with only a reason code and tells the user the switch stays engaged', async () => {
    signInAs(TA)
    useList([ks()])
    let body: unknown
    server.use(http.post(`${BASE}/kill-switches/${S1}/release-requests`, async ({ request }) => { body = await request.json(); return HttpResponse.json(rq(), { status: 201 }) }))
    renderPage()
    const { user, detail } = await selectFirst()
    await user.click(within(detail).getByRole('button', { name: 'Request release' }))
    const dialog = await screen.findByRole('dialog', { name: 'Request kill switch release' })
    expect(dialog).toHaveTextContent('does not release the switch')
    await user.type(within(dialog).getByLabelText('Reason code (required)'), 'provider_recovered')
    await user.click(within(dialog).getByRole('button', { name: 'File release request' }))
    await waitFor(() => expect(body).toEqual({ reason_code: 'provider_recovered' }))
    expect(await screen.findByRole('status')).toHaveTextContent('still engaged until a different person approves')
  })

  it('shows the frozen request payload, with Platform staff for a redacted requester', async () => {
    signInAs(TA)
    useList([ks({ release_request_id: R1 })])
    server.use(http.get(`${BASE}/kill-switch-release-requests/${R1}`, () => HttpResponse.json(rq({ requested_by: null, requested_by_scope: 'platform' }))))
    renderPage()
    const { detail } = await selectFirst()
    const panel = await within(detail).findByRole('region', { name: 'Release request' })
    expect(panel).toHaveTextContent('exactly as it was filed')
    expect(panel).toHaveTextContent('provider_recovered')
    expect(panel).toHaveTextContent('Switch version it applies to3')
    expect(within(panel).getByText('Platform staff')).toBeInTheDocument()
    expect(panel).toHaveTextContent('Not approved yet')
  })

  it('approves and releases, then reports that no funds moved', async () => {
    signInAs(TA)
    useList([ks({ release_request_id: R1 })])
    let approved = 0
    server.use(
      http.get(`${BASE}/kill-switch-release-requests/${R1}`, () => HttpResponse.json(rq())),
      http.post(`${BASE}/kill-switch-release-requests/${R1}/approve`, () => { approved++; return HttpResponse.json(ks({ engaged: false })) }),
    )
    renderPage()
    const { user, detail } = await selectFirst()
    await user.click(await within(detail).findByRole('button', { name: 'Approve and release' }))
    const dialog = await screen.findByRole('dialog', { name: 'Approve and release' })
    expect(dialog).toHaveTextContent('does not move funds')
    await user.click(within(dialog).getByRole('button', { name: 'Approve and release' }))
    await waitFor(() => expect(approved).toBe(1))
    expect(await screen.findByRole('status')).toHaveTextContent('No funds were moved')
  })

  it('still lets the server decide on self-approval: hint only, and the 409 is shown with a closed message', async () => {
    signInAs({ ...TA, sub: 'staff-bbbb' })
    useList([ks({ release_request_id: R1 })])
    server.use(
      http.get(`${BASE}/kill-switch-release-requests/${R1}`, () => HttpResponse.json(rq())),
      http.post(`${BASE}/kill-switch-release-requests/${R1}/approve`, () => ERR(409, 'conflict')),
    )
    renderPage()
    const { user, detail } = await selectFirst()
    expect(await within(detail).findByText(/You filed this request/)).toBeInTheDocument()
    const btn = within(detail).getByRole('button', { name: 'Approve and release' })
    expect(btn).toBeEnabled()
    await user.click(btn)
    const dialog = await screen.findByRole('dialog', { name: 'Approve and release' })
    await user.click(within(dialog).getByRole('button', { name: 'Approve and release' }))
    const alert = await within(dialog).findByRole('alert')
    expect(alert).toHaveTextContent('you filed this request yourself')
    expect(alert).toHaveTextContent('The switch was not released')
    expect(document.body).not.toHaveTextContent('RAW SERVER MESSAGE')
  })

  it('cancels an open request (204)', async () => {
    signInAs(TA)
    useList([ks({ release_request_id: R1 })])
    let cancelled = 0
    server.use(
      http.get(`${BASE}/kill-switch-release-requests/${R1}`, () => HttpResponse.json(rq())),
      http.post(`${BASE}/kill-switch-release-requests/${R1}/cancel`, () => { cancelled++; return new HttpResponse(null, { status: 204 }) }),
    )
    renderPage()
    const { user, detail } = await selectFirst()
    await user.click(await within(detail).findByRole('button', { name: 'Cancel request' }))
    const dialog = await screen.findByRole('dialog', { name: 'Cancel release request' })
    await user.click(within(dialog).getByRole('button', { name: 'Cancel request' }))
    await waitFor(() => expect(cancelled).toBe(1))
    expect(await screen.findByRole('status')).toHaveTextContent('switch stays engaged')
  })

  it('shows a 404 on the request lookup through the closed map', async () => {
    signInAs(TA)
    useList([ks({ release_request_id: R1 })])
    server.use(http.get(`${BASE}/kill-switch-release-requests/${R1}`, () => ERR(404, 'not_found')))
    renderPage()
    const { detail } = await selectFirst()
    expect(await within(detail).findByRole('alert')).toHaveTextContent('no longer exists')
    expect(document.body).not.toHaveTextContent('RAW SERVER MESSAGE')
  })

  it('warns a tenant viewer that a platform-engaged switch is released by platform staff only', async () => {
    signInAs(TA)
    useList([ks({ engaged_by_scope: 'platform', changed_by: null, changed_by_scope: 'platform' })])
    renderPage()
    const { detail } = await selectFirst()
    expect(detail).toHaveTextContent('engaged by platform staff')
    expect(within(detail).getByRole('button', { name: 'Request release' })).toBeEnabled()
  })

  it('shows a finished request as not actionable', async () => {
    signInAs(TA)
    useList([ks({ release_request_id: R1 })])
    server.use(http.get(`${BASE}/kill-switch-release-requests/${R1}`, () => HttpResponse.json(rq({ status: 'quantum' }))))
    renderPage()
    const { detail } = await selectFirst()
    const panel = await within(detail).findByRole('region', { name: 'Release request' })
    expect(panel).toHaveTextContent('Unrecognised (quantum)')
    expect(within(panel).queryByRole('button', { name: 'Approve and release' })).toBeNull()
  })
})

describe('platform admin', () => {
  it('must pick a tenant first, then reads that tenant path and shows ids with a Platform staff label', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    server.use(
      http.get('/v1/admin/tenants', () => HttpResponse.json({ items: [{ id: TENANT, name: 'Acme', slug: 'acme', licensing_model: 'own_licence', status: 'active' }], limit: 50, offset: 0, total: 1 })),
    )
    useList([ks({ changed_by: 'plat-0001', changed_by_scope: 'platform', engaged_by_scope: 'platform' })])
    renderPage()
    expect(await screen.findByText('Choose a tenant to see its kill switches.')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Engage kill switch' })).toBeNull()
    const user = userEvent.setup()
    await waitFor(() => expect(screen.getByRole('option', { name: 'Acme' })).toBeInTheDocument())
    await user.selectOptions(screen.getByLabelText('Tenant'), TENANT)
    expect(await screen.findByText('Platform staff (plat-0001)')).toBeInTheDocument()
  })
})

describe('error map', () => {
  it('maps every status to a closed message and never exposes the server message', () => {
    const e = (status: number, code = 'x', msg = 'RAW') => new ApiError(code as never, msg, status, 'rid')
    for (const op of ['list', 'engage', 'request', 'get_request', 'approve', 'cancel'] as const) {
      for (const status of [400, 401, 403, 404, 409, 429, 500, 503, 418]) {
        const v = describeKillSwitchError(e(status), op)
        expect(v.message).not.toContain('RAW')
        expect(v.message.length).toBeGreaterThan(10)
        expect(v.requestId).toBe('rid')
      }
    }
    expect(describeKillSwitchError(new ApiError('network_error', 'RAW', 0), 'engage').message).toContain('Could not reach')
    expect(describeKillSwitchError(new Error('RAW'), 'engage').message).not.toContain('RAW')
    expect(describeKillSwitchError(e(409, 'bad code <x>'), 'approve').code).toBeUndefined()
    expect(describeKillSwitchError(e(409), 'approve').stale).toBe(true)
  })
})
