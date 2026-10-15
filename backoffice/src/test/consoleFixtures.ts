// Fixtures for the S3/S5/S7/S8 console tests. Shapes follow docs/api/openapi/platform-api.yaml exactly; the
// api module tests pin the request side against the spec, these pin what the screens do with the response side.
import { HttpResponse } from 'msw'
import type { CapabilityGrant, CapabilityGrantRequest } from '../api/capabilityGrants'
import type { PaymentForceResolution } from '../api/forceResolutions'
import type { WithdrawalHoldResolution } from '../api/holdResolutions'
import type { PaymentAttempt } from '../api/paymentAttempts'

export const TENANT = '11111111-1111-4111-8111-111111111111'
export const OTHER_TENANT = '99999999-9999-4999-8999-999999999999'
export const ATT1 = 'a1000000-0000-4000-8000-000000000001'
export const ATT2 = 'a2000000-0000-4000-8000-000000000002'
export const WD1 = 'b1000000-0000-4000-8000-000000000001'
export const RES1 = 'c1000000-0000-4000-8000-000000000001'
export const RES2 = 'c2000000-0000-4000-8000-000000000002'
export const REQ1 = 'd1000000-0000-4000-8000-000000000001'
export const GRANT1 = 'e1000000-0000-4000-8000-000000000001'
export const STAFF_A = 'f1000000-0000-4000-8000-000000000001'
export const STAFF_B = 'f2000000-0000-4000-8000-000000000002'
export const HASH1 = '0123456789abcdef'.repeat(4)
export const HASH2 = 'fedcba9876543210'.repeat(4)

export const SENTINEL = 'RAW SERVER MESSAGE <b>do-not-render</b>'

export const FI = { role: 'finance', tenant_id: TENANT, sub: STAFF_A }
export const CO = { role: 'compliance', tenant_id: TENANT, sub: STAFF_B }
export const TA = { role: 'tenant_admin', tenant_id: TENANT, sub: STAFF_A }

export function attempt(over: Partial<PaymentAttempt> = {}): PaymentAttempt {
  return {
    id: ATT1,
    operation: 'payout',
    attempt_no: 1,
    state: 'disputed',
    terminal_reason: 'destination_mismatch',
    subject_ref: { withdrawal_request_id: WD1 },
    amount_minor_units: '12345',
    asset_code: 'EUR',
    exponent: 2,
    provider_id: 'mock-payments',
    has_provider_reference: true,
    created_at: '2026-10-10T10:00:00Z',
    updated_at: '2026-10-10T11:00:00Z',
    resolved_at: '2026-10-10T11:00:00Z',
    open_resolution_id: null,
    admissible_kinds: ['m2_declare_paid', 'm2_declare_not_paid'],
    ...over,
  }
}

export function resolution(over: Partial<PaymentForceResolution> = {}): PaymentForceResolution {
  return {
    id: RES1,
    tenant_id: TENANT,
    attempt_id: ATT1,
    operation: 'payout',
    kind: 'm2_declare_paid',
    target_state: 'succeeded',
    basis_code: 'reconciliation_exhausted',
    evidence_ref_hash: HASH2,
    amount_minor_units: '12345',
    asset_code: 'EUR',
    exponent: 2,
    reason_code: 'provider_unqueryable_case',
    payload_hash: HASH1,
    requested_by: STAFF_B,
    requested_by_scope: 'tenant',
    required_at_submission: 2,
    tenant_status_at_submission: 'active',
    state: 'pending',
    created_at: '2026-10-11T10:00:00Z',
    expires_at: '2099-10-12T10:00:00Z',
    ...over,
  }
}

export function holdResolution(over: Partial<WithdrawalHoldResolution> = {}): WithdrawalHoldResolution {
  return {
    id: RES2,
    tenant_id: TENANT,
    withdrawal_request_id: WD1,
    kind: 'release_hold_to_player',
    amount_minor_units: '5000',
    asset_code: 'EUR',
    exponent: 2,
    reason_code: 'tenant_closed',
    evidence_ref_hash: HASH2,
    payload_hash: HASH1,
    requested_by: STAFF_B,
    requested_by_scope: 'platform_acting',
    required_at_submission: 2,
    tenant_status_at_submission: 'closed',
    brand_status_at_submission: 'closed',
    state: 'pending',
    created_at: '2026-10-11T10:00:00Z',
    expires_at: '2099-10-12T10:00:00Z',
    ...over,
  }
}

export function grantRequest(over: Partial<CapabilityGrantRequest> = {}): CapabilityGrantRequest {
  return {
    id: REQ1,
    tenant_id: TENANT,
    grantee_staff_id: STAFF_A,
    grantee_scope: 'tenant',
    capability: 'payment_force_resolve:request',
    valid_from: '2026-10-11T10:00:00Z',
    valid_until: '2099-12-01T00:00:00Z',
    reason_code: 'rota_cover',
    requested_by: STAFF_B,
    requested_by_scope: 'tenant',
    status: 'pending',
    created_at: '2026-10-11T10:00:00Z',
    expires_at: '2099-10-18T10:00:00Z',
    ...over,
  }
}

export function grant(over: Partial<CapabilityGrant> = {}): CapabilityGrant {
  return {
    id: GRANT1,
    tenant_id: TENANT,
    grantee_staff_id: STAFF_A,
    grantee_scope: 'tenant',
    capability: 'payment_force_resolve:approve',
    valid_from: '2026-10-01T00:00:00Z',
    valid_until: '2099-12-01T00:00:00Z',
    granted_at: '2026-10-01T00:00:00Z',
    ...over,
  }
}

/** A server refusal. The message is the sentinel unless a closed token is given; the sentinel must never reach the DOM. */
export const refusal = (status: number, code: string, message = SENTINEL) => HttpResponse.json({ code, message, request_id: 'req-9' }, { status })
