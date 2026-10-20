// Fixtures for the lic16 policy screens. Shapes follow docs/api/openapi/platform-api.yaml; every value is synthetic.
import { http, HttpResponse } from 'msw'
import type { KycEnforcementPolicy } from '../../api/kycEnforcementPolicies'
import type { PendingResidenceApproval, ResidencePolicy, ResidencePolicyVersion } from '../../api/residenceApproval'
import type { WrpCause, WrpChange, WrpChangeResponse, WrpEffective, WrpVersion } from '../../api/withdrawalRestriction'
import { server } from '../../test/mswServer'

export const JUR1 = '10000000-0000-4000-8000-000000000001'
export const JUR2 = '10000000-0000-4000-8000-000000000002'
export const TEN1 = '30000000-0000-4000-8000-000000000001'
export const ME = 'f9000000-0000-4000-8000-000000000009'
export const OTHER = 'f8000000-0000-4000-8000-000000000008'
export const POL1 = '70000000-0000-4000-8000-000000000001'
export const POL2 = '70000000-0000-4000-8000-000000000002'
export const POL3 = '70000000-0000-4000-8000-000000000003'
export const VER1 = '71000000-0000-4000-8000-000000000001'
export const VF1 = '72000000-0000-4000-8000-000000000001'
export const WRC1 = '73000000-0000-4000-8000-000000000001'
export const SENTINEL = 'RAW SERVER MESSAGE <b>do-not-render</b>'
export const DET_AT = '2026-10-12T10:00:00.123456Z'
export const refusal = (status: number, code: string) => HttpResponse.json({ code, message: SENTINEL, request_id: 'req-9' }, { status })

export const kycPolicy = (o: Partial<KycEnforcementPolicy> = {}): KycEnforcementPolicy => ({
  id: POL1,
  licensing_jurisdiction_id: JUR1,
  trigger_type: 'cumulative_deposit',
  status: 'draft',
  threshold_minor_units: '1000000',
  asset_code: 'EUR',
  required_tier: null,
  play_operation: null,
  effective_from: '2026-10-01T10:00:00Z',
  legal_review_reference: 'LEGAL-REF-1',
  reason_code: 'compliance_decision',
  created_at: '2026-10-01T10:00:00Z',
  created_by: OTHER,
  successor_policy_id: null,
  ...o,
})

export function kycReads(items: KycEnforcementPolicy[] = [kycPolicy()]) {
  server.use(
    http.get('/v1/admin/jurisdictions', () =>
      HttpResponse.json([
        { id: JUR1, code: 'XX', name: 'Testland', created_at: '2026-01-01T00:00:00Z' },
        { id: JUR2, code: 'YY', name: 'Otherland', created_at: '2026-01-01T00:00:00Z' },
      ]),
    ),
    http.get('/v1/admin/kyc/enforcement-policies', ({ request }) => {
      const u = new URL(request.url)
      const st = u.searchParams.get('status')
      const j = u.searchParams.get('licensing_jurisdiction_id')
      return HttpResponse.json(items.filter((p) => (!st || p.status === st) && (!j || p.licensing_jurisdiction_id === j)))
    }),
  )
}

export const rVersion = (o: Partial<ResidencePolicyVersion> = {}): ResidencePolicyVersion => ({
  id: VER1,
  mode: 'none',
  previous_mode: 'all',
  status: 'pending',
  required_approvals: 2,
  approvals: 0,
  reason_code: 'relax_review',
  requested_by: OTHER,
  requested_at: '2026-10-12T09:00:00Z',
  activated_at: null,
  ...o,
})

export const pendingRow = (o: Partial<PendingResidenceApproval> = {}): PendingResidenceApproval => ({
  verification_id: '74000000-0000-4000-8000-000000000001',
  player_account_id: '75000000-0000-4000-8000-000000000001',
  brand_id: '76000000-0000-4000-8000-000000000001',
  determined_at: DET_AT,
  determined_by: OTHER,
  policy_mode: 'into_open_market',
  ...o,
})

export function residenceReads(policy: ResidencePolicy = { effective_mode: 'none', versions: [] }, pending: PendingResidenceApproval[] = []) {
  server.use(
    http.get(`/v1/admin/tenants/${TEN1}/residence-approval-policy`, () => HttpResponse.json(policy)),
    http.get('/v1/admin/kyc/residence-determinations/pending', () => HttpResponse.json(pending)),
  )
}

