import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it, vi } from 'vitest'
import { AppRoutes } from '../../app/AppRoutes'
import { formatAmountBoth } from '../../lib/minorUnits'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { PLATFORM_ADMIN_CLAIMS, signInAs } from '../../test/session'
import { ATT1, FI, HASH1, HASH2, RES1, RES2, STAFF_A, TENANT, WD1, attempt, holdResolution, refusal, resolution } from '../../test/consoleFixtures'
import { validateForceRequest, EMPTY_FORM } from './ForceRequestDialog'

// Slow under a loaded full run (many keystrokes); the longer timeout applies to this file only.
vi.setConfig({ testTimeout: 30000 })

const K3 = `/v1/admin/tenants/${TENANT}/payment-force-resolutions`
const HS = `/v1/admin/tenants/${TENANT}/withdrawal-hold-resolutions`
const PA = { ...PLATFORM_ADMIN_CLAIMS, sub: 'f9000000-0000-4000-8000-000000000009' }

const tenants = () => server.use(http.get('/v1/admin/tenants', () => HttpResponse.json({ items: [{ id: TENANT, name: 'Acme Gaming', slug: 'acme', status: 'active' }], limit: 50, offset: 0, total: 1 })))

async function openApprove(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole('button', { name: `Open resolution ${RES1.slice(0, 8)}` }))
  const d = await screen.findByRole('region', { name: 'Resolution detail' })
  await user.click(within(d).getByRole('button', { name: 'Approve' }))
  return screen.findByRole('dialog', { name: 'Approve force-resolution' })
}

describe('F2: amounts use the exponent the server returns', () => {
  it('formats major units AND minor units for 2, 8 and 18, and refuses to format without an exponent', () => {
    expect(formatAmountBoth('12345', 2, 'EUR')).toBe('123.45 EUR (12345 minor units)')
    expect(formatAmountBoth('12345', 8, 'BTC')).toBe('0.00012345 BTC (12345 minor units)')
    expect(formatAmountBoth('1500000000000000000', 18, 'ETH')).toBe('1.500000000000000000 ETH (1500000000000000000 minor units)')
    expect(formatAmountBoth('12345', null, 'EUR')).toBe('12345 minor units of EUR (exponent unknown)')
    expect(formatAmountBoth('12345', undefined, 'EUR')).toContain('exponent unknown')
  })

  it('K3 list and detail show the formatted amount, and "exponent unknown" only when the server returned null', async () => {
    signInAs(FI)
    server.use(http.get(K3, () => HttpResponse.json({ items: [resolution({ exponent: 8, asset_code: 'BTC', amount_minor_units: '12345' }), resolution({ id: RES2, exponent: null, amount_minor_units: '777' })] })))
    renderWithProviders(<AppRoutes />, { route: '/force-resolutions' })
    expect(await screen.findByText('0.00012345 BTC (12345 minor units)')).toBeInTheDocument()
    expect(screen.getByText('777 minor units of EUR (exponent unknown)')).toBeInTheDocument()
  })

  it('HSEC list shows the formatted amount', async () => {
    signInAs(PA)
    tenants()
    server.use(http.get(HS, () => HttpResponse.json({ items: [holdResolution({ exponent: 2 })] })))
    renderWithProviders(<AppRoutes />, { route: `/hold-resolutions?tenant=${TENANT}` })
    expect(await screen.findByText('50.00 EUR (5000 minor units)')).toBeInTheDocument()
    expect(document.body).not.toHaveTextContent('exponent unknown')
  })
})

