import { describe, expect, it } from 'vitest'
import { captureRequests } from '../test/contractCapture'
import { PLATFORM_ADMIN_CLAIMS, signInAs } from '../test/session'
import { checkAgainstSpec, schemaEnum, schemaProps } from '../test/openapiLite'
import { CASINO_STREAMS, KINDS_BY_STREAM, STAFF_STREAMS, getStaffReconMismatch, getStaffReconRun, listCasinoReconMismatches, listCasinoReconRuns, listStaffReconMismatches, listStaffReconRuns } from './reconciliation'

const T = '11111111-1111-4111-8111-111111111111'

describe('reconciliation api: UI-to-OpenAPI contract (ADR 0114 R2 and casino routes)', () => {
  it('every request the module sends is documented', async () => {
    signInAs(PLATFORM_ADMIN_CLAIMS)
    const seen = captureRequests(() => ({ body: { items: [], limit: 50, offset: 0, total: 0 } }))
    await listStaffReconRuns(T, { stream: 'payment_statement', limit: 50, offset: 0 })
    await listStaffReconMismatches(T, { stream: 'ledger_vs_projection', status: 'open', kind: 'balance_mismatch', subject_id: T, limit: 50, offset: 50 })
    await listCasinoReconRuns({ stream: 'casino_statement', limit: 20, offset: 0 })
    await listCasinoReconMismatches({ stream: 'casino_consistency', status: 'open', limit: 20, offset: 0 })
    // A10c: the run_id filter and the two single-item reads
    await listStaffReconMismatches(T, { stream: 'payment_statement', run_id: T, limit: 20, offset: 0 })
    await getStaffReconRun(T, T, 'payment_statement')
    await getStaffReconMismatch(T, T, 'sportsbook_settlement')
    expect(seen).toHaveLength(7)
    for (const s of seen) expect(checkAgainstSpec(s)).toEqual([])
    expect(seen[4].query.get('run_id')).toBe(T)
    expect(seen[5].pathname).toBe(`/v1/admin/tenants/${T}/reconciliation/runs/${T}`)
    expect(seen[5].query.get('stream')).toBe('payment_statement')
    expect(seen[6].pathname).toBe(`/v1/admin/tenants/${T}/reconciliation/mismatches/${T}`)
  })

  it('stream and kind vocabularies equal the OpenAPI enums', () => {
    expect([...STAFF_STREAMS]).toEqual(schemaEnum('StaffReconciliationStream'))
    const all = Object.values(KINDS_BY_STREAM).flat().sort()
    expect(all).toEqual(schemaEnum('StaffReconciliationKind').sort())
    expect(CASINO_STREAMS).toEqual(['casino_consistency', 'casino_statement'])
  })

  it('the staff mismatch schema returns no stored key or diagnostic text; the casino one does (rendered as text only)', () => {
    const p = schemaProps('StaffReconciliationMismatch')
    for (const k of ['reconciliation_key', 'expected_value', 'actual_value', 'player_id']) expect(p).not.toContain(k)
    expect(schemaProps('CasinoReconciliationMismatch')).toContain('reconciliation_key')
  })
})
