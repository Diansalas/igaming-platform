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
        return HttpResponse.json({ disclaimer: API_DISCLAIMER, items: [r] })
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
