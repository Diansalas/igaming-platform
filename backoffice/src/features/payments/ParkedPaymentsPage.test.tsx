import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it, vi } from 'vitest'
import { AppRoutes } from '../../app/AppRoutes'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { PLATFORM_ADMIN_CLAIMS, signInAs } from '../../test/session'
import { ATT1, ATT2, CO, FI, TA, TENANT, WD1, attempt, refusal, resolution } from '../../test/consoleFixtures'

// Slow under a loaded full run (many keystrokes); the longer timeout applies to this file only.
vi.setConfig({ testTimeout: 30000 })

const BASE = `/v1/admin/tenants/${TENANT}`

function tenants() {
  server.use(http.get('/v1/admin/tenants', () => HttpResponse.json({ items: [{ id: TENANT, name: 'Acme', slug: 'acme', status: 'active' }], limit: 50, offset: 0, total: 1 })))
}

function page(items = [attempt()], next: string | null = null) {
  const seen: URL[] = []
  server.use(
    http.get(`${BASE}/payment-attempts`, ({ request }) => {
      seen.push(new URL(request.url))
      return HttpResponse.json({ items, next_cursor: next })
    }),
  )
  return seen
}

// The reason catalogue also appears as <option> labels in the filter, so rows are always looked up inside the table.
const inTable = async (text: string) => within(await screen.findByRole('table')).findByText(text)

