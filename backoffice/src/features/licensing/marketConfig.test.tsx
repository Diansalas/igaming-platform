import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it, vi } from 'vitest'
import type { OperatingCountryPolicy, TenantJurisdictionConfig } from '../../api/licensing'
import { AppRoutes } from '../../app/AppRoutes'
import { makeTestJwt } from '../../test/jwt'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { PLATFORM_ADMIN_CLAIMS, signInAs } from '../../test/session'
import { stepUpRequiredResponse } from '../../test/stepUp'
import {
  BRAND1,
  CHG1,
  JUR1,
  LIC1,
  ME,
  OTHER,
  SENTINEL,
  TEN1,
  basisReads,
  change,
  changeResponse,
  licence,
  licensingReads,
  page,
} from './licensingFixtures'

vi.setConfig({ testTimeout: 30000 })

const PA = { ...PLATFORM_ADMIN_CLAIMS, sub: ME }
const refusal = (status: number, code: string, extra: Record<string, unknown> = {}) => HttpResponse.json({ code, message: SENTINEL, request_id: 'req-9', ...extra }, { status })
const pendingBody = (kind: string) => changeResponse(change({ id: CHG1, kind, status: 'requested', widening: true, required_approvals: 2 }), false)
const executedBody = (kind: string) => changeResponse(change({ id: CHG1, kind, status: 'executed', widening: false, required_approvals: 1 }), true)
type User = ReturnType<typeof userEvent.setup>

const policy = (o: Partial<OperatingCountryPolicy> = {}): OperatingCountryPolicy => ({
  id: '70000000-0000-4000-8000-000000000001',
  tenant_id: TEN1,
  scope: 'tenant',
  brand_id: null,
  operation_code: null,
  product_code: null,
  country_code: 'ZZ',
  state: 'enabled',
  status: 'active',
  authorization_reference: 'AUTH-1',
  reason_code: 'initial',
  policy_version: 'v1',
  effective_from: '2026-01-01T00:00:00Z',
  effective_to: null,
  created_by_actor_type: 'staff',
  created_by: null,
  created_at: '2026-01-01T00:00:00Z',
  ...o,
})
const config = (o: Partial<TenantJurisdictionConfig> = {}): TenantJurisdictionConfig => ({
  id: 'cfg1',
  tenant_id: TEN1,
  jurisdiction_id: JUR1,
  kyc_ruleset_id: 'kyc-a',
  aml_ruleset_id: null,
  rg_ruleset_id: null,
  reporting_ruleset_id: null,
  allowed_currencies: ['EUR', 'USD'],
  allowed_payment_methods: ['card'],
  geo_block_list: ['YY'],
  effective_from: '2026-01-01T00:00:00Z',
  effective_to: null,
  ...o,
})

function capturePost(answer: (body: unknown) => Response) {
  const bodies: unknown[] = []
  server.use(http.post('/v1/admin/licensing-changes', async ({ request }) => { const b = await request.json(); bodies.push(b); return answer(b) }))
  return bodies
}

function tenantSetup(o: { policies?: OperatingCountryPolicy[]; configs?: TenantJurisdictionConfig[] } = {}) {
  signInAs(PA)
  licensingReads()
  basisReads()
  server.use(
    http.get('/v1/admin/operating-country-policies', () => HttpResponse.json(page(o.policies ?? []))),
    http.get(`/v1/admin/tenants/${TEN1}/jurisdiction-configs`, () => HttpResponse.json(page(o.configs ?? []))),
  )
}

describe('RBAC: market configuration filing is platform_admin only (UI hint; the server decides)', () => {
  it.each([['tenant_admin'], ['compliance'], ['finance'], ['support'], ['risk_manager'], ['promotions_manager'], ['bonus_operations']])(
    '%s sees NotAuthorized on the licence, tenant and jurisdiction screens and ZERO requests are sent',
    async (role) => {
      signInAs({ role, tenant_id: TEN1, sub: ME })
      let calls = 0
      server.use(http.all('/v1/admin/*', () => { calls++; return HttpResponse.json({}) }))
      for (const route of [`/licensing/licences/${LIC1}`, `/licensing/tenants/${TEN1}`, '/licensing/jurisdictions']) {
        const r = renderWithProviders(<AppRoutes />, { route })
        expect(await screen.findByText('Your role does not have access to this section.')).toBeInTheDocument()
        r.unmount()
      }
      expect(calls).toBe(0)
    },
  )

  it('a server 403 on a filing the UI allowed is shown as a refusal and is not pending', async () => {
    tenantSetup()
    server.use(http.post('/v1/admin/licensing-changes', () => refusal(403, 'forbidden')))
    renderWithProviders(<AppRoutes />, { route: `/licensing/licences/${LIC1}` })
    const user = userEvent.setup()
    const dlg = await openCeiling(user)
    await fillCeiling(user, dlg, 'ZZ', 'disable')
    await user.click(within(dlg).getByRole('button', { name: 'File ceiling change' }))
    expect(await within(dlg).findByRole('alert')).toHaveTextContent(/does not hold the permission/)
    expect(within(dlg).queryByText(/Pending approval/)).toBeNull()
    expect(document.body).not.toHaveTextContent('do-not-render')
  })
})

