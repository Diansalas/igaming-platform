import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse, delay } from 'msw'
import { describe, expect, it, vi } from 'vitest'
import { AppRoutes } from '../../app/AppRoutes'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { PLATFORM_ADMIN_CLAIMS, signInAs } from '../../test/session'
import { makeTestJwt } from '../../test/jwt'
import { GOOD_CODE, stepUpRequiredResponse } from '../../test/stepUp'
import { ATT1, CO, FI, HASH1, HASH2, RES1, STAFF_A, TA, TENANT, attempt, refusal, resolution } from '../../test/consoleFixtures'
import { buildForceRequest, EMPTY_FORM, validateForceRequest } from './ForceRequestDialog'

// Slow under a loaded full run (many keystrokes); the longer timeout applies to this file only.
vi.setConfig({ testTimeout: 30000 })

const BASE = `/v1/admin/tenants/${TENANT}/payment-force-resolutions`
const decision = (over: Record<string, unknown> = {}) => ({ resolution: resolution({ state: 'pending' }), executed: false, refused: false, counted_approvals: 1, required_approvals: 2, ...over })

function list(rows = [resolution()]) {
  server.use(http.get(BASE, () => HttpResponse.json({ items: rows })))
}

async function openDetail(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole('button', { name: `Open resolution ${RES1.slice(0, 8)}` }))
  return screen.findByRole('region', { name: 'Resolution detail' })
}

async function fillDecision(user: ReturnType<typeof userEvent.setup>, reason = 'reviewed_ok', hash = HASH1) {
  const dlg = await screen.findByRole('dialog')
  await user.type(within(dlg).getByLabelText('Reason code (required)'), reason)
  await user.click(within(dlg).getByRole('checkbox'))
  await user.type(within(dlg).getByLabelText(/Type the LAST 8 characters of the payload hash/), hash.slice(-8))
  return dlg
}

describe('force-resolution list and detail (S8)', () => {
  it('shows the MOCK banner, the resolutions, the frozen payload hash and the 100-row API limit', async () => {
    signInAs(FI)
    list()
    renderWithProviders(<AppRoutes />, { route: '/force-resolutions' })
    expect(await screen.findByRole('note', { name: 'Stack status' })).toHaveTextContent('Sandbox/MOCK provider')
    const user = userEvent.setup()
    const d = await openDetail(user)
    expect(within(d).getByText(HASH1)).toBeInTheDocument()
    expect(within(d).getByText('M2 - declare the payout paid')).toBeInTheDocument()
    expect(screen.getByText(/100 most recent/)).toBeInTheDocument()
    // the evidence reference TEXT is never rendered; only its hash
    expect(d).not.toHaveTextContent('evidence_reference')
    expect(within(d).getByRole('link', { name: ATT1 })).toHaveAttribute('href', `/parked-payments?tenant=${TENANT}&attempt=${ATT1}`)
  })

  it('compliance is read only: no propose button, no decision buttons, and it says so', async () => {
    signInAs(CO)
    list()
    renderWithProviders(<AppRoutes />, { route: '/force-resolutions' })
    const user = userEvent.setup()
    const d = await openDetail(user)
    expect(screen.queryByRole('button', { name: 'Propose a force-resolution' })).toBeNull()
    expect(within(d).queryByRole('button', { name: 'Approve' })).toBeNull()
    expect(within(d).getByText(/read only for force-resolutions/)).toBeInTheDocument()
  })

  it('tenant_admin and others are NotAuthorized with no request', async () => {
    signInAs(TA)
    let calls = 0
    server.use(http.get(BASE, () => { calls++; return HttpResponse.json({ items: [] }) }))
    renderWithProviders(<AppRoutes />, { route: '/force-resolutions' })
    expect(await screen.findByText('Your role does not have access to this section.')).toBeInTheDocument()
    expect(calls).toBe(0)
  })

  it('the requester cannot approve their own request (button off, hint shown) but can cancel it', async () => {
    signInAs(FI) // sub STAFF_A
    list([resolution({ requested_by: STAFF_A })])
    renderWithProviders(<AppRoutes />, { route: '/force-resolutions' })
    const user = userEvent.setup()
    const d = await openDetail(user)
    expect(within(d).getByRole('button', { name: 'Approve' })).toBeDisabled()
    expect(within(d).getByText(/server refuses self-approval/)).toBeInTheDocument()
    expect(within(d).getByRole('button', { name: 'Cancel request' })).toBeEnabled()
  })

  it('a non-pending resolution offers no decision', async () => {
    signInAs(FI)
    list([resolution({ state: 'executed', ledger_transaction_id: '90000000-0000-4000-8000-000000000001' })])
    renderWithProviders(<AppRoutes />, { route: '/force-resolutions' })
    const user = userEvent.setup()
    const d = await openDetail(user)
    expect(within(d).queryByRole('button', { name: 'Approve' })).toBeNull()
    expect(within(d).getByText(/no longer pending/)).toBeInTheDocument()
  })
})