describe('F3: the decision dialog lists every field the hash covers', () => {
  it('K3: attempt id, operation, kind, finding/basis/context, evidence hash, reason code, amount, target state', async () => {
    signInAs(FI)
    server.use(http.get(K3, () => HttpResponse.json({ items: [resolution({ finding_code: undefined, context_code: 'provider_unqueryable' })] })))
    renderWithProviders(<AppRoutes />, { route: '/force-resolutions' })
    const dlg = await openApprove(userEvent.setup())
    for (const label of ['Attempt id', 'Operation', 'Kind', 'Finding', 'Basis', 'Context', 'Evidence reference hash', 'Reason code on the request', 'Amount', 'Target state']) {
      expect(within(dlg).getByText(label)).toBeInTheDocument()
    }
    expect(within(dlg).getByText(ATT1)).toBeInTheDocument()
    expect(within(dlg).getByText(HASH2)).toBeInTheDocument()
    expect(within(dlg).getByText('provider_unqueryable_case')).toBeInTheDocument()
    expect(within(dlg).getByText('reconciliation_exhausted')).toBeInTheDocument()
    expect(within(dlg).getByText('provider_unqueryable')).toBeInTheDocument()
    expect(within(dlg).getByText(/I reviewed every field listed above/)).toBeInTheDocument()
  })

  it('HSEC: reason code and evidence hash are listed', async () => {
    signInAs(PA)
    tenants()
    server.use(http.get(HS, () => HttpResponse.json({ items: [holdResolution()] })))
    renderWithProviders(<AppRoutes />, { route: `/hold-resolutions?tenant=${TENANT}` })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: `Open hold-resolution ${RES2.slice(0, 8)}` }))
    await user.click(within(await screen.findByRole('region', { name: 'Hold-resolution detail' })).getByRole('button', { name: 'Approve' }))
    const dlg = await screen.findByRole('dialog', { name: 'Approve hold release' })
    expect(within(dlg).getByText('Reason code on the request')).toBeInTheDocument()
    expect(within(dlg).getByText('tenant_closed')).toBeInTheDocument()
    expect(within(dlg).getByText(HASH2)).toBeInTheDocument()
    expect(within(dlg).getByText(WD1)).toBeInTheDocument()
  })
})

describe('F4: the typed confirmation is not copyable from its own label', () => {
  it('the decision label does not print the required string, and the FIRST 8 characters do not satisfy it', async () => {
    signInAs(FI)
    server.use(http.get(K3, () => HttpResponse.json({ items: [resolution()] })))
    renderWithProviders(<AppRoutes />, { route: '/force-resolutions' })
    const user = userEvent.setup()
    const dlg = await openApprove(user)
    const input = within(dlg).getByLabelText(/Type the LAST 8 characters of the payload hash/)
    expect(input.closest('div')).not.toHaveTextContent(HASH1.slice(-8))
    await user.type(within(dlg).getByLabelText('Reason code (required)'), 'ok')
    await user.click(within(dlg).getByRole('checkbox'))
    await user.type(input, HASH1.slice(0, 8))
    expect(within(dlg).getByRole('button', { name: 'Approve' })).toBeDisabled()
    await user.clear(input)
    await user.type(input, HASH1.slice(-8))
    expect(within(dlg).getByRole('button', { name: 'Approve' })).toBeEnabled()
  })

  it('a ?attempt= pre-fill does not satisfy the request confirmation: the operator still types the last 6 characters', async () => {
    signInAs(FI)
    server.use(http.get(K3, () => HttpResponse.json({ items: [] })), http.get(`/v1/admin/tenants/${TENANT}/payment-attempts/${ATT1}`, () => HttpResponse.json(attempt())))
    renderWithProviders(<AppRoutes />, { route: `/force-resolutions?attempt=${ATT1}` })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Propose a force-resolution' }))
    const dlg = await screen.findByRole('dialog', { name: 'Propose a force-resolution' })
    const confirm = within(dlg).getByLabelText(/Type the LAST 6 characters of the attempt id/)
    expect(confirm).toHaveValue('')
    expect(confirm.closest('div')).not.toHaveTextContent(ATT1.slice(-6))
    await user.selectOptions(within(dlg).getByLabelText('Kind'), 'm2_declare_paid')
    await user.selectOptions(within(dlg).getByLabelText('Basis'), 'reconciliation_exhausted')
    await user.click(within(dlg).getByLabelText('Evidence reference hash'))
    await user.paste(HASH2)
    await user.type(within(dlg).getByLabelText('Reason code (required)'), 'why')
    await user.click(within(dlg).getByRole('checkbox'))
    await user.type(confirm, ATT1.slice(0, 6))
    expect(within(dlg).getByRole('button', { name: 'File proposal' })).toBeDisabled()
    await user.clear(confirm)
    await user.type(confirm, ATT1.slice(-6))
    expect(within(dlg).getByRole('button', { name: 'File proposal' })).toBeEnabled()
  })
})

