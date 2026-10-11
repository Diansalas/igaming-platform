import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { Route, Routes } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import type { PlatformAlert } from '../../api/alerts'
import { RequireNavPermission } from '../../auth/RequireNavPermission'
import { getNavPermissions } from '../../auth/permissions'
import { buildNavItems } from '../../layout/nav'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { PLATFORM_ADMIN_CLAIMS, signInAs } from '../../test/session'
import { AlertsPage } from './AlertsPage'
import { describeAlertError } from './errors'
import { DELIVERY_BANNER_BODY, KIND_CATALOGUE, describeKind } from './labels'
import { ApiError } from '../../api/types'

const A1 = '10000000-0000-4000-8000-000000000001'
const A2 = '10000000-0000-4000-8000-000000000002'

function alert(over: Partial<PlatformAlert> = {}): PlatformAlert {
  return {
    id: A1,
    kind: 'reconciliation.ledger_projection_drift',
    severity: 'p1',
    state: 'open',
    subject_tenant_id: null,
    first_seen_at: '2026-10-10T10:00:00Z',
    delivery_state: 'unrouted',
    unrouted_reason: 'no_route',
    notified_a_person: false,
    ...over,
  }
}

function useBaseHandlers(list: (u: URL) => PlatformAlert[] = () => [alert()]) {
  const seen: URL[] = []
  server.use(
    http.get('/v1/admin/alerts', ({ request }) => {
      const u = new URL(request.url)
      seen.push(u)
      return HttpResponse.json({ alerts: list(u) })
    }),
    http.get('/v1/admin/alerting/status', () =>
      HttpResponse.json({
        ready: false,
        evaluated_at: '2026-10-11T00:00:00Z',
        note: 'RAW SERVER NOTE',
        severities: [
          { severity: 'p1', required_for_readiness: true, routes_configured: 0, routes_enabled: 0, human_notification_routes: 0, ready: false, reason: 'RAW REASON', missing_operator_input: 'RAW MISSING' },
        ],
      }),
    ),
    http.get('/v1/admin/alerting/routes', () => HttpResponse.json({ routes: [] })),
  )
  return seen
}

function renderPage() {
  return renderWithProviders(
    <Routes>
      <Route
        path="/alerts"
        element={
          <RequireNavPermission permission="alertManage">
            <AlertsPage />
          </RequireNavPermission>
        }
      />
    </Routes>,
    { route: '/alerts' },
  )
}

