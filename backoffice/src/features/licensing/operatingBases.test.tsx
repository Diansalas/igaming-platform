import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it, vi } from 'vitest'
import { AppRoutes } from '../../app/AppRoutes'
import { server } from '../../test/mswServer'
import { makeTestJwt } from '../../test/jwt'
import { renderWithProviders } from '../../test/renderWithProviders'
import { PLATFORM_ADMIN_CLAIMS, signInAs } from '../../test/session'
import { stepUpRequiredResponse } from '../../test/stepUp'
import {
  BAS0,
  BAS1,
  BRAND1,
  CHG1,
  JUR1,
  LIC1,
  LIC2,
  ME,
  OTHER,
  SENTINEL,
  TEN1,
  basis,
  basisDetail,
  basisReads,
  change,
  changeResponse,
  daysFromToday,
  licence,
  licensingReads,
} from './licensingFixtures'
import { NO_LICENCE_NOTICE } from './basisLabels'

vi.setConfig({ testTimeout: 30000 })

const PA = { ...PLATFORM_ADMIN_CLAIMS, sub: ME }
const refusal = (status: number, code: string, extra: Record<string, unknown> = {}) => HttpResponse.json({ code, message: SENTINEL, request_id: 'req-9', ...extra }, { status })
const pendingBody = () => changeResponse(change({ id: CHG1, kind: 'operating_basis_set', status: 'requested', widening: true, required_approvals: 2 }), false)
const executedBody = () => changeResponse(change({ id: CHG1, kind: 'operating_basis_withdraw', status: 'executed', widening: false, required_approvals: 1 }), true)

function setup() {
  signInAs(PA)
  licensingReads({ licences: [licence({ status: 'active', licensee: 'platform' }), licence({ id: LIC2, licence_number: 'SYN-0002', licensee: 'tenant', status: 'active', tenant_ids: [] })] })
  basisReads()
}

describe('RBAC: operating bases are platform_admin only (UI hint; the server decides)', () => {
  it.each([['tenant_admin'], ['compliance'], ['finance'], ['support'], ['risk_manager'], ['promotions_manager'], ['bonus_operations']])(
    '%s sees NotAuthorized on every operating-basis route and no request is sent',
    async (role) => {
      signInAs({ role, tenant_id: TEN1, sub: ME })
      let calls = 0
      server.use(http.all('/v1/admin/operating-bases*', () => { calls++; return HttpResponse.json({}) }), http.all('/v1/admin/licensing-changes*', () => { calls++; return HttpResponse.json({}) }))
      for (const route of ['/licensing/operating-bases', `/licensing/operating-bases/tenants/${TEN1}`, `/licensing/operating-bases/${BAS1}`]) {
        const r = renderWithProviders(<AppRoutes />, { route })
        expect(await screen.findByText('Your role does not have access to this section.')).toBeInTheDocument()
        r.unmount()
      }
      expect(calls).toBe(0)
    },
  )

  it('degrades when the API refuses the read: a 403 is shown as a refusal and the server message is not', async () => {
    setup()
    server.use(http.get('/v1/admin/operating-bases', () => refusal(403, 'forbidden')))
    renderWithProviders(<AppRoutes />, { route: `/licensing/operating-bases/tenants/${TEN1}` })
    expect(await screen.findByText(/does not hold the permission/)).toBeInTheDocument()
    expect(document.body).not.toHaveTextContent('do-not-render')
  })
})

