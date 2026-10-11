// Test fixtures shaped exactly like docs/api/openapi/platform-api.yaml's Launch* schemas.
import { http, HttpResponse } from 'msw'
import type { LaunchReadiness, LaunchRequest, LaunchTransition } from '../../api/launch'
import { server } from '../../test/mswServer'

export const TENANT_ID = '11111111-1111-4111-8111-111111111111'
export const BRAND_ID = '22222222-2222-4222-8222-222222222222'
export const JURIS_ID = '33333333-3333-4333-8333-333333333333'
export const LICENCE_ID = '44444444-4444-4444-8444-444444444444'
export const API_DISCLAIMER =
  'A recorded launch decision is evidence that named people took a decision on stated inputs. It is not a legal or regulatory authorisation, and it does not determine whether a licence is required or whether any jurisdiction permits operation.'

export function readiness(over: Partial<LaunchReadiness> = {}): LaunchReadiness {
  return {
    version: 1,
    computed_at: '2026-10-11T10:00:00Z',
    environment: 'staging',
    checks: [
      { code: 'R-JUR-CONFIG', result: 'pass', detail_code: 'ok' },
      { code: 'R-LIC-VALID', result: 'pass', detail_code: 'ok' },
      { code: 'R-PROVIDER-TIER', result: 'fail', detail_code: 'synthetic_environment' },
      { code: 'R-ALERTS', result: 'fail', detail_code: 'no_recipient' },
      { code: 'R-PLAYER-FUNDS', result: 'not_evaluable', detail_code: 'not_evaluated' },
    ],
    readiness_hash: 'a'.repeat(64),
    non_passing_check_codes: ['R-PROVIDER-TIER', 'R-ALERTS', 'R-PLAYER-FUNDS'],
    ...over,
  }
}

export function launchRequest(over: Partial<LaunchRequest> = {}): LaunchRequest {
  return {
    id: '55555555-5555-4555-8555-555555555555',
    tenant_id: TENANT_ID,
    subject_kind: 'brand',
    brand_id: BRAND_ID,
    action: 'activate',
    from_status: 'pending_launch',
    to_status: 'active',
    status: 'pending',
    licensing_model: 'other_manually_approved',
    licensing_status: 'conditional',
    determination_reference: 'DOC-77',
    determination_by_role: 'external counsel',
    responsible_operator_name: 'Operator Ltd',
    responsible_operator_registration: 'REG 123',
    jurisdiction_ids: [JURIS_ID],
    conditions_note: 'Launch only after the soft-launch review.',
    readiness_snapshot_hash: 'a'.repeat(64),
    acknowledged_check_codes: ['R-PROVIDER-TIER'],
    reason_code: 'initial_launch',
    requested_by: 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb',
    requested_by_scope: 'platform',
    required_approvals: 1,
    payload_hash: 'c'.repeat(64),
    created_at: '2026-10-11T09:00:00Z',
    expires_at: '2026-10-14T09:00:00Z',
    ...over,
  }
}

export function transition(over: Partial<LaunchTransition> = {}): LaunchTransition {
  return {
    id: 't-1',
    subject_kind: 'brand',
    brand_id: BRAND_ID,
    kind: 'governed',
    from_status: 'pending_launch',
    to_status: 'active',
    approver_ids: [],
    actor_type: 'platform',
    created_at: '2026-10-11T09:30:00Z',
    ...over,
  }
}

/** Default read handlers for a brand/tenant detail page. */
export function launchReadHandlers(opts: {
  tenantStatus?: string
  brandStatus?: string
  requests?: LaunchRequest[]
  history?: LaunchTransition[]
} = {}) {
  server.use(
    http.get('/v1/admin/tenants/:tid', ({ params }) =>
      HttpResponse.json({ id: params.tid, name: 'Acme', slug: 'acme', licensing_model: 'own_licence', status: opts.tenantStatus ?? 'active' }),
    ),
    http.get('/v1/admin/tenants/:tid/brands', () =>
      HttpResponse.json({ items: [{ id: BRAND_ID, tenant_id: TENANT_ID, name: 'Brand One', slug: 'b1', status: opts.brandStatus ?? 'pending_launch' }], limit: 20, offset: 0, total: 1 }),
    ),
    http.get('/v1/admin/tenants/:tid/brands/:bid', () =>
      HttpResponse.json({ id: BRAND_ID, tenant_id: TENANT_ID, name: 'Brand One', slug: 'b1', status: opts.brandStatus ?? 'pending_launch' }),
    ),
    http.get('/v1/admin/tenants/:tid/launch-authorisations', () => HttpResponse.json({ disclaimer: API_DISCLAIMER, items: opts.requests ?? [] })),
    http.get('/v1/admin/tenants/:tid/launch-authorisations/:rid', ({ params }) => {
      const r = (opts.requests ?? []).find((x) => x.id === params.rid) ?? launchRequest({ id: String(params.rid) })
      return HttpResponse.json({ disclaimer: API_DISCLAIMER, request: r })
    }),
    http.get('/v1/admin/tenants/:tid/launch-status-history', () => HttpResponse.json({ disclaimer: API_DISCLAIMER, items: opts.history ?? [] })),
    http.get('/v1/admin/jurisdictions', () => HttpResponse.json([{ id: JURIS_ID, code: 'AJ', name: 'Anjouan' }])),
    http.get('/v1/admin/licences', () => HttpResponse.json([{ id: LICENCE_ID, jurisdiction_id: JURIS_ID, licensee: 'platform', licence_number: 'LIC-1', status: 'active' }])),
  )
}