describe('approve / reject with a frozen payload hash (S8)', () => {
  it('needs reason + acknowledgement + the typed hash prefix, then posts exactly {payload_hash, reason_code} with an Idempotency-Key and no tenant or proof field', async () => {
    signInAs(FI)
    list()
    const posts: Array<{ url: string; body: Record<string, unknown>; key: string | null }> = []
    server.use(
      http.post(`${BASE}/${RES1}/approve`, async ({ request }) => {
        posts.push({ url: request.url, body: (await request.json()) as Record<string, unknown>, key: request.headers.get('Idempotency-Key') })
        return HttpResponse.json(decision())
      }),
    )
    renderWithProviders(<AppRoutes />, { route: '/force-resolutions' })
    const user = userEvent.setup()
    const d = await openDetail(user)
    await user.click(within(d).getByRole('button', { name: 'Approve' }))
    const dlg = await screen.findByRole('dialog', { name: 'Approve force-resolution' })
    const go = within(dlg).getByRole('button', { name: 'Approve' })
    expect(go).toBeDisabled()
    expect(within(dlg).getByTestId('frozen-hash')).toHaveTextContent(HASH1)
    await user.type(within(dlg).getByLabelText('Reason code (required)'), 'reviewed_ok')
    expect(go).toBeDisabled()
    await user.click(within(dlg).getByRole('checkbox'))
    expect(go).toBeDisabled()
    await user.type(within(dlg).getByLabelText(/Type the LAST 8 characters of the payload hash/), 'wrong123')
    expect(go).toBeDisabled()
    await user.clear(within(dlg).getByLabelText(/Type the LAST 8 characters of the payload hash/))
    await user.type(within(dlg).getByLabelText(/Type the LAST 8 characters of the payload hash/), HASH1.slice(-8))
    expect(go).toBeEnabled()
    await user.click(go)
    expect(await screen.findByRole('status')).toHaveTextContent('Approval recorded: 1 of 2 required')
    expect(posts).toHaveLength(1)
    expect(posts[0].body).toEqual({ payload_hash: HASH1, reason_code: 'reviewed_ok' })
    expect(posts[0].key).toMatch(/^[0-9a-f-]{16,}$/)
    expect(posts[0].url).toContain(`/tenants/${TENANT}/`)
  })

  it('sends the hash it showed even if a refresh returns a different one, and blocks instead of sending the new value', async () => {
    signInAs(FI)
    list()
    let sent = 0
    server.use(http.post(`${BASE}/${RES1}/approve`, () => { sent++; return HttpResponse.json(decision()) }))
    renderWithProviders(<AppRoutes />, { route: '/force-resolutions' })
    const user = userEvent.setup()
    const d = await openDetail(user)
    await user.click(within(d).getByRole('button', { name: 'Approve' }))
    const dlg = await fillDecision(user)
    expect(within(dlg).getByRole('button', { name: 'Approve' })).toBeEnabled()
    list([resolution({ payload_hash: HASH2 })])
    await user.click(screen.getByRole('button', { name: 'Refresh' }))
    expect(await within(dlg).findByText(/changed after you opened this dialog/)).toBeInTheDocument()
    expect(within(dlg).getByTestId('frozen-hash')).toHaveTextContent(HASH1)
    expect(within(dlg).getByRole('button', { name: 'Approve' })).toBeDisabled()
    expect(sent).toBe(0)
  })

  it('double-click sends once', async () => {
    signInAs(FI)
    list()
    let sent = 0
    server.use(http.post(`${BASE}/${RES1}/approve`, async () => { sent++; await delay(150); return HttpResponse.json(decision()) }))
    renderWithProviders(<AppRoutes />, { route: '/force-resolutions' })
    const user = userEvent.setup()
    const d = await openDetail(user)
    await user.click(within(d).getByRole('button', { name: 'Approve' }))
    const dlg = await fillDecision(user)
    const go = within(dlg).getByRole('button', { name: 'Approve' })
    await user.dblClick(go)
    await screen.findByRole('status')
    expect(sent).toBe(1)
  })

  it('shows executed, refused-at-execution and pending outcomes distinctly (refusal code through the map)', async () => {
    for (const [body, text] of [
      [decision({ executed: true, counted_approvals: 2, resolution: resolution({ state: 'executed', ledger_transaction_id: '90000000-0000-4000-8000-000000000001' }) }), /Executed\./],
      [decision({ refused: true, resolution: resolution({ state: 'refused_at_execution', refusal_code: 'no_statement_source' }) }), /Refused at execution: no statement source is registered for the provider\. The approval was recorded, the resolution did NOT run and no money moved/],
      [decision({ refused: true, resolution: resolution({ state: 'refused_at_execution', refusal_code: '<b>x</b>' }) }), /unprintable/],
    ] as const) {
      signInAs(FI)
      list()
      server.use(http.post(`${BASE}/${RES1}/approve`, () => HttpResponse.json(body)))
      const r = renderWithProviders(<AppRoutes />, { route: '/force-resolutions' })
      const user = userEvent.setup()
      const d = await openDetail(user)
      await user.click(within(d).getByRole('button', { name: 'Approve' }))
      const dlg = await fillDecision(user)
      await user.click(within(dlg).getByRole('button', { name: 'Approve' }))
      expect(await screen.findByRole('status')).toHaveTextContent(text)
      expect(document.querySelector('b')).toBeNull()
      r.unmount()
    }
  })

  it('reject posts to the reject route with the same frozen hash', async () => {
    signInAs(FI)
    list()
    let body: unknown
    server.use(http.post(`${BASE}/${RES1}/reject`, async ({ request }) => { body = await request.json(); return HttpResponse.json(decision({ counted_approvals: 0 })) }))
    renderWithProviders(<AppRoutes />, { route: '/force-resolutions' })
    const user = userEvent.setup()
    const d = await openDetail(user)
    await user.click(within(d).getByRole('button', { name: 'Reject' }))
    const dlg = await fillDecision(user, 'not_supported')
    await user.click(within(dlg).getByRole('button', { name: 'Reject' }))
    expect(await screen.findByRole('status')).toHaveTextContent('Rejected. The resolution is closed and nothing was executed.')
    expect(body).toEqual({ payload_hash: HASH1, reason_code: 'not_supported' })
  })

  it('degrades correctly when the server refuses what the UI allowed: 403 not_permitted, 409 expired (refreshes), unknown message never shown', async () => {
    signInAs(FI)
    list()
    let n = 0
    server.use(
      http.post(`${BASE}/${RES1}/approve`, () => {
        n++
        if (n === 1) return refusal(403, 'forbidden', 'force_resolve_not_permitted')
        if (n === 2) return refusal(409, 'conflict', 'force_resolve_expired')
        return refusal(500, 'internal_error')
      }),
    )
    renderWithProviders(<AppRoutes />, { route: '/force-resolutions' })
    const user = userEvent.setup()
    const d = await openDetail(user)
    await user.click(within(d).getByRole('button', { name: 'Approve' }))
    const dlg = await fillDecision(user)
    await user.click(within(dlg).getByRole('button', { name: 'Approve' }))
    expect(await within(dlg).findByRole('alert')).toHaveTextContent(/no in-force capability grant for this action on this tenant/)
    expect(within(dlg).getByRole('alert')).toHaveTextContent('force_resolve_not_permitted')
    // the dialog stays usable after a refusal: a second attempt is allowed and is also rendered
    await user.click(within(dlg).getByRole('button', { name: 'Approve' }))
    await waitFor(() => expect(within(dlg).getByRole('alert')).toHaveTextContent(/expired and can no longer be decided/))
    await user.click(within(dlg).getByRole('button', { name: 'Approve' }))
    await waitFor(() => expect(within(dlg).getByRole('alert')).toHaveTextContent(/could not complete the request/))
    expect(document.body).not.toHaveTextContent('do-not-render')
    expect(n).toBe(3)
  })

  it('runs the step-up flow: the first send is refused step_up_required, the code is entered once, the SAME body and key are re-sent', async () => {
    signInAs(FI)
    // the step-up token must be for the same subject at aal2 or the client fails closed
    const STEPPED_JWT = makeTestJwt({ ...FI, jti: 'stepped-fi' })
    list()
    const sends: Array<{ auth: string | null; body: string; key: string | null }> = []
    server.use(
      http.post(`${BASE}/${RES1}/approve`, async ({ request }) => {
        const rec = { auth: request.headers.get('Authorization'), body: await request.text(), key: request.headers.get('Idempotency-Key') }
        sends.push(rec)
        return rec.auth === `Bearer ${STEPPED_JWT}` ? HttpResponse.json(decision()) : stepUpRequiredResponse()
      }),
      http.post('/v1/staff/auth/step-up', () =>
        HttpResponse.json({ access_token: STEPPED_JWT, token_type: 'Bearer', expires_in: 900, mfa_at: '2026-10-13T10:00:00Z', step_up_expires_at: { financial: '2026-10-13T10:10:00Z', critical: '2026-10-13T10:05:00Z' } }),
      ),
    )
    renderWithProviders(<AppRoutes />, { route: '/force-resolutions' })
    const user = userEvent.setup()
    const d = await openDetail(user)
    await user.click(within(d).getByRole('button', { name: 'Approve' }))
    const dlg = await fillDecision(user)
    await user.click(within(dlg).getByRole('button', { name: 'Approve' }))
    const su = await screen.findByRole('dialog', { name: 'Confirm it is you' })
    await user.type(within(su).getByLabelText('6-digit code'), GOOD_CODE)
    await user.click(within(su).getByRole('button', { name: 'Verify' }))
    expect(await screen.findByText(/Approval recorded/)).toBeInTheDocument()
    expect(sends).toHaveLength(2)
    expect(sends[1].body).toBe(sends[0].body)
    expect(sends[1].key).toBe(sends[0].key)
    expect(sends[1].key).toBeTruthy()
  })

  it('cancelling the step-up leaves the approval unsent and says so', async () => {
    signInAs(FI)
    list()
    let sent = 0
    server.use(http.post(`${BASE}/${RES1}/approve`, () => { sent++; return stepUpRequiredResponse() }))
    renderWithProviders(<AppRoutes />, { route: '/force-resolutions' })
    const user = userEvent.setup()
    const d = await openDetail(user)
    await user.click(within(d).getByRole('button', { name: 'Approve' }))
    const dlg = await fillDecision(user)
    await user.click(within(dlg).getByRole('button', { name: 'Approve' }))
    const su = await screen.findByRole('dialog', { name: 'Confirm it is you' })
    await user.click(within(su).getByRole('button', { name: 'Cancel' }))
    expect(await within(dlg).findByRole('alert')).toHaveTextContent('Verification cancelled. The action was not performed.')
    expect(sent).toBe(1)
  })
})