async function openCeiling(user: User) {
  await user.click(await screen.findByRole('button', { name: 'File a country ceiling change' }))
  return screen.findByRole('dialog', { name: 'File a licence country ceiling change' })
}
async function fillCeiling(user: User, dlg: HTMLElement, country: string, action: 'enable' | 'disable' | 'withdraw', ref?: string) {
  await user.type(within(dlg).getByLabelText('Country code'), country)
  await user.selectOptions(within(dlg).getByLabelText('Action'), action)
  if (ref) await user.type(within(dlg).getByLabelText(/^Authorization reference \(operator-supplied/), ref)
  await user.type(within(dlg).getByLabelText('Reason code'), 'ceiling_ticket')
}

describe('licence country ceiling filing', () => {
  it('preselects nothing, labels the reference as operator-supplied and not verified, and states the widening rule', async () => {
    tenantSetup()
    renderWithProviders(<AppRoutes />, { route: `/licensing/licences/${LIC1}` })
    const user = userEvent.setup()
    const dlg = await openCeiling(user)
    expect(within(dlg).getByLabelText('Country code')).toHaveValue('')
    expect(within(dlg).getByLabelText('Action')).toHaveValue('')
    expect(within(dlg).getByLabelText('Reason code')).toHaveValue('')
    expect(within(dlg).getByRole('button', { name: 'File ceiling change' })).toBeDisabled()
    expect(within(dlg).getByRole('note', { name: 'Market configuration disclaimer' })).toHaveTextContent('not a legal or regulatory authorisation')
    expect(within(dlg).getByRole('note', { name: 'Widening rule' })).toHaveTextContent('2 approvals from 2 different people')
    await user.selectOptions(within(dlg).getByLabelText('Action'), 'enable')
    expect(within(dlg).getByLabelText('Authorization reference (operator-supplied, required)')).toHaveValue('')
    expect(within(dlg).getByRole('note', { name: 'Authorization reference note' })).toHaveTextContent('not verified by the platform')
    expect(within(dlg).getByRole('note', { name: 'Change class hint' })).toHaveTextContent('widening change: it needs 2 approvals')
    await user.selectOptions(within(dlg).getByLabelText('Action'), 'disable')
    expect(within(dlg).getByLabelText('Authorization reference (operator-supplied, optional)')).toBeInTheDocument()
    expect(within(dlg).getByRole('note', { name: 'Change class hint' })).toHaveTextContent('narrowing change: one platform administrator applies it at once')
  })

  it('validates the country as two upper-case letters and requires the reference to enable; nothing is sent while invalid', async () => {
    tenantSetup()
    const bodies = capturePost(() => HttpResponse.json(pendingBody('licence_ceiling_version'), { status: 201 }))
    renderWithProviders(<AppRoutes />, { route: `/licensing/licences/${LIC1}` })
    const user = userEvent.setup()
    const dlg = await openCeiling(user)
    const submit = within(dlg).getByRole('button', { name: 'File ceiling change' })
    await user.type(within(dlg).getByLabelText('Country code'), 'z')
    expect(within(dlg).getByText(/Use two upper-case letters/)).toBeInTheDocument()
    await user.clear(within(dlg).getByLabelText('Country code'))
    await user.type(within(dlg).getByLabelText('Country code'), 'zz')
    expect(within(dlg).getByText(/Use two upper-case letters/)).toBeInTheDocument()
    await user.clear(within(dlg).getByLabelText('Country code'))
    await user.type(within(dlg).getByLabelText('Country code'), 'ZZ')
    await user.selectOptions(within(dlg).getByLabelText('Action'), 'enable')
    await user.type(within(dlg).getByLabelText('Reason code'), 'ceiling_ticket')
    expect(submit).toBeDisabled() // reference missing
    const ref = within(dlg).getByLabelText('Authorization reference (operator-supplied, required)')
    await user.type(ref, ' padded')
    expect(within(dlg).getByText(/Remove the spaces/)).toBeInTheDocument()
    expect(submit).toBeDisabled()
    await user.clear(ref)
    await user.type(ref, 'AUTH-9')
    expect(submit).toBeEnabled()
    expect(bodies).toHaveLength(0)
  })

  it('enable: exact payload keys; 201 executed:false is PENDING approval, never success', async () => {
    tenantSetup()
    const bodies = capturePost(() => HttpResponse.json(pendingBody('licence_ceiling_version'), { status: 201 }))
    renderWithProviders(<AppRoutes />, { route: `/licensing/licences/${LIC1}` })
    const user = userEvent.setup()
    const dlg = await openCeiling(user)
    await fillCeiling(user, dlg, 'ZZ', 'enable', 'AUTH-9')
    await user.click(within(dlg).getByRole('button', { name: 'File ceiling change' }))
    const out = await within(dlg).findByText(/Pending approval\. Nothing has changed yet/)
    expect(out).toHaveTextContent('needs 2 approvals from different people')
    expect(within(dlg).queryByText(/Applied at once/)).toBeNull()
    expect(within(dlg).queryByRole('button', { name: 'File ceiling change' })).toBeNull()
    expect(bodies).toEqual([{ kind: 'licence_ceiling_version', reason_code: 'ceiling_ticket', payload: { licence_id: LIC1, country_code: 'ZZ', state: 'enabled', status: 'active', authorization_reference: 'AUTH-9' } }])
  })

  it('disable and withdraw send state/status pairs; a null reference is sent explicitly; executed:true on an executed request is applied at once', async () => {
    tenantSetup()
    const bodies = capturePost(() => HttpResponse.json(executedBody('licence_ceiling_version'), { status: 201 }))
    renderWithProviders(<AppRoutes />, { route: `/licensing/licences/${LIC1}` })
    const user = userEvent.setup()
    let dlg = await openCeiling(user)
    await fillCeiling(user, dlg, 'ZZ', 'disable')
    await user.click(within(dlg).getByRole('button', { name: 'File ceiling change' }))
    expect(await within(dlg).findByText(/Applied at once/)).toBeInTheDocument()
    await user.click(within(dlg).getAllByRole('button', { name: 'Close' }).slice(-1)[0])
    dlg = await openCeiling(user)
    await fillCeiling(user, dlg, 'YY', 'withdraw')
    await user.click(within(dlg).getByRole('button', { name: 'File ceiling change' }))
    await within(dlg).findByText(/Applied at once/)
    const p = (i: number) => (bodies[i] as { payload: Record<string, unknown> }).payload
    expect(p(0)).toEqual({ licence_id: LIC1, country_code: 'ZZ', state: 'disabled', status: 'active', authorization_reference: null })
    expect(p(1)).toEqual({ licence_id: LIC1, country_code: 'YY', state: 'disabled', status: 'withdrawn', authorization_reference: null })
  })

  it('an unrecognised answer, or executed:true on a request that is not executed, is "not confirmed"', async () => {
    tenantSetup()
    let n = 0
    server.use(http.post('/v1/admin/licensing-changes', () => { n++; return HttpResponse.json(n === 1 ? { ok: true } : changeResponse(change({ status: 'requested' }), true), { status: 201 }) }))
    renderWithProviders(<AppRoutes />, { route: `/licensing/licences/${LIC1}` })
    const user = userEvent.setup()
    let dlg = await openCeiling(user)
    await fillCeiling(user, dlg, 'ZZ', 'disable')
    await user.click(within(dlg).getByRole('button', { name: 'File ceiling change' }))
    expect(await within(dlg).findByText(/could not be recognised\. Nothing is confirmed/)).toBeInTheDocument()
    await user.click(within(dlg).getAllByRole('button', { name: 'Close' }).slice(-1)[0])
    dlg = await openCeiling(user)
    await fillCeiling(user, dlg, 'ZZ', 'disable')
    await user.click(within(dlg).getByRole('button', { name: 'File ceiling change' }))
    expect(await within(dlg).findByText(/Nothing is confirmed/)).toBeInTheDocument()
    expect(within(dlg).queryByText(/Applied at once|Pending approval/)).toBeNull()
  })

  it('a refusal is shown in fixed words with the closed code, never the server message', async () => {
    tenantSetup()
    server.use(http.post('/v1/admin/licensing-changes', () => refusal(409, 'LICENSING_CHANGE_DUPLICATE_PENDING')))
    renderWithProviders(<AppRoutes />, { route: `/licensing/licences/${LIC1}` })
    const user = userEvent.setup()
    const dlg = await openCeiling(user)
    await fillCeiling(user, dlg, 'ZZ', 'disable')
    await user.click(within(dlg).getByRole('button', { name: 'File ceiling change' }))
    const alert = await within(dlg).findByRole('alert')
    expect(alert).toHaveTextContent('An identical request is already open')
    expect(alert).toHaveTextContent('LICENSING_CHANGE_DUPLICATE_PENDING')
    expect(document.body).not.toHaveTextContent('do-not-render')
  })

  it('runs the step-up flow: first send refused step_up_required, code entered once, the SAME body re-sent exactly once', async () => {
    tenantSetup()
    const STEPPED = makeTestJwt({ ...PA, jti: 'stepped-market' })
    const sends: Array<{ auth: string | null; body: string }> = []
    server.use(
      http.post('/v1/admin/licensing-changes', async ({ request }) => {
        const rec = { auth: request.headers.get('Authorization'), body: await request.text() }
        sends.push(rec)
        return rec.auth === `Bearer ${STEPPED}` ? HttpResponse.json(pendingBody('licence_ceiling_version'), { status: 201 }) : stepUpRequiredResponse()
      }),
      http.post('/v1/staff/auth/step-up', () =>
        HttpResponse.json({ access_token: STEPPED, token_type: 'Bearer', expires_in: 900, mfa_at: '2026-10-13T10:00:00Z', step_up_expires_at: { financial: '2026-10-13T10:10:00Z', critical: '2026-10-13T10:05:00Z' } }),
      ),
    )
    renderWithProviders(<AppRoutes />, { route: `/licensing/licences/${LIC1}` })
    const user = userEvent.setup()
    const dlg = await openCeiling(user)
    await fillCeiling(user, dlg, 'ZZ', 'enable', 'AUTH-9')
    await user.click(within(dlg).getByRole('button', { name: 'File ceiling change' }))
    const su = await screen.findByRole('dialog', { name: 'Confirm it is you' })
    await user.type(within(su).getByLabelText('6-digit code'), '123456')
    await user.click(within(su).getByRole('button', { name: 'Verify' }))
    await within(dlg).findByText(/Pending approval/)
    expect(sends).toHaveLength(2)
    expect(sends[1].body).toBe(sends[0].body)
  })

  it('the read view links to the filing dialog, also per current row with the country carried over', async () => {
    tenantSetup()
    server.use(
      http.get('/v1/admin/licence-country-ceilings', () =>
        HttpResponse.json(page([
          { id: 'c1', licence_id: LIC1, country_code: 'ZZ', state: 'enabled', status: 'active', authorization_reference: 'REF-1', reason_code: 'initial', policy_version: 'v1', effective_from: '2026-01-01T00:00:00Z', effective_to: null, created_by_actor_type: 'staff', created_by: null, created_at: '2026-01-01T00:00:00Z' },
          { id: 'c0', licence_id: LIC1, country_code: 'YY', state: 'enabled', status: 'active', authorization_reference: 'REF-0', reason_code: 'old', policy_version: 'v1', effective_from: '2025-01-01T00:00:00Z', effective_to: '2026-01-01T00:00:00Z', created_by_actor_type: 'staff', created_by: null, created_at: '2025-01-01T00:00:00Z' },
        ])),
      ),
    )
    renderWithProviders(<AppRoutes />, { route: `/licensing/licences/${LIC1}` })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'File a change for ZZ' }))
    const dlg = await screen.findByRole('dialog', { name: 'File a licence country ceiling change' })
    expect(within(dlg).getByLabelText('Country code')).toHaveValue('ZZ')
    expect(within(dlg).getByLabelText('Action')).toHaveValue('')
    expect(screen.queryByRole('button', { name: 'File a change for YY' })).toBeNull() // a closed version offers none
  })
})

