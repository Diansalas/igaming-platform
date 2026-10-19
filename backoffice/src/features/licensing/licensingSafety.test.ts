import { readdirSync, readFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

// Static pins over the licensing screens (a grep, not a proof; the behavioural tests and the server are the guarantees).
const DIR = resolve(process.cwd(), 'src/features/licensing')
const files = readdirSync(DIR).filter((f) => /\.(ts|tsx)$/.test(f) && !/\.test\.(ts|tsx)$/.test(f) && f !== 'licensingFixtures.ts')
const code = (t: string) => t.split('\n').filter((l) => !/^\s*(\/\/|\*|\/\*)/.test(l)).join('\n')
const all = files.map((f) => ({ f, text: code(readFileSync(join(DIR, f), 'utf8')) }))

describe('licensing source pins', () => {
  it('finds its sources (non-vacuous)', () => {
    expect(all.length).toBeGreaterThan(8)
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
  it('preselects no licensing model, licence, jurisdiction or licensee (no defaults for those state fields)', () => {
    const dlg = all.find((a) => a.f === 'FileChangeDialog.tsx')!.text
    expect(dlg).toMatch(/useState\(licence\?\.id \?\? ''\)/)
    expect(dlg).toMatch(/useState\(''\)/)
    expect(dlg).toMatch(/const \[licensee, setLicensee\] = useState<Licensee \| ''>\(''\)/)
    expect(dlg).toMatch(/const \[status, setStatus\] = useState<LicenceStatus \| ''>\(''\)/)
    expect(dlg).toMatch(/const \[jurisdictionId, setJurisdictionId\] = useState\(''\)/)
  })
})
