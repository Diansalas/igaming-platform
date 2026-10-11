import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { Route, Routes, useLocation } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import { RequireNavPermission } from '../../auth/RequireNavPermission'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { PLATFORM_ADMIN_CLAIMS, signInAs } from '../../test/session'
import { LaunchQueuePage } from './LaunchQueuePage'
import { LAUNCH_DISCLAIMER } from './labels'
import { API_DISCLAIMER, BRAND_ID, TENANT_ID, launchRequest } from './launchFixtures'

function Where() {
  const l = useLocation()
  return <p data-testid="where">{l.pathname + l.search}</p>
}

function renderQueue() {
  return renderWithProviders(
    <Routes>
      <Route
        path="/launch-queue"
        element={
          <RequireNavPermission permission="launchQueue">
            <LaunchQueuePage />
          </RequireNavPermission>
        }
      />
      <Route path="*" element={<Where />} />
    </Routes>,
    { route: '/launch-queue' },
  )
}

describe('platform launch queue', () => {
  it('lists requests with the disclaimer, defaults to pending, and opens the subject with the request selected', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    const statuses: (string | null)[] = []
    const r = launchRequest({ required_approvals: 2, action: 'close', reason_code: 'offboarding' })
    server.use(
      http.get('/v1/admin/launch-authorisations', ({ request }) => {
        statuses.push(new URL(request.url).searchParams.get('status'))
        return HttpResponse.json({ disclaimer: API_DISCLAIMER, items: [r], next_cursor: null })
      }),
    )
    renderQueue()
    await screen.findByText('Close')
    expect(statuses[0]).toBe('pending')
    expect(screen.getByRole('note', { name: 'Disclaimer' })).toHaveTextContent(LAUNCH_DISCLAIMER)
    expect(screen.getByText('2 approvals')).toBeInTheDocument()
    expect(screen.getAllByText('Pending approval').length).toBeGreaterThan(1) // filter option + row badge

    const user = userEvent.setup()
    await user.selectOptions(screen.getByLabelText('Request status'), 'rejected')
    await waitFor(() => expect(statuses).toContain('rejected'))

    await user.click(await screen.findByText('Close'))
    expect(await screen.findByTestId('where')).toHaveTextContent(`/tenants/${TENANT_ID}/brands/${BRAND_ID}?request=${r.id}`)
  })

  it('shows tenant and brand names, the per-caller answer, and pages with the server cursor (50 per page)', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    const seen: { limit: string | null; cursor: string | null }[] = []
    const p1 = launchRequest({ id: '10000000-0000-4000-8000-000000000001', tenant_name: 'Acme Casino', brand_name: 'Brand One', can_approve: true })
    const p2 = launchRequest({
      id: '10000000-0000-4000-8000-000000000002', subject_kind: 'tenant', brand_id: undefined, action: 'close', reason_code: 'offboarding',
      tenant_name: 'Beta Games', can_approve: false, cannot_approve_reason: 'self_approval',
    })
    server.use(
      http.get('/v1/admin/launch-authorisations', ({ request }) => {
        const u = new URL(request.url)
        seen.push({ limit: u.searchParams.get('limit'), cursor: u.searchParams.get('cursor') })
        return u.searchParams.get('cursor') === 'CUR1'
          ? HttpResponse.json({ disclaimer: API_DISCLAIMER, items: [p2], next_cursor: null })
          : HttpResponse.json({ disclaimer: API_DISCLAIMER, items: [p1], next_cursor: 'CUR1' })
      }),
    )
    renderQueue()
    expect(await screen.findByText('Acme Casino')).toBeInTheDocument()
    expect(screen.getByText('Brand: Brand One')).toBeInTheDocument()
    expect(screen.getByText('Can approve')).toBeInTheDocument()
    expect(screen.queryByText('Beta Games')).not.toBeInTheDocument()
    expect(seen[0]).toEqual({ limit: '50', cursor: null })

    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Load more' }))
    expect(await screen.findByText('Beta Games')).toBeInTheDocument()
    expect(seen[1]).toEqual({ limit: '50', cursor: 'CUR1' })
    expect(screen.getByText('You filed it')).toBeInTheDocument() // mirrors cannot_approve_reason self_approval
    expect(screen.getByText('Acme Casino')).toBeInTheDocument() // page 1 kept
    expect(screen.queryByRole('button', { name: 'Load more' })).not.toBeInTheDocument() // next_cursor null ends it
  })

  it('falls back to short ids when the names are absent and never invents an answer for a missing can_approve', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    server.use(
      http.get('/v1/admin/launch-authorisations', () =>
        HttpResponse.json({ disclaimer: API_DISCLAIMER, items: [launchRequest()], next_cursor: null }),
      ),
    )
    renderQueue()
    await screen.findByText(TENANT_ID.slice(0, 8))
    expect(screen.queryByText('Can approve')).not.toBeInTheDocument()
    expect(screen.queryByText('Not an approver')).not.toBeInTheDocument()
  })

  it('a tenant role never sees the page (route gate) and never calls the queue endpoint', async () => {
    signInAs({ role: 'tenant_admin', tenant_id: TENANT_ID })
    let called = false
    server.use(
      http.get('/v1/admin/launch-authorisations', () => {
        called = true
        return HttpResponse.json({ items: [] })
      }),
    )
    renderQueue()
    await waitFor(() => expect(screen.queryByText('Launch queue')).not.toBeInTheDocument())
    expect(called).toBe(false)
  })

  it('degrades correctly when the server refuses a caller the UI let through (403): fixed message, no raw text', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    server.use(
      http.get('/v1/admin/launch-authorisations', () => HttpResponse.json({ code: 'forbidden', message: 'RAW scope detail' }, { status: 403 })),
    )
    renderQueue()
    const alert = await screen.findByRole('alert')
    expect(within(alert).getByText(/You are not allowed to do this/)).toBeInTheDocument()
    expect(document.body.textContent).not.toContain('RAW scope detail')
  })

  it('shows the 503 unavailable state with a fixed sentence', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    server.use(
      http.get('/v1/admin/launch-authorisations', () => HttpResponse.json({ code: 'LAUNCH_AUTHORISATION_UNAVAILABLE', message: 'RAW wiring' }, { status: 503 })),
    )
    renderQueue()
    expect(await screen.findByText(/launch authorisation service is not available/i)).toBeInTheDocument()
  })
})
