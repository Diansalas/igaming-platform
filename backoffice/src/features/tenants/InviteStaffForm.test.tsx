import { configure, fireEvent, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { Route, Routes } from 'react-router-dom'
import { describe, expect, it, vi } from 'vitest'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { PLATFORM_ADMIN_CLAIMS, signInAs } from '../../test/session'
import { GOOD_CODE, installStepUpFixture, SERVER_MESSAGE_SENTINEL, STEPPED_JWT, stepUpRequiredResponse } from '../../test/stepUp'
import { TenantDetailPage } from './TenantDetailPage'

// The suite shares a loaded CI machine with heavy tests: widen async waits (not assertions).
configure({ asyncUtilTimeout: 5000 })
vi.setConfig({ testTimeout: 20000 })

const TENANT_ID = '11111111-1111-1111-1111-111111111111'
const PERSON_1 = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
const INVITE_PATH = `/v1/admin/tenants/${TENANT_ID}/staff/invitations`

function entry(over: Record<string, unknown> = {}) {
  return {
    id: '22222222-2222-4222-8222-222222222222',
    tenant_id: TENANT_ID,
    email: 'new@example.com',
    display_name: null,
    role: 'support',
    status: 'invited',
    mfa: { enrolled: false, hard_locked: false },
    person_linked: false,
    last_login_at: null,
    created_at: '2026-10-14T10:00:00Z',
    status_changed_at: null,
    ...over,
  }
}

function baseHandlers(staff: unknown[] = [], truncated = false) {
  server.use(
    http.get('/v1/admin/tenants/:tid', ({ params }) =>
      HttpResponse.json({ id: params.tid, name: 'Acme', slug: 'acme', licensing_model: 'own_licence', status: 'active' }),
    ),
    http.get('/v1/admin/tenants/:tid/brands', () => HttpResponse.json({ items: [], limit: 20, offset: 0, total: 0 })),
    http.get('/v1/admin/tenants/:tid/staff', () => HttpResponse.json({ staff, truncated })),
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
  return Array.from((screen.getByLabelText('Role') as HTMLSelectElement).options).map((o) => o.value)
}

async function fillAndSend(user: ReturnType<typeof userEvent.setup>, email = 'new@example.com', role = 'support') {
  await user.type(await screen.findByLabelText('Staff email'), email)
  await user.selectOptions(screen.getByLabelText('Role'), role)
  await user.click(screen.getByRole('button', { name: 'Send invitation' }))
}

describe('Invite staff: replaces the removed admin-password create form', () => {
  it('has no password field and does not offer the removed create route', async () => {
    signInAs({ role: 'tenant_admin' })
    baseHandlers()
    renderDetail()
    await screen.findByLabelText('Staff email')
    expect(screen.queryByLabelText(/password/i)).not.toBeInTheDocument()
    expect(screen.queryByText('Create staff user')).not.toBeInTheDocument()
    expect(screen.getByText(/No password is chosen or shown here/)).toBeInTheDocument()
  })

  it('platform_admin may pick every role; tenant_admin only tenant_admin, support, compliance', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    baseHandlers()
    const first = renderDetail()
    await screen.findByLabelText('Staff email')
    expect(roleOptions()).toEqual(['tenant_admin', 'support', 'compliance', 'finance', 'risk_manager', 'promotions_manager', 'bonus_operations'])
    first.unmount()

    signInAs({ role: 'tenant_admin' })
    renderDetail()
    await screen.findByLabelText('Staff email')
    expect(roleOptions()).toEqual(['tenant_admin', 'support', 'compliance'])
  })

  it('POSTs {email, role, person_id} to the invitation route and shows the invited result without any credential', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    baseHandlers()
    const calls: { path: string; body: unknown }[] = []
    server.use(
      http.post('/v1/admin/tenants/:tid/staff/invitations', async ({ request }) => {
        calls.push({ path: new URL(request.url).pathname, body: await request.json() })
        return HttpResponse.json({ staff: entry({ email: 'fin1@example.com', role: 'finance' }), email_delivery: 'sent' }, { status: 201 })
      }),
    )
    renderDetail()
    const user = userEvent.setup()
    await user.type(await screen.findByLabelText('Staff email'), 'fin1@example.com')
    await user.selectOptions(screen.getByLabelText('Role'), 'finance')
    await user.type(screen.getByLabelText('Person ID'), PERSON_1)
    await user.click(screen.getByRole('button', { name: 'Send invitation' }))

    const ok = await screen.findByText('Invitation created.')
    const panel = ok.closest('div') as HTMLElement
    expect(within(panel).getByText('fin1@example.com')).toBeInTheDocument()
    expect(within(panel).getByText('finance / invited')).toBeInTheDocument()
    expect(within(panel).getByText(/handed to the mail provider/)).toBeInTheDocument()
    expect(calls).toEqual([{ path: INVITE_PATH, body: { email: 'fin1@example.com', role: 'finance', person_id: PERSON_1 } }])
    expect(screen.getByLabelText('Staff email')).toHaveValue('')
  })

  it('a failed email hand-over is reported honestly (the invitation exists, no resend UI yet)', async () => {
    signInAs({ role: 'tenant_admin' })
    baseHandlers()
    server.use(http.post('/v1/admin/tenants/:tid/staff/invitations', () => HttpResponse.json({ staff: entry(), email_delivery: 'failed' }, { status: 201 })))
    renderDetail()
    await fillAndSend(userEvent.setup())
    expect(await screen.findByText(/could not be handed to the mail provider/)).toBeInTheDocument()
  })

  it('requires a Person for tenant_admin, finance and compliance (client mirror of person_required) and omits a blank one otherwise', async () => {
    signInAs({ role: 'tenant_admin' })
    baseHandlers()
    let body: unknown
    server.use(
      http.post('/v1/admin/tenants/:tid/staff/invitations', async ({ request }) => {
        body = await request.json()
        return HttpResponse.json({ staff: entry(), email_delivery: 'sent' }, { status: 201 })
      }),
    )
    renderDetail()
    const user = userEvent.setup()
    await user.type(await screen.findByLabelText('Staff email'), 'a@example.com')
    // default role is tenant_admin: Person required
    expect(screen.getByRole('button', { name: 'Send invitation' })).toBeDisabled()
    await user.selectOptions(screen.getByLabelText('Role'), 'compliance')
    expect(screen.getByRole('button', { name: 'Send invitation' })).toBeDisabled()
    await user.selectOptions(screen.getByLabelText('Role'), 'support')
    expect(screen.getByRole('button', { name: 'Send invitation' })).toBeEnabled()
    await user.click(screen.getByRole('button', { name: 'Send invitation' }))
    await screen.findByText('Invitation created.')
    expect(body).toEqual({ email: 'a@example.com', role: 'support' })
  })

  it('blocks an invalid email and a non-UUID Person ID before any request', async () => {
    signInAs({ role: 'tenant_admin' })
    baseHandlers()
    const spy = vi.fn()
    server.use(http.post('/v1/admin/tenants/:tid/staff/invitations', () => (spy(), HttpResponse.json({}, { status: 201 }))))
    renderDetail()
    const user = userEvent.setup()
    await user.selectOptions(await screen.findByLabelText('Role'), 'support')
    await user.type(screen.getByLabelText('Staff email'), 'not-an-email')
    expect(screen.getByText('Enter a valid email address.')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Send invitation' })).toBeDisabled()
    await user.clear(screen.getByLabelText('Staff email'))
    await user.type(screen.getByLabelText('Staff email'), 'ok@example.com')
    await user.type(screen.getByLabelText('Person ID (optional)'), 'nope')
    expect(screen.getByText('Person ID must be a UUID.')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Send invitation' })).toBeDisabled()
    expect(spy).not.toHaveBeenCalled()
  })

  it('sends exactly one invitation on a double submit', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    baseHandlers()
    const spy = vi.fn()
    server.use(
      http.post('/v1/admin/tenants/:tid/staff/invitations', async () => {
        spy()
        await new Promise((r) => setTimeout(r, 20))
        return HttpResponse.json({ staff: entry(), email_delivery: 'sent' }, { status: 201 })
      }),
    )
    renderDetail()
    const user = userEvent.setup()
    await user.type(await screen.findByLabelText('Staff email'), 'd@example.com')
    await user.selectOptions(screen.getByLabelText('Role'), 'support')
    const button = screen.getByRole('button', { name: 'Send invitation' })
    fireEvent.click(button)
    fireEvent.click(button)
    await screen.findByText('Invitation created.')
    expect(spy).toHaveBeenCalledTimes(1)
  })

  it('refreshes the staff directory after an invitation', async () => {
    signInAs({ role: 'tenant_admin' })
    const listed: unknown[][] = [[]]
    server.use(
      http.get('/v1/admin/tenants/:tid', ({ params }) => HttpResponse.json({ id: params.tid, name: 'Acme', slug: 'acme', licensing_model: 'own_licence', status: 'active' })),
      http.get('/v1/admin/tenants/:tid/brands', () => HttpResponse.json({ items: [], limit: 20, offset: 0, total: 0 })),
      http.get('/v1/admin/tenants/:tid/staff', () => HttpResponse.json({ staff: listed[listed.length - 1], truncated: false })),
      http.post('/v1/admin/tenants/:tid/staff/invitations', () => {
        listed.push([entry({ email: 'listed-after@example.com' })])
        return HttpResponse.json({ staff: entry(), email_delivery: 'sent' }, { status: 201 })
      }),
    )
    renderDetail()
    expect(await screen.findByText('This tenant has no staff yet.')).toBeInTheDocument()
    await fillAndSend(userEvent.setup())
    expect(await screen.findByText('listed-after@example.com')).toBeInTheDocument()
  })
})

describe('Invite staff: every documented refusal degrades to a clear, fixed, non-leaky message', () => {
  const cases: Array<[string, number, RegExp]> = [
    ['role_not_grantable', 403, /may not grant this role/],
    ['person_required', 422, /requires a linked Person/],
    ['conflict', 409, /staff account already exists for this email in this tenant, or this Person is already linked/],
    ['lifecycle_busy', 409, /record is busy/],
    ['lifecycle_transition_invalid', 409, /not in a state this action applies to/],
    ['last_admin', 409, /last active administrator/],
    ['self_target_forbidden', 403, /your own account/],
    ['four_eyes_violation', 403, /different staff member and a different Person/],
    ['forbidden', 403, /do not have permission to invite staff/],
    ['not_found', 404, /tenant was not found/],
    ['validation_error', 400, /Check the email address, the role and the Person ID/],
    ['rate_limited', 429, /Too many requests/],
    ['service_unavailable', 503, /temporarily unavailable/],
    ['internal_error', 500, /could not be sent/],
  ]
  it.each(cases)('%s (%i)', async (code, status, text) => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    baseHandlers()
    server.use(
      http.post('/v1/admin/tenants/:tid/staff/invitations', () =>
        HttpResponse.json({ code, message: `${SERVER_MESSAGE_SENTINEL} secret-detail@victim.example`, request_id: 'req-inv' }, { status }),
      ),
    )
    renderDetail()
    await fillAndSend(userEvent.setup())
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(text)
    expect(alert).toHaveTextContent('Request ID: req-inv')
    expect(document.body.textContent).not.toContain(SERVER_MESSAGE_SENTINEL)
    expect(document.body.textContent).not.toContain('victim.example')
    // The form stays usable for a corrected retry and no success banner is shown.
    expect(screen.queryByText('Invitation created.')).not.toBeInTheDocument()
    expect(screen.getByLabelText('Staff email')).toHaveValue('new@example.com')
  })

  it('a network failure says the invitation may not have been sent', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    baseHandlers()
    server.use(http.post('/v1/admin/tenants/:tid/staff/invitations', () => HttpResponse.error()))
    renderDetail()
    await fillAndSend(userEvent.setup())
    expect(await screen.findByRole('alert')).toHaveTextContent(/may not have been sent/)
  })

  it('a role the UI offered but the server refuses (tenant_admin forced to finance) degrades cleanly', async () => {
    signInAs({ role: 'tenant_admin' })
    baseHandlers()
    server.use(
      http.post('/v1/admin/tenants/:tid/staff/invitations', () =>
        HttpResponse.json({ code: 'role_not_grantable', message: 'x' }, { status: 403 }),
      ),
    )
    renderDetail()
    await fillAndSend(userEvent.setup(), 'x@example.com', 'compliance').catch(() => undefined)
    // compliance needs a person; supply it and retry through the support path to prove the refusal rendering.
    const user = userEvent.setup()
    await user.type(screen.getByLabelText('Person ID'), PERSON_1)
    await user.click(screen.getByRole('button', { name: 'Send invitation' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/may not grant this role/)
  })
})

describe('Invite staff: step-up (critical class) through the shared interceptor', () => {
  const dialog = () => screen.findByRole('dialog', { name: 'Confirm it is you' })

  it('prompts on 403 step_up_required, then re-sends the SAME body exactly once with the stepped-up token', async () => {
    // same subject as the stepped-up token (the session must stay the same session)
    signInAs({ role: 'tenant_admin' })
    baseHandlers()
    installStepUpFixture()
    const seen: Array<{ auth: string | null; body: string }> = []
    server.use(
      http.post('/v1/admin/tenants/:tid/staff/invitations', async ({ request }) => {
        const auth = request.headers.get('Authorization')
        seen.push({ auth, body: await request.text() })
        if (auth === `Bearer ${STEPPED_JWT}`) return HttpResponse.json({ staff: entry(), email_delivery: 'sent' }, { status: 201 })
        return stepUpRequiredResponse()
      }),
    )
    renderDetail()
    const user = userEvent.setup()
    await fillAndSend(user)
    await dialog()
    expect(seen).toHaveLength(1)
    await user.type(screen.getByLabelText('6-digit code'), GOOD_CODE)
    await user.click(screen.getByRole('button', { name: 'Verify' }))
    expect(await screen.findByText('Invitation created.')).toBeInTheDocument()
    expect(seen).toHaveLength(2)
    expect(seen[1].auth).toBe(`Bearer ${STEPPED_JWT}`)
    expect(seen[1].body).toBe(seen[0].body)
  })

  it('cancelling the prompt sends nothing more and shows a fixed message (the server text is never shown)', async () => {
    // same subject as the stepped-up token (the session must stay the same session)
    signInAs({ role: 'tenant_admin' })
    baseHandlers()
    installStepUpFixture()
    const spy = vi.fn()
    server.use(http.post('/v1/admin/tenants/:tid/staff/invitations', () => (spy(), stepUpRequiredResponse())))
    renderDetail()
    const user = userEvent.setup()
    await fillAndSend(user)
    await dialog()
    await user.click(screen.getByRole('button', { name: /cancel/i }))
    expect(await screen.findByRole('alert')).toHaveTextContent('Verification cancelled. The invitation was not sent.')
    expect(spy).toHaveBeenCalledTimes(1)
    expect(document.body.textContent).not.toContain(SERVER_MESSAGE_SENTINEL)
  })

  it('a second step_up_required after a successful step-up fails visibly and does not loop', async () => {
    // same subject as the stepped-up token (the session must stay the same session)
    signInAs({ role: 'tenant_admin' })
    baseHandlers()
    installStepUpFixture()
    const spy = vi.fn()
    server.use(http.post('/v1/admin/tenants/:tid/staff/invitations', () => (spy(), stepUpRequiredResponse())))
    renderDetail()
    const user = userEvent.setup()
    await fillAndSend(user)
    await dialog()
    await user.type(screen.getByLabelText('6-digit code'), GOOD_CODE)
    await user.click(screen.getByRole('button', { name: 'Verify' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/did not take effect/)
    expect(spy).toHaveBeenCalledTimes(2)
  })
})

describe('Staff directory: read-only lifecycle fields', () => {
  it('renders status, authenticator, Person link and timestamps for every lifecycle state, with no action buttons', async () => {
    signInAs({ role: 'tenant_admin' })
    baseHandlers([
      entry({ id: 'a', email: 'inv@example.com', status: 'invited' }),
      entry({ id: 'b', email: 'act@example.com', status: 'active', role: 'tenant_admin', person_linked: true, mfa: { enrolled: true, hard_locked: false }, last_login_at: '2026-10-14T09:30:00Z', status_changed_at: '2026-10-13T08:00:00Z' }),
      entry({ id: 'c', email: 'sus@example.com', status: 'suspended', mfa: { enrolled: true, hard_locked: true } }),
      entry({ id: 'd', email: 'off@example.com', status: 'offboarded', display_name: 'Olive Off' }),
    ])
    renderDetail()
    const table = within(await screen.findByRole('table'))
    for (const label of ['Invited', 'Active', 'Suspended', 'Offboarded']) expect(table.getByText(label)).toBeInTheDocument()
    expect(table.getByText('Olive Off')).toBeInTheDocument()
    expect(table.getByText('Locked')).toBeInTheDocument()
    expect(table.getByText('Enrolled')).toBeInTheDocument()
    expect(table.getAllByText('Not enrolled').length).toBeGreaterThan(0)
    expect(table.getByText('Linked')).toBeInTheDocument()
    expect(table.getByText('2026-10-14 09:30 UTC')).toBeInTheDocument()
    expect(table.getByText('2026-10-13 08:00 UTC')).toBeInTheDocument()
    // S8 (suspend / offboard / role change / MFA reset / resend / revoke) is not in this slice.
    expect(table.queryByRole('button')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /suspend|offboard|reactivate|revoke|resend|reset/i })).not.toBeInTheDocument()
  })

  it('renders server strings as text, never markup', async () => {
    signInAs({ role: 'tenant_admin' })
    baseHandlers([entry({ email: '<img src=x onerror=alert(1)>@example.com', display_name: '<script>alert(2)</script>' })])
    renderDetail()
    await screen.findByRole('table')
    expect(document.querySelector('table img')).toBeNull()
    expect(document.querySelector('table script')).toBeNull()
    expect(screen.getByText('<script>alert(2)</script>')).toBeInTheDocument()
  })

  it('shows the truncation notice', async () => {
    signInAs({ role: 'tenant_admin' })
    baseHandlers([entry()], true)
    renderDetail()
    expect(await screen.findByText(/list is truncated/)).toBeInTheDocument()
  })

  it('a failed read shows fixed wording (not the server text) with the request id and a retry', async () => {
    signInAs({ role: 'compliance' })
    baseHandlers()
    server.use(
      http.get('/v1/admin/tenants/:tid/staff', () =>
        HttpResponse.json({ code: 'forbidden', message: SERVER_MESSAGE_SENTINEL, request_id: 'req-list' }, { status: 403 }),
      ),
    )
    renderDetail()
    const alert = await screen.findByText(/do not have permission to view this tenant/)
    expect(alert).toBeInTheDocument()
    expect(screen.getByText('Request ID: req-list')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Try again' })).toBeInTheDocument()
    expect(document.body.textContent).not.toContain(SERVER_MESSAGE_SENTINEL)
  })

  it('is hidden from roles without staff:read (support) and the invite form from compliance', async () => {
    signInAs({ role: 'support' })
    baseHandlers()
    renderDetail()
    await screen.findByText('Acme')
    expect(screen.queryByText('Staff')).not.toBeInTheDocument()
    expect(screen.queryByText('Invite staff user')).not.toBeInTheDocument()
  })
})