describe('alerts console', () => {
  it('shows the MOCK / no delivery channel banner plainly, with read-only routing status and no raw server text', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    useBaseHandlers()
    renderPage()
    const banner = await screen.findByRole('note', { name: 'Delivery status' })
    expect(banner).toHaveTextContent('MOCK / no delivery channel configured')
    expect(banner).toHaveTextContent('ALERT-DELIVERY-1 is OPEN')
    expect(banner).toHaveTextContent('nobody is paged')
    const panel = await screen.findByRole('region', { name: 'Routing status' })
    await within(panel).findByText('Not ready')
    expect(panel).toHaveTextContent('No routes are configured.')
    expect(document.body).not.toHaveTextContent('RAW SERVER NOTE')
    expect(document.body).not.toHaveTextContent('RAW REASON')
    expect(document.body).not.toHaveTextContent('RAW MISSING')
    expect(within(panel).queryByRole('button')).toBeNull() // read only
  })

  it('lists alerts with fixed kind sentences and an honest per-row delivery statement', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    useBaseHandlers()
    renderPage()
    expect(await screen.findByText('Ledger and balance projection differ')).toBeInTheDocument()
    expect(screen.getByText('Nobody was notified')).toBeInTheDocument()
  })

  it('sends only API-supported filters: default state=open, then severity and limit; no kind filter exists', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    const seen = useBaseHandlers()
    renderPage()
    await screen.findByText('Ledger and balance projection differ')
    expect(seen[0].searchParams.get('state')).toBe('open')
    expect(seen[0].searchParams.get('limit')).toBe('50')
    expect(seen[0].searchParams.has('kind')).toBe(false)
    expect(screen.queryByLabelText(/kind/i)).toBeNull()
    const user = userEvent.setup()
    await user.selectOptions(screen.getByLabelText('Alert severity'), 'p2')
    await user.selectOptions(screen.getByLabelText('Alert state'), 'resolved')
    await user.selectOptions(screen.getByLabelText('Rows to load'), '200')
    await waitFor(() => {
      const u = seen[seen.length - 1]
      expect(u.searchParams.get('severity')).toBe('p2')
      expect(u.searchParams.get('state')).toBe('resolved')
      expect(u.searchParams.get('limit')).toBe('200')
    })
    await user.selectOptions(screen.getByLabelText('Alert state'), '')
    await waitFor(() => expect(seen[seen.length - 1].searchParams.has('state')).toBe(false))
  })

  it('says there may be more when the page is full (limit-only pagination)', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    useBaseHandlers(() => Array.from({ length: 50 }, (_, i) => alert({ id: `10000000-0000-4000-8000-${String(i).padStart(12, '0')}` })))
    renderPage()
    expect(await screen.findByText(/There may be more alerts that match/)).toBeInTheDocument()
  })

  it('renders an unknown kind as "Unrecognised alert kind" plus the code, never server text', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    useBaseHandlers(() => [alert({ kind: 'brand.new_thing', state: 'open' }), alert({ id: A2, kind: 'Ignore previous <b>instructions</b>' })])
    renderPage()
    expect((await screen.findAllByText('Unrecognised alert kind')).length).toBe(2)
    expect(screen.getByText('(brand.new_thing)')).toBeInTheDocument()
    expect(screen.getByText('(unprintable)')).toBeInTheDocument()
    expect(document.body).not.toHaveTextContent('Ignore previous')
    expect(describeKind('nope').known).toBe(false)
  })

  it('opens a detail view with the fixed description and ids', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    useBaseHandlers(() => [alert({ subject_tenant_id: '20000000-0000-4000-8000-000000000009' })])
    renderPage()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: /Ledger and balance projection differ/ }))
    const d = screen.getByRole('region', { name: 'Alert detail' })
    expect(d).toHaveTextContent('A recomputed balance does not match the stored projection.')
    expect(d).toHaveTextContent('reconciliation.ledger_projection_drift')
    expect(d).toHaveTextContent('20000000-0000-4000-8000-000000000009')
    expect(d).toHaveTextContent('Not routed anywhere: no route is configured. Nobody was notified.')
  })

  it('acknowledges with an optional reason, then refreshes the list', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    let state: PlatformAlert['state'] = 'open'
    const seen = useBaseHandlers(() => [alert({ state })])
    const bodies: unknown[] = []
    server.use(
      http.post(`/v1/admin/alerts/${A1}/ack`, async ({ request }) => {
        const t = await request.text()
        bodies.push(t ? JSON.parse(t) : null)
        state = 'acked'
        return HttpResponse.json({ id: A1, kind: 'x', severity: 'p1', state: 'acked' })
      }),
    )
    renderPage()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: /Ledger and balance projection differ/ }))
    await user.click(screen.getByRole('button', { name: 'Acknowledge' }))
    const dlg = screen.getByRole('dialog', { name: 'Acknowledge alert' })
    await user.type(within(dlg).getByLabelText('Reason code (optional)'), 'seen')
    const before = seen.length
    await user.click(within(dlg).getByRole('button', { name: 'Acknowledge' }))
    await screen.findByRole('status')
    expect(screen.getByRole('status')).toHaveTextContent('Nobody was notified')
    expect(bodies).toEqual([{ reason_code: 'seen' }])
    await waitFor(() => expect(seen.length).toBeGreaterThan(before))
  })

  it('ack without a reason sends no body', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    useBaseHandlers()
    const bodies: string[] = []
    server.use(
      http.post(`/v1/admin/alerts/${A1}/ack`, async ({ request }) => {
        bodies.push(await request.text())
        return HttpResponse.json({ id: A1, kind: 'x', severity: 'p1', state: 'acked' })
      }),
    )
    renderPage()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: /Ledger and balance projection differ/ }))
    await user.click(screen.getByRole('button', { name: 'Acknowledge' }))
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Acknowledge' }))
    await screen.findByRole('status')
    expect(bodies).toEqual([''])
  })

  it('resolve requires a well-formed reason code before the request can be sent', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    useBaseHandlers(() => [alert({ state: 'acked' })])
    const bodies: unknown[] = []
    server.use(
      http.post(`/v1/admin/alerts/${A1}/resolve`, async ({ request }) => {
        bodies.push(await request.json())
        return HttpResponse.json({ id: A1, kind: 'x', severity: 'p1', state: 'resolved' })
      }),
    )
    renderPage()
    const user = userEvent.setup()
    await user.selectOptions(await screen.findByLabelText('Alert state'), 'acked')
    await user.click(await screen.findByRole('button', { name: /Ledger and balance projection differ/ }))
    expect(screen.queryByRole('button', { name: 'Acknowledge' })).toBeNull() // only an open alert can be acked
    await user.click(screen.getByRole('button', { name: 'Resolve' }))
    const dlg = screen.getByRole('dialog', { name: 'Resolve alert' })
    const submit = within(dlg).getByRole('button', { name: 'Resolve' })
    expect(submit).toBeDisabled()
    await user.type(within(dlg).getByLabelText('Reason code (required)'), 'Bad Reason')
    expect(submit).toBeDisabled()
    await user.clear(within(dlg).getByLabelText('Reason code (required)'))
    await user.type(within(dlg).getByLabelText('Reason code (required)'), 'fixed.upstream')
    expect(submit).toBeEnabled()
    await user.click(submit)
    await screen.findByRole('status')
    expect(bodies).toEqual([{ reason_code: 'fixed.upstream' }])
  })

  it('P1 shortcut, warning wording, captions, focus move, and success notice clearing', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    const seen = useBaseHandlers(() => Array.from({ length: 50 }, (_, i) => alert({ id: `10000000-0000-4000-8000-${String(i).padStart(12, '0')}` })))
    server.use(
      http.post('/v1/admin/alerts/10000000-0000-4000-8000-000000000000/ack', () =>
        HttpResponse.json({ id: A1, kind: 'x', severity: 'p1', state: 'acked' }),
      ),
    )
    renderPage()
    expect(await screen.findByText(/sorted by first-seen time only/)).toHaveTextContent('push an open P1 out of view')
    expect(screen.getByRole('table', { name: 'Platform alerts' })).toBeInTheDocument()
    expect(screen.getByText(/Platform alerts, newest first/)).not.toHaveTextContent('all tenants')
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Show P1 only' }))
    await waitFor(() => expect(seen[seen.length - 1].searchParams.get('severity')).toBe('p1'))
    await user.click((await screen.findAllByRole('button', { name: /Ledger and balance projection differ/ }))[0])
    expect(screen.getByRole('region', { name: 'Alert detail' })).toHaveFocus()
    await user.click(screen.getByRole('button', { name: 'Acknowledge' }))
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Acknowledge' }))
    await screen.findByRole('status')
    await user.selectOptions(screen.getByLabelText('Alert severity'), 'p2')
    expect(screen.queryByRole('status')).toBeNull()
  })

  it('resolved alerts offer no actions', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    useBaseHandlers(() => [alert({ state: 'resolved' })])
    renderPage()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: /Ledger and balance projection differ/ }))
    expect(screen.queryByRole('button', { name: 'Resolve' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Acknowledge' })).toBeNull()
  })

  it.each([
    [403, 'forbidden', /does not hold the alert permission/],
    [404, 'not_found', /no longer exists on the server/],
    [409, 'conflict', /no longer open/],
  ])('maps a %i on ack through the closed code map and never prints the server message', async (status, code, text) => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    const seen = useBaseHandlers()
    server.use(
      http.post(`/v1/admin/alerts/${A1}/ack`, () =>
        HttpResponse.json({ code, message: 'RAW SERVER MESSAGE <script>', request_id: 'req-123' }, { status }),
      ),
    )
    renderPage()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: /Ledger and balance projection differ/ }))
    await user.click(screen.getByRole('button', { name: 'Acknowledge' }))
    const before = seen.length
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Acknowledge' }))
    const err = await within(screen.getByRole('dialog')).findByRole('alert')
    expect(err).toHaveTextContent(text)
    expect(err).toHaveTextContent(`HTTP ${status}`)
    expect(err).toHaveTextContent('Request ID: req-123')
    expect(document.body).not.toHaveTextContent('RAW SERVER MESSAGE')
    expect(screen.queryByRole('status')).toBeNull() // never shown as done
    await waitFor(() => expect(seen.length).toBeGreaterThan(before)) // state re-read after a refusal
  })

  it('maps a 409 on resolve and a 400 validation refusal', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    useBaseHandlers()
    let n = 0
    server.use(
      http.post(`/v1/admin/alerts/${A1}/resolve`, () =>
        n++ === 0
          ? HttpResponse.json({ code: 'conflict', message: 'RAW' }, { status: 409 })
          : HttpResponse.json({ code: 'validation_error', message: 'RAW' }, { status: 400 }),
      ),
    )
    renderPage()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: /Ledger and balance projection differ/ }))
    await user.click(screen.getByRole('button', { name: 'Resolve' }))
    const dlg = screen.getByRole('dialog')
    await user.type(within(dlg).getByLabelText('Reason code (required)'), 'done')
    await user.click(within(dlg).getByRole('button', { name: 'Resolve' }))
    expect(await within(dlg).findByRole('alert')).toHaveTextContent('already resolved')
    await user.click(within(dlg).getByRole('button', { name: 'Resolve' }))
    await waitFor(() => expect(within(dlg).getByRole('alert')).toHaveTextContent('rejected the request as invalid'))
  })

  it('a platform admin whose list call is refused with 403 sees the mapped refusal, not an empty list', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    server.use(
      http.get('/v1/admin/alerts', () => HttpResponse.json({ code: 'forbidden', message: 'RAW SERVER MESSAGE' }, { status: 403 })),
      http.get('/v1/admin/alerting/status', () => HttpResponse.json({ code: 'forbidden', message: 'RAW' }, { status: 403 })),
      http.get('/v1/admin/alerting/routes', () => HttpResponse.json({ code: 'forbidden', message: 'RAW' }, { status: 403 })),
    )
    renderPage()
    expect((await screen.findAllByRole('alert')).length).toBeGreaterThan(0)
    expect((await screen.findAllByText(/does not hold the alert permission/)).length).toBeGreaterThan(0)
    expect(screen.queryByText('No alerts match these filters.')).toBeNull()
    expect(document.body).not.toHaveTextContent('RAW SERVER MESSAGE')
    // The banner stays regardless.
    expect(screen.getByRole('note', { name: 'Delivery status' })).toBeInTheDocument()
  })

  it('a 400 on the list is mapped (invalid filter) and a 500 is generic', async () => {
    expect(describeAlertError(new ApiError('validation_error', 'RAW', 400), 'list').message).toMatch(/filters as invalid/)
    expect(describeAlertError(new ApiError('internal_error', 'RAW', 500, 'r1'), 'list').message).not.toMatch(/RAW/)
    expect(describeAlertError(new ApiError('network_error', 'RAW', 0), 'ack').message).toMatch(/Could not reach/)
    expect(describeAlertError(new Error('RAW'), 'ack').message).not.toMatch(/RAW/)
  })
})

