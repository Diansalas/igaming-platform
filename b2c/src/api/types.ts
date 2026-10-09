// Shared wire-format types for the Platform API's pagination envelope
// ({items, limit, offset, total} - internal/httpserver/pagination.go) and
// its platform-wide error shape (internal/apierror.Error). This is the
// ONLY layer of the app allowed to know these shapes exist - mirrors
// backoffice/src/api/types.ts exactly, since both apps talk to the same
// Platform API.

export interface PagedResponse<T> {
  items: T[]
  limit: number
  offset: number
  total: number
}

export interface ApiErrorBody {
  code: string
  message: string
  request_id?: string
}

/** Stable, machine-readable error codes the API returns (apierror.Code). */
export type ApiErrorCode =
  | 'validation_error'
  | 'unauthorized'
  | 'forbidden'
  | 'not_found'
  | 'conflict'
  | 'internal_error'
  | 'service_unavailable'
  | 'tenant_mismatch'
  | 'rate_limited'
  | 'network_error'
  // B13 payout instrument codes (409 unless noted); these are upper-case on the wire.
  | 'PAYOUT_INSTRUMENT_REQUIRED'
  | 'PAYOUT_INSTRUMENT_NOT_USABLE'
  | 'TENANT_OR_BRAND_NOT_ACTIVE'

export class ApiError extends Error {
  readonly code: ApiErrorCode
  readonly status: number
  readonly requestId?: string

  constructor(code: ApiErrorCode, message: string, status: number, requestId?: string) {
    super(message)
    this.name = 'ApiError'
    this.code = code
    this.status = status
    this.requestId = requestId
  }
}
