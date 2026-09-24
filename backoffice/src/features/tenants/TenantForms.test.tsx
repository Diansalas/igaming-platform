import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { Route, Routes } from 'react-router-dom'
import { describe, expect, it, vi } from 'vitest'
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

function roleOptions(): string[] {
  const select = screen.getByLabelText('Role') as HTMLSelectElement
  return Array.from(select.options).map((o) => o.value)
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

describe('Create staff', () => {
  it('platform_admin sees every role and can create a finance user with person_id; the ids are shown', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    tenantReadHandlers()
    const calls: { path: string; body: unknown }[] = []
    server.use(
      http.post('/v1/admin/tenants/:tid/staff', async ({ request }) => {
        calls.push({ path: new URL(request.url).pathname, body: await request.json() })
        return HttpResponse.json(
          { id: 'staff-fin-1', tenant_id: TENANT_ID, email: 'fin1@example.com', role: 'finance', status: 'active', person_id: PERSON_1 },
          { status: 201 },
        )
      }),
    )
    renderDetail()
    const user = userEvent.setup()
    await screen.findByLabelText('Staff email')
    expect(roleOptions()).toEqual([
      'tenant_admin',
      'support',
      'compliance',
      'finance',
      'risk_manager',
      'promotions_manager',
      'bonus_operations',
    ])
    expect(screen.getAllByText(/Person IDs come from `deploy.sh seed-admin <email>` output/).length).toBeGreaterThan(0)

    await user.type(screen.getByLabelText('Staff email'), 'fin1@example.com')
    await user.type(screen.getByLabelText('Initial password'), 'long-enough-pw')
    await user.selectOptions(screen.getByLabelText('Role'), 'finance')
    await user.type(screen.getByLabelText('Person ID (optional)'), PERSON_1)
    await user.click(screen.getByRole('button', { name: 'Create staff user' }))

    const result = await screen.findByText(/Staff user created - note these values now/)
    const panel = result.closest('div') as HTMLElement
    expect(within(panel).getByText('staff-fin-1')).toBeInTheDocument()
    expect(within(panel).getByText(PERSON_1)).toBeInTheDocument()
    expect(calls).toEqual([
      {
        path: `/v1/admin/tenants/${TENANT_ID}/staff`,
        body: { email: 'fin1@example.com', password: 'long-enough-pw', role: 'finance', person_id: PERSON_1 },
      },
    ])
    // The password never lingers in the form after success.
    expect(screen.getByLabelText('Initial password')).toHaveValue('')
  })

  it('tenant_admin role options exclude finance and the other platform-only roles', async () => {
    signInAs({ role: 'tenant_admin' })
    tenantReadHandlers()
    renderDetail()
    await screen.findByLabelText('Staff email')
    expect(roleOptions()).toEqual(['tenant_admin', 'support', 'compliance'])
  })

  it('omits person_id when blank and blocks submission for a short password (client-side mirror of the 8-char minimum)', async () => {
    signInAs({ role: 'tenant_admin' })
    tenantReadHandlers()
    let body: unknown
    server.use(
      http.post('/v1/admin/tenants/:tid/staff', async ({ request }) => {
        body = await request.json()
        return HttpResponse.json({ id: 's2', email: 'c@example.com', role: 'compliance', status: 'active' }, { status: 201 })
      }),
    )
    renderDetail()
    const user = userEvent.setup()
    await user.type(await screen.findByLabelText('Staff email'), 'c@example.com')
    await user.type(screen.getByLabelText('Initial password'), 'short')
    await user.selectOptions(screen.getByLabelText('Role'), 'compliance')
    expect(screen.getByText('Password must be at least 8 characters.')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Create staff user' })).toBeDisabled()

    await user.type(screen.getByLabelText('Initial password'), '-now-long')
    await user.click(screen.getByRole('button', { name: 'Create staff user' }))
    expect(await screen.findByText('s2')).toBeInTheDocument()
    expect(body).toEqual({ email: 'c@example.com', password: 'short-now-long', role: 'compliance' })
    expect(screen.getByText('not linked')).toBeInTheDocument()
  })

  it('shows a 403 from the backend verbatim', async () => {
    signInAs({ role: 'tenant_admin' })
    tenantReadHandlers()
    server.use(
      http.post('/v1/admin/tenants/:tid/staff', () =>
        HttpResponse.json(
          { code: 'forbidden', message: 'cannot act on a different tenant', request_id: 'req-s' },
          { status: 403 },
        ),
      ),
    )
    renderDetail()
    const user = userEvent.setup()
    await user.type(await screen.findByLabelText('Staff email'), 'x@example.com')
    await user.type(screen.getByLabelText('Initial password'), 'password123')
    await user.click(screen.getByRole('button', { name: 'Create staff user' }))
    expect(await screen.findByText('cannot act on a different tenant')).toBeInTheDocument()
    expect(screen.getByText('Request ID: req-s')).toBeInTheDocument()
  })

  it('creates exactly once on a double submit', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    tenantReadHandlers()
    const spy = vi.fn()
    server.use(
      http.post('/v1/admin/tenants/:tid/staff', async () => {
        spy()
        await new Promise((r) => setTimeout(r, 20))
        return HttpResponse.json({ id: 's3', email: 'd@example.com', role: 'support', status: 'active' }, { status: 201 })
      }),
    )
    renderDetail()
    const user = userEvent.setup()
    await user.type(await screen.findByLabelText('Staff email'), 'd@example.com')
    await user.type(screen.getByLabelText('Initial password'), 'password123')
    const button = screen.getByRole('button', { name: 'Create staff user' })
    fireEvent.click(button)
    fireEvent.click(button)
    expect(await screen.findByText('s3')).toBeInTheDocument()
    expect(spy).toHaveBeenCalledTimes(1)
  })

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
    expect(screen.queryByText('Create staff user')).not.toBeInTheDocument()
    expect(screen.queryByText('Create brand')).not.toBeInTheDocument()
  })
})