describe('list per tenant', () => {
  it('no tenant is selected by default; choosing one opens its list and sends tenant_id as the explicit target', async () => {
    setup()
    const seen: URL[] = []
    server.use(http.get('/v1/admin/operating-bases', ({ request }) => { seen.push(new URL(request.url)); return HttpResponse.json({ items: [basis()], limit: 20, offset: 0, total: 1 }) }))
    renderWithProviders(<AppRoutes />, { route: '/licensing/operating-bases' })
    const user = userEvent.setup()
    const sel = await screen.findByLabelText('Tenant')
    expect(sel).toHaveValue('')
    expect(seen).toHaveLength(0)
    expect(screen.getByRole('note', { name: 'Operating basis disclaimer' })).toHaveTextContent('not legal proof')
    await user.selectOptions(sel, await screen.findByRole('option', { name: 'Acme (acme)' }))
    expect(await screen.findByText('Operating bases of tenant Acme')).toBeInTheDocument()
    await waitFor(() => expect(seen.length).toBeGreaterThan(0))
    expect(seen[0].searchParams.get('tenant_id')).toBe(TEN1)
    expect(seen[0].searchParams.get('current')).toBe('true')
    expect(await screen.findByRole('link', { name: 'ZZ' })).toHaveAttribute('href', `/licensing/operating-bases/${BAS1}`)
    expect(screen.getByText('Documented decision without a licence')).toBeInTheDocument()
  })

  it('shows overdue, due-soon and re-review indicators (the due date itself is not overdue)', async () => {
    signInAs(PA)
    licensingReads()
    basisReads({
      items: [
        basis({ id: '50000000-0000-4000-8000-0000000000a1', country_code: 'AA', review_due_at: daysFromToday(-1) }),
        basis({ id: '50000000-0000-4000-8000-0000000000a2', country_code: 'BB', review_due_at: daysFromToday(0) }),
        basis({ id: '50000000-0000-4000-8000-0000000000a3', country_code: 'CC', review_due_at: daysFromToday(200), needs_rereview: true }),
        basis({ id: '50000000-0000-4000-8000-0000000000a4', country_code: 'DD', review_due_at: daysFromToday(200) }),
        basis({ id: '50000000-0000-4000-8000-0000000000a5', country_code: 'EE', basis_kind: 'platform_licence', licence_id: LIC1, jurisdiction_id: null, review_due_at: null }),
      ],
    })
    renderWithProviders(<AppRoutes />, { route: `/licensing/operating-bases/tenants/${TEN1}` })
    const row = async (cc: string) => (await screen.findByRole('link', { name: cc })).closest('tr') as HTMLElement
    expect(within(await row('AA')).getByText(/Review overdue: no longer permitting/)).toBeInTheDocument()
    expect(within(await row('BB')).getByText('Review due soon')).toBeInTheDocument()
    expect(within(await row('CC')).getByText(/Flagged for re-review/)).toBeInTheDocument()
    expect(within(await row('DD')).getByText('Review not due')).toBeInTheDocument()
    expect(within(await row('EE')).getByText('No review date')).toBeInTheDocument()
  })

  it('an empty list says nothing is assumed in its place; an invalid country filter sends no request', async () => {
    signInAs(PA)
    licensingReads()
    basisReads({ items: [] })
    const seen: URL[] = []
    server.use(http.get('/v1/admin/operating-bases', ({ request }) => { seen.push(new URL(request.url)); return HttpResponse.json({ items: [], limit: 20, offset: 0, total: 0 }) }))
    renderWithProviders(<AppRoutes />, { route: `/licensing/operating-bases/tenants/${TEN1}` })
    expect(await screen.findByText(/Nothing is assumed in its place/)).toBeInTheDocument()
    const n = seen.length
    await userEvent.setup().type(screen.getByLabelText('Country filter'), 'Z')
    expect(await screen.findByText('Use two upper-case letters.')).toBeInTheDocument()
    expect(seen.length).toBe(n)
  })

  it('the tenant licensing page links to the tenant\'s operating bases', async () => {
    setup()
    renderWithProviders(<AppRoutes />, { route: `/licensing/tenants/${TEN1}` })
    expect(await screen.findByRole('link', { name: 'Open operating bases of this tenant' })).toHaveAttribute('href', `/licensing/operating-bases/tenants/${TEN1}`)
  })
})

