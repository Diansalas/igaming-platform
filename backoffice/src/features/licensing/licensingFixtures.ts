// Fixtures for the licensing screens. Shapes follow docs/api/openapi/platform-api.yaml; every value is synthetic.
import { http, HttpResponse } from 'msw'
import type { Jurisdiction, LicenceDetail, LicenceVersion, LicensingChange, LicensingChangeResponse } from '../../api/licensing'
import { server } from '../../test/mswServer'

export const JUR1 = '10000000-0000-4000-8000-000000000001'
export const LIC1 = '20000000-0000-4000-8000-000000000001'
export const LIC2 = '20000000-0000-4000-8000-000000000002'
export const TEN1 = '30000000-0000-4000-8000-000000000001'
export const CHG1 = '40000000-0000-4000-8000-000000000001'
export const ME = 'f9000000-0000-4000-8000-000000000009'
export const OTHER = 'f8000000-0000-4000-8000-000000000008'
export const THIRD = 'f7000000-0000-4000-8000-000000000007'
export const SENTINEL = 'RAW SERVER MESSAGE <b>do-not-render</b>'
export const DISCLAIMER =
  'A licensing change record is a decision recorded by named staff. It is not a legal or regulatory authorisation and does not determine whether any licence or jurisdiction permits operation.'

export const jurisdiction = (o: Partial<Jurisdiction> = {}): Jurisdiction => ({ id: JUR1, code: 'XX', name: 'Testland', regulatory_body: 'Test Body', notes: 'synthetic', created_at: '2026-01-01T00:00:00Z', ...o })

export const licence = (o: Partial<LicenceDetail> = {}): LicenceDetail => ({
  id: LIC1,
  jurisdiction_id: JUR1,
  licensee: 'platform',
  licence_number: 'SYN-0001',
  status: 'suspended',
  permitted_products: ['casino'],
  permitted_markets: [],
  created_at: '2026-01-01T00:00:00Z',
  issued_at: '2026-01-02',
  expires_at: '2027-01-02',
  review_due_at: '2026-12-01',
  licence_evidence_reference: 'TICKET-123',
  updated_at: '2026-01-03T00:00:00Z',
  tenant_ids: [TEN1],
  ...o,
})

export const version = (o: Partial<LicenceVersion> = {}): LicenceVersion => ({
  version_no: 1,
  operation: 'baseline',
  changed_at: '2026-01-01T00:00:00Z',
  changed_by: null,
  status: 'active',
  issued_at: null,
  expires_at: null,
  review_due_at: null,
  licence_evidence_reference: null,
  licence_number: 'SYN-0001',
  ...o,
})

export const change = (o: Partial<LicensingChange> = {}): LicensingChange => ({
  id: CHG1,
  kind: 'licence_status_change',
  payload: { licence_id: LIC1, status: 'active' },
  payload_hash: 'abc123',
  reason_code: 'reinstated',
  note: null,
  widening: true,
  required_approvals: 2,
  subject_tenant_id: null,
  subject_licence_id: LIC1,
  requested_by: OTHER,
  requested_scope: 'platform',
  status: 'requested',
  created_at: '2026-10-18T10:00:00Z',
  expires_at: '2099-01-01T10:00:00Z',
  approvals: [],
  ...o,
})

export const changeResponse = (c: LicensingChange = change(), executed = false): LicensingChangeResponse => ({ change_request: c, executed, disclaimer: DISCLAIMER })

export const page = <T,>(items: T[]) => ({ items, limit: 20, offset: 0, total: items.length })

/** Installs default read handlers. Override per test with server.use. */
export function licensingReads(o: { licences?: LicenceDetail[]; changes?: LicensingChange[] } = {}) {
  const licences = o.licences ?? [licence()]
  server.use(
    http.get('/v1/admin/jurisdictions', () => HttpResponse.json([jurisdiction()])),
    http.get(`/v1/admin/jurisdictions/${JUR1}`, () => HttpResponse.json(jurisdiction())),
    http.get('/v1/admin/licences', () => HttpResponse.json(licences)),
    http.get(`/v1/admin/licences/${LIC1}`, () => HttpResponse.json(licences[0])),
    http.get(`/v1/admin/licences/${LIC1}/versions`, () => HttpResponse.json([version()])),
    http.get('/v1/admin/licence-country-ceilings', () => HttpResponse.json(page([]))),
    http.get('/v1/admin/licensing-changes', () => HttpResponse.json({ items: o.changes ?? [change()], disclaimer: DISCLAIMER })),
    http.get(`/v1/admin/licensing-changes/${CHG1}`, () => HttpResponse.json(changeResponse((o.changes ?? [change()])[0]))),
    http.get('/v1/admin/tenants', () => HttpResponse.json({ items: [{ id: TEN1, name: 'Acme', slug: 'acme', licensing_model: 'own_licence', status: 'active' }], limit: 50, offset: 0, total: 1 })),
    http.get(`/v1/admin/tenants/${TEN1}`, () => HttpResponse.json({ id: TEN1, name: 'Acme', slug: 'acme', licensing_model: 'under_platform_licence', status: 'active' })),
    http.get(`/v1/admin/tenants/${TEN1}/jurisdiction-configs`, () => HttpResponse.json(page([]))),
    http.get('/v1/admin/operating-country-policies', () => HttpResponse.json(page([]))),
  )
}