describe('cancel (S8)', () => {
  it('cancels the caller\'s own pending request through the cancel route', async () => {
    signInAs(FI)
    list([resolution({ requested_by: STAFF_A })])
    let called = 0
    server.use(http.post(`${BASE}/${RES1}/cancel`, ({ request }) => { called++; expect(request.headers.get('Idempotency-Key')).toBeTruthy(); return HttpResponse.json(resolution({ state: 'cancelled' })) }))
    renderWithProviders(<AppRoutes />, { route: '/force-resolutions' })
    const user = userEvent.setup()
    const d = await openDetail(user)
    await user.click(within(d).getByRole('button', { name: 'Cancel request' }))
    const dlg = await screen.findByRole('dialog', { name: 'Cancel force-resolution request' })
    await user.click(within(dlg).getByRole('button', { name: 'Cancel the request' }))
    await waitFor(() => expect(called).toBe(1))
    expect(await screen.findByRole('status')).toHaveTextContent('Force-resolution request cancelled.')
  })

  it('a refused cancel (409, not the requester) is shown, not hidden', async () => {
    signInAs(FI)
    list([resolution({ requested_by: STAFF_A })])
    server.use(http.post(`${BASE}/${RES1}/cancel`, () => refusal(409, 'conflict', 'force_resolve_conflict')))
    renderWithProviders(<AppRoutes />, { route: '/force-resolutions' })
    const user = userEvent.setup()
    const d = await openDetail(user)
    await user.click(within(d).getByRole('button', { name: 'Cancel request' }))
    const dlg = await screen.findByRole('dialog', { name: 'Cancel force-resolution request' })
    await user.click(within(dlg).getByRole('button', { name: 'Cancel the request' }))
    expect(await within(dlg).findByRole('alert')).toHaveTextContent(/not pending any more/)
  })
})

