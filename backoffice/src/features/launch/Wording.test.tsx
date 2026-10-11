import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { useState } from 'react'
import { Route, Routes, useLocation } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import { RequireNavPermission } from '../../auth/RequireNavPermission'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { PLATFORM_ADMIN_CLAIMS, signInAs } from '../../test/session'
import { BrandDetailPage } from '../tenants/BrandDetailPage'
import { ApproveDialog } from './DecisionDialogs'
import { LaunchQueuePage } from './LaunchQueuePage'
import { FUNDS_STRANDED_NOTICE, LAUNCH_DISCLAIMER } from './labels'
import { API_DISCLAIMER, BRAND_ID, TENANT_ID, launchReadHandlers, launchRequest, readiness, transition } from './launchFixtures'

const FORBIDDEN_WORDS = /\blicensed\b|\bauthorised\b|compliant|approved for operation|\bscore\b|traffic/i

function renderBrand() {
  return renderWithProviders(
    <Routes>
      <Route path="/tenants/:tenantId/brands/:brandId" element={<BrandDetailPage />} />
    </Routes>,
    { route: `/tenants/${TENANT_ID}/brands/${BRAND_ID}` },
  )
}

function Where() {
  const l = useLocation()
  return <p data-testid="where">{l.pathname + l.search}</p>
}

describe('wording and no-autopilot rules', () => {
  it('request form: visible disclaimer, "recorded basis" labels, note labelled record-only, nothing pre-ticked or pre-selected, no verdict words', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    launchReadHandlers({ brandStatus: 'pending_launch', history: [transition()] })
    server.use(http.get('/v1/admin/tenants/:tid/launch-readiness', () => HttpResponse.json({ disclaimer: API_DISCLAIMER, readiness: readiness() })))
    renderBrand()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Request activate' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByRole('note', { name: 'Disclaimer' })).toHaveTextContent(LAUNCH_DISCLAIMER)
    expect(within(dialog).getByLabelText('Licensing model (recorded basis)')).toHaveValue('')
    expect(within(dialog).getByLabelText('Licensing status (recorded basis)')).toHaveValue('')
    expect(within(dialog).getByLabelText('Reason code')).toHaveValue('')
    expect(within(dialog).getByText(/Conditions \/ notes \(optional\) - record only, not enforced/)).toBeInTheDocument()
    // the history view carries the disclaimer itself (the page behind the modal is aria-hidden by role query, so use the DOM)
    const history = document.querySelector('section[aria-label="Status history"]') as HTMLElement
    expect(within(history).getByRole('note', { name: 'Disclaimer' })).toHaveTextContent(LAUNCH_DISCLAIMER)

    await user.selectOptions(within(dialog).getByLabelText('Licensing model (recorded basis)'), 'other_manually_approved')
    await user.selectOptions(within(dialog).getByLabelText('Licensing status (recorded basis)'), 'conditional')
    await user.type(within(dialog).getByLabelText('Responsible operator (legal entity)'), 'Op')
    await user.click(await within(dialog).findByRole('checkbox', { name: /AJ - Anjouan/ }))
    await within(dialog).findByLabelText('Readiness checklist')
    for (const name of ['R-PROVIDER-TIER', 'R-ALERTS', 'R-PLAYER-FUNDS']) {
      expect(within(dialog).getByRole('checkbox', { name: `Acknowledge ${name}` })).not.toBeChecked()
    }
    expect(within(dialog).getByRole('button', { name: 'File activate request' })).toBeDisabled()
    expect(within(dialog).getByText('Info: not evaluable')).toBeInTheDocument()
    expect(dialog.textContent).not.toMatch(FORBIDDEN_WORDS)
  })

  it('close with balances: the acknowledgement itself carries the fixed funds-stranded notice, unchecked by default', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    launchReadHandlers({ brandStatus: 'active' })
    server.use(
      http.get('/v1/admin/tenants/:tid/launch-readiness', () =>
        HttpResponse.json({
          disclaimer: API_DISCLAIMER,
          readiness: readiness({ checks: [{ code: 'R-PLAYER-FUNDS', result: 'fail', detail_code: 'x' }], non_passing_check_codes: ['R-PLAYER-FUNDS'], balances: { EUR: '5' } }),
        }),
      ),
    )
    renderBrand()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Request closure' }))
    const dialog = await screen.findByRole('dialog')
    const box = await within(dialog).findByRole('checkbox', { name: 'Acknowledge R-PLAYER-FUNDS' })
    expect(box).not.toBeChecked()
    expect(box.closest('label')).toHaveTextContent(FUNDS_STRANDED_NOTICE)
  })

  it('approval dialog: disclaimer visible, reason not pre-selected, and the readiness is frozen at open', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    let body: unknown
    server.use(
      http.post('/v1/admin/tenants/:tid/launch-authorisations/:rid/approve', async ({ request }) => {
        body = await request.json()
        return HttpResponse.json({ code: 'LAUNCH_ALREADY_DECIDED', message: 'x' }, { status: 409 })
      }),
    )
    function Harness() {
      const [r, setR] = useState(readiness({ readiness_hash: '1'.repeat(64) }))
      return (
        <>
          <button onClick={() => setR(readiness({ readiness_hash: '2'.repeat(64), checks: [] }))}>refetch</button>
          <ApproveDialog tenantId={TENANT_ID} request={launchRequest()} readiness={r} onClose={() => {}} onDone={() => {}} onStale={() => {}} />
        </>
      )
    }
    renderWithProviders(<Harness />)
    const user = userEvent.setup()
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByRole('note', { name: 'Disclaimer' })).toHaveTextContent(LAUNCH_DISCLAIMER)
    expect(within(dialog).getByLabelText('Reason code')).toHaveValue('')
    expect(within(dialog).getByRole('button', { name: 'Approve request' })).toBeDisabled()
    await user.click(screen.getByRole('button', { name: 'refetch' }))
    expect(within(dialog).getByText('R-PROVIDER-TIER')).toBeInTheDocument()
    await user.selectOptions(within(dialog).getByLabelText('Reason code'), 'reviewed_ok')
    await user.click(within(dialog).getByRole('button', { name: 'Approve request' }))
    await waitFor(() => expect(body).toEqual({ reason_code: 'reviewed_ok', seen_readiness_hash: '1'.repeat(64) }))
  })

  it('queue: ids are URI-encoded when building the destination', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    const r = launchRequest({ tenant_id: 'a/b?c', brand_id: 'd#e', id: 'f&g=h' })
    server.use(http.get('/v1/admin/launch-authorisations', () => HttpResponse.json({ disclaimer: API_DISCLAIMER, items: [r] })))
    renderWithProviders(
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
    const user = userEvent.setup()
    await user.click(await screen.findByText('Activate'))
    expect((await screen.findByTestId('where')).textContent).toBe('/tenants/a%2Fb%3Fc/brands/d%23e?request=f%26g%3Dh')
  })
})
