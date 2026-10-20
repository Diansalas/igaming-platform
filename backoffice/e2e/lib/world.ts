import { readFileSync, writeFileSync } from 'node:fs'
import { nextCode } from './totp'

export interface Account {
  key: string
  email: string
  password: string
  tenantSlug: string // '' for platform staff
  role: string
  totpSecret?: string
  lastStep: number
}
export interface World {
  baseUrl: string
  apiUrl: string
  tenantSlug: string
  jurisdictionCode: string
  syntheticLicenceNumber: string
  accounts: Record<string, Account>
}

const file = () => {
  const f = process.env.E2E_WORLD_FILE
  if (!f) throw new Error('E2E_WORLD_FILE is not set (run through e2e/run.sh)')
  return f
}
export const loadWorld = (): World => JSON.parse(readFileSync(file(), 'utf8'))
export function account(key: string): Account {
  const a = loadWorld().accounts[key]
  if (!a) throw new Error(`no e2e account ${key}`)
  return a
}

/** Next TOTP code for the account; the used step is persisted so no two logins/step-ups replay a step. */
export async function totpFor(key: string): Promise<string> {
  const w = loadWorld()
  const a = w.accounts[key]
  if (!a?.totpSecret) throw new Error(`account ${key} is not enrolled`)
  const { code, step } = await nextCode(a.totpSecret, a.lastStep)
  a.lastStep = step
  writeFileSync(file(), JSON.stringify(w, null, 2), { mode: 0o600 })
  return code
}
