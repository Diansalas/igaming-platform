import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it, vi } from 'vitest'
import { reconciliationStreams } from '../../auth/permissions'
import { AppRoutes } from '../../app/AppRoutes'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { PLATFORM_ADMIN_CLAIMS, signInAs } from '../../test/session'
import { ATT1, CO, FI, TA, TENANT, refusal } from '../../test/consoleFixtures'

// Slow under a loaded full run (many keystrokes); the longer timeout applies to this file only.
vi.setConfig({ testTimeout: 30000 })

const BASE = `/v1/admin/tenants/${TENANT}/reconciliation`
const RUN = '71000000-0000-4000-8000-000000000001'
const MM = '72000000-0000-4000-8000-000000000001'

const run = (over = {}) => ({ id: RUN, stream: 'payment_statement', period_start: '2026-10-09T00:00:00Z', period_end: '2026-10-10T00:00:00Z', run_at: '2026-10-10T01:00:00Z', status: 'mismatches_found', ...over })
const mm = (over = {}) => ({
  id: MM,
  run_id: RUN,
  stream: 'payment_statement',
  kind: 'pay_amount_mismatch',
  status: 'open',
  subject_ref: { type: 'payment_attempt', id: ATT1 },
  expected_minor_units: '12345',
  actual_minor_units: '12000',
  asset_code: 'EUR',
  exponent: 2,
  detected_at: '2026-10-10T01:00:00Z',
  ...over,
})
const paged = (items: unknown[]) => HttpResponse.json({ items, limit: 20, offset: 0, total: items.length })

// `singles` records the A10c single-item reads (kept apart from `seen` so list assertions are unaffected).
const singles: URL[] = []

function staff(streamItems: { runs?: unknown[]; mismatches?: unknown[] } = {}) {
  const seen: URL[] = []
  singles.length = 0
  const runs = (streamItems.runs ?? [run()]) as Array<{ id: string }>
  const mismatches = (streamItems.mismatches ?? [mm()]) as Array<{ id: string; run_id?: string }>
  server.use(
    http.get(`${BASE}/runs`, ({ request }) => { seen.push(new URL(request.url)); return paged(runs) }),
    http.get(`${BASE}/mismatches`, ({ request }) => {
      const u = new URL(request.url)
      seen.push(u)
      const rid = u.searchParams.get('run_id')
      return paged(rid ? mismatches.filter((m) => m.run_id === rid) : mismatches)
    }),
    http.get(`${BASE}/runs/:id`, ({ request, params }) => {
      singles.push(new URL(request.url))
      const r = runs.find((x) => x.id === params.id)
      return r ? HttpResponse.json(r) : refusal(404, 'not_found')
    }),
    http.get(`${BASE}/mismatches/:id`, ({ request, params }) => {
      singles.push(new URL(request.url))
      const m = mismatches.find((x) => x.id === params.id)
      return m ? HttpResponse.json(m) : refusal(404, 'not_found')
    }),
  )
  return seen
}

describe('reconciliation streams by role (UI hint; the server checks the stream permission before any query)', () => {
  it('matches ADR 0114 section 3 / D-OPS-2', () => {
    expect(reconciliationStreams('platform_admin', true)).toEqual(['payment_statement', 'ledger_vs_projection'])
    expect(reconciliationStreams('platform_admin', false)).toEqual([])
    expect(reconciliationStreams('tenant_admin', false)).toEqual(['sportsbook_settlement', 'casino_consistency', 'casino_statement'])
    expect(reconciliationStreams('finance', false)).toEqual(['payment_statement', 'ledger_vs_projection', 'sportsbook_settlement', 'casino_consistency', 'casino_statement'])
    expect(reconciliationStreams('compliance', false)).toEqual(reconciliationStreams('finance', false))
    for (const r of ['support', 'risk_manager', 'promotions_manager', 'bonus_operations', undefined]) expect(reconciliationStreams(r, false)).toEqual([])
  })
})

