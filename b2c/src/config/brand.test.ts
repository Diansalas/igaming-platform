import { describe, expect, it } from 'vitest'
import { resolveBrandSlug } from './brand'

// Stage 9 production-readiness finding: a production build with no
// VITE_BRAND_SLUG used to silently deploy as "demo-casino" - a
// cross-tenant misconfiguration, not a cosmetic one (brand_slug is what
// resolves which tenant every register/login call is scoped to
// server-side). This now fails loudly in a production build instead of
// silently falling back.
describe('resolveBrandSlug', () => {
  it('uses the configured slug when set', () => {
    expect(resolveBrandSlug({ VITE_BRAND_SLUG: 'acme-casino', PROD: true })).toBe('acme-casino')
    expect(resolveBrandSlug({ VITE_BRAND_SLUG: 'acme-casino', PROD: false })).toBe('acme-casino')
  })

  it('falls back to the demo slug outside a production build', () => {
    expect(resolveBrandSlug({ PROD: false })).toBe('demo-casino')
    expect(resolveBrandSlug({})).toBe('demo-casino')
  })

  it('throws instead of silently defaulting when missing in a production build', () => {
    expect(() => resolveBrandSlug({ PROD: true })).toThrow(/VITE_BRAND_SLUG/)
  })
})
