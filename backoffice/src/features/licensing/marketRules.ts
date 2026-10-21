// Client-side checks that MIRROR the server rules for the market-configuration payloads (internal/licensingchange/kinds.go and
// internal/jurisdiction/tenant_jurisdiction_config_admin.go). They exist only to save a round trip: the server stays
// authoritative and every refusal is rendered. A value accepted here can still be refused (an unassigned country code, an
// unknown operation, an inactive currency, a missing precondition); a value refused here would have been refused by the server.
import type { CeilingState, PolicyScope, VersionStatus } from '../../api/marketConfig'
import type { TenantJurisdictionConfig } from '../../api/licensing'
import { codeProblem, countryProblem, splitList } from './basisRules'

export { codeProblem, countryProblem, splitList }

export const MAX_AUTH_REF = 500
const encoder = new TextEncoder()
// eslint-disable-next-line no-control-regex
const CONTROL = /[\u0000-\u001f\u007f]/

export type MarketAction = 'enable' | 'disable' | 'withdraw'
export const ACTION_LABEL: Record<MarketAction, string> = {
  enable: 'Enable the country',
  disable: 'Disable the country (emergency stop)',
  withdraw: 'Withdraw the version (record that no decision stands)',
}

export function stateStatus(a: MarketAction): { state: CeilingState; status: VersionStatus } {
  if (a === 'enable') return { state: 'enabled', status: 'active' }
  if (a === 'disable') return { state: 'disabled', status: 'active' }
  return { state: 'disabled', status: 'withdrawn' }
}

/**
 * Operator-supplied authorization reference: 1..500 bytes, trimmed, no control characters (the server's rule). It names a document
 * supplied by a human with the authority; the platform never verifies it.
 */
export function authRefProblem(v: string): string | null {
  if (v === '') return 'This is required.'
  if (v !== v.trim()) return 'Remove the spaces at the start or end.'
  if (encoder.encode(v).length > MAX_AUTH_REF) return `Use at most ${MAX_AUTH_REF} bytes.`
  if (CONTROL.test(v)) return 'Control characters are not allowed.'
  return null
}

export const optAuthRefProblem = (v: string) => (v === '' ? null : authRefProblem(v))

/** Ceiling: only enabling widens. A disable or a withdrawal narrows and is applied at once by one platform administrator. */
export function ceilingClass(a: MarketAction): 'widening' | 'narrowing' {
  return a === 'enable' ? 'widening' : 'narrowing'
}

/**
 * Operating-country policy: enabling widens at every rung; a withdrawal widens at the brand and operation rungs (absence there
 * inherits) and narrows at the tenant rung (absence there is a terminal block); a disable narrows.
 */
export function policyClass(scope: PolicyScope, a: MarketAction): 'widening' | 'narrowing' {
  if (a === 'enable') return 'widening'
  if (a === 'withdraw' && scope !== 'tenant') return 'widening'
  return 'narrowing'
}

/** Authorization reference required by the server for this action at this rung (enable; withdraw below the tenant rung). */
export function policyNeedsRef(scope: PolicyScope, a: MarketAction): boolean {
  return a === 'enable' || (a === 'withdraw' && scope !== 'tenant')
}

export const ceilingNeedsRef = (a: MarketAction) => a === 'enable'

const RULESET = /^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$/
const CURRENCY = /^[A-Z0-9]{2,12}$/
const PAYMENT = /^[a-z0-9][a-z0-9_]{0,63}$/

export function rulesetProblem(v: string): string | null {
  return v === '' || RULESET.test(v) ? null : 'Use 1 to 128 characters of letters, digits and _ . : - (starting with a letter or digit), or leave blank for none.'
}

function listProblem(v: string, max: number, ok: (s: string) => boolean, what: string): string | null {
  const items = splitList(v)
  if (items.length > max) return `Use at most ${max} entries.`
  const seen = new Set<string>()
  for (const i of items) {
    if (!ok(i)) return `"${i.slice(0, 20)}" is not ${what}.`
    if (seen.has(i)) return 'An entry is repeated.'
    seen.add(i)
  }
  return null
}

/** Currency codes: 2..12 upper-case letters or digits, at most 64, no repeats. The server also requires each to be an active asset. */
export const currencyListProblem = (v: string) => listProblem(v, 64, (s) => CURRENCY.test(s), 'a currency code (2 to 12 upper-case letters or digits)')
/** Payment method codes: lower-case letters, digits and underscores (1..64), at most 64, no repeats. */
export const paymentListProblem = (v: string) => listProblem(v, 64, (s) => PAYMENT.test(s), 'a payment method code (lower-case letters, digits and _)')
/** Geo block list: ISO 3166-1 alpha-2 shapes, at most 300, no repeats. The server checks each is currently assigned. */
export const geoListProblem = (v: string) => listProblem(v, 300, (s) => countryProblem(s) === null, 'two upper-case letters (ISO 3166-1 alpha-2)')

const same = (a: string[], b: string[]) => a.length === b.length && a.every((x) => b.includes(x))

/**
 * The class the server will compute for a configuration change: a first version, any ruleset id change, an added currency or
 * payment method or a removed geo block WIDEN; only removals of currencies / payment methods and added geo blocks with the same
 * ruleset ids NARROW; a close narrows. A hint only: the database classifies.
 */
export function configClass(
  current: TenantJurisdictionConfig | null,
  next: { kyc: string | null; aml: string | null; rg: string | null; reporting: string | null; currencies: string[]; payments: string[]; geo: string[] },
): 'widening' | 'narrowing' | 'neutral' {
  if (current === null) return 'widening'
  if (current.kyc_ruleset_id !== next.kyc || current.aml_ruleset_id !== next.aml || current.rg_ruleset_id !== next.rg || current.reporting_ruleset_id !== next.reporting) return 'widening'
  if (next.currencies.some((c) => !current.allowed_currencies.includes(c))) return 'widening'
  if (next.payments.some((c) => !current.allowed_payment_methods.includes(c))) return 'widening'
  if (current.geo_block_list.some((c) => !next.geo.includes(c))) return 'widening'
  if (same(current.allowed_currencies, next.currencies) && same(current.allowed_payment_methods, next.payments) && same(current.geo_block_list, next.geo)) return 'neutral'
  return 'narrowing'
}

/** Jurisdiction registry text: required, trimmed, no control characters. */
export function registryTextProblem(v: string, max: number): string | null {
  if (v === '') return 'This is required.'
  if (v !== v.trim()) return 'Remove the spaces at the start or end.'
  if (encoder.encode(v).length > max) return `Use at most ${max} bytes.`
  if (CONTROL.test(v)) return 'Control characters are not allowed.'
  return null
}
export const optRegistryTextProblem = (v: string, max: number) => (v === '' ? null : registryTextProblem(v, max))
