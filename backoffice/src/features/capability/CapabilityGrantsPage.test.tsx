import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it, vi } from 'vitest'
import { AppRoutes } from '../../app/AppRoutes'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { PLATFORM_ADMIN_CLAIMS, signInAs } from '../../test/session'
import { CO, FI, GRANT1, REQ1, STAFF_A, STAFF_B, TA, TENANT, grant, grantRequest, refusal } from '../../test/consoleFixtures'
import { grantStatus } from './CapabilityGrantsPage'

// Slow under a loaded full run (many keystrokes); the longer timeout applies to this file only.
vi.setConfig({ testTimeout: 30000 })

const BASE = `/v1/admin/tenants/${TENANT}/capability-grants`
const PA = { ...PLATFORM_ADMIN_CLAIMS, sub: 'f9000000-0000-4000-8000-000000000009' }
const route = `/capability-grants?tenant=${TENANT}`

function setup(reqs = [grantRequest()], grants = [grant()]) {
  server.use(
    http.get('/v1/admin/tenants', () => HttpResponse.json({ items: [{ id: TENANT, name: 'Acme', slug: 'acme', status: 'active' }], limit: 50, offset: 0, total: 1 })),
    http.get(`${BASE}/requests`, () => HttpResponse.json(reqs)),
    http.get(BASE, () => HttpResponse.json(grants)),
    http.get(`/v1/admin/tenants/${TENANT}/staff`, () => HttpResponse.json({ staff: [{ id: STAFF_A, display_name: 'Ana Finance', role: 'finance', status: 'active', person_linked: true, email: 'secret@example.com' }], truncated: false })),
  )
}

describe('role-driven actions (UI hint; the server decides)', () => {
  it('platform admin sees approve / reject / revoke; tenant_admin sees request + revoke + cancel-own but never approve; compliance reads only', async () => {
    signInAs(PA)
    setup([grantRequest({ requested_by: STAFF_B })])
    const r = renderWithProviders(<AppRoutes />, { route })
    expect(await screen.findByRole('button', { name: `Approve request ${REQ1.slice(0, 8)}` })).toBeEnabled()
    expect(screen.getByRole('button', { name: `Reject request ${REQ1.slice(0, 8)}` })).toBeEnabled()
    expect(screen.getByRole('button', { name: `Revoke grant ${GRANT1.slice(0, 8)}` })).toBeEnabled()
    expect(screen.getByRole('button', { name: 'Request a grant' })).toBeInTheDocument()
    r.unmount()

    signInAs(TA) // sub STAFF_A
    setup([grantRequest({ requested_by: STAFF_A })])
    renderWithProviders(<AppRoutes />, { route: '/capability-grants' })
    expect(await screen.findByRole('button', { name: `Cancel request ${REQ1.slice(0, 8)}` })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Approve request/ })).toBeNull()
    expect(screen.queryByRole('button', { name: /Reject request/ })).toBeNull()
    expect(screen.getByRole('button', { name: `Revoke grant ${GRANT1.slice(0, 8)}` })).toBeInTheDocument()
    expect(screen.getByText(/Only platform administrators approve or reject/)).toBeInTheDocument()
  })

  it('compliance can read but has no action; finance and others are NotAuthorized with no request', async () => {
    signInAs(CO)
    setup()
    const r = renderWithProviders(<AppRoutes />, { route: '/capability-grants' })
    await screen.findByText('Capability grants')
    await screen.findByRole('table', { name: 'Capability grant requests' })
    expect(screen.queryByRole('button', { name: 'Request a grant' })).toBeNull()
    expect(screen.queryByRole('button', { name: /Revoke grant/ })).toBeNull()
    r.unmount()

    signInAs(FI)
    let calls = 0
    server.use(http.get(/capability-grants/, () => { calls++; return HttpResponse.json([]) }))
    renderWithProviders(<AppRoutes />, { route: '/capability-grants' })
    expect(await screen.findByText('Your role does not have access to this section.')).toBeInTheDocument()
    expect(calls).toBe(0)
  })

  it('the requester\'s own pending request has its approve button off (the server also refuses)', async () => {
    signInAs(PA)
    setup([grantRequest({ requested_by: PA.sub, requested_by_scope: 'platform' })])
    renderWithProviders(<AppRoutes />, { route })
    expect(await screen.findByRole('button', { name: `Approve request ${REQ1.slice(0, 8)}` })).toBeDisabled()
  })
})

