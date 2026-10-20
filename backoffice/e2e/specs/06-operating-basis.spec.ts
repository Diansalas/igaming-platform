// Operating-basis screens are built in parallel (lic9 follow-up). Every test here is test.fixme unless the app registers an
// operating-basis route; the selectors below are PLACEHOLDERS to be completed after that screen merges.
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { test } from 'playwright/test'

const routes = readFileSync(resolve(process.cwd(), '../src/app/AppRoutes.tsx'), 'utf8')
const route = /path="((?:licensing\/)?operating-?bas[^"]*)"/i.exec(routes)?.[1]

test.describe('operating basis (real stack)', () => {
  test.fixme(!route, 'No operating-basis route in src/app/AppRoutes.tsx yet; complete after the screen merges.')

  test('read page lists operating bases for the synthetic tenant', async () => {
    // TODO(after merge): loginAs PA_D; spaGoto(`/${route}`); expect the list/table for tenant e2e-tenant; no basis shown as valid
    // unless one was set; absent basis shown as "relies on no basis", never as a fallback.
  })
  test('set operating basis is filed as widening: pending approval, two distinct approvers execute', async () => {
    // TODO(after merge): file `operating_basis_set` (platform licence basis for e2e-tenant) as PA_A; PA_B then PA_C approve (see
    // 03-licence-lifecycle-approval.spec.ts); assert the new basis version row and R-OPERATING-BASIS readiness text.
  })
  test('withdraw operating basis is narrowing: executes at once and the scope shows no basis', async () => {
    // TODO(after merge): file `operating_basis_withdraw`; assert applied at once.
  })
  test('tenant staff cannot see or call the operating-basis screens', async () => {
    // TODO(after merge): loginAs T_TENANT_ADMIN; nav hidden; server 403 on the read API.
  })
})
