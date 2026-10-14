import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { server } from '../test/mswServer'
import { PLATFORM_ADMIN_CLAIMS, signInAs } from '../test/session'
import { getAlert, listAlerts } from './alerts'

// ADR 0114 S1b contract (types only; the console UI does not consume the cursor yet).
describe('alerts api: S1b contract', () => {
  it('sends kind and cursor, and reads next_cursor and the acted-by fields', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    const seen: URL[] = []
    server.use(
      http.get('/v1/admin/alerts', ({ request }) => {
        seen.push(new URL(request.url))
        return HttpResponse.json({
          alerts: [
            {
              id: '10000000-0000-4000-8000-000000000001',
              kind: 'payment.kill_switch_engaged',
              severity: 'p2',
              state: 'resolved',
              first_seen_at: '2026-10-10T10:00:00Z',
              acked_at: '2026-10-10T10:05:00Z',
              resolved_at: '2026-10-10T10:06:00Z',
              acked_by: '20000000-0000-4000-8000-000000000001',
              resolved_by: '20000000-0000-4000-8000-000000000002',
              delivery_state: 'recorded_non_human',
              notified_a_person: false,
              escalation_step: 0,
              delivery_channel_kind: 'log',
              attempt_no: 1,
            },
          ],
          next_cursor: 'opaque',
        })
      }),
    )
    const page = await listAlerts({ state: 'resolved', kind: 'payment.kill_switch_engaged', limit: 1, cursor: 'abc' })
    expect(seen[0].searchParams.get('kind')).toBe('payment.kill_switch_engaged')
    expect(seen[0].searchParams.get('cursor')).toBe('abc')
    expect(seen[0].searchParams.get('limit')).toBe('1')
    expect(page.next_cursor).toBe('opaque')
    expect(page.alerts[0].acked_by).toBe('20000000-0000-4000-8000-000000000001')
    expect(page.alerts[0].resolved_at).toBe('2026-10-10T10:06:00Z')
  })

  it('omits an unset cursor and reads one alert from the single route', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    const seen: URL[] = []
    server.use(
      http.get('/v1/admin/alerts', ({ request }) => {
        seen.push(new URL(request.url))
        return HttpResponse.json({ alerts: [], next_cursor: null })
      }),
      http.get('/v1/admin/alerts/:id', ({ params }) =>
        HttpResponse.json({
          alert: { id: params.id, kind: 'k', severity: 'p1', state: 'open', first_seen_at: '2026-10-10T10:00:00Z', delivery_state: 'pending', notified_a_person: false },
        }),
      ),
    )
    const page = await listAlerts({ cursor: undefined, limit: 50 })
    expect(seen[0].searchParams.has('cursor')).toBe(false)
    expect(page.next_cursor).toBeNull()
    const one = await getAlert('10000000-0000-4000-8000-0000000000aa')
    expect(one.alert.id).toBe('10000000-0000-4000-8000-0000000000aa')
  })
})
