import { createHmac } from 'node:crypto'

const ALPHA = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'
export function base32Decode(s: string): Buffer {
  let bits = 0
  let val = 0
  const out: number[] = []
  for (const ch of s.replace(/=+$/, '').toUpperCase()) {
    const i = ALPHA.indexOf(ch)
    if (i < 0) throw new Error('bad base32')
    val = (val << 5) | i
    bits += 5
    if (bits >= 8) {
      out.push((val >>> (bits - 8)) & 0xff)
      bits -= 8
    }
  }
  return Buffer.from(out)
}

export function codeAtStep(secretB32: string, step: number): string {
  const msg = Buffer.alloc(8)
  msg.writeBigUInt64BE(BigInt(step))
  const h = createHmac('sha1', base32Decode(secretB32)).update(msg).digest()
  const o = h[h.length - 1] & 0x0f
  const n = ((h[o] & 0x7f) << 24) | (h[o + 1] << 16) | (h[o + 2] << 8) | h[o + 3]
  return String(n % 1_000_000).padStart(6, '0')
}

/**
 * The server accepts steps T-1..T+1 and refuses a step not strictly after the last one used (replay rule). Returns the
 * earliest acceptable code and the step used; waits for the next 30 s step when the window is exhausted.
 */
export async function nextCode(secretB32: string, lastStep: number): Promise<{ code: string; step: number }> {
  for (;;) {
    const cur = Math.floor(Date.now() / 30000)
    const step = Math.max(lastStep + 1, cur - 1)
    if (step <= cur + 1) return { code: codeAtStep(secretB32, step), step }
    await new Promise((r) => setTimeout(r, (cur + 1) * 30000 - Date.now() + 100))
  }
}