describe('parked payments queue (S3)', () => {
  it('lists parked attempts for a finance user from the token tenant, with the MOCK banner, age, reason and no provider reference text', async () => {
    signInAs(FI)
    const seen = page()
    renderWithProviders(<AppRoutes />, { route: '/parked-payments' })
    expect(await screen.findByRole('heading', { name: 'Parked payments' })).toBeInTheDocument()
    expect(screen.getByRole('note', { name: 'Stack status' })).toHaveTextContent('Sandbox/MOCK provider - software capability only, not a regulatory approval.')
    const row = (await inTable('Payout destination differs from the bound instrument')).closest('tr') as HTMLElement
    expect(within(row).getByText('123.45 EUR')).toBeInTheDocument()
    expect(within(row).getByText('Parked (disputed)')).toBeInTheDocument()
    expect(within(row).getByText('mock-payments')).toBeInTheDocument()
    expect(within(row).getByText('Yes')).toBeInTheDocument() // provider reference HELD, never its text
    // default filter is the parked state; the path tenant is the token tenant; no tenant_id query parameter
    expect(seen[0].pathname).toBe(`${BASE}/payment-attempts`)
    expect(seen[0].searchParams.get('state')).toBe('disputed')
    expect(seen[0].searchParams.has('tenant_id')).toBe(false)
    expect(screen.queryByLabelText('Tenant')).toBeNull()
  })

  it('sends only closed-vocabulary filters, resets paging on a filter change, and refuses a malformed withdrawal id locally', async () => {
    signInAs(FI)
    const seen = page()
    renderWithProviders(<AppRoutes />, { route: '/parked-payments' })
    await inTable('Payout destination differs from the bound instrument')
    const user = userEvent.setup()
    await user.selectOptions(screen.getByLabelText('Operation'), 'deposit')
    await user.selectOptions(screen.getByLabelText('Park reason'), 'kill_switch')
    await waitFor(() => expect(seen[seen.length - 1]?.searchParams.get('terminal_reason')).toBe('kill_switch'))
    expect(seen[seen.length - 1]?.searchParams.get('operation')).toBe('deposit')
    const before = seen.length
    await user.type(screen.getByLabelText('Withdrawal id'), 'not-a-uuid')
    expect(screen.getByText(/Not a valid id/)).toBeInTheDocument()
    expect(seen.length).toBe(before) // nothing sent for an invalid id
    await user.clear(screen.getByLabelText('Withdrawal id'))
    await user.click(screen.getByLabelText('Withdrawal id'))
    await user.paste(WD1)
    await waitFor(() => expect(seen[seen.length - 1]?.searchParams.get('withdrawal_request_id')).toBe(WD1))
  })

  it('A10c: the provider and age filters are sent to the server, and a malformed provider id is refused locally', async () => {
    signInAs(FI)
    const seen = page()
    renderWithProviders(<AppRoutes />, { route: '/parked-payments' })
    await inTable('Payout destination differs from the bound instrument')
    expect(seen[0].searchParams.has('provider_id')).toBe(false)
    expect(seen[0].searchParams.has('min_age_seconds')).toBe(false)
    expect(screen.queryByText(/no provider or age filter/)).toBeNull()
    const user = userEvent.setup()
    await user.selectOptions(screen.getByLabelText('Older than'), '86400')
    await waitFor(() => expect(seen[seen.length - 1]?.searchParams.get('min_age_seconds')).toBe('86400'))
    await user.click(screen.getByLabelText('Provider'))
    await user.paste('mock-payments')
    await waitFor(() => expect(seen[seen.length - 1]?.searchParams.get('provider_id')).toBe('mock-payments'))
    const before = seen.length
    await user.clear(screen.getByLabelText('Provider'))
    await user.click(screen.getByLabelText('Provider'))
    await user.paste('Bad_Provider')
    expect(screen.getByText(/Not a valid provider id/)).toBeInTheDocument()
    expect(seen.length).toBe(before + 1) // only the clear (provider removed) re-queried; the invalid id sent nothing
    expect(seen[seen.length - 1]?.searchParams.has('provider_id')).toBe(false)
  })

  it('walks the opaque keyset cursor forward and back', async () => {
    signInAs(FI)
    const cursors: Array<string | null> = []
    server.use(
      http.get(`${BASE}/payment-attempts`, ({ request }) => {
        const c = new URL(request.url).searchParams.get('cursor')
        cursors.push(c)
        if (c === 'CUR2') return HttpResponse.json({ items: [attempt({ id: ATT2, terminal_reason: 'kill_switch' })], next_cursor: null })
        return HttpResponse.json({ items: [attempt()], next_cursor: 'CUR2' })
      }),
    )
    renderWithProviders(<AppRoutes />, { route: '/parked-payments' })
    await inTable('Payout destination differs from the bound instrument')
    const user = userEvent.setup()
    expect(screen.getByRole('button', { name: 'Previous' })).toBeDisabled()
    await user.click(screen.getByRole('button', { name: 'Next' }))
    expect(await inTable('Blocked by a payment kill switch')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Next' })).toBeDisabled()
    await user.click(screen.getByRole('button', { name: 'Previous' }))
    expect(await inTable('Payout destination differs from the bound instrument')).toBeInTheDocument()
    expect(cursors[0]).toBeNull()
    expect(cursors).toContain('CUR2')
  })

  it('shows loading-free empty and error states; a 403 is shown through the closed map, never the server text', async () => {
    signInAs(FI)
    page([])
    const r = renderWithProviders(<AppRoutes />, { route: '/parked-payments' })
    expect(await screen.findByText('No payment attempts match these filters.')).toBeInTheDocument()
    r.unmount()
    server.use(http.get(`${BASE}/payment-attempts`, () => refusal(403, 'forbidden')))
    renderWithProviders(<AppRoutes />, { route: '/parked-payments' })
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('The server refused this')
    expect(alert).toHaveTextContent('HTTP 403')
    expect(alert).toHaveTextContent('Request ID: req-9')
    expect(document.body).not.toHaveTextContent('do-not-render')
  })

  it('platform admin picks a tenant (UUID from the URL only); nothing is requested before a tenant is chosen', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    tenants()
    let calls = 0
    server.use(http.get(/\/payment-attempts/, () => { calls++; return HttpResponse.json({ items: [], next_cursor: null }) }))
    renderWithProviders(<AppRoutes />, { route: '/parked-payments' })
    expect(await screen.findByText('Choose a tenant to see its payment attempts.')).toBeInTheDocument()
    expect(calls).toBe(0)
  })

  it('platform admin with a tenant in the URL reads that tenant; a non-UUID tenant parameter is ignored', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    tenants()
    const seen = page([attempt()])
    renderWithProviders(<AppRoutes />, { route: `/parked-payments?tenant=${TENANT}` })
    await inTable('Payout destination differs from the bound instrument')
    expect(seen[0].pathname).toBe(`${BASE}/payment-attempts`)
  })

  it('a non-UUID tenant parameter never reaches a path', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    tenants()
    let calls = 0
    server.use(http.get(/\/payment-attempts/, () => { calls++; return HttpResponse.json({ items: [], next_cursor: null }) }))
    renderWithProviders(<AppRoutes />, { route: '/parked-payments?tenant=../../etc' })
    expect(await screen.findByText('Choose a tenant to see its payment attempts.')).toBeInTheDocument()
    expect(calls).toBe(0)
  })

  it('is gated: tenant_admin and others get NotAuthorized and no request; compliance can read', async () => {
    signInAs(TA)
    let calls = 0
    server.use(http.get(/\/payment-attempts/, () => { calls++; return HttpResponse.json({ items: [], next_cursor: null }) }))
    const r = renderWithProviders(<AppRoutes />, { route: '/parked-payments' })
    expect(await screen.findByText('Your role does not have access to this section.')).toBeInTheDocument()
    expect(calls).toBe(0)
    r.unmount()
    signInAs(CO)
    page()
    renderWithProviders(<AppRoutes />, { route: '/parked-payments' })
    expect(await inTable('Payout destination differs from the bound instrument')).toBeInTheDocument()
  })
})

