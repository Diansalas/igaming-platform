import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { Route, Routes } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import type { KillSwitch, KillSwitchReleaseRequest } from '../../api/killSwitch'
import { ApiError } from '../../api/types'
import { AppRoutes } from '../../app/AppRoutes'
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

/**
 * Server-reachable switch rows ONLY (ADR 0114 S6b). The database fills `release_request_id` at RELEASE time only
 * (migration 0106 forces NULL on insert and engage), so an ENGAGED switch always has release_request_id null; a
 * pending request is `open_release_request_id`. The fixture refuses to build the shape the server can never produce.
 */
function ks(over: Partial<KillSwitch> = {}): KillSwitch {
  const row: KillSwitch = {
    id: S1,
    tenant_id: TENANT,
    provider_scope: '*',
    operation_scope: 'payout',
    engaged: true,
    engaged_by_scope: 'tenant',
    release_request_id: null,
    open_release_request_id: null,
    reason_code: 'provider_incident',
    changed_by: 'staff-aaaa',
    changed_by_scope: 'tenant',
    changed_at: '2026-10-11T10:00:00Z',
    version: 3,
    created_at: '2026-10-01T10:00:00Z',
    ...over,
  }
  // Rules the database really produces (migrations 0105/0106 payment_kill_switches_guard):
  //  engaged  => engaged_by_scope set, release_request_id null (forced NULL on insert and on every engage)
  //  released => engaged_by_scope null, release_request_id = the approved request, no open request
  if (row.engaged && row.release_request_id) throw new Error('fixture: an engaged switch never has release_request_id set')
  if (row.engaged && !row.engaged_by_scope) throw new Error('fixture: an engaged switch always has engaged_by_scope')
  if (!row.engaged && !row.release_request_id) throw new Error('fixture: a released switch always has the approved release_request_id')
  if (!row.engaged && row.engaged_by_scope) throw new Error('fixture: a released switch has engaged_by_scope null')
  if (!row.engaged && row.open_release_request_id) throw new Error('fixture: a released switch has no open request')
  return row
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

  // Rendered through the real AppRoutes, so removing the RequireNavPermission gate from AppRoutes.tsx fails these.
  it('shows NotAuthorized, and makes no kill-switch request, to a compliance user who typed the URL', async () => {
    signInAs({ ...TA, role: 'compliance' })
    let calls = 0
    server.use(http.get(`${BASE}/kill-switches`, () => { calls++; return HttpResponse.json({ kill_switches: [] }) }))
    renderWithProviders(<AppRoutes />, { route: '/payment-kill-switch' })
    expect(await screen.findByText('Your role does not have access to this section.')).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Payment kill switch' })).toBeNull()
    expect(screen.queryByRole('note', { name: 'Stack status' })).toBeNull()
    expect(calls).toBe(0)
  })

  it('renders the page through AppRoutes for a tenant_admin', async () => {
    signInAs(TA)
    useList([])
    renderWithProviders(<AppRoutes />, { route: '/payment-kill-switch' })
    expect(await screen.findByRole('heading', { name: 'Payment kill switch' })).toBeInTheDocument()
    expect(screen.queryByText('Your role does not have access to this section.')).toBeNull()
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
    useList([ks(), ks({ id: S2, provider_scope: 'mockpay', operation_scope: 'deposit', engaged: false, engaged_by_scope: null, release_request_id: R1 })])
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
    server.use(
      http.post(`${BASE}/kill-switches/${S1}/release-requests`, async ({ request }) => { body = await request.json(); return HttpResponse.json(rq(), { status: 201 }) }),
      http.get(`${BASE}/kill-switch-release-requests/${R1}`, () => HttpResponse.json(rq())),
    )
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
    useList([ks({ open_release_request_id: R1 })])
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
    useList([ks({ open_release_request_id: R1 })])
    let approved = 0
    server.use(
      http.get(`${BASE}/kill-switch-release-requests/${R1}`, () => HttpResponse.json(rq())),
      http.post(`${BASE}/kill-switch-release-requests/${R1}/approve`, () => { approved++; return HttpResponse.json(ks({ engaged: false, engaged_by_scope: null, release_request_id: R1 })) }),
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
    useList([ks({ open_release_request_id: R1 })])
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
    useList([ks({ open_release_request_id: R1 })])
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
    useList([ks({ open_release_request_id: R1 })])
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
    useList([ks({ open_release_request_id: R1 })])
    server.use(http.get(`${BASE}/kill-switch-release-requests/${R1}`, () => HttpResponse.json(rq({ status: 'quantum' }))))
    renderPage()
    const { detail } = await selectFirst()
    const panel = await within(detail).findByRole('region', { name: 'Release request' })
    expect(panel).toHaveTextContent('Unrecognised (quantum)')
    expect(within(panel).queryByRole('button', { name: 'Approve and release' })).toBeNull()
  })
})