async function openPolicy(user: User) {
  await user.click(await screen.findByRole('button', { name: 'File an operating-country policy change' }))
  return screen.findByRole('dialog', { name: 'File an operating-country policy change' })
}

describe('operating-country policy filing', () => {
  it('starts empty; the rung decides which fields exist; a withdrawal below the tenant rung widens and needs a reference', async () => {
    tenantSetup()
    renderWithProviders(<AppRoutes />, { route: `/licensing/tenants/${TEN1}` })
    const user = userEvent.setup()
    const dlg = await openPolicy(user)
    for (const l of ['Rung', 'Country code', 'Action', 'Reason code']) expect(within(dlg).getByLabelText(l)).toHaveValue('')
    expect(within(dlg).queryByLabelText('Operation code')).toBeNull()
    expect(within(dlg).getByRole('note', { name: 'Policy note' })).toHaveTextContent('terminal block')
    await user.selectOptions(within(dlg).getByLabelText('Rung'), 'tenant')
    await user.selectOptions(within(dlg).getByLabelText('Action'), 'withdraw')
    expect(within(dlg).getByRole('note', { name: 'Change class hint' })).toHaveTextContent('narrowing')
    expect(within(dlg).getByLabelText('Authorization reference (operator-supplied, optional)')).toBeInTheDocument()
    await user.selectOptions(within(dlg).getByLabelText('Rung'), 'brand')
    await user.selectOptions(within(dlg).getByLabelText('Action'), 'withdraw')
    expect(within(dlg).getByRole('note', { name: 'Change class hint' })).toHaveTextContent('widening')
    expect(within(dlg).getByRole('note', { name: 'Withdrawal note' })).toHaveTextContent('removes a block')
    expect(within(dlg).getByLabelText('Authorization reference (operator-supplied, required)')).toBeInTheDocument()
    await user.selectOptions(within(dlg).getByLabelText('Rung'), 'operation')
    expect(within(dlg).getByLabelText('Operation code')).toHaveValue('')
    expect(within(dlg).getByLabelText('Brand scope')).toHaveValue('')
    expect(within(dlg).getByLabelText('Product scope')).toHaveValue('')
  })

  it('tenant rung enable: exact payload (brand, operation and product null); pending approval', async () => {
    tenantSetup()
    const bodies = capturePost(() => HttpResponse.json(pendingBody('operating_policy_version'), { status: 201 }))
    renderWithProviders(<AppRoutes />, { route: `/licensing/tenants/${TEN1}` })
    const user = userEvent.setup()
    const dlg = await openPolicy(user)
    await user.selectOptions(within(dlg).getByLabelText('Rung'), 'tenant')
    await user.type(within(dlg).getByLabelText('Country code'), 'ZZ')
    await user.selectOptions(within(dlg).getByLabelText('Action'), 'enable')
    await user.type(within(dlg).getByLabelText(/^Authorization reference \(operator-supplied/), 'AUTH-3')
    await user.type(within(dlg).getByLabelText('Reason code'), 'optin')
    await user.click(within(dlg).getByRole('button', { name: 'File policy change' }))
    await within(dlg).findByText(/Pending approval\. Nothing has changed yet/)
    expect(bodies).toEqual([
      { kind: 'operating_policy_version', reason_code: 'optin', payload: { tenant_id: TEN1, scope: 'tenant', brand_id: null, operation_code: null, product_code: null, country_code: 'ZZ', state: 'enabled', status: 'active', authorization_reference: 'AUTH-3' } },
    ])
  })

  it('operation rung for one brand and one product: payload carries every key; the brand is chosen from the tenant\'s brands', async () => {
    tenantSetup()
    const bodies = capturePost(() => HttpResponse.json(executedBody('operating_policy_version'), { status: 201 }))
    renderWithProviders(<AppRoutes />, { route: `/licensing/tenants/${TEN1}` })
    const user = userEvent.setup()
    const dlg = await openPolicy(user)
    await user.selectOptions(within(dlg).getByLabelText('Rung'), 'operation')
    await user.selectOptions(within(dlg).getByLabelText('Brand scope'), 'brand')
    await user.selectOptions(await within(dlg).findByRole('combobox', { name: 'Brand' }), 'Brand One (brand-one)')
    await user.type(within(dlg).getByLabelText('Operation code'), 'deposit')
    await user.selectOptions(within(dlg).getByLabelText('Product scope'), 'one')
    await user.type(within(dlg).getByLabelText('Product code'), 'casino')
    await user.type(within(dlg).getByLabelText('Country code'), 'ZZ')
    await user.selectOptions(within(dlg).getByLabelText('Action'), 'disable')
    await user.type(within(dlg).getByLabelText('Reason code'), 'stop_deposit')
    await user.click(within(dlg).getByRole('button', { name: 'File policy change' }))
    await within(dlg).findByText(/Applied at once/)
    expect(bodies).toEqual([
      { kind: 'operating_policy_version', reason_code: 'stop_deposit', payload: { tenant_id: TEN1, scope: 'operation', brand_id: BRAND1, operation_code: 'deposit', product_code: 'casino', country_code: 'ZZ', state: 'disabled', status: 'active', authorization_reference: null } },
    ])
  })

  it('brand rung withdrawal cannot be submitted without a reference, and a row opens the dialog with its scope and country carried over', async () => {
    tenantSetup({ policies: [policy({ scope: 'brand', brand_id: BRAND1 })] })
    renderWithProviders(<AppRoutes />, { route: `/licensing/tenants/${TEN1}` })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'File a change for ZZ (brand)' }))
    const dlg = await screen.findByRole('dialog', { name: 'File an operating-country policy change' })
    expect(within(dlg).getByLabelText('Rung')).toHaveValue('brand')
    expect(within(dlg).getByLabelText('Country code')).toHaveValue('ZZ')
    expect(within(dlg).getByLabelText('Action')).toHaveValue('')
    await user.selectOptions(await within(dlg).findByRole('combobox', { name: 'Brand' }), 'Brand One (brand-one)')
    await user.selectOptions(within(dlg).getByLabelText('Action'), 'withdraw')
    await user.type(within(dlg).getByLabelText('Reason code'), 'lift_block')
    expect(within(dlg).getByRole('button', { name: 'File policy change' })).toBeDisabled()
    await user.type(within(dlg).getByLabelText(/^Authorization reference \(operator-supplied/), 'AUTH-4')
    expect(within(dlg).getByRole('button', { name: 'File policy change' })).toBeEnabled()
  })

  it('a server refusal (ceiling exceeded at execution) is shown in fixed words', async () => {
    tenantSetup()
    server.use(http.post('/v1/admin/licensing-changes', () => refusal(409, 'LICENSING_CHANGE_REFUSED')))
    renderWithProviders(<AppRoutes />, { route: `/licensing/tenants/${TEN1}` })
    const user = userEvent.setup()
    const dlg = await openPolicy(user)
    await user.selectOptions(within(dlg).getByLabelText('Rung'), 'tenant')
    await user.type(within(dlg).getByLabelText('Country code'), 'ZZ')
    await user.selectOptions(within(dlg).getByLabelText('Action'), 'disable')
    await user.type(within(dlg).getByLabelText('Reason code'), 'stop')
    await user.click(within(dlg).getByRole('button', { name: 'File policy change' }))
    expect(await within(dlg).findByRole('alert')).toHaveTextContent('refused by the domain rules')
    expect(document.body).not.toHaveTextContent('do-not-render')
  })
})

