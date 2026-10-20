// Enrols a TOTP factor for every seeded account through the REAL HTTP flow (bootstrap token -> redeem -> start -> confirm).
// The secret the server returns is kept in the 0600 world file for the specs. Usage: node enrol.mjs <worldFile>
import { readFileSync, writeFileSync } from 'node:fs'
import { createHmac } from 'node:crypto'

const file = process.argv[2]
const w = JSON.parse(readFileSync(file, 'utf8'))
const ALPHA = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'
function b32(s) { let bits = 0, v = 0; const o = []; for (const c of s.replace(/=+$/, '')) { v = (v << 5) | ALPHA.indexOf(c); bits += 5; if (bits >= 8) { o.push((v >>> (bits - 8)) & 255); bits -= 8 } } return Buffer.from(o) }
function code(secret, step) { const m = Buffer.alloc(8); m.writeBigUInt64BE(BigInt(step)); const h = createHmac('sha1', b32(secret)).update(m).digest(); const o = h[h.length - 1] & 15; return String((((h[o] & 127) << 24) | (h[o + 1] << 16) | (h[o + 2] << 8) | h[o + 3]) % 1e6).padStart(6, '0') }
async function post(path, body) {
  const r = await fetch(w.apiUrl + path, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) })
  const text = await r.text()
  if (!r.ok) throw new Error(`${path} -> ${r.status} ${text.slice(0, 200)}`)
  return JSON.parse(text)
}
for (const a of Object.values(w.accounts)) {
  const redeem = { token: a.bootstrapToken, email: a.email, password: a.password }
  if (a.tenantSlug) redeem.tenant_slug = a.tenantSlug
  const ch = (await post('/v1/staff/auth/enrol/redeem', redeem)).challenge
  const start = await post('/v1/staff/auth/enrol/start', { challenge_token: ch.token })
  const step = Math.floor(Date.now() / 30000)
  await post('/v1/staff/auth/enrol/confirm', { challenge_token: ch.token, factor_id: start.factor_id, code: code(start.secret_base32, step) })
  a.totpSecret = start.secret_base32
  a.lastStep = step
  delete a.bootstrapToken
  console.log(`enrolled ${a.key}`)
}
writeFileSync(file, JSON.stringify(w, null, 2), { mode: 0o600 })