describe('reconciliation views (S5)', () => {
  it('finance sees payment_statement runs and findings with age, severity-not-provided, formatted amounts and a link to the attempt; no mutation control exists', async () => {
    signInAs(FI)
    const seen = staff()
    renderWithProviders(<AppRoutes />, { route: '/reconciliation' })
    expect(await screen.findByRole('heading', { name: 'Reconciliation' })).toBeInTheDocument()
    const tabs = screen.getAllByRole('tab').map((t) => t.textContent)
    expect(tabs).toEqual(['Payment statement', 'Ledger vs projection', 'Sportsbook settlement', 'Casino consistency', 'Casino statement'])
    await waitFor(() => expect(seen.some((u) => u.pathname === `${BASE}/runs` && u.searchParams.get('stream') === 'payment_statement')).toBe(true))
    expect(await within((await screen.findAllByRole('table'))[1]).findByText('pay amount mismatch')).toBeInTheDocument()
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: `Open mismatch ${MM.slice(0, 8)}` }))
    const d = await screen.findByRole('region', { name: 'Finding detail' })
    expect(within(d).getByText('Not provided by the API')).toBeInTheDocument()
    expect(within(d).getByText('123.45 EUR')).toBeInTheDocument()
    expect(within(d).getByText('120.00 EUR')).toBeInTheDocument()
    expect(within(d).getByRole('link', { name: ATT1 })).toHaveAttribute('href', `/parked-payments?tenant=${TENANT}&attempt=${ATT1}`)
    // detection only: no resolve / compensate / acknowledge control anywhere
    expect(screen.queryByRole('button', { name: /resolve|compensate|acknowledge|approve|fix/i })).toBeNull()
    expect(within(d).getByText(/K2\) or a force-resolution \(K3\)/)).toBeInTheDocument()
  })

  it('platform admin has a tenant picker and only the two staff streams; no casino tab; nothing is read before a tenant is chosen', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    server.use(http.get('/v1/admin/tenants', () => HttpResponse.json({ items: [{ id: TENANT, name: 'Acme', slug: 'acme', status: 'active' }], limit: 50, offset: 0, total: 1 })))
    let calls = 0
    server.use(http.get(/\/reconciliation\//, () => { calls++; return paged([]) }))
    renderWithProviders(<AppRoutes />, { route: '/reconciliation' })
    expect(await screen.findByText('Choose a tenant to see its reconciliation data.')).toBeInTheDocument()
    expect(screen.getAllByRole('tab').map((t) => t.textContent)).toEqual(['Payment statement', 'Ledger vs projection'])
    expect(calls).toBe(0)
  })

  it('filters send only closed values and a UUID subject; kind options follow the stream', async () => {
    signInAs(FI)
    const seen = staff()
    renderWithProviders(<AppRoutes />, { route: '/reconciliation' })
    await within((await screen.findAllByRole('table'))[1]).findByText('pay amount mismatch')
    const user = userEvent.setup()
    await user.selectOptions(screen.getByLabelText('Status'), 'open')
    await user.selectOptions(screen.getByLabelText('Kind'), 'pay_duplicate')
    await waitFor(() => expect(seen[seen.length - 1]?.searchParams.get('kind')).toBe('pay_duplicate'))
    expect(seen[seen.length - 1]?.searchParams.get('status')).toBe('open')
    await user.type(screen.getByLabelText('Subject id'), 'nope')
    expect(screen.getByText(/Not a valid id/)).toBeInTheDocument()
    await user.click(screen.getByRole('tab', { name: 'Sportsbook settlement' }))
    await waitFor(() => expect(seen[seen.length - 1]?.searchParams.get('stream')).toBe('sportsbook_settlement'))
    const kinds = Array.from((screen.getByLabelText('Kind') as HTMLSelectElement).options).map((o) => o.value)
    expect(kinds).toContain('sb_orphan_ledger')
    expect(kinds).not.toContain('pay_duplicate')
  })

  it('the staff shape has no stored key or diagnostic text, so none is rendered; other and unknown kinds are labelled, not guessed', async () => {
    signInAs(FI)
    staff({ mismatches: [mm({ kind: 'other', subject_ref: null, expected_minor_units: null, actual_minor_units: null, asset_code: null, exponent: null }), mm({ id: '72000000-0000-4000-8000-000000000002', kind: '<b>zzz</b>' })] })
    renderWithProviders(<AppRoutes />, { route: '/reconciliation' })
    expect(await screen.findByText('Other (not in the closed vocabulary)')).toBeInTheDocument()
    expect(screen.getByText('Unrecognised (unprintable)')).toBeInTheDocument()
    expect(document.querySelector('b')).toBeNull()
    const user = userEvent.setup()
    await user.click(screen.getAllByRole('button', { name: /Open mismatch/ })[0])
    const d = await screen.findByRole('region', { name: 'Finding detail' })
    expect(within(d).getByText('The API names no subject for this finding')).toBeInTheDocument()
    expect(within(d).getAllByText('Not available').length).toBe(2)
  })

  it('check D (withdrawal hold) kinds are first-class on the ledger stream, not "other"', async () => {
    signInAs(FI)
    staff({ mismatches: [mm({ stream: 'ledger_vs_projection', kind: 'withdrawal_hold_open_mismatch', subject_ref: null, expected_minor_units: null, actual_minor_units: null, asset_code: null, exponent: null })] })
    renderWithProviders(<AppRoutes />, { route: '/reconciliation' })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('tab', { name: 'Ledger vs projection' }))
    const t = (await screen.findAllByRole('table'))[1]
    expect(await within(t).findByText('withdrawal hold open mismatch')).toBeInTheDocument()
    expect(t).not.toHaveTextContent('Other (not in the closed vocabulary)')
    const kinds = Array.from((screen.getByLabelText('Kind') as HTMLSelectElement).options).map((o) => o.value)
    expect(kinds).toEqual(expect.arrayContaining(['withdrawal_hold_open_mismatch', 'withdrawal_request_ledger_link_invalid']))
  })

  it('casino streams use the tenant-scoped casino routes, show P1, and render stored diagnostic text as plain text', async () => {
    signInAs(TA)
    const seen: URL[] = []
    server.use(
      http.get('/v1/admin/casino/reconciliation/runs', ({ request }) => { seen.push(new URL(request.url)); return paged([run({ stream: 'casino_consistency', status: 'clean' })]) }),
      http.get('/v1/admin/casino/reconciliation/mismatches', ({ request }) => {
        seen.push(new URL(request.url))
        return paged([{ id: MM, run_id: RUN, mismatch_kind: 'cas_orphan_win', reconciliation_key: 'k <b>bold</b>', expected_value: 'e', actual_value: '<img src=x onerror=alert(1)>', investigation_status: 'open', created_at: '2026-10-10T01:00:00Z', resolved_at: null }])
      }),
    )
    // tenant_admin: sportsbook first (staff route), casino tabs after; switch to casino
    server.use(http.get(`${BASE}/runs`, () => paged([])), http.get(`${BASE}/mismatches`, () => paged([])))
    renderWithProviders(<AppRoutes />, { route: '/reconciliation' })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('tab', { name: 'Casino consistency' }))
    expect(await screen.findByText('cas orphan win')).toBeInTheDocument()
    await waitFor(() => expect(seen.some((u) => u.pathname === '/v1/admin/casino/reconciliation/runs' && u.searchParams.get('stream') === 'casino_consistency')).toBe(true))
    await user.click(screen.getByRole('button', { name: `Open mismatch ${MM.slice(0, 8)}` }))
    const d = await screen.findByRole('region', { name: 'Finding detail' })
    expect(within(d).getByText(/P1/)).toBeInTheDocument()
    expect(d).toHaveTextContent('k <b>bold</b>')
    expect(d.querySelector('b')).toBeNull()
    expect(document.querySelector('img')).toBeNull()
    expect(screen.queryByLabelText('Tenant')).toBeNull()
  })

  it('A10c: run detail is the single-run read and "Show only this run\'s findings" sends the server-side run_id filter', async () => {
    signInAs(CO)
    const OTHER_RUN = '71000000-0000-4000-8000-000000000009'
    const seen = staff({ mismatches: [mm(), mm({ id: '72000000-0000-4000-8000-000000000002', run_id: OTHER_RUN })] })
    renderWithProviders(<AppRoutes />, { route: '/reconciliation' })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: `Open run ${RUN.slice(0, 8)}` }))
    const d = await screen.findByRole('region', { name: 'Run detail' })
    await waitFor(() => expect(singles.some((u) => u.pathname === `${BASE}/runs/${RUN}` && u.searchParams.get('stream') === 'payment_statement')).toBe(true))
    expect(within(d).queryByText(/cannot filter findings by run/)).toBeNull()
    // no client-side "In selected run" column on staff streams any more
    expect(within(screen.getAllByRole('table')[1]).queryByText('In selected run')).toBeNull()
    expect(within(screen.getAllByRole('table')[1]).getAllByRole('row').slice(1)).toHaveLength(2)
    await user.click(within(d).getByRole('button', { name: /Show only this run/ }))
    await waitFor(() => expect(seen[seen.length - 1]?.searchParams.get('run_id')).toBe(RUN))
    await waitFor(() => expect(within(screen.getAllByRole('table')[1]).getAllByRole('row').slice(1)).toHaveLength(1))
    expect(screen.getByRole('status')).toHaveTextContent('Showing only the findings of run')
    await user.click(screen.getByRole('button', { name: 'Show all runs' }))
    await waitFor(() => expect(seen[seen.length - 1]?.searchParams.has('run_id')).toBe(false))
  })

  it('A10c: finding detail comes from the single-mismatch read, and a refused single read is shown through the closed map', async () => {
    signInAs(FI)
    staff()
    renderWithProviders(<AppRoutes />, { route: '/reconciliation' })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: `Open mismatch ${MM.slice(0, 8)}` }))
    await waitFor(() => expect(singles.some((u) => u.pathname === `${BASE}/mismatches/${MM}` && u.searchParams.get('stream') === 'payment_statement')).toBe(true))
    server.use(http.get(`${BASE}/mismatches/${MM}`, () => refusal(404, 'not_found')))
    await user.click(screen.getByRole('button', { name: 'Close detail' }))
    await user.click(screen.getByRole('button', { name: `Open mismatch ${MM.slice(0, 8)}` }))
    const d = await screen.findByRole('region', { name: 'Finding detail' })
    expect(await within(d).findByRole('alert')).toBeInTheDocument()
    expect(document.body).not.toHaveTextContent('do-not-render')
  })

  it('casino streams still mark which loaded findings belong to the selected run (their route has no run filter)', async () => {
    signInAs(TA)
    const OTHER_RUN = '71000000-0000-4000-8000-000000000009'
    server.use(
      http.get('/v1/admin/casino/reconciliation/runs', () => paged([run({ stream: 'casino_consistency', status: 'clean' })])),
      http.get('/v1/admin/casino/reconciliation/mismatches', () => paged([
        { id: MM, run_id: RUN, mismatch_kind: 'cas_orphan_win', reconciliation_key: 'k', expected_value: 'e', actual_value: 'a', investigation_status: 'open', created_at: '2026-10-10T01:00:00Z', resolved_at: null },
        { id: '72000000-0000-4000-8000-000000000002', run_id: OTHER_RUN, mismatch_kind: 'cas_orphan_win', reconciliation_key: 'k2', expected_value: 'e', actual_value: 'a', investigation_status: 'open', created_at: '2026-10-10T01:00:00Z', resolved_at: null },
      ])),
      http.get(`${BASE}/runs`, () => paged([])),
      http.get(`${BASE}/mismatches`, () => paged([])),
    )
    renderWithProviders(<AppRoutes />, { route: '/reconciliation' })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('tab', { name: 'Casino consistency' }))
    await user.click(await screen.findByRole('button', { name: `Open run ${RUN.slice(0, 8)}` }))
    const d = await screen.findByRole('region', { name: 'Run detail' })
    expect(within(d).getByText(/cannot filter findings by run/)).toBeInTheDocument()
    const rows = within(screen.getAllByRole('table')[1]).getAllByRole('row').slice(1)
    expect(rows[0]).toHaveTextContent('Yes')
    expect(rows[1]).toHaveTextContent('No')
  })

  it('a server refusal of the stream is shown through the closed map (403 and a 400)', async () => {
    signInAs(FI)
    server.use(http.get(`${BASE}/runs`, () => refusal(403, 'forbidden')), http.get(`${BASE}/mismatches`, () => refusal(400, 'validation_error')))
    renderWithProviders(<AppRoutes />, { route: '/reconciliation' })
    const alerts = await screen.findAllByRole('alert')
    await waitFor(() => expect(screen.getAllByRole('alert').length).toBe(2))
    expect(alerts[0]).toHaveTextContent('The server refused this')
    expect(document.body).not.toHaveTextContent('do-not-render')
  })

  it('is gated: support has no section and a typed URL shows NotAuthorized without any request', async () => {
    signInAs({ role: 'support', tenant_id: TENANT, sub: 's' })
    let calls = 0
    server.use(http.get(/reconciliation/, () => { calls++; return paged([]) }))
    renderWithProviders(<AppRoutes />, { route: '/reconciliation' })
    expect(await screen.findByText('Your role does not have access to this section.')).toBeInTheDocument()
    expect(calls).toBe(0)
  })
})