async function openConfig(user: User) {
  await user.click(await screen.findByRole('button', { name: 'File a jurisdiction configuration change' }))
  return screen.findByRole('dialog', { name: 'File a tenant jurisdiction configuration change' })
}

describe('tenant jurisdiction configuration filing', () => {
  it('starts empty; a first version for a jurisdiction is hinted as widening; payload carries every key and empty lists as arrays', async () => {
    tenantSetup()
    const bodies = capturePost(() => HttpResponse.json(pendingBody('tenant_jurisdiction_config_version'), { status: 201 }))
    renderWithProviders(<AppRoutes />, { route: `/licensing/tenants/${TEN1}` })
    const user = userEvent.setup()
    const dlg = await openConfig(user)
    expect(within(dlg).getByLabelText('Action')).toHaveValue('')
    expect(within(dlg).getByLabelText('Jurisdiction')).toHaveValue('')
    expect(within(dlg).getByRole('button', { name: 'File configuration change' })).toBeDisabled()
    await user.selectOptions(within(dlg).getByLabelText('Action'), 'set')
    await user.selectOptions(await within(dlg).findByRole('combobox', { name: 'Jurisdiction' }), 'XX - Testland')
    await user.type(within(dlg).getByLabelText('KYC ruleset id (optional)'), 'kyc-1')
    await user.type(within(dlg).getByLabelText('Allowed currencies'), 'EUR, USD')
    await user.type(within(dlg).getByLabelText('Geo block list'), 'YY')
    await user.type(within(dlg).getByLabelText('Reason code'), 'first_config')
    expect(within(dlg).getByRole('note', { name: 'Change class hint' })).toHaveTextContent('widening change')
    await user.click(within(dlg).getByRole('button', { name: 'File configuration change' }))
    await within(dlg).findByText(/Pending approval/)
    expect(bodies).toEqual([
      {
        kind: 'tenant_jurisdiction_config_version',
        reason_code: 'first_config',
        payload: {
          tenant_id: TEN1, jurisdiction_id: JUR1, action: 'set', kyc_ruleset_id: 'kyc-1', aml_ruleset_id: null, rg_ruleset_id: null, reporting_ruleset_id: null,
          allowed_currencies: ['EUR', 'USD'], allowed_payment_methods: [], geo_block_list: ['YY'],
        },
      },
    ])
  })

  it('a row loads the version in force; removing a currency is hinted narrowing, adding one widening; invalid codes block submit', async () => {
    tenantSetup({ configs: [config()] })
    const bodies = capturePost(() => HttpResponse.json(executedBody('tenant_jurisdiction_config_version'), { status: 201 }))
    renderWithProviders(<AppRoutes />, { route: `/licensing/tenants/${TEN1}` })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: /^Set a new version for / }))
    const dlg = await screen.findByRole('dialog', { name: 'File a tenant jurisdiction configuration change' })
    expect(within(dlg).getByLabelText('Action')).toHaveValue('set')
    expect(within(dlg).getByLabelText('Allowed currencies')).toHaveValue('EUR, USD')
    expect(within(dlg).getByLabelText('KYC ruleset id (optional)')).toHaveValue('kyc-a')
    expect(within(dlg).getByRole('note', { name: 'Configuration note' })).toHaveTextContent('A field you leave out is removed')
    await user.type(within(dlg).getByLabelText('Reason code'), 'trim_currency')
    const cur = within(dlg).getByLabelText('Allowed currencies')
    await user.clear(cur)
    await user.type(cur, 'EUR')
    expect(within(dlg).getByRole('note', { name: 'Change class hint' })).toHaveTextContent('narrowing change')
    await user.clear(cur)
    await user.type(cur, 'EUR, USD, GBP')
    expect(within(dlg).getByRole('note', { name: 'Change class hint' })).toHaveTextContent('widening change')
    await user.clear(cur)
    await user.type(cur, 'eur')
    expect(within(dlg).getByText(/is not a currency code/)).toBeInTheDocument()
    expect(within(dlg).getByRole('button', { name: 'File configuration change' })).toBeDisabled()
    await user.clear(cur)
    await user.type(cur, 'EUR')
    await user.click(within(dlg).getByRole('button', { name: 'File configuration change' }))
    await within(dlg).findByText(/Applied at once/)
    expect((bodies[0] as { payload: Record<string, unknown> }).payload).toMatchObject({ allowed_currencies: ['EUR'], allowed_payment_methods: ['card'], geo_block_list: ['YY'], kyc_ruleset_id: 'kyc-a' })
  })

  it('close sends every content key as null and is a narrowing change', async () => {
    tenantSetup({ configs: [config()] })
    const bodies = capturePost(() => HttpResponse.json(executedBody('tenant_jurisdiction_config_version'), { status: 201 }))
    renderWithProviders(<AppRoutes />, { route: `/licensing/tenants/${TEN1}` })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: /^Close / }))
    const dlg = await screen.findByRole('dialog', { name: 'File a tenant jurisdiction configuration change' })
    expect(within(dlg).getByRole('note', { name: 'Close note' })).toHaveTextContent('narrowing change')
    expect(within(dlg).queryByLabelText('Allowed currencies')).toBeNull()
    await user.type(within(dlg).getByLabelText('Reason code'), 'wind_down')
    await user.click(within(dlg).getByRole('button', { name: 'File configuration change' }))
    await within(dlg).findByText(/Applied at once/)
    expect(bodies).toEqual([
      {
        kind: 'tenant_jurisdiction_config_version',
        reason_code: 'wind_down',
        payload: { tenant_id: TEN1, jurisdiction_id: JUR1, action: 'close', kyc_ruleset_id: null, aml_ruleset_id: null, rg_ruleset_id: null, reporting_ruleset_id: null, allowed_currencies: null, allowed_payment_methods: null, geo_block_list: null },
      },
    ])
  })
})