describe('display and redaction', () => {
  it('shows scope-only for a platform actor seen by a tenant viewer (null id), never an id or "null", and classifies expiry', async () => {
    signInAs(TA)
    setup([grantRequest({ requested_by: null, requested_by_scope: 'platform' })], [grant({ valid_until: '2020-01-01T00:00:00Z' }), grant({ id: 'e2000000-0000-4000-8000-000000000002', revoked_at: '2026-10-02T00:00:00Z', revoke_reason_code: 'left_team' })])
    renderWithProviders(<AppRoutes />, { route: '/capability-grants' })
    const reqs = await screen.findByRole('table', { name: 'Capability grant requests' })
    expect(within(reqs).getByText('Platform staff')).toBeInTheDocument()
    expect(reqs).not.toHaveTextContent('null')
    const grants = await screen.findByRole('table', { name: 'Capability grants' })
    expect(within(grants).getByText('Expired')).toBeInTheDocument()
    expect(within(grants).getByText('Revoked')).toBeInTheDocument()
    expect(within(grants).getByText('left_team')).toBeInTheDocument()
    expect(document.body).not.toHaveTextContent('secret@example.com')
  })

  it('grantStatus classifies against a clock without trusting it', () => {
    const now = Date.parse('2026-10-15T00:00:00Z')
    expect(grantStatus(grant({ valid_from: '2026-11-01T00:00:00Z' }), now).label).toBe('Not yet valid')
    expect(grantStatus(grant({ valid_until: undefined }), now).label).toBe('In force, no expiry')
    expect(grantStatus(grant({ valid_until: '2026-10-14T00:00:00Z' }), now).label).toBe('Expired')
    expect(grantStatus(grant({ revoked_at: '2026-10-10T00:00:00Z' }), now).label).toBe('Revoked')
  })

  it('renders hostile server strings as text', async () => {
    signInAs(TA)
    setup([grantRequest({ capability: '<img src=x onerror=alert(1)>', reason_code: '<script>1</script>', status: '<b>x</b>' })], [])
    renderWithProviders(<AppRoutes />, { route: '/capability-grants' })
    await screen.findAllByText(/Unrecognised/)
    expect(document.querySelector('img')).toBeNull()
    expect(document.querySelector('script')).toBeNull()
  })

  it('shows loading-free empty states and a 403 through the closed map', async () => {
    signInAs(CO)
    setup([], [])
    const r = renderWithProviders(<AppRoutes />, { route: '/capability-grants' })
    expect(await screen.findByText('No requests for this tenant.')).toBeInTheDocument()
    expect(screen.getByText('No grants for this tenant.')).toBeInTheDocument()
    r.unmount()
    server.use(http.get(`${BASE}/requests`, () => refusal(403, 'forbidden')), http.get(BASE, () => refusal(500, 'internal_error')))
    renderWithProviders(<AppRoutes />, { route: '/capability-grants' })
    await waitFor(() => expect(screen.getAllByRole('alert').length).toBe(2))
    expect(document.body).not.toHaveTextContent('do-not-render')
  })
})

