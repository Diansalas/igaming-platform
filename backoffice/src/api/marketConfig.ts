// Market configuration client (lic31). Wire shapes are copied from docs/api/openapi/platform-api.yaml
// (LicensingChangeCreateRequest, LicensingChangeResponse, Jurisdiction) and internal/licensingchange/kinds.go. Facts relied on:
//  - licence country ceilings, operating-country policies and tenant jurisdiction configurations are written ONLY by filing a
//    licensing change through POST /v1/admin/licensing-changes (kinds licence_ceiling_version, operating_policy_version,
//    tenant_jurisdiction_config_version). The route answers 201 with {change_request, executed}: executed:false is PENDING
//    APPROVAL (a widening change needs two other people), executed:true is a single-actor narrowing change applied in the same
//    call. apiFetch returns the body only, so classification reads the BODY (classifyBasisFiling), and anything it cannot
//    recognise is "unconfirmed", never success;
//  - the DATABASE classifies each change as widening or narrowing; the UI only hints and never chooses;
//  - filing is step-up `critical` (the apiFetch interceptor prompts and re-sends the same body once). The signed actor proof
//    (ADR 0110) is produced server-side: nothing here signs, hashes or sends a proof;
//  - every payload key of a kind is always sent (null when empty), unknown keys are refused by the server;
//  - POST /v1/admin/jurisdictions creates a registry row directly: it is NOT a licensing change and NOT four-eyes gated (the
//    OpenAPI says so). It is platform_admin only, step-up critical, needs a reason code and writes an audit entry;
//  - an authorization reference is an opaque string supplied by the operator and is NOT verified by the platform.
import { apiFetch } from './client'
import type { Jurisdiction } from './licensing'

export type CeilingState = 'enabled' | 'disabled'
export type VersionStatus = 'active' | 'withdrawn'

export interface CeilingPayload {
  licence_id: string
  country_code: string
  state: CeilingState
  status: VersionStatus
  authorization_reference: string | null
}

export type PolicyScope = 'tenant' | 'brand' | 'operation'

export interface PolicyPayload {
  tenant_id: string
  scope: PolicyScope
  brand_id: string | null
  operation_code: string | null
  product_code: string | null
  country_code: string
  state: CeilingState
  status: VersionStatus
  authorization_reference: string | null
}

export type ConfigAction = 'set' | 'close'

/** For `close` every content field is null; for `set` the three lists are arrays (possibly empty) and rulesets are string | null. */
export interface TenantConfigPayload {
  tenant_id: string
  jurisdiction_id: string
  action: ConfigAction
  kyc_ruleset_id: string | null
  aml_ruleset_id: string | null
  rg_ruleset_id: string | null
  reporting_ruleset_id: string | null
  allowed_currencies: string[] | null
  allowed_payment_methods: string[] | null
  geo_block_list: string[] | null
}

export type MarketChangeKind = 'licence_ceiling_version' | 'operating_policy_version' | 'tenant_jurisdiction_config_version'

export type FileMarketChangeBody =
  | { kind: 'licence_ceiling_version'; payload: CeilingPayload; reason_code: string }
  | { kind: 'operating_policy_version'; payload: PolicyPayload; reason_code: string }
  | { kind: 'tenant_jurisdiction_config_version'; payload: TenantConfigPayload; reason_code: string }

export function fileMarketChange(body: FileMarketChangeBody): Promise<unknown> {
  return apiFetch('/v1/admin/licensing-changes', {
    method: 'POST',
    body: JSON.stringify({ kind: body.kind, payload: body.payload, reason_code: body.reason_code }),
  })
}

export interface CreateJurisdictionBody {
  code: string
  name: string
  regulatory_body?: string
  notes?: string
  reason_code: string
}

/** Direct registry write (201 + the Jurisdiction). Optional text keys are present only when the operator typed something. */
export function createJurisdiction(body: CreateJurisdictionBody): Promise<unknown> {
  return apiFetch('/v1/admin/jurisdictions', { method: 'POST', body: JSON.stringify(body) })
}

export type JurisdictionCreateResult = { kind: 'created'; jurisdiction: Jurisdiction } | { kind: 'unconfirmed' }

const UUID = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/

/** A 201 is "created" only when the body is a Jurisdiction (uuid id and a code). Anything else is not a confirmation. */
export function classifyJurisdictionCreate(body: unknown): JurisdictionCreateResult {
  if (typeof body === 'object' && body !== null) {
    const b = body as Record<string, unknown>
    if (typeof b.id === 'string' && UUID.test(b.id) && typeof b.code === 'string' && b.code !== '') {
      return { kind: 'created', jurisdiction: body as Jurisdiction }
    }
  }
  return { kind: 'unconfirmed' }
}