describe('jurisdiction creation', () => {
  async function openCreate(user: User) {
    await user.click(await screen.findByRole('button', { name: 'Create a jurisdiction' }))
    return screen.findByRole('dialog', { name: 'Create a jurisdiction' })
  }

  it('says plainly that it is a registry record that is NOT four-eyes gated; starts empty', async () => {
    signInAs(PA)
    licensingReads()
    renderWithProviders(<AppRoutes />, { route: '/licensing/jurisdictions' })
    const user = userEvent.setup()
    const dlg = await openCreate(user)
    for (const l of ['Code', 'Name', 'Regulatory body (optional)', 'Notes (optional)', 'Reason code']) expect(within(dlg).getByLabelText(l)).toHaveValue('')
    expect(within(dlg).getByRole('note', { name: 'Jurisdiction note' })).toHaveTextContent('NOT four-eyes gated')
    expect(within(dlg).getByRole('note', { name: 'Jurisdiction note' })).toHaveTextContent('not an approval of the jurisdiction')
    expect(within(dlg).getByRole('button', { name: 'Create jurisdiction' })).toBeDisabled()
  })

  it('sends only what was typed (optional keys absent when blank) and shows the created row as a link', async () => {
    signInAs(PA)
    licensingReads()
    const bodies: unknown[] = []
    server.use(http.post('/v1/admin/jurisdictions', async ({ request }) => { bodies.push(await request.json()); return HttpResponse.json({ id: JUR1, code: 'NEW1', name: 'New Land', created_at: '2026-10-01T00:00:00Z' }, { status: 201 }) }))
    renderWithProviders(<AppRoutes />, { route: '/licensing/jurisdictions' })
    const user = userEvent.setup()
    const dlg = await openCreate(user)
    await user.type(within(dlg).getByLabelText('Code'), 'NEW1')
    await user.type(within(dlg).getByLabelText('Name'), 'New Land')
    await user.type(within(dlg).getByLabelText('Reason code'), 'registry_add')
    await user.click(within(dlg).getByRole('button', { name: 'Create jurisdiction' }))
    expect(await within(dlg).findByText(/Jurisdiction recorded/)).toBeInTheDocument()
    expect(within(dlg).getByRole('link', { name: 'Open NEW1' })).toHaveAttribute('href', `/licensing/jurisdictions/${JUR1}`)
    expect(bodies).toEqual([{ code: 'NEW1', name: 'New Land', reason_code: 'registry_add' }])
  })

  it('a duplicate code (400) is shown in fixed words; a 201 without a jurisdiction body is not confirmed; a 403 is a refusal', async () => {
    signInAs(PA)
    licensingReads()
    let n = 0
    server.use(http.post('/v1/admin/jurisdictions', () => { n++; return n === 1 ? refusal(400, 'validation_error') : n === 2 ? HttpResponse.json({ ok: true }, { status: 201 }) : refusal(403, 'forbidden') }))
    renderWithProviders(<AppRoutes />, { route: '/licensing/jurisdictions' })
    const user = userEvent.setup()
    let dlg = await openCreate(user)
    const fill = async () => {
      await user.type(within(dlg).getByLabelText('Code'), 'DUP')
      await user.type(within(dlg).getByLabelText('Name'), 'Dup')
      await user.type(within(dlg).getByLabelText('Reason code'), 'r_one')
      await user.click(within(dlg).getByRole('button', { name: 'Create jurisdiction' }))
    }
    await fill()
    expect(await within(dlg).findByRole('alert')).toHaveTextContent('rejected the request as invalid')
    expect(document.body).not.toHaveTextContent('do-not-render')
    await user.click(within(dlg).getByRole('button', { name: 'Cancel' }))
    dlg = await openCreate(user)
    await fill()
    expect(await within(dlg).findByText(/Nothing is confirmed/)).toBeInTheDocument()
    expect(within(dlg).queryByText(/Jurisdiction recorded/)).toBeNull()
    await user.click(within(dlg).getAllByRole('button', { name: 'Close' }).slice(-1)[0])
    dlg = await openCreate(user)
    await fill()
    expect(await within(dlg).findByRole('alert')).toHaveTextContent(/does not hold the permission/)
  })

  it('hostile text in a created row renders as text', async () => {
    signInAs(PA)
    licensingReads()
    const evil = '<img src=x onerror="alert(1)">'
    server.use(http.get('/v1/admin/jurisdictions', () => HttpResponse.json([{ id: JUR1, code: 'XX', name: evil, regulatory_body: evil, created_at: '2026-01-01T00:00:00Z' }])))
    const { container } = renderWithProviders(<AppRoutes />, { route: '/licensing/jurisdictions' })
    expect((await screen.findAllByText(evil)).length).toBeGreaterThan(0)
    expect(container.querySelector('img')).toBeNull()
  })
})

