import { describe, expect, it } from 'vitest'
import type { TenantJurisdictionConfig } from '../../api/licensing'
import { authRefProblem, ceilingClass, configClass, countryProblem, currencyListProblem, geoListProblem, paymentListProblem, policyClass, policyNeedsRef, rulesetProblem, stateStatus } from './marketRules'

const cfg: TenantJurisdictionConfig = {
  id: 'c', tenant_id: 't', jurisdiction_id: 'j', kyc_ruleset_id: 'k', aml_ruleset_id: null, rg_ruleset_id: null, reporting_ruleset_id: null,
  allowed_currencies: ['EUR', 'USD'], allowed_payment_methods: ['card'], geo_block_list: ['YY'], effective_from: 'x', effective_to: null,
}
const same = { kyc: 'k', aml: null, rg: null, reporting: null, currencies: ['EUR', 'USD'], payments: ['card'], geo: ['YY'] }

describe('market rules mirror the server classification (hints only)', () => {
  it('state/status per action', () => {
    expect(stateStatus('enable')).toEqual({ state: 'enabled', status: 'active' })
    expect(stateStatus('disable')).toEqual({ state: 'disabled', status: 'active' })
    expect(stateStatus('withdraw')).toEqual({ state: 'disabled', status: 'withdrawn' })
  })
  it('ceiling: only enabling widens', () => {
    expect(ceilingClass('enable')).toBe('widening')
    expect(ceilingClass('disable')).toBe('narrowing')
    expect(ceilingClass('withdraw')).toBe('narrowing')
  })
  it('policy: enable widens; withdraw widens below the tenant rung only; disable narrows', () => {
    expect(policyClass('tenant', 'enable')).toBe('widening')
    expect(policyClass('tenant', 'withdraw')).toBe('narrowing')
    expect(policyClass('brand', 'withdraw')).toBe('widening')
    expect(policyClass('operation', 'withdraw')).toBe('widening')
    for (const s of ['tenant', 'brand', 'operation'] as const) expect(policyClass(s, 'disable')).toBe('narrowing')
  })
  it('a reference is required to enable and to withdraw below the tenant rung', () => {
    expect(policyNeedsRef('tenant', 'enable')).toBe(true)
    expect(policyNeedsRef('tenant', 'withdraw')).toBe(false)
    expect(policyNeedsRef('brand', 'withdraw')).toBe(true)
    expect(policyNeedsRef('operation', 'disable')).toBe(false)
  })
  it('configuration: first version, ruleset change, added currency / method, removed geo block widen; removals and added geo blocks narrow; same is neutral', () => {
    expect(configClass(null, same)).toBe('widening')
    expect(configClass(cfg, same)).toBe('neutral')
    expect(configClass(cfg, { ...same, kyc: 'k2' })).toBe('widening')
    expect(configClass(cfg, { ...same, aml: 'a' })).toBe('widening')
    expect(configClass(cfg, { ...same, currencies: ['EUR', 'USD', 'GBP'] })).toBe('widening')
    expect(configClass(cfg, { ...same, payments: ['card', 'bank'] })).toBe('widening')
    expect(configClass(cfg, { ...same, geo: [] })).toBe('widening')
    expect(configClass(cfg, { ...same, currencies: ['EUR'] })).toBe('narrowing')
    expect(configClass(cfg, { ...same, payments: [] })).toBe('narrowing')
    expect(configClass(cfg, { ...same, geo: ['YY', 'ZZ'] })).toBe('narrowing')
  })
  it('input checks', () => {
    expect(authRefProblem('')).toMatch(/required/)
    expect(authRefProblem(' x')).toMatch(/Remove the spaces/)
    expect(authRefProblem('x\ny')).toMatch(/Control/)
    expect(authRefProblem('a'.repeat(501))).toMatch(/at most 500/)
    expect(authRefProblem('AUTH-1')).toBeNull()
    expect(countryProblem('ZZ')).toBeNull()
    for (const bad of ['', 'z', 'zz', 'ZZZ', 'Z1']) expect(countryProblem(bad), bad).not.toBeNull()
    expect(currencyListProblem('EUR, USD')).toBeNull()
    expect(currencyListProblem('eur')).toMatch(/currency code/)
    expect(currencyListProblem('EUR, EUR')).toMatch(/repeated/)
    expect(paymentListProblem('card, bank_transfer')).toBeNull()
    expect(paymentListProblem('Card')).toMatch(/payment method/)
    expect(geoListProblem('YY, ZZ')).toBeNull()
    expect(geoListProblem('yy')).toMatch(/ISO 3166-1/)
    expect(rulesetProblem('')).toBeNull()
    expect(rulesetProblem('kyc-a_1.0')).toBeNull()
    expect(rulesetProblem('-bad')).not.toBeNull()
  })
})
