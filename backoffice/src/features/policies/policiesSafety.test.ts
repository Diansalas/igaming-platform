import { readdirSync, readFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

// Static pins over the lic16 policy screens (a grep, not a proof; the behavioural tests and the server are the guarantees).
const DIR = resolve(process.cwd(), 'src/features/policies')
const files = readdirSync(DIR).filter((f) => /\.(ts|tsx)$/.test(f) && !/\.test\.(ts|tsx)$/.test(f) && f !== 'policyFixtures.ts')
const code = (t: string) => t.split('\n').filter((l) => !/^\s*(\/\/|\*|\/\*)/.test(l)).join('\n')
const all = files.map((f) => ({ f, text: code(readFileSync(join(DIR, f), 'utf8')) }))
const get = (f: string) => all.find((a) => a.f === f)!.text

describe('policy screens source pins', () => {
  it('finds its sources (non-vacuous)', () => {
    expect(all.map((a) => a.f).sort()).toEqual(['KycPoliciesPage.tsx', 'PolicyBits.tsx', 'ResidencePage.tsx', 'WithdrawalRestrictionPages.tsx', 'errors.ts', 'labels.ts'])
  })
  it('renders no server HTML, touches no web storage, calls no fetch, prints no server error message', () => {
    for (const { f, text } of all) {
      expect(text, f).not.toMatch(/dangerouslySetInnerHTML|innerHTML|insertAdjacentHTML/)
      expect(text, f).not.toMatch(/localStorage|sessionStorage|document\.cookie/)
      expect(text, f).not.toMatch(/\bfetch\(/)
      expect(text, f).not.toMatch(/(err|error|e)\.message/)
    }
  })
  it('does no signing or proof handling in the browser', () => {
    for (const { f, text } of all) expect(text, f).not.toMatch(/crypto\.subtle|sha256|hmac|X-Actor-Proof|actorProof|\bsign\(/i)
  })
  it('makes no legal or jurisdiction-approval claim in its wording', () => {
    for (const { f, text } of all) {
      expect(text, f).not.toMatch(/legally|fully licensed|is licensed|compliant|approved for operation|jurisdiction (is )?approved|licence (is )?verified|verified by the platform(?!\.)/i)
    }
  })
  it('never converts a threshold to a number (integer minor units stay strings)', () => {
    for (const { f, text } of all) expect(text, f).not.toMatch(/\bNumber\s*\(|parseInt|parseFloat|BigInt\s*\(|\.toFixed/)
  })
  it('the KYC author form starts empty: no default jurisdiction, trigger, threshold, asset, operation, legal reference or reason', () => {
    const t = get('KycPoliciesPage.tsx')
    for (const re of [
      /\[jur, setJur\] = useState\(''\)/,
      /\[trigger, setTrigger\] = useState<AuthorableTriggerType \| ''>\(''\)/,
      /\[threshold, setThreshold\] = useState\(''\)/,
      /\[asset, setAsset\] = useState\(''\)/,
      /\[op, setOp\] = useState<PlayOperation \| ''>\(''\)/,
      /\[legal, setLegal\] = useState\(''\)/,
      /\[reason, setReason\] = useState\(''\)/,
    ]) expect(t).toMatch(re)
  })
  it('the KYC screen offers no deactivate or switch-off action for an active policy', () => {
    const t = get('KycPoliciesPage.tsx')
    expect(t).not.toMatch(/deactivate|switch off|disable policy|turn off/i)
    expect(t).toMatch(/p\.status === 'active' && <Button[\s\S]*?>Supersede<\/Button>/)
    expect(t).toMatch(/p\.status === 'draft' && <Button[\s\S]*?>Withdraw draft<\/Button>/)
  })
  it('the residence mode choice and the withdrawal-restriction cause / action choices start empty', () => {
    expect(get('ResidencePage.tsx')).toMatch(/\[mode, setMode\] = useState<ModeOrEmpty>\(''\)/)
    const w = get('WithdrawalRestrictionPages.tsx')
    expect(w).toMatch(/\[cause, setCause\] = useState<WrpConfigurableCause \| ''>\(''\)/)
    expect(w).toMatch(/\[action, setAction\] = useState<WrpAction \| ''>\(''\)/)
  })
  it('tenant / brand status and market causes are never offered as a filing choice', () => {
    const w = get('WithdrawalRestrictionPages.tsx')
    expect(w).toMatch(/const CONFIGURABLE: readonly WrpConfigurableCause\[\] = \['licence_suspended', 'licence_expired', 'licence_not_yet_issued', 'licence_missing'\]/)
    expect(w).not.toMatch(/CONFIGURABLE[^\n]*(tenant_|brand_|market_)/)
  })
  it('router paths built from ids are encoded', () => {
    const w = get('WithdrawalRestrictionPages.tsx')
    expect(w).toMatch(/tenantPath = \(id: string\) => `\/withdrawal-restrictions\/tenants\/\$\{encodeURIComponent\(id\)\}`/)
    expect(w).toMatch(/changePath = \(id: string\) => `\/withdrawal-restrictions\/changes\/\$\{encodeURIComponent\(id\)\}`/)
  })
})
