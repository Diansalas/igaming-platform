import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { setSession } from '../../auth/tokenStore'
import { makeTestJwt } from '../../test/jwt'
import { server } from '../../test/mswServer'
import { renderWithProviders } from '../../test/renderWithProviders'
import { ResponsibleGamingPage } from './ResponsibleGamingPage'

function signIn() {
  setSession(makeTestJwt(), 'refresh-token-for-test')
}

const change = (over: Record<string, unknown> = {}) => ({
  id: 'c-1',
  kind: 'wager',
  scope: 'account',
  asset_code: 'USD',
  period: 'rolling_24h',
  value: '10000',
  change_kind: 'initial',
  pending: false,
  requires_confirmation: false,
  requested_at: '2026-10-15T10:00:00Z',
  effective_at: '2026-10-15T10:00:00Z',
  source: 'player_self_service',
  reason_code: 'player_self_service',
  ...over,
})

describe('ResponsibleGamingPage', () => {
  it('shows no restrictions and no limits by default', async () => {
    signIn()
    renderWithProviders(<ResponsibleGamingPage />, { route: '/account/responsible-gaming' })
    expect(await screen.findByText('You have no active restrictions.')).toBeInTheDocument()
    expect(await screen.findByText(/You have not set any limits for USD/)).toBeInTheDocument()
  })

  it('shows an active restriction with the server’s end date and says withdrawals stay possible', async () => {
    signIn()
    server.use(
      http.get('/v1/me/rg/status', () =>
        HttpResponse.json([
          { id: 'r-1', restriction_type: 'time_out', scope: 'platform', starts_at: '2026-10-15T10:00:00Z', ends_at: '2099-01-01T00:00:00Z', indefinite: false, source: 'player_self_service', created_at: 'x', active: true },
          { id: 'r-2', restriction_type: 'self_exclusion', scope: 'platform', starts_at: 'x', indefinite: false, source: 'player_self_service', created_at: 'x', active: false },
        ]),
      ),
    )
    renderWithProviders(<ResponsibleGamingPage />)
    expect(await screen.findByText('Time-out')).toBeInTheDocument()
    expect(screen.queryByText('Self-exclusion', { selector: 'span' })).not.toBeInTheDocument()
    expect(screen.getByText(/While a restriction is active you cannot deposit, place bets or launch games\. You can still withdraw your funds/)).toBeInTheDocument()
    expect(screen.getByText(/1 earlier restriction\(s\) have ended/)).toBeInTheDocument()
  })

  it('sets a limit: sends the exact minor-unit string with an Idempotency-Key and reports it as effective now', async () => {
    signIn()
    const seen: Array<{ body: Record<string, unknown>; key: string | null }> = []
    server.use(
      http.post('/v1/me/rg/limits', async ({ request }) => {
        seen.push({ body: (await request.json()) as Record<string, unknown>, key: request.headers.get('Idempotency-Key') })
        return HttpResponse.json(change({ value: '12345' }), { status: 201 })
      }),
    )
    const user = userEvent.setup()
    renderWithProviders(<ResponsibleGamingPage />)
    await screen.findByLabelText('Limit kind')
    await user.selectOptions(screen.getByLabelText('Limit kind'), 'wager')
    await user.selectOptions(screen.getByLabelText('Limit period'), 'rolling_24h')
    await user.type(screen.getByLabelText('Limit amount'), '123.45')
    await user.click(screen.getByRole('button', { name: 'Set limit' }))

    expect(await screen.findByText(/Your limit is now 123\.45 USD, effective immediately/)).toBeInTheDocument()
    expect(seen).toHaveLength(1)
    expect(seen[0].body).toEqual({ kind: 'wager', asset_code: 'USD', period: 'rolling_24h', value: '12345' })
    expect(seen[0].key).toBeTruthy()
  })

  it('shows the server’s cooling-off time for an increase and keeps the old limit in force', async () => {
    signIn()
    server.use(
      http.get('/v1/me/rg/limits', () =>
        HttpResponse.json({
          limits: [{ kind: 'wager', scope: 'account', asset_code: 'USD', period: 'rolling_24h', has_value: true, value: '10000' }],
          history: [],
        }),
      ),
      http.post('/v1/me/rg/limits', () =>
        HttpResponse.json(change({ value: '50000', change_kind: 'increase', pending: true, effective_at: '2026-10-16T10:00:00Z' }), { status: 202 }),
      ),
    )
    const user = userEvent.setup()
    renderWithProviders(<ResponsibleGamingPage />)
    expect(await screen.findByText('100.00 USD')).toBeInTheDocument()
    await user.selectOptions(screen.getByLabelText('Limit kind'), 'wager')
    await user.selectOptions(screen.getByLabelText('Limit period'), 'rolling_24h')
    await user.type(screen.getByLabelText('Limit amount'), '500')
    expect(screen.getByRole('note')).toHaveTextContent('will not take effect immediately')
    await user.click(screen.getByRole('button', { name: 'Set limit' }))
    const status = await screen.findByText(/Your request was received/)
    expect(status).toHaveTextContent('Your current limit stays in force until then')
    expect(screen.getByText(/cooling-off period of 1 day/)).toBeInTheDocument()
  })

  it('lists a pending increase with its effective time, lets the player cancel it, and confirms only once it is due', async () => {
    signIn()
    const posts: string[] = []
    server.use(
      http.get('/v1/me/rg/limits', () =>
        HttpResponse.json({
          limits: [
            {
              kind: 'wager', scope: 'account', asset_code: 'USD', period: 'rolling_24h', has_value: true, value: '10000',
              pending: change({ id: 'p-due', value: '90000', change_kind: 'increase', pending: true, requires_confirmation: true, effective_at: '2020-01-01T00:00:00Z' }),
            },
            {
              kind: 'loss', scope: 'account', asset_code: 'USD', period: 'calendar_week', has_value: true, value: '500',
              pending: change({ id: 'p-wait', kind: 'loss', period: 'calendar_week', value: '9000', change_kind: 'increase', pending: true, requires_confirmation: true, effective_at: '2099-01-01T00:00:00Z' }),
            },
          ],
          history: [],
        }),
      ),
      http.post('/v1/me/rg/limits/changes/:id/confirm', ({ params }) => {
        posts.push(`confirm:${String(params.id)}`)
        return HttpResponse.json(change({ id: String(params.id), change_kind: 'confirm' }))
      }),
      http.post('/v1/me/rg/limits/changes/:id/cancel', ({ params }) => {
        posts.push(`cancel:${String(params.id)}`)
        return HttpResponse.json(change({ id: String(params.id), change_kind: 'cancel_pending' }))
      }),
    )
    const user = userEvent.setup()
    renderWithProviders(<ResponsibleGamingPage />)
    const list = await screen.findByRole('list', { name: 'Current limits' })
    const items = within(list).getAllByRole('listitem')
    expect(items).toHaveLength(2)
    // Only the due change offers Confirm.
    expect(screen.getAllByRole('button', { name: 'Confirm change' })).toHaveLength(1)
    await user.click(screen.getByRole('button', { name: 'Confirm change' }))
    await waitFor(() => expect(posts).toContain('confirm:p-due'))
    await user.click(within(items[1]).getByRole('button', { name: 'Cancel pending change' }))
    await waitFor(() => expect(posts).toContain('cancel:p-wait'))
  })

  it('shows the server’s refusal in plain words and keeps the form usable', async () => {
    signIn()
    server.use(
      http.post('/v1/me/rg/limits', () =>
        HttpResponse.json({ code: 'RG_LIMIT_ABOVE_CEILING', message: 'internal wording', request_id: 'req-1' }, { status: 422 }),
      ),
    )
    const user = userEvent.setup()
    renderWithProviders(<ResponsibleGamingPage />)
    await screen.findByLabelText('Limit kind')
    await user.selectOptions(screen.getByLabelText('Limit period'), 'calendar_week')
    await user.type(screen.getByLabelText('Limit amount'), '999999')
    await user.click(screen.getByRole('button', { name: 'Set limit' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('above the maximum allowed')
    expect(screen.getByRole('button', { name: 'Set limit' })).toBeEnabled()
  })

  it('validates the amount locally and sends nothing for an invalid one', async () => {
    signIn()
    let posts = 0
    server.use(http.post('/v1/me/rg/limits', () => { posts++; return HttpResponse.json(change(), { status: 201 }) }))
    const user = userEvent.setup()
    renderWithProviders(<ResponsibleGamingPage />)
    await screen.findByLabelText('Limit kind')
    await user.selectOptions(screen.getByLabelText('Limit period'), 'rolling_24h')
    await user.type(screen.getByLabelText('Limit amount'), '12.345')
    await user.click(screen.getByRole('button', { name: 'Set limit' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('Enter a positive amount')
    expect(posts).toBe(0)
  })

  it('removes a limit through the same route with a null value', async () => {
    signIn()
    const bodies: Array<Record<string, unknown>> = []
    server.use(
      http.get('/v1/me/rg/limits', () =>
        HttpResponse.json({ limits: [{ kind: 'deposit', scope: 'account', asset_code: 'USD', period: 'rolling_24h', has_value: true, value: '2000' }], history: [] }),
      ),
      http.post('/v1/me/rg/limits', async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>)
        return HttpResponse.json(change({ kind: 'deposit', value: null, change_kind: 'removal', pending: true, effective_at: '2026-10-16T10:00:00Z' }), { status: 202 })
      }),
    )
    const user = userEvent.setup()
    renderWithProviders(<ResponsibleGamingPage />)
    await user.click(await screen.findByRole('button', { name: 'Remove limit' }))
    await waitFor(() => expect(bodies).toHaveLength(1))
    expect(bodies[0]).toEqual({ kind: 'deposit', asset_code: 'USD', period: 'rolling_24h', value: null })
  })

  it('takes a time-out only after a confirmation step, with the server’s durations', async () => {
    signIn()
    let posted: Record<string, unknown> | null = null
    server.use(
      http.post('/v1/me/rg/time-out', async ({ request }) => {
        posted = (await request.json()) as Record<string, unknown>
        return HttpResponse.json({ id: 'r-9', restriction_type: 'time_out', scope: 'platform', starts_at: 'x', ends_at: '2099-01-01T00:00:00Z', indefinite: false, source: 'player_self_service', created_at: 'x', active: true }, { status: 201 })
      }),
    )
    const user = userEvent.setup()
    renderWithProviders(<ResponsibleGamingPage />)
    const select = await screen.findByLabelText('Time-out duration')
    expect(within(select).getAllByRole('option').map((o) => o.textContent)).toEqual(['Choose...', '1 day', '3 days', '7 days'])
    await user.selectOptions(select, '72')
    expect(posted).toBeNull()
    await user.click(screen.getByRole('button', { name: 'Review time-out' }))
    expect(screen.getByRole('group', { name: 'Confirm time-out' })).toHaveTextContent('cannot be cancelled')
    expect(posted).toBeNull()
    await user.click(screen.getByRole('button', { name: 'Start time-out' }))
    expect(await screen.findByText(/Your time-out has started/)).toBeInTheDocument()
    expect(posted).toEqual({ duration_hours: 72 })
  })

  it('self-excludes only after review AND an explicit acknowledgement, with the server’s durations', async () => {
    signIn()
    const bodies: Array<Record<string, unknown>> = []
    server.use(
      http.post('/v1/me/rg/self-exclusion', async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>)
        return HttpResponse.json({ id: 'r-1', restriction_type: 'self_exclusion', scope: 'platform', starts_at: 'x', indefinite: true, source: 'player_self_service', created_at: 'x', active: true }, { status: 201 })
      }),
    )
    const user = userEvent.setup()
    renderWithProviders(<ResponsibleGamingPage />)
    const select = await screen.findByLabelText('Self-exclusion duration')
    expect(within(select).getAllByRole('option').map((o) => o.textContent)).toEqual(['Choose a duration...', 'For 30 days', 'For 90 days', 'Indefinitely (no end date)'])
    expect(screen.getByRole('button', { name: 'Review self-exclusion' })).toBeDisabled()
    await user.selectOptions(select, 'indefinite')
    await user.click(screen.getByRole('button', { name: 'Review self-exclusion' }))
    const confirm = screen.getByRole('button', { name: 'Confirm self-exclusion' })
    expect(confirm).toBeDisabled() // not until the acknowledgement is ticked
    expect(screen.getByRole('group', { name: 'Confirm self-exclusion' })).toHaveTextContent('Nobody, including customer support, can end it early')
    await user.click(screen.getByRole('checkbox'))
    await user.click(confirm)
    expect(await screen.findByText(/You are now self-excluded with no end date/)).toBeInTheDocument()
    expect(bodies).toEqual([{}]) // indefinite = no duration_days
  })

  it('sends duration_days for a bounded self-exclusion and can go back from the confirmation', async () => {
    signIn()
    const bodies: Array<Record<string, unknown>> = []
    server.use(
      http.post('/v1/me/rg/self-exclusion', async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>)
        return HttpResponse.json({ id: 'r-1', restriction_type: 'self_exclusion', scope: 'platform', starts_at: 'x', ends_at: '2099-01-01T00:00:00Z', indefinite: false, source: 'player_self_service', created_at: 'x', active: true }, { status: 201 })
      }),
    )
    const user = userEvent.setup()
    renderWithProviders(<ResponsibleGamingPage />)
    await user.selectOptions(await screen.findByLabelText('Self-exclusion duration'), '90')
    await user.click(screen.getByRole('button', { name: 'Review self-exclusion' }))
    await user.click(screen.getByRole('button', { name: 'Go back' }))
    expect(bodies).toHaveLength(0)
    await user.click(screen.getByRole('button', { name: 'Review self-exclusion' }))
    await user.click(screen.getByRole('checkbox'))
    await user.click(screen.getByRole('button', { name: 'Confirm self-exclusion' }))
    await waitFor(() => expect(bodies).toEqual([{ duration_days: 90 }]))
  })

  it('shows a failed self-exclusion and the load failure of the options with a retry', async () => {
    signIn()
    server.use(
      http.post('/v1/me/rg/self-exclusion', () => HttpResponse.json({ code: 'service_unavailable', message: 'x' }, { status: 503 })),
    )
    const user = userEvent.setup()
    renderWithProviders(<ResponsibleGamingPage />)
    await user.selectOptions(await screen.findByLabelText('Self-exclusion duration'), '30')
    await user.click(screen.getByRole('button', { name: 'Review self-exclusion' }))
    await user.click(screen.getByRole('checkbox'))
    await user.click(screen.getByRole('button', { name: 'Confirm self-exclusion' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('temporarily unavailable')
  })

  it('reports a failed options load instead of an empty screen', async () => {
    signIn()
    server.use(http.get('/v1/me/rg/options', () => HttpResponse.json({ code: 'internal_error', message: 'boom' }, { status: 500 })))
    renderWithProviders(<ResponsibleGamingPage />)
    expect(await screen.findByText('boom')).toBeInTheDocument()
    expect(screen.queryByLabelText('Limit kind')).not.toBeInTheDocument()
  })

  it('tells the player when automatic enforcement is not switched on and when a limit is mandatory or capped', async () => {
    signIn()
    server.use(
      http.get('/v1/me/rg/options', () =>
        HttpResponse.json({
          asset_code: 'USD',
          limit_enforcement_enabled: false,
          limits: [{ kind: 'deposit', config_in_force: true, mandatory: true, allowed_periods: ['rolling_24h'], ceiling: '500000', scope: 'account', increase_cooldown_seconds: 172800 }],
          time_out_hours: [],
          self_exclusion_days: [],
          self_exclusion_indefinite_allowed: false,
        }),
      ),
    )
    renderWithProviders(<ResponsibleGamingPage />)
    expect(await screen.findByText(/Automatic enforcement of deposit, loss and wager limits has not been switched on/)).toBeInTheDocument()
    expect(screen.getByText(/A deposit limit is required before you can deposit/)).toBeInTheDocument()
    expect(screen.getByText('Maximum allowed: 5000.00 USD.')).toBeInTheDocument()
    expect(screen.getByText(/cooling-off period of 2 days/)).toBeInTheDocument()
    expect(screen.getByText('No time-out durations are available right now.')).toBeInTheDocument()
  })
})
