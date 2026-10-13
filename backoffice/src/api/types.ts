// Shared wire-format types for every admin list endpoint's envelope
// ({items, limit, offset, total} - internal/httpserver/pagination.go) and
// the platform-wide error shape (internal/apierror.Error). This is the
// ONLY layer of the app allowed to know these shapes exist.

export interface PagedResponse<T> {
  items: T[];
  limit: number;
  offset: number;
  total: number;
}

export interface ApiErrorBody {
  code: string;
  message: string;
  request_id?: string;
  /** Set only by the launch authorisation routes: a closed H-/R-/refusal/SQLSTATE vocabulary, never free text. */
  check_codes?: string[];
}

/** ADR 0112 section 9 / OpenAPI Error.code: every LAUNCH_* code the launch routes can answer. */
export const LAUNCH_ERROR_CODES = [
  'LAUNCH_PRECONDITION_FAILED',
  'LAUNCH_UNACKNOWLEDGED_CHECKS',
  'LAUNCH_REQUEST_PENDING',
  'LAUNCH_SELF_APPROVAL',
  'LAUNCH_ALREADY_DECIDED',
  'LAUNCH_ILLEGAL_TRANSITION',
  'LAUNCH_SUBJECT_CLOSED',
  'LAUNCH_WITHDRAWALS_HOLDING_FUNDS',
  'LAUNCH_OPEN_BETS_BLOCK_CLOSURE',
  'LAUNCH_RETRYABLE_LOCK_TIMEOUT',
  'LAUNCH_REQUEST_NOT_PENDING',
  'LAUNCH_STALE_READINESS',
  'LAUNCH_PRIOR_REJECT',
  'LAUNCH_PRIOR_REJECTER',
  'LAUNCH_NOT_LEGACY_BASELINE',
  'LAUNCH_REFUSED_AT_EXECUTION',
  'LAUNCH_GUARD_REFUSED',
  'LAUNCH_PROOF_REFUSED',
  'LAUNCH_NO_PERSON',
  'LAUNCH_AUTHORISATION_UNAVAILABLE',
] as const;
export type LaunchErrorCode = (typeof LAUNCH_ERROR_CODES)[number];

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
  | 'PAYMENT_METHOD_MISMATCH'
  | 'PAYOUT_DESTINATION_NOT_USABLE'
  | 'invalid_request'
  | 'TENANT_CLOSE_BLOCKED_OPEN_ROUNDS'
  // ADR 0114 S6b: a second kill-switch release request while one is open (409).
  | 'KILL_SWITCH_RELEASE_REQUEST_ALREADY_OPEN'
  // ADR 0112 section 9: the closed LAUNCH_* set (internal/apierror).
  | LaunchErrorCode
  // ADR 0119 S3 staff-auth codes (docs/plans/adr0119-s3-api-contract.md section 2).
  | 'mfa_enrolment_required'
  | 'mfa_locked'
  | 'invalid_challenge'
  | 'invalid_mfa_code'
  | 'invalid_enrolment_token'
  | 'mfa_enrolment_expired'
  | 'network_error';

export class ApiError extends Error {
  readonly code: ApiErrorCode;
  readonly status: number;
  readonly requestId?: string;
  /** Closed check codes of a refused launch precondition (ADR 0112); empty otherwise. */
  readonly checkCodes: string[];
  /** Seconds from a `Retry-After` header, when the server sent a valid one. */
  readonly retryAfterSeconds?: number;

  constructor(
    code: ApiErrorCode,
    message: string,
    status: number,
    requestId?: string,
    checkCodes: string[] = [],
    retryAfterSeconds?: number,
  ) {
    super(message);
    this.name = 'ApiError';
    this.code = code;
    this.status = status;
    this.requestId = requestId;
    this.checkCodes = checkCodes;
    this.retryAfterSeconds = retryAfterSeconds;
  }
}