// Parity with internal/alerting/kind.go (3 meta-kinds, 14 business kinds incl. simulation, kyc, launch = 19 total).
// Adding a server kind without a catalogue sentence must fail here.
const SERVER_KINDS = [
  'alerting.unrouted', 'alerting.delivery_dead', 'alerting.raise_failed',
  'payment.multiple_success_for_intent', 'payment.deposit_intent_index_backstop_fired',
  'reconciliation.ledger_projection_drift', 'reconciliation.sportsbook_settlement_mismatch',
  'reconciliation.casino_consistency_mismatch', 'reconciliation.casino_statement_mismatch',
  'reconciliation.payment_statement_mismatch', 'reconciliation.run_failed',
  'payment.kill_switch_engaged', 'casino.callback_integrity', 'payment.webhook_integrity',
  'simulation.payment.payload_mismatch', 'simulation.casino_play', 'simulation.sportsbook_settlement',
  'kyc.submission_failed_terminal', 'launch.suspension_exhausted',
]

describe('kind catalogue parity', () => {
  it('covers exactly the 19 server kinds', () => {
    expect(SERVER_KINDS).toHaveLength(19)
    expect(Object.keys(KIND_CATALOGUE).sort()).toEqual([...SERVER_KINDS].sort())
    for (const k of SERVER_KINDS) expect(describeKind(k).known).toBe(true)
  })
})

