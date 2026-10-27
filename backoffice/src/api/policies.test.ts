import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'
import { BACKOFFICE_API_DIR, checkCalls, extractCalls, formatFindings, loadOpenApi } from '../test/openapiContract'
import { classifyKycPolicyWrite, THRESHOLD_PATTERN } from './kycEnforcementPolicies'
import { classifyResidencePolicyApproval, classifyResidenceSet, isDeterminationApproved } from './residenceApproval'
import { classifyWrpDecision, classifyWrpFiling } from './withdrawalRestriction'

const spec = loadOpenApi()
const read = (f: string) => readFileSync(join(BACKOFFICE_API_DIR, f), 'utf8')

// Pins that each lic16 module contains exactly these calls, so the generic contract test cannot pass vacuously for them.
const EXPECTED: Record<string, string[]> = {
  'kycEnforcementPolicies.ts': [
    'GET /v1/admin/kyc/enforcement-policies',
    'POST /v1/admin/kyc/enforcement-policies',
    'POST /v1/admin/kyc/enforcement-policies/{}/activate',
    'POST /v1/admin/kyc/enforcement-policies/{}/withdraw',
    'POST /v1/admin/kyc/enforcement-policies/{}/supersede',
  ],
  'residenceApproval.ts': [
    'GET /v1/admin/tenants/{}/residence-approval-policy',
    'POST /v1/admin/tenants/{}/residence-approval-policy',
    'POST /v1/admin/tenants/{}/residence-approval-policy/{}/approvals',
    'GET /v1/admin/kyc/residence-determinations/pending',
    'POST /v1/admin/kyc/verifications/{}/residence-approval',
  ],
  'withdrawalRestriction.ts': [
    'GET /v1/admin/tenants/{}/withdrawal-restriction-policy',
    'GET /v1/admin/tenants/{}/withdrawal-restriction-policy/versions',
    'GET /v1/admin/withdrawal-restriction-policy-changes',
    'GET /v1/admin/withdrawal-restriction-policy-changes/{}',
    'POST /v1/admin/withdrawal-restriction-policy-changes',
    'POST /v1/admin/withdrawal-restriction-policy-changes/{}/approve',
    'POST /v1/admin/withdrawal-restriction-policy-changes/{}/reject',
    'POST /v1/admin/withdrawal-restriction-policy-changes/{}/cancel',
  ],
}

describe.each(Object.keys(EXPECTED))('api/%s against docs/api/openapi/platform-api.yaml', (file) => {
  const calls = extractCalls(`api/${file}`, read(file))
  it('calls exactly the expected routes, each in the spec with the same method, body fields and query keys', () => {
    expect(calls.map((c) => `${c.method} ${c.path}`).sort()).toEqual([...EXPECTED[file]].sort())
    const { findings } = checkCalls(spec, calls, {})
    expect(findings, `\n${formatFindings(findings)}\n`).toEqual([])
  })
  it('does no signing, hashing or proof handling in the browser (the actor proof is server-side)', () => {
    expect(read(file).split('\n').filter((l) => !/^\s*(\/\/|\*|\/\*)/.test(l)).join('\n')).not.toMatch(/crypto\.subtle|sha256|hmac|X-Actor-Proof|actorProof|signature/i)
  })
})

describe('request pins', () => {
  const kyc = extractCalls('api/kycEnforcementPolicies.ts', read('kycEnforcementPolicies.ts'))
  const res = extractCalls('api/residenceApproval.ts', read('residenceApproval.ts'))
  const wrp = extractCalls('api/withdrawalRestriction.ts', read('withdrawalRestriction.ts'))
  const body = (calls: typeof kyc, m: string, p: string) => calls.find((c) => c.method === m && c.path === p)?.bodyKeys
  it('KYC: filters, create body, reason and successor bodies; no edit or deactivate route', () => {
    expect(kyc.find((c) => c.method === 'GET')?.queryKeys).toEqual(expect.arrayContaining(['licensing_jurisdiction_id', 'status']))
    expect(body(kyc, 'POST', '/v1/admin/kyc/enforcement-policies/{}/activate')).toEqual(['reason_code'])
    expect(body(kyc, 'POST', '/v1/admin/kyc/enforcement-policies/{}/withdraw')).toEqual(['reason_code'])
    expect(body(kyc, 'POST', '/v1/admin/kyc/enforcement-policies/{}/supersede')).toEqual(['successor_policy_id', 'reason_code'])
    expect(kyc.some((c) => c.method !== 'GET' && c.method !== 'POST')).toBe(false)
    expect(kyc.some((c) => /deactivate|disable|switch/i.test(c.path))).toBe(false)
  })
  it('the spec create schema accepts exactly the trigger types the UI authors', () => {
    const e = spec.components?.schemas?.KYCEnforcementPolicyCreateRequest?.properties?.trigger_type?.enum as string[]
    expect(e).toEqual(['cumulative_deposit', 'play'])
  })
  it('residence: bodies', () => {
    expect(body(res, 'POST', '/v1/admin/tenants/{}/residence-approval-policy')).toEqual(['mode', 'reason_code'])
    expect(body(res, 'POST', '/v1/admin/tenants/{}/residence-approval-policy/{}/approvals')).toEqual(['reason_code'])
    expect(body(res, 'POST', '/v1/admin/kyc/verifications/{}/residence-approval')).toEqual(['determined_at', 'reason_code'])
  })
  it('withdrawal restriction: bodies; cancel sends none', () => {
    expect(body(wrp, 'POST', '/v1/admin/withdrawal-restriction-policy-changes/{}/approve')).toEqual(['reason_code'])
    expect(body(wrp, 'POST', '/v1/admin/withdrawal-restriction-policy-changes/{}/reject')).toEqual(['reason_code'])
    expect(body(wrp, 'POST', '/v1/admin/withdrawal-restriction-policy-changes/{}/cancel') ?? []).toEqual([])
    expect(wrp.find((c) => c.method === 'GET' && c.path === '/v1/admin/withdrawal-restriction-policy-changes')?.queryKeys).toEqual(expect.arrayContaining(['tenant_id', 'status', 'limit']))
  })
})

