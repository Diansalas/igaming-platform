// U-5 static guard (lint-style): production source must not use floating-point idioms that can touch money.
// CLAUDE.md: never floating point for money. Money goes through src/lib/amount.ts (BigInt / string arithmetic).
//
// Banned in every production .ts/.tsx file under src/ (tests and src/test/ excluded), comments ignored:
//   parseFloat(, Number.parseFloat(, Number(, parseInt(, Number.parseInt(, .toFixed(, Math.round/floor/ceil/trunc(,
//   `* 100` / `/ 100`, and `10 **` (float scaling by an exponent).
// The ONLY exceptions are the explicit, audited ALLOW entries below: each names the file, the exact (trimmed) source line
// and the reason it is not an amount. A new use anywhere else fails this test and must either be rewritten with
// lib/amount.ts or added here with a justification that a reviewer can check. Entries may not mention amount-like words.
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join, relative, resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

const SRC = resolve(__dirname, '..')

const BANNED: { name: string; re: RegExp }[] = [
  { name: 'parseFloat', re: /\bparseFloat\s*\(/ },
  { name: 'Number.parseFloat', re: /Number\.parseFloat\s*\(/ },
  { name: 'Number(', re: /\bNumber\s*\(/ },
  { name: 'parseInt', re: /\bparseInt\s*\(|Number\.parseInt\s*\(/ },
  { name: 'toFixed', re: /\.toFixed\s*\(/ },
  { name: 'Math.round/floor/ceil/trunc', re: /\bMath\.(round|floor|ceil|trunc)\s*\(/ },
  { name: '* 100 or / 100', re: /[*/]\s*100\b/ },
  { name: '10 ** exponent', re: /\b10\s*\*\*/ },
]

interface Allow {
  file: string
  line: string
  reason: string
}

// BEGIN-ALLOW (audited non-money uses)
const ALLOW: Allow[] = [
  {
    file: 'features/rg/SelfExclusionCard.tsx',
    line: "const days = choice === '' || choice === INDEFINITE ? null : Number(choice)",
    reason: 'Duration in days chosen from a fixed <select> of integer options; not an amount.',
  },
  {
    file: 'features/rg/TimeOutCard.tsx',
    line: "const selected = hours === '' ? null : Number(hours)",
    reason: 'Duration in hours chosen from a fixed <select> of integer options; not an amount.',
  },
  {
    file: 'lib/amount.ts',
    line: 'return Number(value) // exact: bounded to the safe-integer range just above',
    reason: 'toWireInteger: converts a bigint already proven to be within +/-(2^53-1) to the JSON number the int64 DTO needs; lossless.',
  },
  {
    file: 'lib/odds.ts',
    line: 'return decimal.toFixed(2)',
    reason: 'Display of a decimal ODDS ratio (price), never a monetary amount and never sent back to the server.',
  },
]
// END-ALLOW

function walk(dir: string, out: string[] = []): string[] {
  for (const name of readdirSync(dir)) {
    const p = join(dir, name)
    if (statSync(p).isDirectory()) {
      if (name === 'test' || name === 'node_modules') continue
      walk(p, out)
    } else if (/\.(ts|tsx)$/.test(name) && !/\.test\.(ts|tsx)$/.test(name) && !/\.d\.ts$/.test(name)) {
      out.push(p)
    }
  }
  return out
}

function code(line: string): string {
  const t = line.trim()
  if (t.startsWith('//') || t.startsWith('*') || t.startsWith('/*')) return ''
  return line.replace(/\s\/\/.*$/, '')
}

function scan(): { file: string; line: string; rule: string }[] {
  const found: { file: string; line: string; rule: string }[] = []
  for (const f of walk(SRC)) {
    const rel = relative(SRC, f).split('\\').join('/')
    for (const raw of readFileSync(f, 'utf8').split('\n')) {
      const c = code(raw)
      if (!c.trim()) continue
      for (const b of BANNED) if (b.re.test(c)) found.push({ file: rel, line: raw.trim(), rule: b.name })
    }
  }
  return found
}

describe('no floating-point money idioms in production source (U-5)', () => {
  it('finds nothing outside the audited allow-list', () => {
    const unlisted = scan().filter((f) => !ALLOW.some((a) => a.file === f.file && a.line === f.line))
    expect(unlisted.map((f) => `${f.file}: [${f.rule}] ${f.line}`)).toEqual([])
  })

  it('has no stale allow-list entries (every entry still matches a real line)', () => {
    const found = scan()
    const stale = ALLOW.filter((a) => !found.some((f) => f.file === a.file && f.line === a.line))
    expect(stale.map((a) => `${a.file}: ${a.line}`)).toEqual([])
  })

  it('allow-list entries never concern amounts and always carry a reason', () => {
    const amountLike = /(amount|balance|stake|payout|wager|price|money|minor|cash|ceiling|limit_value|\bwin\b|funds|fee)/i
    for (const a of ALLOW) {
      expect(a.reason.length, a.file).toBeGreaterThan(10)
      expect(amountLike.test(a.line), `${a.file}: ${a.line}`).toBe(false)
    }
  })

  it('the scanner itself detects each banned pattern (self-test)', () => {
    const sample: Record<string, string> = {
      parseFloat: 'const x = parseFloat(amount)',
      'Number(': 'const x = Number(total)',
      parseInt: 'const x = parseInt(s, 10)',
      toFixed: 'v.toFixed(2)',
      'Math.round/floor/ceil/trunc': 'Math.round(v)',
      '* 100 or / 100': 'const c = a * 100',
      '10 ** exponent': 'const d = 10 ** e',
    }
    for (const [name, text] of Object.entries(sample)) {
      const hit = BANNED.some((b) => b.name === name && b.re.test(code(text)))
      expect(hit, name).toBe(true)
    }
    expect(code('// Number(x) in a comment')).toBe('')
    expect(code(' * Number(x) in a block comment')).toBe('')
  })
})