// The REAL server behaviour (ADR 0114 S6b): filing a request does not touch release_request_id; the pending
// request is reported as open_release_request_id; the DB allows one open request per switch.
describe('release flow against server-shaped state (S6b)', () => {
  const ALREADY_OPEN = 'KILL_SWITCH_RELEASE_REQUEST_ALREADY_OPEN'

  /** The viewer as the server sees it: the `sub` of the bearer token on THIS request. */
  function subOf(request: Request): string {
    const tok = (request.headers.get('Authorization') ?? '').replace('Bearer ', '')
    return JSON.parse(atob(tok.split('.')[1].replace(/-/g, '+').replace(/_/g, '/'))).sub as string
  }

  function statefulServer(opts: { reportOpen?: boolean } = {}) {
    const reportOpen = opts.reportOpen ?? true
    const st = { sw: ks(), req: null as KillSwitchReleaseRequest | null, lists: 0, approves: 0, cancels: 0 }
    server.use(
      http.get(`${BASE}/kill-switches`, () => {
        st.lists++
        return HttpResponse.json({ kill_switches: [reportOpen ? st.sw : { ...st.sw, open_release_request_id: null }] })
      }),
      http.post(`${BASE}/kill-switches/${S1}/release-requests`, async ({ request }) => {
        if (st.req?.status === 'open') return ERR(409, ALREADY_OPEN)
        const body = (await request.json()) as { reason_code: string }
        st.req = rq({ reason_code: body.reason_code, expected_version: st.sw.version, requested_by: subOf(request) })
        st.sw = ks({ ...st.sw, open_release_request_id: st.req.id }) // release_request_id stays null: the DB fills it at release only
        return HttpResponse.json(st.req, { status: 201 })
      }),
      http.get(`${BASE}/kill-switch-release-requests/:rid`, ({ params }) =>
        st.req && st.req.id === params.rid ? HttpResponse.json(st.req) : ERR(404, 'not_found'),
      ),
      http.post(`${BASE}/kill-switch-release-requests/:rid/approve`, ({ request }) => {
        if ((st.req as KillSwitchReleaseRequest).requested_by === subOf(request)) return ERR(409, 'conflict') // four-eyes, decided by the server
        st.approves++
        st.req = { ...(st.req as KillSwitchReleaseRequest), status: 'approved', approved_by: subOf(request), approved_by_scope: 'tenant' }
        st.sw = ks({ ...st.sw, engaged: false, engaged_by_scope: null, open_release_request_id: null, release_request_id: st.req.id, version: st.sw.version + 1 })
        return HttpResponse.json(st.sw)
      }),
      http.post(`${BASE}/kill-switch-release-requests/:rid/cancel`, () => {
        st.cancels++
        st.req = { ...(st.req as KillSwitchReleaseRequest), status: 'cancelled' }
        st.sw = ks({ ...st.sw, open_release_request_id: null })
        return new HttpResponse(null, { status: 204 })
      }),
    )
    return st
  }

  async function open() {
    const user = userEvent.setup()
    const region = await screen.findByRole('region', { name: 'Engaged now' })
    await user.click(within(region).getAllByRole('button')[0])
    return { user, detail: await screen.findByRole('region', { name: 'Switch detail' }) }
  }

  async function file(user: ReturnType<typeof userEvent.setup>, detail: HTMLElement, reason = 'provider_recovered') {
    await user.click(within(detail).getByRole('button', { name: 'Request release' }))
    const dialog = await screen.findByRole('dialog', { name: 'Request kill switch release' })
    await user.type(within(dialog).getByLabelText('Reason code (required)'), reason)
    await user.click(within(dialog).getByRole('button', { name: 'File release request' }))
  }

  it('engaged switch with no request offers Request release; after filing, the open request, Approve and Cancel appear', async () => {
    signInAs(TA)
    const st = statefulServer()
    renderPage()
    const { user, detail } = await open()
    expect(detail).toHaveTextContent('No release request is open')
    await file(user, detail)
    const panel = await within(detail).findByRole('region', { name: 'Release request' })
    expect(panel).toHaveTextContent('provider_recovered')
    expect(within(panel).getByRole('button', { name: 'Approve and release' })).toBeEnabled()
    expect(within(panel).getByRole('button', { name: 'Cancel request' })).toBeEnabled()
    expect(detail).not.toHaveTextContent('No release request is open')
    expect(st.sw.release_request_id).toBeNull() // the premise: the server never fills it while engaged
    // the table marks the switch as release-requested from open_release_request_id
    expect(within(screen.getByRole('region', { name: 'Engaged now' })).getByText('release requested')).toBeInTheDocument()
  })

  it('keeps the id from the 201: the panel shows the request even if the next list read does not report it yet', async () => {
    signInAs(TA)
    statefulServer({ reportOpen: false })
    renderPage()
    const { user, detail } = await open()
    await file(user, detail)
    const panel = await within(detail).findByRole('region', { name: 'Release request' })
    expect(within(panel).getByRole('button', { name: 'Approve and release' })).toBeEnabled()
    expect(detail).not.toHaveTextContent('No release request is open')
  })

  it('approve releases the switch and the panel reports it; cancel closes the request and offers a new one', async () => {
    signInAs(TA)
    const st = statefulServer()
    renderPage()
    const { user, detail } = await open()
    await file(user, detail)
    await user.click(await within(detail).findByRole('button', { name: 'Cancel request' }))
    let dialog = await screen.findByRole('dialog', { name: 'Cancel release request' })
    await user.click(within(dialog).getByRole('button', { name: 'Cancel request' }))
    await waitFor(() => expect(st.cancels).toBe(1))
    await waitFor(() => expect(detail).toHaveTextContent('No release request is open'))
    await file(user, detail, 'second_try')
    // the filer approving their own request is refused by the (fake) server, which decides from the token
    await user.click(await within(detail).findByRole('button', { name: 'Approve and release' }))
    dialog = await screen.findByRole('dialog', { name: 'Approve and release' })
    await user.click(within(dialog).getByRole('button', { name: 'Approve and release' }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('you filed this request yourself')
    expect(st.approves).toBe(0)
    await user.click(within(dialog).getByRole('button', { name: 'Back' }))
    // a SECOND user approves
    signInAs({ ...TA, sub: 'ta-2' })
    await user.click(await within(detail).findByRole('button', { name: 'Approve and release' }))
    dialog = await screen.findByRole('dialog', { name: 'Approve and release' })
    await user.click(within(dialog).getByRole('button', { name: 'Approve and release' }))
    await waitFor(() => expect(st.approves).toBe(1))
    expect(st.req?.approved_by).toBe('ta-2')
    expect(st.req?.requested_by).toBe('ta-1')
    expect(await within(detail).findByText(/This switch is released/)).toBeInTheDocument()
  })

  it('a duplicate filing is the stable 409: closed message, the list is refreshed, the open request becomes visible', async () => {
    signInAs(TA)
    const st = statefulServer()
    renderPage()
    const { user, detail } = await open()
    // someone else filed meanwhile: the server has an open request this tab has not read yet
    st.req = rq({ requested_by: 'someone-else' })
    await file(user, detail, 'mine')
    const dialog = await screen.findByRole('dialog', { name: 'Request kill switch release' })
    const alert = await within(dialog).findByRole('alert')
    expect(alert).toHaveTextContent('A release request is already open for this switch')
    expect(alert).toHaveTextContent('HTTP 409')
    expect(alert).toHaveTextContent(ALREADY_OPEN)
    expect(document.body).not.toHaveTextContent('RAW SERVER MESSAGE')
    await waitFor(() => expect(st.lists).toBeGreaterThan(1)) // stale refusal refreshes
  })

  it('returns to "Request release" when the request id kept from the 201 is cancelled elsewhere (colleague or platform takeover)', async () => {
    signInAs(TA)
    const st = statefulServer()
    renderPage()
    const { user, detail } = await open()
    await file(user, detail)
    await within(detail).findByRole('region', { name: 'Release request' })
    // elsewhere: a platform takeover cancelled the request; the list now reports no open request
    st.req = { ...(st.req as KillSwitchReleaseRequest), status: 'cancelled' }
    st.sw = ks({ ...st.sw, open_release_request_id: null, engaged_by_scope: 'platform', version: st.sw.version + 1 })
    await user.click(screen.getByRole('button', { name: 'Refresh' }))
    await waitFor(() => expect(detail).toHaveTextContent('No release request is open'))
    expect(within(detail).queryByRole('region', { name: 'Release request' })).toBeNull()
    expect(within(detail).getByRole('button', { name: 'Request release' })).toBeInTheDocument()
  })

  it('does not offer a tenant viewer enabled Approve/Cancel on a platform-filed request (hint; the server also refuses)', async () => {
    signInAs(TA)
    useList([ks({ open_release_request_id: R1 })])
    server.use(http.get(`${BASE}/kill-switch-release-requests/${R1}`, () => HttpResponse.json(rq({ requested_by: null, requested_by_scope: 'platform' }))))
    renderPage()
    const { detail } = await open()
    const panel = await within(detail).findByRole('region', { name: 'Release request' })
    expect(panel).toHaveTextContent('Platform staff filed this request')
    expect(within(panel).getByRole('button', { name: 'Approve and release' })).toBeDisabled()
    expect(within(panel).getByRole('button', { name: 'Cancel request' })).toBeDisabled()
  })

  it('shows an expired-but-open request as blocking, with cancel as the way out', async () => {
    signInAs(TA)
    useList([ks({ open_release_request_id: R1 })])
    server.use(http.get(`${BASE}/kill-switch-release-requests/${R1}`, () => HttpResponse.json(rq({ expires_at: '2020-01-01T00:00:00Z' }))))
    renderPage()
    const { detail } = await open()
    expect(await within(detail).findByText(/has expired but is still open/)).toBeInTheDocument()
    expect(within(detail).getByRole('button', { name: 'Cancel request' })).toBeEnabled()
  })

  describe('engage dialog notices and the byte limit', () => {
    async function openEngage() {
      const user = userEvent.setup()
      await screen.findByRole('region', { name: 'Engaged now' })
      await user.click(screen.getByRole('button', { name: 'Engage kill switch' }))
      return { user, dialog: await screen.findByRole('dialog', { name: 'Engage payment kill switch' }) }
    }

    it('tells a tenant viewer that re-engaging overwrites the reason, bumps the version and leaves an open request stale, and about a platform takeover', async () => {
      signInAs(TA)
      useList([])
      renderPage()
      const { dialog } = await openEngage()
      const note = within(dialog).getByRole('note', { name: 'Re-engage and takeover' })
      expect(note).toHaveTextContent('overwrites the stored reason code and raises the switch version')
      expect(note).toHaveTextContent('does not cancel an open release request')
      expect(note).toHaveTextContent('refuse to approve it')
      expect(note).toHaveTextContent('platform engage over a tenant-engaged switch, which is a takeover and does cancel it')
      expect(note).toHaveTextContent('If platform staff engage a switch that you engaged')
      expect(note).toHaveTextContent('any open release request is cancelled')
    })

    it('tells platform staff that engaging a tenant-engaged switch takes it over and cancels the open tenant request', async () => {
      signInAs(PLATFORM_ADMIN_CLAIMS)
      server.use(
        http.get('/v1/admin/tenants', () => HttpResponse.json({ items: [{ id: TENANT, name: 'Acme', slug: 'acme', licensing_model: 'own_licence', status: 'active' }], limit: 50, offset: 0, total: 1 })),
      )
      useList([])
      renderPage()
      const user = userEvent.setup()
      await waitFor(() => expect(screen.getByRole('option', { name: 'Acme' })).toBeInTheDocument())
      await user.selectOptions(screen.getByLabelText('Tenant'), TENANT)
      await screen.findByRole('region', { name: 'Engaged now' })
      await user.click(screen.getByRole('button', { name: 'Engage kill switch' }))
      const dialog = await screen.findByRole('dialog', { name: 'Engage payment kill switch' })
      const note = within(dialog).getByRole('note', { name: 'Re-engage and takeover' })
      expect(note).toHaveTextContent('takes it over')
      expect(note).toHaveTextContent('any open tenant release request is cancelled')
      expect(note).not.toHaveTextContent('If platform staff engage a switch that you engaged')
    })

    it('measures the reason code in BYTES (TextEncoder), not characters', async () => {
      signInAs(TA)
      useList([])
      renderPage()
      const { user, dialog } = await openEngage()
      const reason = within(dialog).getByLabelText('Reason code (required)')
      const submit = within(dialog).getByRole('button', { name: 'Engage kill switch' })
      await user.click(within(dialog).getByRole('checkbox'))
      await user.type(reason, 'é'.repeat(33)) // 33 characters, 66 bytes
      expect(submit).toBeDisabled()
      expect(within(dialog).getByRole('alert')).toHaveTextContent('66 bytes')
      await user.clear(reason)
      await user.type(reason, 'é'.repeat(32)) // 32 characters, 64 bytes
      expect(within(dialog).queryByRole('alert')).toBeNull()
      expect(submit).toBeEnabled()
      await user.clear(reason)
      await user.type(reason, 'a'.repeat(65))
      expect(submit).toBeDisabled()
    })

    it('the release-request dialog applies the same byte limit', async () => {
      signInAs(TA)
      statefulServer()
      renderPage()
      const { user, detail } = await open()
      await user.click(within(detail).getByRole('button', { name: 'Request release' }))
      const dialog = await screen.findByRole('dialog', { name: 'Request kill switch release' })
      const reason = within(dialog).getByLabelText('Reason code (required)')
      await user.type(reason, 'é'.repeat(33))
      expect(within(dialog).getByRole('button', { name: 'File release request' })).toBeDisabled()
      await user.clear(reason)
      await user.type(reason, 'é'.repeat(32))
      expect(within(dialog).getByRole('button', { name: 'File release request' })).toBeEnabled()
    })
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