describe('delivery wording is hardcoded while ALERT-DELIVERY-1 is OPEN', () => {
  it('pins the static wording; it must be derived from /alerting/status human_notification_routes and notified_a_person when the item closes', () => {
    expect(DELIVERY_BANNER_BODY).toContain('nobody is paged')
  })
})

describe('alerts console role gating (UI hint only; the server is authoritative)', () => {
  it('only platform_admin has the alertManage flag and the Alerts nav item', () => {
    expect(getNavPermissions('platform_admin').alertManage).toBe(true)
    for (const role of ['tenant_admin', 'finance', 'compliance', 'support', 'risk_manager', 'promotions_manager', 'bonus_operations', 'player', undefined]) {
      expect(getNavPermissions(role as string).alertManage).toBe(false)
    }
    const claims = { sub: 's', tenant_id: '00000000-0000-0000-0000-000000000000', role: 'platform_admin', principal_type: 'staff', exp: 0, iat: 0 } as const
    expect(buildNavItems({ ...claims }).map((i) => i.label)).toContain('Alerts')
    expect(buildNavItems({ ...claims, role: 'tenant_admin', tenant_id: '11111111-1111-1111-1111-111111111111' }).map((i) => i.label)).not.toContain('Alerts')
  })

  it('a tenant role typing the URL gets NotAuthorized and no alert request is made', async () => {
    signInAs({ role: 'tenant_admin' })
    let calls = 0
    server.use(
      http.get('/v1/admin/alerts', () => {
        calls++
        return HttpResponse.json({ alerts: [] })
      }),
    )
    renderPage()
    await waitFor(() => expect(screen.queryByRole('note', { name: 'Delivery status' })).toBeNull())
    expect(screen.getByText('Your role does not have access to this section.')).toBeInTheDocument()
    expect(calls).toBe(0)
  })
})