describe('classifiers never fake success', () => {
  it('KYC write is confirmed only for the expected status and id', () => {
    expect(classifyKycPolicyWrite({ id: 'a', status: 'active' }, 'active', 'a').kind).toBe('confirmed')
    expect(classifyKycPolicyWrite({ id: 'b', status: 'active' }, 'active', 'a').kind).toBe('unconfirmed')
    expect(classifyKycPolicyWrite({ id: 'a', status: 'draft' }, 'active', 'a').kind).toBe('unconfirmed')
    for (const v of [null, undefined, {}, 'ok', []]) expect(classifyKycPolicyWrite(v, 'draft').kind).toBe('unconfirmed')
  })
  it('threshold pattern is a digit string, never a number', () => {
    for (const ok of ['1', '1000', '9'.repeat(38)]) expect(THRESHOLD_PATTERN.test(ok)).toBe(true)
    for (const bad of ['0', '01', '-1', '1.5', '1e3', '', ' 1', '9'.repeat(39)]) expect(THRESHOLD_PATTERN.test(bad)).toBe(false)
  })
  it('residence set: applied needs active+0 approvals, pending needs pending+2, and the requested mode must be echoed', () => {
    const ch = (o: object) => ({ policy_id: 'p', status: 'active', required_approvals: 0, previous_mode: 'none', requested_mode: 'all', ...o })
    expect(classifyResidenceSet(ch({}), 'all').kind).toBe('applied')
    expect(classifyResidenceSet(ch({ status: 'pending', required_approvals: 2, previous_mode: 'all', requested_mode: 'none' }), 'none').kind).toBe('pending')
    expect(classifyResidenceSet(ch({ status: 'active', required_approvals: 2 }), 'all').kind).toBe('unconfirmed')
    expect(classifyResidenceSet(ch({ status: 'pending', required_approvals: 0 }), 'all').kind).toBe('unconfirmed')
    expect(classifyResidenceSet(ch({}), 'none').kind).toBe('unconfirmed')
    for (const v of [null, {}, 'x']) expect(classifyResidenceSet(v, 'all').kind).toBe('unconfirmed')
  })
  it('residence approvals', () => {
    expect(classifyResidencePolicyApproval({ status: 'active', approvals: 2, required_approvals: 2 }).kind).toBe('activated')
    expect(classifyResidencePolicyApproval({ status: 'pending', approvals: 1, required_approvals: 2 }).kind).toBe('recorded')
    expect(classifyResidencePolicyApproval({ status: 'active', approvals: 1, required_approvals: 2 }).kind).toBe('unconfirmed')
    expect(classifyResidencePolicyApproval({ status: 'pending', approvals: 2, required_approvals: 2 }).kind).toBe('unconfirmed')
    expect(isDeterminationApproved({ status: 'approved', verification_id: 'v' }, 'v')).toBe(true)
    expect(isDeterminationApproved({ status: 'approved', verification_id: 'w' }, 'v')).toBe(false)
    expect(isDeterminationApproved({ status: 'pending', verification_id: 'v' }, 'v')).toBe(false)
  })
  it('wrp filing and decisions', () => {
    const r = (status: string, executed: boolean) => ({ change: { id: 'c', status, approvals: [] }, executed, disclaimer: 'd' })
    expect(classifyWrpFiling(r('executed', true)).kind).toBe('applied')
    expect(classifyWrpFiling(r('requested', false)).kind).toBe('pending')
    expect(classifyWrpFiling(r('requested', true)).kind).toBe('unconfirmed')
    expect(classifyWrpFiling(r('executed', false)).kind).toBe('unconfirmed')
    expect(classifyWrpFiling({ change: { id: 'c' } }).kind).toBe('unconfirmed')
    expect(classifyWrpDecision('approve', r('executed', true)).kind).toBe('executed')
    expect(classifyWrpDecision('approve', r('requested', false)).kind).toBe('recorded')
    expect(classifyWrpDecision('approve', r('rejected', false)).kind).toBe('unconfirmed')
    expect(classifyWrpDecision('reject', r('rejected', false)).kind).toBe('rejected')
    expect(classifyWrpDecision('reject', r('requested', false)).kind).toBe('unconfirmed')
    expect(classifyWrpDecision('cancel', r('cancelled', false)).kind).toBe('cancelled')
    expect(classifyWrpDecision('cancel', null).kind).toBe('unconfirmed')
  })
})