describe('attempt detail (S3)', () => {
  const event = (over = {}) => ({
    id: 'e1000000-0000-4000-8000-000000000001',
    event_type: 'payout',
    outcome: 'pending',
    disposition_at_receipt: 'applied',
    resolution: null,
    decline_stage: null,
    amount_minor_units: null,
    asset_code: null,
    received_at: '2026-10-10T10:30:00Z',
    resolved_at: null,
    ...over,
  })

  function detail(a = attempt(), resolutions = [resolution()], events: unknown[] = []) {
    server.use(
      http.get(`${BASE}/payment-attempts/${a.id}/events`, () => HttpResponse.json({ items: events, limit: 200, offset: 0, total: events.length })),
      http.get(`${BASE}/payment-attempts/${a.id}`, () => HttpResponse.json(a)),
      http.get(`${BASE}/payment-force-resolutions`, () => HttpResponse.json({ items: resolutions })),
    )
  }

  it('shows the fields and a timeline built from returned timestamps, the provider events and the attempt\'s force-resolutions', async () => {
    signInAs(FI)
    page()
    detail(attempt(), [resolution()], [event(), event({ id: 'e2000000-0000-4000-8000-000000000002', outcome: 'ambiguous', received_at: '2026-10-10T10:45:00Z' })])
    renderWithProviders(<AppRoutes />, { route: `/parked-payments?attempt=${ATT1}` })
    const d = await screen.findByRole('region', { name: 'Attempt detail' })
    await within(d).findByText('Timeline')
    const tl = await within(d).findByRole('list', { name: 'Timeline' })
    await waitFor(() => expect(within(tl).getAllByRole('listitem').length).toBe(6))
    const items = within(tl).getAllByRole('listitem').map((li) => li.textContent)
    expect(items[0]).toContain('Attempt 1 created')
    expect(items.some((t) => t?.includes('Force-resolution filed'))).toBe(true)
    expect(items.filter((t) => t?.includes('Provider event: payout')).length).toBe(2)
    expect(items.some((t) => t?.includes('Provider event: payout ambiguous, applied'))).toBe(true)
    expect(within(d).getByText(/Yes \(the reference text is never shown here\)/)).toBeInTheDocument()
    expect(within(d).getByText(/verified provider events linked to this attempt/)).toBeInTheDocument()
    expect(within(d).queryByText(/no event history/)).toBeNull()
    expect(within(d).getByRole('link', { name: WD1 })).toHaveAttribute('href', `/withdrawals/${WD1}`)
  })

  it('offers a proposal only to roles that may act, only for M1/M2 kinds, and points to a pending resolution instead', async () => {
    signInAs(FI)
    page()
    detail(attempt({ admissible_kinds: ['m2_declare_paid', 'm4_evidence_paid'] }), [])
    const r = renderWithProviders(<AppRoutes />, { route: `/parked-payments?attempt=${ATT1}` })
    const d = await screen.findByRole('region', { name: 'Attempt detail' })
    expect(await within(d).findByRole('link', { name: /Propose a force-resolution/ })).toHaveAttribute('href', `/force-resolutions?tenant=${TENANT}&attempt=${ATT1}`)
    expect(within(d).getByText(/M4 kinds are not offered by this console/)).toBeInTheDocument()
    r.unmount()

    signInAs(CO)
    page()
    detail()
    renderWithProviders(<AppRoutes />, { route: `/parked-payments?attempt=${ATT1}` })
    const d2 = await screen.findByRole('region', { name: 'Attempt detail' })
    await within(d2).findByText(/read only for force-resolutions/)
    expect(within(d2).queryByRole('link', { name: /Propose a force-resolution/ })).toBeNull()
    expect(within(d2).getByText(/Pending/)).toBeInTheDocument()
  })

  it('a refused event history degrades to the closed message and the rest of the detail still renders', async () => {
    signInAs(FI)
    page()
    detail()
    server.use(http.get(`${BASE}/payment-attempts/${ATT1}/events`, () => refusal(403, 'forbidden')))
    renderWithProviders(<AppRoutes />, { route: `/parked-payments?attempt=${ATT1}` })
    const d = await screen.findByRole('region', { name: 'Attempt detail' })
    expect(await within(d).findByRole('alert')).toBeInTheDocument()
    expect(within(d).getByRole('list', { name: 'Timeline' })).toBeInTheDocument()
    expect(document.body).not.toHaveTextContent('do-not-render')
  })

  it('a refused or unknown attempt degrades to the closed message', async () => {
    signInAs(FI)
    page()
    server.use(
      http.get(`${BASE}/payment-attempts/${ATT2}`, () => refusal(404, 'not_found')),
      http.get(`${BASE}/payment-force-resolutions`, () => HttpResponse.json({ items: [] })),
    )
    renderWithProviders(<AppRoutes />, { route: `/parked-payments?attempt=${ATT2}` })
    const d = await screen.findByRole('region', { name: 'Attempt detail' })
    expect(await within(d).findByRole('alert')).toHaveTextContent('no longer exists')
    expect(document.body).not.toHaveTextContent('do-not-render')
  })

  it('renders hostile server strings as text (no HTML), and unknown enum values as Unrecognised', async () => {
    signInAs(FI)
    const hostile = attempt({ state: '<img src=x onerror=alert(1)>', terminal_reason: '<b>boom</b>', provider_id: '<script>alert(1)</script>', asset_code: '<i>X</i>' })
    page([hostile])
    renderWithProviders(<AppRoutes />, { route: '/parked-payments' })
    await screen.findAllByText('Unrecognised')
    expect(document.querySelector('img')).toBeNull()
    expect(document.querySelector('script')).toBeNull()
    expect(document.querySelector('b')).toBeNull()
    expect(document.body).not.toHaveTextContent('boom')
  })
})