describe('F5: after a retried send, a definite refusal does not claim "nothing was changed"', () => {
  it('first send fails with a 500, the retry gets 409 expired: the message says it may already be recorded', async () => {
    signInAs(FI)
    server.use(http.get(K3, () => HttpResponse.json({ items: [resolution()] })))
    let n = 0
    server.use(http.post(`${K3}/${RES1}/approve`, () => (++n === 1 ? refusal(500, 'internal_error') : refusal(409, 'conflict', 'force_resolve_conflict'))))
    renderWithProviders(<AppRoutes />, { route: '/force-resolutions' })
    const user = userEvent.setup()
    const dlg = await openApprove(user)
    await user.type(within(dlg).getByLabelText('Reason code (required)'), 'ok')
    await user.click(within(dlg).getByRole('checkbox'))
    await user.type(within(dlg).getByLabelText(/Type the LAST 8 characters/), HASH1.slice(-8))
    await user.click(within(dlg).getByRole('button', { name: 'Approve' }))
    expect(await within(dlg).findByRole('alert')).toHaveTextContent(/could not complete the request/)
    await user.click(within(dlg).getByRole('button', { name: 'Approve' }))
    const alert = await within(dlg).findByText(/may already be recorded/)
    expect(alert).toBeInTheDocument()
    expect(within(dlg).getByRole('alert')).not.toHaveTextContent('Nothing was changed')
  })

  it('a first-send refusal still says nothing was changed', async () => {
    signInAs(FI)
    server.use(http.get(K3, () => HttpResponse.json({ items: [resolution()] })), http.post(`${K3}/${RES1}/approve`, () => refusal(409, 'conflict', 'force_resolve_disabled')))
    renderWithProviders(<AppRoutes />, { route: '/force-resolutions' })
    const user = userEvent.setup()
    const dlg = await openApprove(user)
    await user.type(within(dlg).getByLabelText('Reason code (required)'), 'ok')
    await user.click(within(dlg).getByRole('checkbox'))
    await user.type(within(dlg).getByLabelText(/Type the LAST 8 characters/), HASH1.slice(-8))
    await user.click(within(dlg).getByRole('button', { name: 'Approve' }))
    expect(await within(dlg).findByRole('alert')).toHaveTextContent('Nothing was changed.')
  })
})

describe('F6: a platform decision dialog names the tenant it acts on', () => {
  it('shows the tenant name and full id in the K3 approve dialog, the request dialog and the HSEC dialogs', async () => {
    signInAs(PA)
    tenants()
    server.use(http.get(K3, () => HttpResponse.json({ items: [resolution()] })), http.get(HS, () => HttpResponse.json({ items: [holdResolution()] })))
    const r = renderWithProviders(<AppRoutes />, { route: `/force-resolutions?tenant=${TENANT}` })
    const user = userEvent.setup()
    const dlg = await openApprove(user)
    expect(within(dlg).getByLabelText('Acting on tenant')).toHaveTextContent(`Acme Gaming ${TENANT}`)
    await user.click(within(dlg).getByRole('button', { name: 'Back' }))
    await user.click(screen.getByRole('button', { name: 'Propose a force-resolution' }))
    expect(within(await screen.findByRole('dialog', { name: 'Propose a force-resolution' })).getByLabelText('Acting on tenant')).toHaveTextContent('Acme Gaming')
    r.unmount()
    renderWithProviders(<AppRoutes />, { route: `/hold-resolutions?tenant=${TENANT}` })
    await user.click(await screen.findByRole('button', { name: 'Request a hold release' }))
    expect(within(await screen.findByRole('dialog', { name: 'Request a hold release' })).getByLabelText('Acting on tenant')).toHaveTextContent(TENANT)
  })
})

describe('F9: the K3 reason is bounded in BYTES like the server', () => {
  it('33 two-byte characters (66 bytes) are refused locally; 32 are accepted', () => {
    const f = { ...EMPTY_FORM, attemptId: ATT1, kind: 'm1_deposit_evidence' as const, findingCode: 'awaiting_psp_refund' }
    expect(validateForceRequest({ ...f, reason: 'é'.repeat(32) })).toEqual([])
    expect(validateForceRequest({ ...f, reason: 'é'.repeat(33) })).toHaveLength(1)
    expect(validateForceRequest({ ...f, reason: 'a'.repeat(64) })).toEqual([])
    expect(validateForceRequest({ ...f, reason: 'a'.repeat(65) })).toHaveLength(1)
  })

  it('the approve dialog also blocks a 66-byte reason', async () => {
    signInAs(FI)
    server.use(http.get(K3, () => HttpResponse.json({ items: [resolution()] })))
    renderWithProviders(<AppRoutes />, { route: '/force-resolutions' })
    const user = userEvent.setup()
    const dlg = await openApprove(user)
    await user.click(within(dlg).getByLabelText('Reason code (required)'))
    await user.paste('é'.repeat(33))
    await user.click(within(dlg).getByRole('checkbox'))
    await user.type(within(dlg).getByLabelText(/Type the LAST 8 characters/), HASH1.slice(-8))
    expect(within(dlg).getByRole('button', { name: 'Approve' })).toBeDisabled()
    expect(STAFF_A).toBeTruthy()
  })
})
