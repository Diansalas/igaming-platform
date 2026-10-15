import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it, vi } from 'vitest'
import { AppRoutes } from '../../app/AppRoutes'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { PLATFORM_ADMIN_CLAIMS, signInAs } from '../../test/session'
import { FI, HASH1, HASH2, RES2, TA, TENANT, WD1, holdResolution, refusal } from '../../test/consoleFixtures'
import { buildHoldRequest, validateHoldRequest } from './HoldResolutionsPage'

// Slow under a loaded full run (many keystrokes); the longer timeout applies to this file only.
vi.setConfig({ testTimeout: 30000 })

const BASE = `/v1/admin/tenants/${TENANT}/withdrawal-hold-resolutions`
const PA = { ...PLATFORM_ADMIN_CLAIMS, sub: 'f9000000-0000-4000-8000-000000000009' }
const route = `/hold-resolutions?tenant=${TENANT}`

function setup(rows = [holdResolution()]) {
  server.use(
    http.get('/v1/admin/tenants', () => HttpResponse.json({ items: [{ id: TENANT, name: 'Acme', slug: 'acme', status: 'closed' }], limit: 50, offset: 0, total: 1 })),
    http.get(BASE, () => HttpResponse.json({ items: rows })),
  )
}

describe('withdrawal hold release (S8, HSEC)', () => {
  it('is platform_admin only: every tenant role gets NotAuthorized and no request is made', async () => {
    for (const claims of [FI, TA, { role: 'compliance', tenant_id: TENANT, sub: 'x' }]) {
      signInAs(claims)
      let calls = 0
      server.use(http.get(/withdrawal-hold-resolutions/, () => { calls++; return HttpResponse.json({ items: [] }) }))
      const r = renderWithProviders(<AppRoutes />, { route })
      expect(await screen.findByText('Your role does not have access to this section.')).toBeInTheDocument()
      expect(calls).toBe(0)
      r.unmount()
    }
  })

  it('platform admin sees the banner, the candidate-list gap, and the frozen hash; own request cannot be approved', async () => {
    signInAs(PA)
    setup([holdResolution({ requested_by: PA.sub })])
    renderWithProviders(<AppRoutes />, { route })
    expect(await screen.findByRole('note', { name: 'Stack status' })).toHaveTextContent('not a regulatory approval')
    expect(screen.getByText(/does not show candidate withdrawals/)).toBeInTheDocument()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: `Open hold-resolution ${RES2.slice(0, 8)}` }))
    const d = await screen.findByRole('region', { name: 'Hold-resolution detail' })
    expect(within(d).getByText(HASH1)).toBeInTheDocument()
    expect(within(d).getByRole('button', { name: 'Approve' })).toBeDisabled()
    expect(within(d).getByRole('button', { name: 'Cancel request' })).toBeEnabled()
  })

  it('pure form rules match the spec (reason token, 64-hex hash, UUID, only release_hold_to_player)', () => {
    const f = { withdrawalId: WD1, reason: 'tenant_closed', evidenceHash: HASH2, note: '' }
    expect(buildHoldRequest(f)).toEqual({ withdrawal_request_id: WD1, kind: 'release_hold_to_player', reason_code: 'tenant_closed', evidence_ref_hash: HASH2 })
    expect(validateHoldRequest(f)).toEqual([])
    expect(validateHoldRequest({ ...f, reason: 'Tenant Closed' })).toHaveLength(1)
    expect(validateHoldRequest({ ...f, reason: '1abc' })).toHaveLength(1)
    expect(validateHoldRequest({ ...f, evidenceHash: HASH2.toUpperCase() })).toHaveLength(1)
    expect(validateHoldRequest({ ...f, withdrawalId: 'x' })).toHaveLength(1)
    expect(buildHoldRequest({ ...f, note: 'n' }).note).toBe('n')
  })

  it('files a request: strong confirmation first, then one POST with the exact body and an Idempotency-Key', async () => {
    signInAs(PA)
    setup([])
    const posts: Array<{ body: unknown; key: string | null }> = []
    server.use(http.post(BASE, async ({ request }) => { posts.push({ body: await request.json(), key: request.headers.get('Idempotency-Key') }); return HttpResponse.json(holdResolution(), { status: 201 }) }))
    renderWithProviders(<AppRoutes />, { route })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Request a hold release' }))
    const dlg = await screen.findByRole('dialog', { name: 'Request a hold release' })
    await user.click(within(dlg).getByLabelText('Withdrawal id'))
    await user.paste(WD1)
    await user.type(within(dlg).getByLabelText('Reason code (required)'), 'tenant_closed')
    await user.click(within(dlg).getByLabelText('Evidence reference hash'))
    await user.paste(HASH2)
    const go = within(dlg).getByRole('button', { name: 'File request' })
    expect(go).toBeDisabled()
    await user.click(within(dlg).getByRole('checkbox'))
    expect(go).toBeDisabled()
    await user.type(within(dlg).getByLabelText(/Type the LAST 6 characters of the withdrawal id/), WD1.slice(-6))
    await user.dblClick(go)
    expect(await screen.findByRole('status')).toHaveTextContent('Nothing was released')
    expect(posts).toHaveLength(1)
    expect(posts[0].body).toEqual({ withdrawal_request_id: WD1, kind: 'release_hold_to_player', reason_code: 'tenant_closed', evidence_ref_hash: HASH2 })
    expect(posts[0].key).toBeTruthy()
  })

  it('maps every HSEC refusal through the closed table and never prints the server message', async () => {
    signInAs(PA)
    setup()
    const tokens: Array<[number, string, string, RegExp]> = [
      [409, 'conflict', 'hold_resolution_disabled', /not enabled for this tenant/],
      [409, 'conflict', 'hold_resolution_precondition_failed', /withdrawal must be approved/],
      [409, 'conflict', 'hold_resolution_expired', /expired/],
      [403, 'forbidden', 'hold_resolution_not_permitted', /no in-force platform grant or acting session/],
      [404, 'not_found', 'hold_resolution_not_found', /does not exist/],
    ]
    let i = 0
    server.use(http.post(`${BASE}/${RES2}/approve`, () => { const t = tokens[Math.min(i++, tokens.length - 1)]; return refusal(t[0], t[1], t[2]) }))
    renderWithProviders(<AppRoutes />, { route })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: `Open hold-resolution ${RES2.slice(0, 8)}` }))
    const d = await screen.findByRole('region', { name: 'Hold-resolution detail' })
    await user.click(within(d).getByRole('button', { name: 'Approve' }))
    const dlg = await screen.findByRole('dialog', { name: 'Approve hold release' })
    await user.type(within(dlg).getByLabelText('Reason code (required)'), 'reviewed_ok')
    await user.click(within(dlg).getByRole('checkbox'))
    await user.type(within(dlg).getByLabelText(/Type the LAST 8 characters of the payload hash/), HASH1.slice(-8))
    for (const [, , , re] of tokens) {
      await user.click(within(dlg).getByRole('button', { name: 'Approve' }))
      await waitFor(() => expect(within(dlg).getByRole('alert')).toHaveTextContent(re))
    }
    expect(document.body).not.toHaveTextContent('do-not-render')
  })

  it('an approved release reports execution, and a refused-at-execution one reports no movement', async () => {
    for (const [body, text] of [
      [{ resolution: holdResolution({ state: 'executed', ledger_transaction_id: '90000000-0000-4000-8000-000000000001' }), executed: true, refused: false, counted_approvals: 2, required_approvals: 2 }, /Executed\./],
      [{ resolution: holdResolution({ state: 'refused_at_execution', refusal_code: 'tenant_brand_active_again' }), executed: false, refused: true, counted_approvals: 2, required_approvals: 2 }, /the tenant and brand are active again.*no money moved/],
    ] as const) {
      signInAs(PA)
      setup()
      server.use(http.post(`${BASE}/${RES2}/approve`, () => HttpResponse.json(body)))
      const r = renderWithProviders(<AppRoutes />, { route })
      const user = userEvent.setup()
      await user.click(await screen.findByRole('button', { name: `Open hold-resolution ${RES2.slice(0, 8)}` }))
      const d = await screen.findByRole('region', { name: 'Hold-resolution detail' })
      await user.click(within(d).getByRole('button', { name: 'Approve' }))
      const dlg = await screen.findByRole('dialog', { name: 'Approve hold release' })
      await user.type(within(dlg).getByLabelText('Reason code (required)'), 'reviewed_ok')
      await user.click(within(dlg).getByRole('checkbox'))
      await user.type(within(dlg).getByLabelText(/Type the LAST 8 characters of the payload hash/), HASH1.slice(-8))
      await user.click(within(dlg).getByRole('button', { name: 'Approve' }))
      expect(await screen.findByRole('status')).toHaveTextContent(text)
      r.unmount()
    }
  })

  it('the hold reason must be a lowercase token (the spec pattern) before a decision can be sent', async () => {
    signInAs(PA)
    setup()
    renderWithProviders(<AppRoutes />, { route })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: `Open hold-resolution ${RES2.slice(0, 8)}` }))
    const d = await screen.findByRole('region', { name: 'Hold-resolution detail' })
    await user.click(within(d).getByRole('button', { name: 'Reject' }))
    const dlg = await screen.findByRole('dialog', { name: 'Reject hold release' })
    await user.type(within(dlg).getByLabelText('Reason code (required)'), 'Not A Token')
    await user.click(within(dlg).getByRole('checkbox'))
    await user.type(within(dlg).getByLabelText(/Type the LAST 8 characters of the payload hash/), HASH1.slice(-8))
    expect(within(dlg).getByRole('button', { name: 'Reject' })).toBeDisabled()
  })
})