describe('propose (S8)', () => {
  it('pure form rules: M1 sends finding only, M2 sends basis + hash (+ optional context), never a tenant or proof, and validation mirrors the spec', () => {
    const m2 = { ...EMPTY_FORM, attemptId: ATT1, kind: 'm2_declare_paid' as const, basisCode: 'reconciliation_exhausted', evidenceHash: HASH2, reason: 'r', contextCode: 'provider_unqueryable' }
    expect(buildForceRequest(m2)).toEqual({ attempt_id: ATT1, kind: 'm2_declare_paid', basis_code: 'reconciliation_exhausted', evidence_ref_hash: HASH2, context_code: 'provider_unqueryable', reason_code: 'r' })
    const m1 = { ...EMPTY_FORM, attemptId: ATT1, kind: 'm1_deposit_evidence' as const, findingCode: 'awaiting_psp_refund', reason: 'r', basisCode: 'reconciliation_exhausted', evidenceHash: HASH2 }
    expect(buildForceRequest(m1)).toEqual({ attempt_id: ATT1, kind: 'm1_deposit_evidence', finding_code: 'awaiting_psp_refund', reason_code: 'r' })
    expect(validateForceRequest(m2)).toEqual([])
    expect(validateForceRequest({ ...m2, evidenceHash: 'A'.repeat(64) })).toHaveLength(1)
    expect(validateForceRequest({ ...m2, evidenceHash: 'a'.repeat(63) })).toHaveLength(1)
    expect(validateForceRequest({ ...m2, reason: ' ' })).toHaveLength(1)
    expect(validateForceRequest({ ...m2, reason: 'x'.repeat(65) })).toHaveLength(1)
    expect(validateForceRequest({ ...m2, attemptId: 'nope' })).toHaveLength(1)
    expect(validateForceRequest({ ...m1, findingCode: '' })).toHaveLength(1)
    expect(Object.keys(buildForceRequest({ ...m2, note: '  ' }))).not.toContain('note')
  })

  it('files an M2 request from an attempt id in the URL: previews the attempt, needs ack + typed id, posts the exact body once', async () => {
    signInAs(FI)
    list([])
    server.use(http.get(`/v1/admin/tenants/${TENANT}/payment-attempts/${ATT1}`, () => HttpResponse.json(attempt())))
    const posts: Array<{ body: Record<string, unknown>; key: string | null }> = []
    server.use(
      http.post(BASE, async ({ request }) => {
        posts.push({ body: (await request.json()) as Record<string, unknown>, key: request.headers.get('Idempotency-Key') })
        return HttpResponse.json(resolution({ id: RES1, requested_by: STAFF_A }), { status: 201 })
      }),
    )
    renderWithProviders(<AppRoutes />, { route: `/force-resolutions?attempt=${ATT1}` })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Propose a force-resolution' }))
    const dlg = await screen.findByRole('dialog', { name: 'Propose a force-resolution' })
    expect(await within(dlg).findByText(/123\.45 EUR \(12345 minor units\)/)).toBeInTheDocument()
    await user.selectOptions(within(dlg).getByLabelText('Kind'), 'm2_declare_paid')
    await user.selectOptions(within(dlg).getByLabelText('Basis'), 'reconciliation_exhausted')
    await user.click(within(dlg).getByLabelText('Evidence reference hash'))
    await user.paste(HASH2)
    await user.type(within(dlg).getByLabelText('Reason code (required)'), 'provider_unqueryable_case')
    const go = within(dlg).getByRole('button', { name: 'File proposal' })
    expect(go).toBeDisabled()
    await user.click(within(dlg).getByRole('checkbox'))
    await user.type(within(dlg).getByLabelText(/Type the LAST 6 characters of the attempt id/), ATT1.slice(-6))
    expect(go).toBeEnabled()
    await user.dblClick(go)
    expect(await screen.findByRole('status')).toHaveTextContent('Nothing has executed')
    expect(posts).toHaveLength(1)
    expect(posts[0].body).toEqual({ attempt_id: ATT1, kind: 'm2_declare_paid', basis_code: 'reconciliation_exhausted', evidence_ref_hash: HASH2, reason_code: 'provider_unqueryable_case' })
    expect(posts[0].key).toBeTruthy()
  })

  it('warns when the server hint does not admit the kind, and maps a 409 precondition refusal', async () => {
    signInAs(FI)
    list([])
    server.use(
      http.get(`/v1/admin/tenants/${TENANT}/payment-attempts/${ATT1}`, () => HttpResponse.json(attempt({ admissible_kinds: [] }))),
      http.post(BASE, () => refusal(409, 'conflict', 'force_resolve_precondition_failed')),
    )
    renderWithProviders(<AppRoutes />, { route: `/force-resolutions?attempt=${ATT1}` })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Propose a force-resolution' }))
    const dlg = await screen.findByRole('dialog', { name: 'Propose a force-resolution' })
    await user.selectOptions(within(dlg).getByLabelText('Kind'), 'm2_declare_not_paid')
    expect(await within(dlg).findByText(/does not list/)).toBeInTheDocument()
    await user.selectOptions(within(dlg).getByLabelText('Basis'), 'provider_confirmed_out_of_band')
    await user.click(within(dlg).getByLabelText('Evidence reference hash'))
    await user.paste(HASH2)
    await user.type(within(dlg).getByLabelText('Reason code (required)'), 'why')
    await user.click(within(dlg).getByRole('checkbox'))
    await user.type(within(dlg).getByLabelText(/Type the LAST 6 characters of the attempt id/), ATT1.slice(-6))
    await user.click(within(dlg).getByRole('button', { name: 'File proposal' }))
    expect(await within(dlg).findByRole('alert')).toHaveTextContent(/precondition no longer holds/)
  })

  it('platform admin picks a tenant first and the dialog never contains a tenant field', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    server.use(http.get('/v1/admin/tenants', () => HttpResponse.json({ items: [{ id: TENANT, name: 'Acme', slug: 'acme', status: 'active' }], limit: 50, offset: 0, total: 1 })))
    list([])
    renderWithProviders(<AppRoutes />, { route: `/force-resolutions?tenant=${TENANT}` })
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Propose a force-resolution' }))
    const dlg = await screen.findByRole('dialog', { name: 'Propose a force-resolution' })
    expect(within(dlg).queryByLabelText(/^tenant( id)?$/i)).toBeNull()
  })
})
