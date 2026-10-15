import { describe, expect, it } from 'vitest'
import { captureRequests } from '../test/contractCapture'
import { PLATFORM_ADMIN_CLAIMS, signInAs } from '../test/session'
import { checkAgainstSpec, schemaEnum, schemaProps } from '../test/openapiLite'
import { BASIS_CODES, CONTEXT_CODES, FINDING_CODES, approveForceResolution, cancelForceResolution, getForceResolution, listForceResolutions, rejectForceResolution, requestForceResolution } from './forceResolutions'

const T = '11111111-1111-4111-8111-111111111111'
const R = '44444444-4444-4444-8444-444444444444'
const H = 'a'.repeat(64)

describe('forceResolutions api: UI-to-OpenAPI contract (K3 M1/M2)', () => {
  it('every request the module sends is documented', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    const seen = captureRequests()
    await listForceResolutions(T)
    await getForceResolution(T, R)
    await requestForceResolution(T, { attempt_id: R, kind: 'm2_declare_paid', basis_code: 'reconciliation_exhausted', evidence_ref_hash: H, reason_code: 'provider_unqueryable_case', note: 'n' }, 'a')
    await requestForceResolution(T, { attempt_id: R, kind: 'm1_deposit_evidence', finding_code: 'awaiting_psp_refund', reason_code: 'x' }, 'b')
    await approveForceResolution(T, R, H, 'ok', 'c')
    await rejectForceResolution(T, R, H, 'no', 'd')
    await cancelForceResolution(T, R, 'e')
    expect(seen).toHaveLength(7)
    for (const s of seen) expect(checkAgainstSpec(s)).toEqual([])
    const approve = seen[4]
    expect(approve.body).toEqual({ payload_hash: H, reason_code: 'ok' })
    expect(approve.headers.get('Idempotency-Key')).toBe('c')
    // the client supplies no proof / signature field
    for (const s of seen) expect(JSON.stringify(s.body ?? {})).not.toMatch(/proof|signature|tenant_id/)
  })

  it('code catalogues equal the OpenAPI enums', () => {
    expect([...FINDING_CODES]).toEqual(schemaEnum('PaymentForceResolutionRequest', 'finding_code'))
    expect([...BASIS_CODES]).toEqual(schemaEnum('PaymentForceResolutionRequest', 'basis_code'))
    expect([...CONTEXT_CODES]).toEqual(schemaEnum('PaymentForceResolutionRequest', 'context_code'))
  })

  it('the response schema never returns the evidence reference text', () => {
    const p = schemaProps('PaymentForceResolution')
    expect(p).not.toContain('evidence_reference')
    expect(p).toContain('evidence_reference_present')
    expect(p).toContain('payload_hash')
  })
})
