// Generates run-time-only secrets for the e2e stack. Prints KEY=VALUE lines for a 0600 env file. Nothing is committed.
import { randomBytes } from 'node:crypto'
const b64 = (n) => randomBytes(n).toString('base64')
const actor = randomBytes(32)
const out = {
  JWT_SIGNING_SECRET: randomBytes(32).toString('hex'),
  STAFF_MFA_FACTOR_KEYS: `e2ef1:${b64(32)}`,
  STAFF_MFA_FACTOR_ACTIVE_KID: 'e2ef1',
  STAFF_MFA_MAC_KEYS: `e2em1:${b64(32)}`,
  STAFF_MFA_MAC_ACTIVE_KID: 'e2em1',
  ACTOR_PROOF_KEYS: `e2ea1:${actor.toString('base64')}`,
  ACTOR_PROOF_ACTIVE_KID: 'e2ea1',
  E2E_ACTOR_PROOF_HEX: actor.toString('hex'),
}
for (const [k, v] of Object.entries(out)) console.log(`${k}=${v}`)
