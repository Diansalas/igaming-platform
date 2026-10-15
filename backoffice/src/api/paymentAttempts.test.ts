import { describe, expect, it } from 'vitest'
import { captureRequests } from '../test/contractCapture'
import { PLATFORM_ADMIN_CLAIMS, signInAs } from '../test/session'
import { checkAgainstSpec, hasOperation, queryParams, schemaEnum, schemaProps } from '../test/openapiLite'
import { ATTEMPT_OPERATIONS, ATTEMPT_STATES, TERMINAL_REASONS, getPaymentAttempt, listPaymentAttempts } from './paymentAttempts'

const T = '11111111-1111-4111-8111-111111111111'
const A = '22222222-2222-4222-8222-222222222222'

describe('paymentAttempts api: UI-to-OpenAPI contract (ADR 0114 R1)', () => {
  it('every request the module sends is documented (path, method, query)', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    const seen = captureRequests()
    await listPaymentAttempts(T, { operation: 'payout', state: 'disputed', terminal_reason: 'destination_mismatch', withdrawal_request_id: A, limit: 50, cursor: 'c' })
    await getPaymentAttempt(T, A)
    expect(seen).toHaveLength(2)
    for (const s of seen) expect(checkAgainstSpec(s)).toEqual([])
    expect(seen[0].pathname).toBe(`/v1/admin/tenants/${T}/payment-attempts`)
    expect(seen[0].query.get('cursor')).toBe('c')
    // a client tenant id must never be sent in the query (the route 400s on unknown parameters)
    expect(seen[0].query.has('tenant_id')).toBe(false)
  })

  it('the closed vocabularies equal the OpenAPI enums', () => {
    expect([...ATTEMPT_STATES]).toEqual(schemaEnum('PaymentAttemptState'))
    expect([...TERMINAL_REASONS]).toEqual(schemaEnum('PaymentAttemptTerminalReason'))
    expect([...ATTEMPT_OPERATIONS]).toEqual(['payout', 'deposit'])
    expect(hasOperation('/v1/admin/tenants/{tenantID}/payment-attempts', 'get')).toBe(true)
    expect(queryParams('/v1/admin/tenants/{tenantID}/payment-attempts', 'get').sort()).toEqual(['cursor', 'limit', 'operation', 'state', 'terminal_reason', 'withdrawal_request_id'])
  })

  it('the schema has no field that would carry a provider reference, player or destination (redaction pin)', () => {
    const props = schemaProps('PaymentAttempt')
    for (const forbidden of ['provider_reference', 'merchant_reference', 'player_id', 'wallet_id', 'destination', 'idempotency_key', 'staff_id']) {
      expect(props).not.toContain(forbidden)
    }
    expect(props).toContain('has_provider_reference')
  })
})