const stages = (action: 'allow' | 'hold_for_review' | 'block', source: 'configured' | 'default_unconfigured', v: number | null) =>
  (['request', 'dispatch', 'redispatch'] as const).map((stage) => ({ stage, action, source, policy_id: source === 'configured' ? POL1 : null, set_version: v }))

export const wrpCause = (cause: string, o: Partial<WrpCause> = {}): WrpCause => ({
  cause,
  class: 'licence',
  configurable: true,
  evaluated: true,
  governing_reference: 'REF-LICENCE',
  current_behaviour: 'synthetic behaviour text',
  stages: stages('allow', 'default_unconfigured', null),
  unconfigured: true,
  notice: 'SERVER UNCONFIGURED NOTICE',
  ...o,
})

export const wrpEffective = (o: Partial<WrpEffective> = {}): WrpEffective => ({
  tenant_id: TEN1,
  set_version: 0,
  configured: false,
  causes: [
    wrpCause('licence_suspended'),
    wrpCause('licence_expired'),
    wrpCause('licence_not_yet_issued'),
    wrpCause('licence_missing'),
    wrpCause('tenant_suspended', { class: 'subject_status', configurable: false, unconfigured: false, notice: undefined, stages: [], governing_reference: 'ADR 0095 s44', current_behaviour: 'request refused' }),
    wrpCause('brand_closed', { class: 'subject_status', configurable: false, unconfigured: false, notice: undefined, stages: [], governing_reference: 'ADR 0095 s44', current_behaviour: 'request refused' }),
    wrpCause('market_closed', { class: 'market', configurable: false, evaluated: false, unconfigured: false, notice: undefined, stages: [], governing_reference: 'Q18', current_behaviour: 'not checked' }),
  ],
  disclaimer: 'A withdrawal restriction policy is an operator configuration recorded by named staff. It is not a legal or regulatory authorisation and does not determine what any licence or jurisdiction requires.',
  ...o,
})

export const wrpConfigured = (): WrpEffective =>
  wrpEffective({
    set_version: 3,
    configured: true,
    causes: wrpEffective().causes.map((c) => (c.cause === 'licence_suspended' ? { ...c, unconfigured: false, notice: undefined, stages: stages('block', 'configured', 3) } : c)),
  })

export const wrpChange = (o: Partial<WrpChange> = {}): WrpChange => ({
  id: WRC1,
  tenant_id: TEN1,
  cause: 'licence_suspended',
  action: 'allow',
  reason_code: 'loosen_after_review',
  note: null,
  payload_hash: 'abc123',
  widening: true,
  required_approvals: 2,
  base_set_version: 3,
  requested_by: OTHER,
  status: 'requested',
  created_at: '2026-10-18T10:00:00Z',
  expires_at: '2099-01-01T10:00:00Z',
  approvals: [],
  ...o,
})

export const wrpResponse = (c: WrpChange = wrpChange(), executed = false): WrpChangeResponse => ({ change: c, executed, disclaimer: 'd' })

export const wrpVersion = (o: Partial<WrpVersion> = {}): WrpVersion => ({ id: VF1, cause: 'licence_suspended', action: 'block', set_version: 3, change_id: WRC1, reason_code: 'initial', created_by: null, created_at: '2026-10-10T10:00:00Z', ...o })

export function wrpReads(effective: WrpEffective = wrpEffective(), changes: WrpChange[] = []) {
  server.use(
    http.get(`/v1/admin/tenants/${TEN1}/withdrawal-restriction-policy`, () => HttpResponse.json(effective)),
    http.get(`/v1/admin/tenants/${TEN1}/withdrawal-restriction-policy/versions`, () => HttpResponse.json({ items: effective.configured ? [wrpVersion()] : [], disclaimer: 'd' })),
    http.get('/v1/admin/withdrawal-restriction-policy-changes', () => HttpResponse.json({ items: changes, disclaimer: 'd' })),
    http.get(`/v1/admin/withdrawal-restriction-policy-changes/${WRC1}`, () => HttpResponse.json(wrpResponse(changes[0] ?? wrpChange()))),
    http.get('/v1/admin/tenants', () => HttpResponse.json({ items: [{ id: TEN1, name: 'Acme', slug: 'acme', licensing_model: 'own_licence', status: 'active' }], limit: 50, offset: 0, total: 1 })),
    http.get(`/v1/admin/tenants/${TEN1}`, () => HttpResponse.json({ id: TEN1, name: 'Acme', slug: 'acme', licensing_model: 'own_licence', status: 'active' })),
  )
}
