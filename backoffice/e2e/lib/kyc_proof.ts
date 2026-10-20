// lic21: an ADR 0110 actor-proof signer for the BELOW-THE-APPLICATION-LAYER probes only. It signs with the harness's own
// throwaway key (E2E_ACTOR_PROOF_HEX, generated per run by setup/genkeys.mjs and inserted into the private database by
// setup/seed.mjs) in the exact wire format migration 0185's actor_proof_require verifies:
//   v1|kid|actor|scope|tenant|operation|target|payload_hash|iat|exp|nonce|hex(hmac_sha256(first eleven fields joined by '|'))
// It exists so a probe can present a VALID proof and show that the database still refuses on the person rules, and so a probe can
// present a deliberately WRONG proof and show which verifier code refuses it. It is test code: it is not a second production signer
// (the Go signer-owner static pin is untouched; this file is outside every Go package).
import { createHash, createHmac, randomBytes } from 'node:crypto'

export interface ProofClaims {
  actor: string
  scope: 'tenant' | 'platform'
  tenant: string // '' for platform
  operation: string
  target: string
  payloadHash: string
}

/** k2_sha256_hex(k2_canonical(fields...)): each field "<octet length>:<value>", NULL as "~", joined by ",". */
export function digest(fields: (string | null)[]): string {
  const parts = fields.map((f) => (f === null ? '~' : `${Buffer.byteLength(f, 'utf8')}:${f}`))
  return createHash('sha256').update(parts.join(','), 'utf8').digest('hex')
}

export interface SignOpts {
  /** Issue time offset in seconds (negative => already expired when presented). Default 0. */
  iatOffset?: number
  /** Replace the MAC with this (wrong) value. */
  badMac?: boolean
  /** Use a key id that is not registered. */
  kid?: string
}

export function signProof(c: ProofClaims, o: SignOpts = {}): string {
  const hex = process.env.E2E_ACTOR_PROOF_HEX
  const kid = o.kid ?? process.env.ACTOR_PROOF_ACTIVE_KID
  if (!hex || !/^[0-9a-f]{64}$/.test(hex) || !kid) throw new Error('actor-proof key material is not in the environment (run through e2e/run.sh)')
  const now = Math.floor(Date.now() / 1000) + (o.iatOffset ?? 0)
  const nonce = randomBytes(18).toString('base64url')
  const signed = ['v1', kid, c.actor, c.scope, c.tenant, c.operation, c.target, c.payloadHash, String(now), String(now + 30), nonce].join('|')
  const mac = o.badMac ? '0'.repeat(64) : createHmac('sha256', Buffer.from(hex, 'hex')).update(signed, 'utf8').digest('hex')
  return `${signed}|${mac}`
}

/** The proof the residence service attaches for `residence_determination_approve` (see internal/residenceapproval). */
export function determinationApproveProof(a: { tenant: string; actor: string; verification: string; determinedAtMicro: string; reason: string }, o: SignOpts = {}, over: Partial<ProofClaims> = {}): string {
  const hash = digest([a.tenant, a.verification, a.determinedAtMicro, a.reason])
  return signProof({ actor: a.actor, scope: 'tenant', tenant: a.tenant, operation: 'residence_determination:approve', target: a.verification, payloadHash: hash, ...over }, o)
}