describe('request (S7)', () => {
  it('files a request from the staff directory with the exact body, no tenant field, an Idempotency-Key; the email is never shown', async () => {
    signInAs(TA)
    setup([], [])
    const posts: Array<{ body: Record<string, unknown>; key: string | null }> = []
    server.use(http.post(`${BASE}/requests`, async ({ request }) => { posts.push({ body: (await request.json()) as Record<string, unknown>, key: request.headers.get('Idempotency-Key') }); return HttpResponse.json(grantRequest(), { status: 201 }) }))
    renderWithProviders(<AppRoutes />, { route: '/capability-grants' })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Request a grant' }))
    const dlg = await screen.findByRole('dialog', { name: 'Request a capability grant' })
    await waitFor(() => expect(within(dlg).getByRole('option', { name: /Ana Finance/ })).toBeInTheDocument())
    expect(dlg).not.toHaveTextContent('secret@example.com')
    const go = within(dlg).getByRole('button', { name: 'File request' })
    expect(go).toBeDisabled()
    await user.selectOptions(within(dlg).getByLabelText('Grantee (tenant staff)'), STAFF_A)
    await user.selectOptions(within(dlg).getByLabelText('Capability'), 'payment_force_resolve:approve')
    await user.type(within(dlg).getByLabelText('Reason code (required)'), 'rota_cover')
    expect(go).toBeDisabled()
    await user.click(within(dlg).getByRole('checkbox'))
    expect(go).toBeEnabled()
    expect(within(dlg).getByText(/No expiry: the grant stays in force until it is revoked/)).toBeInTheDocument()
    await user.dblClick(go)
    expect(await screen.findByRole('status')).toHaveTextContent('Nothing is granted until a platform administrator approves it')
    expect(posts).toHaveLength(1)
    expect(posts[0].body).toEqual({ grantee_staff_id: STAFF_A, capability: 'payment_force_resolve:approve', reason_code: 'rota_cover' })
    expect(posts[0].key).toBeTruthy()
  })

  it('sends valid_until as an ISO time, rejects a past time locally, and a typed staff id overrides the selection', async () => {
    signInAs(PA)
    setup([], [])
    const bodies: Array<Record<string, unknown>> = []
    server.use(http.post(`${BASE}/requests`, async ({ request }) => { bodies.push((await request.json()) as Record<string, unknown>); return HttpResponse.json(grantRequest(), { status: 201 }) }))
    renderWithProviders(<AppRoutes />, { route })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Request a grant' }))
    const dlg = await screen.findByRole('dialog', { name: 'Request a capability grant' })
    expect(within(dlg).getByText(/cannot request a grant for a tenant staff member/)).toBeInTheDocument()
    await user.type(within(dlg).getByLabelText('Or enter a staff id'), STAFF_B)
    await user.type(within(dlg).getByLabelText('Reason code (required)'), 'platform_cover')
    await user.click(within(dlg).getByRole('checkbox'))
    const until = within(dlg).getByLabelText('Valid until (optional)')
    await user.type(until, '2020-01-01T00:00')
    expect(within(dlg).getByRole('button', { name: 'File request' })).toBeDisabled()
    await user.clear(until)
    await user.type(until, '2099-01-01T00:00')
    await user.click(within(dlg).getByRole('button', { name: 'File request' }))
    await screen.findByRole('status')
    expect(bodies[0].grantee_staff_id).toBe(STAFF_B)
    expect(String(bodies[0].valid_until)).toMatch(/^2099-01-01T\d\d:00:00\.000Z$/)
  })

  it('maps the platform-session 409 and keeps the dialog open with nothing created', async () => {
    signInAs(PA)
    setup([], [])
    server.use(http.post(`${BASE}/requests`, () => refusal(409, 'conflict')))
    renderWithProviders(<AppRoutes />, { route })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Request a grant' }))
    const dlg = await screen.findByRole('dialog', { name: 'Request a capability grant' })
    await user.type(within(dlg).getByLabelText('Or enter a staff id'), STAFF_A)
    await user.type(within(dlg).getByLabelText('Reason code (required)'), 'x')
    await user.click(within(dlg).getByRole('checkbox'))
    await user.click(within(dlg).getByRole('button', { name: 'File request' }))
    expect(await within(dlg).findByRole('alert')).toHaveTextContent(/platform session may not request a grant/)
    expect(document.body).not.toHaveTextContent('do-not-render')
  })
})

