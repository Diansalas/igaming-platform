import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { Route, Routes } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { PLATFORM_ADMIN_CLAIMS, signInAs } from '../../test/session'
import { TenantDetailPage } from './TenantDetailPage'
import { TenantListPage } from './TenantListPage'

const TENANT_ID = '11111111-1111-1111-1111-111111111111'
const PERSON_1 = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'

function tenantReadHandlers() {
  server.use(
    http.get('/v1/admin/tenants', () =>
      HttpResponse.json({
        items: [{ id: TENANT_ID, name: 'Acme', slug: 'acme', licensing_model: 'own_licence', status: 'active' }],
        limit: 20,
        offset: 0,
        total: 1,
      }),
    ),
    http.get('/v1/admin/tenants/:tid', ({ params }) =>
      HttpResponse.json({ id: params.tid, name: 'Acme', slug: 'acme', licensing_model: 'own_licence', status: 'active' }),
    ),
    http.get('/v1/admin/tenants/:tid/brands', () => HttpResponse.json({ items: [], limit: 20, offset: 0, total: 0 })),
    http.get('/v1/admin/tenants/:tid/staff', () => HttpResponse.json({ staff: [], truncated: false })),
  )
}

function renderDetail() {
  return renderWithProviders(
    <Routes>
      <Route path="/tenants/:tenantId" element={<TenantDetailPage />} />
    </Routes>,
    { route: `/tenants/${TENANT_ID}` },
  )
}

describe('Create tenant (platform_admin)', () => {
  it('POSTs /v1/admin/tenants with name/slug/licensing_model/reason_code and shows the new tenant id', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    tenantReadHandlers()
    let body: unknown
    server.use(
      http.post('/v1/admin/tenants', async ({ request }) => {
        body = await request.json()
        return HttpResponse.json(
          { id: 'new-tenant-id', name: 'Beta', slug: 'beta', licensing_model: 'own_licence', status: 'active' },
          { status: 201 },
        )
      }),
    )
    renderWithProviders(<TenantListPage />, { route: '/tenants' })
    const user = userEvent.setup()
    await user.type(await screen.findByLabelText('Tenant name'), 'Beta')
    await user.type(screen.getByLabelText('Tenant slug'), 'beta')
    await user.selectOptions(screen.getByLabelText('Licensing model'), 'own_licence')
    await user.type(screen.getByLabelText(/Reason code/), 'staging_acceptance')
    await user.click(screen.getByRole('button', { name: 'Create tenant' }))

    expect(await screen.findByText('new-tenant-id')).toBeInTheDocument()
    expect(body).toEqual({ name: 'Beta', slug: 'beta', licensing_model: 'own_licence', reason_code: 'staging_acceptance' })
  })

  it('shows a 409 conflict message with request id', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    tenantReadHandlers()
    server.use(
      http.post('/v1/admin/tenants', () =>
        HttpResponse.json({ code: 'conflict', message: 'a tenant with this slug already exists', request_id: 'req-t' }, { status: 409 }),
      ),
    )
    renderWithProviders(<TenantListPage />, { route: '/tenants' })
    const user = userEvent.setup()
    await user.type(await screen.findByLabelText('Tenant name'), 'Acme')
    await user.type(screen.getByLabelText('Tenant slug'), 'acme')
    await user.type(screen.getByLabelText(/Reason code/), 'x')
    await user.click(screen.getByRole('button', { name: 'Create tenant' }))
    expect(await screen.findByText('a tenant with this slug already exists')).toBeInTheDocument()
    expect(screen.getByText('Request ID: req-t')).toBeInTheDocument()
  })

  it('is not rendered for tenant_admin', async () => {
    signInAs({ role: 'tenant_admin' })
    tenantReadHandlers()
    renderWithProviders(<TenantListPage />, { route: '/tenants' })
    await screen.findByText('Acme')
    expect(screen.queryByRole('button', { name: 'Create tenant' })).not.toBeInTheDocument()
  })
})

describe('Create brand', () => {
  it('POSTs /v1/admin/tenants/{tid}/brands with {name, slug} and shows the brand id', async () => {
    signInAs({ role: 'tenant_admin' })
    tenantReadHandlers()
    const calls: { path: string; body: unknown }[] = []
    server.use(
      http.post('/v1/admin/tenants/:tid/brands', async ({ request }) => {
        calls.push({ path: new URL(request.url).pathname, body: await request.json() })
        return HttpResponse.json(
          { id: 'brand-new', tenant_id: TENANT_ID, name: 'Main', slug: 'main', status: 'active' },
          { status: 201 },
        )
      }),
    )
    renderDetail()
    const user = userEvent.setup()
    await user.type(await screen.findByLabelText('Brand name'), 'Main')
    await user.type(screen.getByLabelText('Brand slug'), 'main')
    await user.click(screen.getByRole('button', { name: 'Create brand' }))
    expect(await screen.findByText('brand-new')).toBeInTheDocument()
    expect(calls).toEqual([{ path: `/v1/admin/tenants/${TENANT_ID}/brands`, body: { name: 'Main', slug: 'main' } }])
  })
})

describe('Staff forms on the tenant page', () => {
  it('person-link POSTs {person_id} to .../staff/{staffID}/person-link', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    tenantReadHandlers()
    const calls: { path: string; body: unknown }[] = []
    server.use(
      http.post('/v1/admin/tenants/:tid/staff/:sid/person-link', async ({ request }) => {
        calls.push({ path: new URL(request.url).pathname, body: await request.json() })
        return new HttpResponse(null, { status: 204 })
      }),
    )
    renderDetail()
    const user = userEvent.setup()
    await user.type(await screen.findByLabelText('Staff ID to link'), 'staff-9')
    await user.type(screen.getByLabelText(/Person ID to link/), PERSON_1)
    await user.click(screen.getByRole('button', { name: 'Link person' }))
    expect(await screen.findByRole('status')).toHaveTextContent(`Staff staff-9 linked to person ${PERSON_1}.`)
    expect(calls).toEqual([{ path: `/v1/admin/tenants/${TENANT_ID}/staff/staff-9/person-link`, body: { person_id: PERSON_1 } }])
  })

  it('compliance sees no staff or brand forms', async () => {
    signInAs({ role: 'compliance' })
    tenantReadHandlers()
    renderDetail()
    await screen.findByText('Acme')
    // staff:read lets compliance see the directory, but no invite / link / brand form (staff:manage / brand:write).
    expect(await screen.findByText('Staff')).toBeInTheDocument()
    expect(screen.queryByText('Invite staff user')).not.toBeInTheDocument()
    expect(screen.queryByText('Link staff to person')).not.toBeInTheDocument()
    expect(screen.queryByText('Create brand')).not.toBeInTheDocument()
  })
})
