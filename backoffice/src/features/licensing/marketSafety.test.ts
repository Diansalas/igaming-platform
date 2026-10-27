import { readFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

// Static pins over the lic31 market-configuration sources (a grep, not a proof; the behavioural tests and the server are the guarantees).
const DIR = resolve(process.cwd(), 'src/features/licensing')
const FILES = ['MarketDialogs.tsx', 'MarketBits.tsx', 'marketRules.ts', 'marketLabels.ts']
const code = (t: string) => t.split('\n').filter((l) => !/^\s*(\/\/|\*|\/\*)/.test(l)).join('\n')
const text = Object.fromEntries(FILES.map((f) => [f, code(readFileSync(join(DIR, f), 'utf8'))]))

describe('market configuration source pins (lic31)', () => {
  it('scans real sources (non-vacuous)', () => {
    for (const f of FILES) expect(text[f].length, f).toBeGreaterThan(500)
  })
  it('no float math, no HTML injection, no storage, no raw fetch, no server message', () => {
    for (const f of FILES) {
      expect(text[f], f).not.toMatch(/\bNumber\s*\(|parseInt|parseFloat|\.toFixed|Math\.(round|floor|ceil)/)
      expect(text[f], f).not.toMatch(/dangerouslySetInnerHTML|innerHTML|insertAdjacentHTML|localStorage|sessionStorage|document\.cookie|\bfetch\(/)
      expect(text[f], f).not.toMatch(/(err|error|e)\.message/)
      expect(text[f], f).not.toMatch(/crypto\.subtle|sha256|hmac|X-Actor-Proof|actorProof|\bsign\(/i)
    }
  })
  it('claims nothing legal and approves no jurisdiction', () => {
    for (const f of FILES) expect(text[f], f).not.toMatch(/legally|fully licensed|is licensed|compliant|approved for operation|jurisdiction (is )?approved|licence (is )?verified/i)
  })
  it('every dialog choice starts empty (nothing preselected, no default country, rung, action or jurisdiction)', () => {
    const t = text['MarketDialogs.tsx']
    for (const re of [
      /\[action, setAction\] = useState<MarketAction \| ''>\(''\)/,
      /\[reason, setReason\] = useState\(''\)/,
      /\[ref, setRef\] = useState\(''\)/,
      /\[action, setAction\] = useState<ConfigAction \| ''>\(initialAction \?\? ''\)/,
      /\[brandScope, setBrandScope\] = useState<'' \| 'tenant' \| 'brand'>\(initial \?/,
      /\[code, setCode\] = useState\(''\)/,
    ]) expect(t).toMatch(re)
    expect(t).not.toMatch(/useState\(['"][A-Z]{2}['"]\)/)
  })
  it('ids in paths are encoded in links', () => {
    for (const f of ['MarketBits.tsx', 'MarketDialogs.tsx']) expect(text[f].match(/to=\{`[^`]*\$\{(?!encodeURIComponent)[^}]*\}/g) ?? [], f).toEqual([])
  })
})