describe('approve / reject / cancel / revoke (S7)', () => {
  it('approve shows the frozen request content, needs reason + acknowledgement, posts only {reason_code}; there is no payload hash on K1', async () => {
    signInAs(PA)
    setup([grantRequest({ requested_by: STAFF_B })])
    const posts: Array<{ body: unknown; key: string | null }> = []
    server.use(http.post(`${BASE}/requests/${REQ1}/approve`, async ({ request }) => { posts.push({ body: await request.json(), key: request.headers.get('Idempotency-Key') }); return HttpResponse.json(grant()) }))
    renderWithProviders(<AppRoutes />, { route })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: `Approve request ${REQ1.slice(0, 8)}` }))
    const dlg = await screen.findByRole('dialog', { name: 'Approve capability grant' })
    expect(within(dlg).getByText('Payment force-resolution: request')).toBeInTheDocument()
    expect(within(dlg).getByText(/K1 has no payload hash/)).toBeInTheDocument()
    const go = within(dlg).getByRole('button', { name: 'Approve' })
    expect(go).toBeDisabled()
    await user.type(within(dlg).getByLabelText('Reason code (required)'), 'checked_with_manager')
    await user.click(within(dlg).getByRole('checkbox'))
    await user.dblClick(go)
    expect(await screen.findByRole('status')).toHaveTextContent('Approved. The grant now exists')
    expect(posts).toHaveLength(1)
    expect(posts[0].body).toEqual({ reason_code: 'checked_with_manager' })
    expect(posts[0].key).toBeTruthy()
  })

  it('reject returns 204 and is reported; a 409 on approve is shown and the list refreshed', async () => {
    signInAs(PA)
    setup([grantRequest({ requested_by: STAFF_B })])
    let listCalls = 0
    server.use(
      http.get(`${BASE}/requests`, () => { listCalls++; return HttpResponse.json([grantRequest({ requested_by: STAFF_B })]) }),
      http.post(`${BASE}/requests/${REQ1}/reject`, () => new HttpResponse(null, { status: 204 })),
      http.post(`${BASE}/requests/${REQ1}/approve`, () => refusal(409, 'conflict')),
    )
    renderWithProviders(<AppRoutes />, { route })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: `Approve request ${REQ1.slice(0, 8)}` }))
    let dlg = await screen.findByRole('dialog', { name: 'Approve capability grant' })
    await user.type(within(dlg).getByLabelText('Reason code (required)'), 'ok')
    await user.click(within(dlg).getByRole('checkbox'))
    const before = listCalls
    await user.click(within(dlg).getByRole('button', { name: 'Approve' }))
    expect(await within(dlg).findByRole('alert')).toHaveTextContent(/refused the decision/)
    await waitFor(() => expect(listCalls).toBeGreaterThan(before))
    await user.click(within(dlg).getByRole('button', { name: 'Back' }))
    await user.click(await screen.findByRole('button', { name: `Reject request ${REQ1.slice(0, 8)}` }))
    dlg = await screen.findByRole('dialog', { name: 'Reject capability grant' })
    await user.type(within(dlg).getByLabelText('Reason code (required)'), 'no')
    await user.click(within(dlg).getByRole('checkbox'))
    await user.click(within(dlg).getByRole('button', { name: 'Reject' }))
    expect(await screen.findByRole('status')).toHaveTextContent('Rejected. No grant was created.')
  })

  it('revoke explains immediacy, needs reason + acknowledgement, posts {reason_code}; 404 and 409 are mapped', async () => {
    signInAs(TA)
    setup([], [grant()])
    const seen: unknown[] = []
    let n = 0
    server.use(http.post(`${BASE}/${GRANT1}/revoke`, async ({ request }) => { seen.push(await request.json()); n++; if (n === 1) return refusal(409, 'conflict'); return new HttpResponse(null, { status: 204 }) }))
    renderWithProviders(<AppRoutes />, { route: '/capability-grants' })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: `Revoke grant ${GRANT1.slice(0, 8)}` }))
    const dlg = await screen.findByRole('dialog', { name: 'Revoke capability grant' })
    expect(within(dlg).getByText(/immediately, needs no second person, and cannot be undone/)).toBeInTheDocument()
    const go = within(dlg).getByRole('button', { name: 'Revoke grant' })
    expect(go).toBeDisabled()
    await user.type(within(dlg).getByLabelText('Reason code (required)'), 'left_team')
    await user.click(within(dlg).getByRole('checkbox'))
    await user.click(go)
    expect(await within(dlg).findByRole('alert')).toHaveTextContent(/already revoked/)
    await user.click(within(dlg).getByRole('button', { name: 'Revoke grant' }))
    expect(await screen.findByRole('status')).toHaveTextContent('Grant revoked')
    expect(seen).toEqual([{ reason_code: 'left_team' }, { reason_code: 'left_team' }])
  })

  it('cancel of own request goes through the cancel route with no body', async () => {
    signInAs(TA)
    setup([grantRequest({ requested_by: STAFF_A })], [])
    let bodyText: string | null = null
    server.use(http.post(`${BASE}/requests/${REQ1}/cancel`, async ({ request }) => { bodyText = await request.text(); return new HttpResponse(null, { status: 204 }) }))
    renderWithProviders(<AppRoutes />, { route: '/capability-grants' })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: `Cancel request ${REQ1.slice(0, 8)}` }))
    const dlg = await screen.findByRole('dialog', { name: 'Cancel grant request' })
    await user.click(within(dlg).getByRole('button', { name: 'Cancel the request' }))
    expect(await screen.findByRole('status')).toHaveTextContent('Request cancelled.')
    expect(bodyText).toBe('')
  })
})