describe('detail', () => {
  it('shows every field, labels the evidence as operator-supplied, the no-licence notice, the decision and the approvers', async () => {
    setup()
    server.use(
      http.get(`/v1/admin/licensing-changes/${CHG1}`, () =>
        HttpResponse.json(changeResponse(change({ id: CHG1, kind: 'operating_basis_set', status: 'executed', approvals: [{ approver_id: ME, reason_code: 'checked_ok', decided_at: '2026-10-01T09:00:00Z' }, { approver_id: OTHER, reason_code: 'second_ok', decided_at: '2026-10-01T09:30:00Z' }] }), true)),
      ),
    )
    renderWithProviders(<AppRoutes />, { route: `/licensing/operating-bases/${BAS1}` })
    expect(await screen.findByText('Operating basis ZZ / wagering')).toBeInTheDocument()
    expect(screen.getByRole('note', { name: 'No-licence basis notice' })).toHaveTextContent(NO_LICENCE_NOTICE)
    expect(screen.getByText('DOC-0001')).toBeInTheDocument()
    expect(screen.getByText('head_of_compliance')).toBeInTheDocument()
    expect(screen.getByText('Evidence reference (supplied by the operator, not verified)')).toBeInTheDocument()
    expect(screen.getByText('EVID-77')).toBeInTheDocument()
    expect(screen.getByText('synthetic restriction')).toBeInTheDocument()
    expect(screen.getByText('Whole tenant (all brands)')).toBeInTheDocument()
    expect(screen.getByText('Review not due')).toBeInTheDocument()
    expect(await screen.findByRole('link', { name: 'XX - Testland' })).toHaveAttribute('href', `/licensing/jurisdictions/${JUR1}`)
    const approvers = await screen.findByRole('list', { name: 'Approvers' })
    expect(within(approvers).getAllByRole('listitem')).toHaveLength(2)
    expect(await within(approvers).findByText('checked_ok')).toBeInTheDocument()
    expect(screen.getAllByRole('link', { name: CHG1.slice(0, 8) })[0]).toHaveAttribute('href', `/licensing/changes/${CHG1}`)
    // wording never claims legality or an approval of a jurisdiction
    expect(document.body.textContent).not.toMatch(/legally|is licensed|jurisdiction (is )?approved|approved for operation/i)
  })

  it('a row without a decision record is flagged as not an approved basis', async () => {
    signInAs(PA)
    licensingReads()
    basisReads({ detail: basisDetail({ decision: null, change_request_id: null }) })
    renderWithProviders(<AppRoutes />, { route: `/licensing/operating-bases/${BAS1}` })
    expect(await screen.findByText(/No decision record exists for this version/)).toBeInTheDocument()
  })

  it('lists the version history of the SAME scope only', async () => {
    signInAs(PA)
    licensingReads()
    basisReads({ items: [basis(), basis({ id: BAS0, status: 'withdrawn', effective_to: '2026-10-05T00:00:00Z', reason_code: 'older_version' }), basis({ id: '50000000-0000-4000-8000-0000000000c9', operation_code: 'deposit', reason_code: 'other_scope' })] })
    renderWithProviders(<AppRoutes />, { route: `/licensing/operating-bases/${BAS1}` })
    expect(await screen.findByText('older_version')).toBeInTheDocument()
    expect(screen.queryByText('other_scope')).toBeNull()
  })

  it('renders hostile text as text (XSS-safe) and prints only safe tokens for codes', async () => {
    signInAs(PA)
    licensingReads()
    const evil = '<img src=x onerror="alert(1)"><script>alert(2)</script>'
    basisReads({ detail: basisDetail({ restrictions_note: evil, determination_reference: evil, basis_evidence_reference: evil, reason_code: evil, restricted_product_codes: [evil] }) })
    const { container } = renderWithProviders(<AppRoutes />, { route: `/licensing/operating-bases/${BAS1}` })
    await screen.findByText('Operating basis ZZ / wagering')
    expect(container.querySelector('img, script')).toBeNull()
    expect(screen.getAllByText(evil).length).toBeGreaterThan(0)
    expect(screen.getAllByText('unprintable').length).toBeGreaterThan(0)
  })

  it('actions exist only on the open version', async () => {
    signInAs(PA)
    licensingReads()
    basisReads({ detail: basisDetail({ effective_to: '2026-10-05T00:00:00Z' }) })
    renderWithProviders(<AppRoutes />, { route: `/licensing/operating-bases/${BAS1}` })
    await screen.findByText('Operating basis ZZ / wagering')
    expect(screen.queryByRole('button', { name: 'Withdraw this basis' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Record a review' })).toBeNull()
  })
})

async function fillCommon(user: ReturnType<typeof userEvent.setup>, dlg: HTMLElement) {
  await user.selectOptions(within(dlg).getByLabelText('Brand scope'), 'tenant')
  await user.type(within(dlg).getByLabelText('Country code'), 'ZZ')
  await user.type(within(dlg).getByLabelText('Operation code'), 'wagering')
  await user.selectOptions(within(dlg).getByLabelText('Product scope'), 'all')
}

async function openFile(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole('button', { name: 'File an operating basis' }))
  return screen.findByRole('dialog', { name: 'File an operating basis' })
}

describe('filing a basis', () => {
  it('preselects nothing; the no-licence notice appears with the choice and stays; review date is mandatory', async () => {
    setup()
    renderWithProviders(<AppRoutes />, { route: `/licensing/operating-bases/tenants/${TEN1}` })
    const user = userEvent.setup()
    const dlg = await openFile(user)
    for (const l of ['Brand scope', 'Product scope', 'Basis']) expect(within(dlg).getByLabelText(l)).toHaveValue('')
    expect(within(dlg).getByLabelText('Country code')).toHaveValue('')
    expect(within(dlg).getByLabelText('Review due on (optional)')).toHaveValue('')
    expect(within(dlg).queryByRole('note', { name: 'No-licence basis notice' })).toBeNull()
    expect(within(dlg).getByRole('button', { name: 'File basis' })).toBeDisabled()
    await user.selectOptions(within(dlg).getByLabelText('Basis'), 'no_licence_documented')
    const notice = within(dlg).getByRole('note', { name: 'No-licence basis notice' })
    expect(notice).toHaveTextContent('documented decision, not proof of legality')
    expect(notice).toHaveTextContent('Review metadata is mandatory')
    expect(within(notice).queryByRole('button')).toBeNull()
    // the notice survives every other interaction
    await fillCommon(user, dlg)
    expect(within(dlg).getByRole('note', { name: 'No-licence basis notice' })).toBeInTheDocument()
    expect(within(dlg).getByLabelText('Jurisdiction relied on')).toHaveValue('')
    expect(within(dlg).getByLabelText('Review due on (required)')).toHaveValue('')
    // switching away removes the licence-less fields and the notice (it belongs to that choice only)
    await user.selectOptions(within(dlg).getByLabelText('Basis'), 'platform_licence')
    expect(within(dlg).queryByRole('note', { name: 'No-licence basis notice' })).toBeNull()
    expect(within(dlg).getByLabelText('Licence')).toHaveValue('')
  })

  it('files a no-licence basis with every payload key; 201 executed:false is PENDING approval, never success', async () => {
    setup()
    let body: Record<string, unknown> = {}
    server.use(http.post('/v1/admin/licensing-changes', async ({ request }) => { body = (await request.json()) as Record<string, unknown>; return HttpResponse.json(pendingBody(), { status: 201 }) }))
    renderWithProviders(<AppRoutes />, { route: `/licensing/operating-bases/tenants/${TEN1}` })
    const user = userEvent.setup()
    const dlg = await openFile(user)
    await fillCommon(user, dlg)
    await user.selectOptions(within(dlg).getByLabelText('Basis'), 'no_licence_documented')
    await user.selectOptions(await within(dlg).findByRole('combobox', { name: 'Jurisdiction relied on' }), 'XX - Testland')
    await user.type(within(dlg).getByLabelText('Determination reference'), 'DOC-0001')
    await user.type(within(dlg).getByLabelText('Determined by (function)'), 'head_of_compliance')
    await user.type(within(dlg).getByLabelText('Restrictions note (optional)'), 'no live dealer')
    await user.type(within(dlg).getByLabelText('Products not covered (optional)'), 'sportsbook, poker')
    await user.type(within(dlg).getByLabelText('Reason code'), 'initial_basis')
    expect(within(dlg).getByRole('button', { name: 'File basis' })).toBeDisabled() // review date still missing
    await user.type(within(dlg).getByLabelText('Review due on (required)'), '2027-03-31')
    await user.click(within(dlg).getByRole('button', { name: 'File basis' }))
    const out = await within(dlg).findByText(/Pending approval\. Nothing has changed yet/)
    expect(out).toHaveTextContent('needs 2 approvals from different people')
    expect(within(dlg).queryByText(/Applied at once/)).toBeNull()
    expect(within(dlg).queryByRole('button', { name: 'File basis' })).toBeNull()
    expect(body).toEqual({
      kind: 'operating_basis_set',
      reason_code: 'initial_basis',
      payload: {
        tenant_id: TEN1,
        brand_id: null,
        country_code: 'ZZ',
        operation_code: 'wagering',
        product_code: null,
        basis_kind: 'no_licence_documented',
        licence_id: null,
        jurisdiction_id: JUR1,
        determination_reference: 'DOC-0001',
        determined_by_role: 'head_of_compliance',
        basis_evidence_reference: null,
        restrictions_note: 'no live dealer',
        restricted_product_codes: ['sportsbook', 'poker'],
        review_due_at: '2027-03-31',
      },
    })
  })

  it('files a platform-licence basis for one brand and one product; the licence list is filtered by licensee', async () => {
    setup()
    let body: { payload?: Record<string, unknown> } = {}
    server.use(http.post('/v1/admin/licensing-changes', async ({ request }) => { body = (await request.json()) as typeof body; return HttpResponse.json(pendingBody(), { status: 201 }) }))
    renderWithProviders(<AppRoutes />, { route: `/licensing/operating-bases/tenants/${TEN1}` })
    const user = userEvent.setup()
    const dlg = await openFile(user)
    await user.selectOptions(within(dlg).getByLabelText('Brand scope'), 'brand')
    await user.selectOptions(await within(dlg).findByRole('combobox', { name: 'Brand' }), 'Brand One (brand-one)')
    await user.type(within(dlg).getByLabelText('Country code'), 'ZZ')
    await user.type(within(dlg).getByLabelText('Operation code'), 'deposit')
    await user.selectOptions(within(dlg).getByLabelText('Product scope'), 'one')
    await user.type(within(dlg).getByLabelText('Product code'), 'casino')
    await user.selectOptions(within(dlg).getByLabelText('Basis'), 'platform_licence')
    const lic = await within(dlg).findByRole('combobox', { name: 'Licence' })
    await within(dlg).findByRole('option', { name: 'SYN-0001 (active)' })
    expect(within(lic).queryByRole('option', { name: 'SYN-0002 (active)' })).toBeNull()
    await user.selectOptions(lic, 'SYN-0001 (active)')
    await user.type(within(dlg).getByLabelText('Determination reference'), 'DOC-2')
    await user.type(within(dlg).getByLabelText('Determined by (function)'), 'legal_counsel')
    await user.type(within(dlg).getByLabelText('Reason code'), 'brand_basis')
    await user.click(within(dlg).getByRole('button', { name: 'File basis' }))
    await within(dlg).findByText(/Pending approval/)
    expect(body.payload).toMatchObject({ brand_id: BRAND1, product_code: 'casino', basis_kind: 'platform_licence', licence_id: LIC1, jurisdiction_id: null, review_due_at: null, restricted_product_codes: [], operation_code: 'deposit' })
  })

  it('client validation mirrors the server: role must be a function code, references carry no "@", digits runs or untrimmed text; the server still decides', async () => {
    setup()
    const posts: unknown[] = []
    server.use(http.post('/v1/admin/licensing-changes', async ({ request }) => { posts.push(await request.json()); return HttpResponse.json(pendingBody(), { status: 201 }) }))
    renderWithProviders(<AppRoutes />, { route: `/licensing/operating-bases/tenants/${TEN1}` })
    const user = userEvent.setup()
    const dlg = await openFile(user)
    await fillCommon(user, dlg)
    await user.selectOptions(within(dlg).getByLabelText('Basis'), 'no_licence_documented')
    await user.selectOptions(await within(dlg).findByRole('combobox', { name: 'Jurisdiction relied on' }), 'XX - Testland')
    await user.type(within(dlg).getByLabelText('Review due on (required)'), '2027-03-31')
    await user.type(within(dlg).getByLabelText('Reason code'), 'x_reason')
    const ref = within(dlg).getByLabelText('Determination reference')
    const role = within(dlg).getByLabelText('Determined by (function)')
    await user.type(role, 'Jane Doe')
    expect(within(dlg).getByText(/Use a function code/)).toBeInTheDocument()
    await user.clear(role)
    await user.type(role, 'head_of_compliance')
    const submit = within(dlg).getByRole('button', { name: 'File basis' })
    for (const [bad, msg] of [['a@b.example', /"@" is not allowed/], ['call 123456789012', /nine or more digits/], ['trailing ', /spaces at the start or end/], ['DE89370400440532013000', /IBAN|digits/]] as const) {
      await user.clear(ref)
      await user.type(ref, bad)
      expect(within(dlg).getAllByText(msg).length).toBeGreaterThan(0)
      expect(submit).toBeDisabled()
    }
    await user.clear(ref)
    await user.type(ref, 'DOC-9')
    expect(submit).toBeEnabled()
    await user.clear(within(dlg).getByLabelText('Country code'))
    await user.type(within(dlg).getByLabelText('Country code'), 'zz')
    expect(submit).toBeDisabled()
    expect(posts).toHaveLength(0)
  })

  it('an answer that is not a recognised change request is "not confirmed", never success', async () => {
    setup()
    server.use(http.post('/v1/admin/licensing-changes', () => HttpResponse.json({ ok: true }, { status: 201 })))
    renderWithProviders(<AppRoutes />, { route: `/licensing/operating-bases/tenants/${TEN1}` })
    const user = userEvent.setup()
    const dlg = await openFile(user)
    await fillCommon(user, dlg)
    await user.selectOptions(within(dlg).getByLabelText('Basis'), 'no_licence_documented')
    await user.selectOptions(await within(dlg).findByRole('combobox', { name: 'Jurisdiction relied on' }), 'XX - Testland')
    await user.type(within(dlg).getByLabelText('Review due on (required)'), '2027-03-31')
    await user.type(within(dlg).getByLabelText('Determination reference'), 'DOC-1')
    await user.type(within(dlg).getByLabelText('Determined by (function)'), 'head_of_compliance')
    await user.type(within(dlg).getByLabelText('Reason code'), 'x_reason')
    await user.click(within(dlg).getByRole('button', { name: 'File basis' }))
    expect(await within(dlg).findByText(/could not be recognised\. Nothing is confirmed/)).toBeInTheDocument()
    expect(within(dlg).queryByText(/Applied at once/)).toBeNull()
  })

  it('a server refusal is shown in fixed words with the closed code, never the server message', async () => {
    setup()
    server.use(http.post('/v1/admin/licensing-changes', () => refusal(409, 'LICENSING_CHANGE_DUPLICATE_PENDING')))
    renderWithProviders(<AppRoutes />, { route: `/licensing/operating-bases/tenants/${TEN1}` })
    const user = userEvent.setup()
    const dlg = await openFile(user)
    await fillCommon(user, dlg)
    await user.selectOptions(within(dlg).getByLabelText('Basis'), 'no_licence_documented')
    await user.selectOptions(await within(dlg).findByRole('combobox', { name: 'Jurisdiction relied on' }), 'XX - Testland')
    await user.type(within(dlg).getByLabelText('Review due on (required)'), '2027-03-31')
    await user.type(within(dlg).getByLabelText('Determination reference'), 'DOC-1')
    await user.type(within(dlg).getByLabelText('Determined by (function)'), 'head_of_compliance')
    await user.type(within(dlg).getByLabelText('Reason code'), 'x_reason')
    await user.click(within(dlg).getByRole('button', { name: 'File basis' }))
    const alert = await within(dlg).findByRole('alert')
    expect(alert).toHaveTextContent('An identical request is already open')
    expect(alert).toHaveTextContent('LICENSING_CHANGE_DUPLICATE_PENDING')
    expect(document.body).not.toHaveTextContent('do-not-render')
  })

  it('a server 403 on filing is a refusal (the UI hint did not authorise anything)', async () => {
    setup()
    server.use(http.post('/v1/admin/licensing-changes', () => refusal(403, 'forbidden')))
    renderWithProviders(<AppRoutes />, { route: `/licensing/operating-bases/tenants/${TEN1}` })
    const user = userEvent.setup()
    const dlg = await openFile(user)
    await fillCommon(user, dlg)
    await user.selectOptions(within(dlg).getByLabelText('Basis'), 'no_licence_documented')
    await user.selectOptions(await within(dlg).findByRole('combobox', { name: 'Jurisdiction relied on' }), 'XX - Testland')
    await user.type(within(dlg).getByLabelText('Review due on (required)'), '2027-03-31')
    await user.type(within(dlg).getByLabelText('Determination reference'), 'DOC-1')
    await user.type(within(dlg).getByLabelText('Determined by (function)'), 'head_of_compliance')
    await user.type(within(dlg).getByLabelText('Reason code'), 'x_reason')
    await user.click(within(dlg).getByRole('button', { name: 'File basis' }))
    expect(await within(dlg).findByRole('alert')).toHaveTextContent(/does not hold the permission/)
    expect(within(dlg).queryByText(/Pending approval/)).toBeNull()
  })

  it('runs the step-up flow: first send refused step_up_required, code entered once, the SAME body re-sent exactly once', async () => {
    setup()
    const STEPPED = makeTestJwt({ ...PA, jti: 'stepped-basis' })
    const sends: Array<{ auth: string | null; body: string }> = []
    server.use(
      http.post('/v1/admin/licensing-changes', async ({ request }) => {
        const rec = { auth: request.headers.get('Authorization'), body: await request.text() }
        sends.push(rec)
        return rec.auth === `Bearer ${STEPPED}` ? HttpResponse.json(pendingBody(), { status: 201 }) : stepUpRequiredResponse()
      }),
      http.post('/v1/staff/auth/step-up', () =>
        HttpResponse.json({ access_token: STEPPED, token_type: 'Bearer', expires_in: 900, mfa_at: '2026-10-13T10:00:00Z', step_up_expires_at: { financial: '2026-10-13T10:10:00Z', critical: '2026-10-13T10:05:00Z' } }),
      ),
    )
    renderWithProviders(<AppRoutes />, { route: `/licensing/operating-bases/tenants/${TEN1}` })
    const user = userEvent.setup()
    const dlg = await openFile(user)
    await fillCommon(user, dlg)
    await user.selectOptions(within(dlg).getByLabelText('Basis'), 'no_licence_documented')
    await user.selectOptions(await within(dlg).findByRole('combobox', { name: 'Jurisdiction relied on' }), 'XX - Testland')
    await user.type(within(dlg).getByLabelText('Review due on (required)'), '2027-03-31')
    await user.type(within(dlg).getByLabelText('Determination reference'), 'DOC-1')
    await user.type(within(dlg).getByLabelText('Determined by (function)'), 'head_of_compliance')
    await user.type(within(dlg).getByLabelText('Reason code'), 'x_reason')
    await user.click(within(dlg).getByRole('button', { name: 'File basis' }))
    const su = await screen.findByRole('dialog', { name: 'Confirm it is you' })
    await user.type(within(su).getByLabelText('6-digit code'), '123456')
    await user.click(within(su).getByRole('button', { name: 'Verify' }))
    await within(dlg).findByText(/Pending approval/)
    expect(sends).toHaveLength(2)
    expect(sends[1].body).toBe(sends[0].body)
  })
})

describe('withdraw and review', () => {
  async function openDetailAction(user: ReturnType<typeof userEvent.setup>, name: string, title: string) {
    await screen.findByText('Operating basis ZZ / wagering')
    await user.click(screen.getByRole('button', { name }))
    return screen.findByRole('dialog', { name: title })
  }

  it('withdraw sends exactly the scope keys (+ reason), explains no fall-back, and a 201 executed:true is "applied at once"', async () => {
    setup()
    let body: unknown
    server.use(http.post('/v1/admin/licensing-changes', async ({ request }) => { body = await request.json(); return HttpResponse.json(executedBody(), { status: 201 }) }))
    renderWithProviders(<AppRoutes />, { route: `/licensing/operating-bases/${BAS1}` })
    const user = userEvent.setup()
    const dlg = await openDetailAction(user, 'Withdraw this basis', 'Withdraw operating basis')
    expect(dlg).toHaveTextContent('does not fall back to the tenant')
    expect(within(dlg).getByRole('button', { name: 'Withdraw basis' })).toBeDisabled()
    await user.type(within(dlg).getByLabelText('Reason code'), 'no_longer_relied_on')
    await user.click(within(dlg).getByRole('button', { name: 'Withdraw basis' }))
    expect(await within(dlg).findByText(/Applied at once/)).toBeInTheDocument()
    expect(body).toEqual({
      kind: 'operating_basis_withdraw',
      reason_code: 'no_longer_relied_on',
      payload: { tenant_id: TEN1, brand_id: null, country_code: 'ZZ', operation_code: 'wagering', product_code: null },
    })
  })

  it('withdraw answered with executed:false is pending, not applied; executed:true on a request that is not executed is unconfirmed', async () => {
    setup()
    let n = 0
    server.use(
      http.post('/v1/admin/licensing-changes', () => {
        n++
        return HttpResponse.json(n === 1 ? pendingBody() : changeResponse(change({ status: 'requested' }), true), { status: 201 })
      }),
    )
    renderWithProviders(<AppRoutes />, { route: `/licensing/operating-bases/${BAS1}` })
    const user = userEvent.setup()
    let dlg = await openDetailAction(user, 'Withdraw this basis', 'Withdraw operating basis')
    await user.type(within(dlg).getByLabelText('Reason code'), 'r_one')
    await user.click(within(dlg).getByRole('button', { name: 'Withdraw basis' }))
    expect(await within(dlg).findByText(/Pending approval/)).toBeInTheDocument()
    expect(within(dlg).queryByText(/Applied at once/)).toBeNull()
    await user.click(within(dlg).getAllByRole('button', { name: 'Close' }).slice(-1)[0])
    dlg = await openDetailAction(user, 'Withdraw this basis', 'Withdraw operating basis')
    await user.type(within(dlg).getByLabelText('Reason code'), 'r_two')
    await user.click(within(dlg).getByRole('button', { name: 'Withdraw basis' }))
    expect(await within(dlg).findByText(/Nothing is confirmed/)).toBeInTheDocument()
    expect(within(dlg).queryByText(/Applied at once/)).toBeNull()
  })

  it('review: outcome, new date and reference are explicit; the class hint follows the date; the payload carries every key', async () => {
    setup()
    let body: unknown
    server.use(http.post('/v1/admin/licensing-changes', async ({ request }) => { body = await request.json(); return HttpResponse.json(pendingBody(), { status: 201 }) }))
    renderWithProviders(<AppRoutes />, { route: `/licensing/operating-bases/${BAS1}` })
    const user = userEvent.setup()
    const dlg = await openDetailAction(user, 'Record a review', 'Record an operating basis review')
    expect(within(dlg).getByLabelText('Review outcome')).toHaveValue('')
    expect(within(dlg).getByLabelText('New review due on')).toHaveValue('')
    expect(within(dlg).getByRole('note', { name: 'No-licence basis notice' })).toBeInTheDocument()
    const submit = within(dlg).getByRole('button', { name: 'File review' })
    await user.selectOptions(within(dlg).getByLabelText('Review outcome'), 'clear')
    await user.type(within(dlg).getByLabelText('New review due on'), daysFromToday(400))
    await user.type(within(dlg).getByLabelText('Review reference'), 'REV-2027-01')
    await user.type(within(dlg).getByLabelText('Reason code'), 'annual_review')
    expect(within(dlg).getByRole('note', { name: 'Review class hint' })).toHaveTextContent(/widening change: it needs 2 approvals/)
    expect(submit).toBeEnabled()
    await user.click(submit)
    await within(dlg).findByText(/Pending approval/)
    expect(body).toEqual({
      kind: 'operating_basis_review_record',
      reason_code: 'annual_review',
      payload: { tenant_id: TEN1, brand_id: null, country_code: 'ZZ', operation_code: 'wagering', product_code: null, review_due_at: daysFromToday(400), needs_rereview: false, review_reference: 'REV-2027-01' },
    })
  })

  it('flagging for re-review is hinted as narrowing; an email-like review reference blocks submit', async () => {
    setup()
    renderWithProviders(<AppRoutes />, { route: `/licensing/operating-bases/${BAS1}` })
    const user = userEvent.setup()
    const dlg = await openDetailAction(user, 'Record a review', 'Record an operating basis review')
    await user.selectOptions(within(dlg).getByLabelText('Review outcome'), 'flag')
    await user.type(within(dlg).getByLabelText('New review due on'), daysFromToday(100))
    await user.type(within(dlg).getByLabelText('Review reference'), 'a@b.example')
    await user.type(within(dlg).getByLabelText('Reason code'), 'doubt')
    expect(within(dlg).getByRole('note', { name: 'Review class hint' })).toHaveTextContent(/narrowing change: it takes effect at once/)
    expect(within(dlg).getByRole('button', { name: 'File review' })).toBeDisabled()
  })
})

describe('change detail renders operating-basis payloads readably', () => {
  it('shows the summary in words for a set request, with the no-licence notice and the link to the tenant\'s bases; hostile text stays text', async () => {
    signInAs(PA)
    const evil = '<img src=x onerror="alert(1)">'
    licensingReads({
      changes: [
        change({
          kind: 'operating_basis_set',
          requested_by: OTHER,
          payload: {
            tenant_id: TEN1, brand_id: null, country_code: 'ZZ', operation_code: 'wagering', product_code: null, basis_kind: 'no_licence_documented', licence_id: null, jurisdiction_id: JUR1,
            determination_reference: evil, determined_by_role: 'head_of_compliance', basis_evidence_reference: null, restrictions_note: null, restricted_product_codes: ['x'], review_due_at: '2027-03-31',
          },
        }),
      ],
    })
    const { container } = renderWithProviders(<AppRoutes />, { route: `/licensing/changes/${CHG1}` })
    const sum = await screen.findByRole('region', { name: 'Operating basis summary' })
    expect(within(sum).getByText('Documented decision without a licence')).toBeInTheDocument()
    expect(within(sum).getByText('Whole tenant')).toBeInTheDocument()
    expect(within(sum).getByText('All products')).toBeInTheDocument()
    expect(within(sum).getByText('2027-03-31')).toBeInTheDocument()
    expect(within(sum).getByRole('note', { name: 'No-licence basis notice' })).toBeInTheDocument()
    expect(within(sum).getByText(evil)).toBeInTheDocument()
    expect(container.querySelector('img')).toBeNull()
    expect(within(sum).getByRole('link', { name: "See this tenant's operating bases" })).toHaveAttribute('href', `/licensing/operating-bases/tenants/${TEN1}`)
    // approve / reject are still the existing, unchanged controls
    expect(screen.getByRole('button', { name: 'Approve' })).toBeEnabled()
  })

  it('a withdraw request is explained in words; an executed one links to the resulting basis version', async () => {
    signInAs(PA)
    licensingReads({ changes: [change({ kind: 'operating_basis_withdraw', status: 'executed', widening: false, payload: { tenant_id: TEN1, brand_id: null, country_code: 'ZZ', operation_code: 'wagering', product_code: null } })] })
    server.use(http.get(`/v1/admin/licensing-changes/${CHG1}`, () => HttpResponse.json({ ...changeResponse(change({ kind: 'operating_basis_withdraw', status: 'executed', widening: false, payload: { tenant_id: TEN1, brand_id: null, country_code: 'ZZ', operation_code: 'wagering', product_code: null } }), true), outcome: { operating_basis_id: BAS0, status: 'withdrawn' } })))
    renderWithProviders(<AppRoutes />, { route: `/licensing/changes/${CHG1}` })
    const sum = await screen.findByRole('region', { name: 'Operating basis summary' })
    expect(sum).toHaveTextContent('does not fall back to the tenant')
    expect(screen.getByRole('link', { name: 'Open the resulting basis version' })).toHaveAttribute('href', `/licensing/operating-bases/${BAS0}`)
  })

  it('an unrelated kind shows no basis summary', async () => {
    signInAs(PA)
    licensingReads()
    renderWithProviders(<AppRoutes />, { route: `/licensing/changes/${CHG1}` })
    await screen.findByText(/request/i)
    expect(screen.queryByRole('region', { name: 'Operating basis summary' })).toBeNull()
  })
})