describe('change detail renders market payloads in words', () => {
  it('ceiling: sentence, approval path, reference labelled operator-supplied; hostile text stays text', async () => {
    signInAs(PA)
    const evil = '<img src=x onerror="alert(1)">'
    licensingReads({ changes: [change({ kind: 'licence_ceiling_version', requested_by: OTHER, widening: true, payload: { licence_id: LIC1, country_code: 'ZZ', state: 'enabled', status: 'active', authorization_reference: evil } })] })
    const { container } = renderWithProviders(<AppRoutes />, { route: `/licensing/changes/${CHG1}` })
    const sum = await screen.findByRole('region', { name: 'Market configuration summary' })
    expect(within(sum).getByLabelText('Change in words')).toHaveTextContent(`On licence ${LIC1}: ENABLE country ZZ in the licence country ceiling.`)
    expect(within(sum).getByLabelText('Approval path')).toHaveTextContent('Widening: needs 2 approvals from 2 different people')
    expect(within(sum).getByText('Authorization reference (supplied by the operator, not verified)')).toBeInTheDocument()
    expect(within(sum).getByText(evil)).toBeInTheDocument()
    expect(container.querySelector('img')).toBeNull()
    expect(within(sum).getByRole('link', { name: 'See this licence' })).toHaveAttribute('href', `/licensing/licences/${LIC1}`)
    expect(screen.getByRole('button', { name: 'Approve' })).toBeEnabled()
  })

  it('policy: rung, brand, operation and product in words; narrowing path for a disable', async () => {
    signInAs(PA)
    licensingReads({ changes: [change({ kind: 'operating_policy_version', widening: false, required_approvals: 1, payload: { tenant_id: TEN1, scope: 'operation', brand_id: BRAND1, operation_code: 'deposit', product_code: null, country_code: 'ZZ', state: 'disabled', status: 'active', authorization_reference: null } })] })
    renderWithProviders(<AppRoutes />, { route: `/licensing/changes/${CHG1}` })
    const sum = await screen.findByRole('region', { name: 'Market configuration summary' })
    expect(within(sum).getByLabelText('Change in words')).toHaveTextContent(`operation rung: DISABLE country ZZ for operation deposit, brand ${BRAND1}, all products.`)
    expect(within(sum).getByLabelText('Approval path')).toHaveTextContent('Narrowing: applied at once')
    expect(within(sum).getByRole('link', { name: "See this tenant's licensing" })).toHaveAttribute('href', `/licensing/tenants/${TEN1}`)
  })

  it('configuration: set and close are explained; lists are printed', async () => {
    signInAs(PA)
    licensingReads({ changes: [change({ kind: 'tenant_jurisdiction_config_version', payload: { tenant_id: TEN1, jurisdiction_id: JUR1, action: 'set', kyc_ruleset_id: 'kyc-a', aml_ruleset_id: null, rg_ruleset_id: null, reporting_ruleset_id: null, allowed_currencies: ['EUR'], allowed_payment_methods: [], geo_block_list: ['YY'] } })] })
    renderWithProviders(<AppRoutes />, { route: `/licensing/changes/${CHG1}` })
    const sum = await screen.findByRole('region', { name: 'Market configuration summary' })
    expect(within(sum).getByLabelText('Change in words')).toHaveTextContent('SET a new version (replacing the version in force)')
    expect(within(sum).getByText('EUR')).toBeInTheDocument()
    expect(within(sum).getByText('YY')).toBeInTheDocument()
  })

  it('an unrelated kind shows no market summary', async () => {
    signInAs(PA)
    licensingReads()
    renderWithProviders(<AppRoutes />, { route: `/licensing/changes/${CHG1}` })
    await screen.findByText(/request/i)
    expect(screen.queryByRole('region', { name: 'Market configuration summary' })).toBeNull()
  })
})
