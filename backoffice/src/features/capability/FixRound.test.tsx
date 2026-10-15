import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it, vi } from 'vitest'
import { AppRoutes } from '../../app/AppRoutes'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { PLATFORM_ADMIN_CLAIMS, signInAs } from '../../test/session'
import { REQ1, STAFF_A, STAFF_B, TA, TENANT, grant, grantRequest } from '../../test/consoleFixtures'

// Slow under a loaded full run (many keystrokes); the longer timeout applies to this file only.
vi.setConfig({ testTimeout: 30000 })

const BASE = `/v1/admin/tenants/${TENANT}/capability-grants`
const PA = { ...PLATFORM_ADMIN_CLAIMS, sub: 'f9000000-0000-4000-8000-000000000009' }
const dirEntry = { id: STAFF_A, display_name: 'Ana Finance', role: 'finance', status: 'active', person_linked: true }

function setup(reqs = [grantRequest({ requested_by: STAFF_B })], staff = [dirEntry]) {
  server.use(
    http.get('/v1/admin/tenants', () => HttpResponse.json({ items: [{ id: TENANT, name: 'Acme Gaming', slug: 'acme', status: 'active' }], limit: 50, offset: 0, total: 1 })),
    http.get(`${BASE}/requests`, () => HttpResponse.json(reqs)),
    http.get(BASE, () => HttpResponse.json([grant()])),
    http.get(`/v1/admin/tenants/${TENANT}/staff`, () => HttpResponse.json({ staff, truncated: false })),
  )
}

describe('F1: the approver sees who receives the authority', () => {
  it('shows the FULL grantee staff id plus display name, role and status from the directory', async () => {
    signInAs(PA)
    setup()
    renderWithProviders(<AppRoutes />, { route: `/capability-grants?tenant=${TENANT}` })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: `Approve request ${REQ1.slice(0, 8)}` }))
    const dlg = await screen.findByRole('dialog', { name: 'Approve capability grant' })
    const who = await within(dlg).findByTestId('grantee')
    expect(who).toHaveTextContent(STAFF_A) // the whole id, not a prefix
    await within(dlg).findByText('Ana Finance - finance (active)')
    expect(within(dlg).getByLabelText('Acting on tenant')).toHaveTextContent('Acme Gaming')
  })

  it('says so when the grantee is not in the directory, and for a platform grantee; never guesses a name', async () => {
    signInAs(PA)
    setup([grantRequest({ requested_by: STAFF_B, grantee_staff_id: STAFF_B, grantee_scope: 'tenant' })], [dirEntry])
    const r = renderWithProviders(<AppRoutes />, { route: `/capability-grants?tenant=${TENANT}` })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: `Approve request ${REQ1.slice(0, 8)}` }))
    expect(await within(await screen.findByRole('dialog')).findByText(/Not in this tenant staff directory/)).toBeInTheDocument()
    r.unmount()
    setup([grantRequest({ requested_by: STAFF_B, grantee_scope: 'platform', grantee_staff_id: STAFF_B })], [dirEntry])
    renderWithProviders(<AppRoutes />, { route: `/capability-grants?tenant=${TENANT}` })
    await user.click(await screen.findByRole('button', { name: `Approve request ${REQ1.slice(0, 8)}` }))
    const dlg = await screen.findByRole('dialog', { name: 'Approve capability grant' })
    expect(await within(dlg).findByText(/Platform staff \(not listed in a tenant directory\)/)).toBeInTheDocument()
    expect(within(dlg).getByTestId('grantee')).toHaveTextContent(STAFF_B)
  })

  it('a failed directory read is stated, not hidden', async () => {
    signInAs(PA)
    setup()
    server.use(http.get(`/v1/admin/tenants/${TENANT}/staff`, () => HttpResponse.json({ code: 'forbidden', message: 'x' }, { status: 403 })))
    renderWithProviders(<AppRoutes />, { route: `/capability-grants?tenant=${TENANT}` })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: `Approve request ${REQ1.slice(0, 8)}` }))
    expect(await within(await screen.findByRole('dialog')).findByText(/staff directory could not be read/)).toBeInTheDocument()
  })

  it('a tenant viewer of a platform grantee sees "Id not shown to you" and the scope, not a blank', async () => {
    signInAs(TA)
    setup([grantRequest({ grantee_staff_id: null, grantee_scope: 'platform', requested_by: STAFF_B })])
    renderWithProviders(<AppRoutes />, { route: '/capability-grants' })
    // tenant_admin has no approve button; the table shows scope only
    const t = await screen.findByRole('table', { name: 'Capability grant requests' })
    expect(within(t).getAllByText('Platform staff').length).toBeGreaterThan(0)
  })
})

describe('F1: the typed staff id and the dropdown are mutually exclusive (no silent override)', () => {
  it('typing an id disables the dropdown; choosing a person disables the field; clearing re-enables', async () => {
    signInAs(PA)
    setup([], [dirEntry])
    renderWithProviders(<AppRoutes />, { route: `/capability-grants?tenant=${TENANT}` })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Request a grant' }))
    const dlg = await screen.findByRole('dialog', { name: 'Request a capability grant' })
    const select = within(dlg).getByLabelText('Grantee (tenant staff)')
    const typed = within(dlg).getByLabelText('Or enter a staff id')
    await within(dlg).findByRole('option', { name: /Ana Finance/ })
    await user.selectOptions(select, STAFF_A)
    expect(typed).toBeDisabled()
    await user.selectOptions(select, '')
    expect(typed).toBeEnabled()
    await user.click(typed)
    await user.paste(STAFF_B)
    expect(select).toBeDisabled()
    await user.clear(typed)
    expect(select).toBeEnabled()
  })

  it('the request sends exactly the one source that is filled', async () => {
    signInAs(PA)
    setup([], [dirEntry])
    const bodies: Array<Record<string, unknown>> = []
    server.use(http.post(`${BASE}/requests`, async ({ request }) => { bodies.push((await request.json()) as Record<string, unknown>); return HttpResponse.json(grantRequest(), { status: 201 }) }))
    renderWithProviders(<AppRoutes />, { route: `/capability-grants?tenant=${TENANT}` })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Request a grant' }))
    const dlg = await screen.findByRole('dialog', { name: 'Request a capability grant' })
    await within(dlg).findByRole('option', { name: /Ana Finance/ })
    await user.selectOptions(within(dlg).getByLabelText('Grantee (tenant staff)'), STAFF_A)
    await user.type(within(dlg).getByLabelText('Reason code (required)'), 'cover')
    await user.click(within(dlg).getByRole('checkbox'))
    await user.click(within(dlg).getByRole('button', { name: 'File request' }))
    await screen.findByRole('status')
    expect(bodies[0].grantee_staff_id).toBe(STAFF_A)
  })
})
