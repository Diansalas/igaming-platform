import { describe, expect, it } from 'vitest'
import { ApiError } from '../api/types'
import { describeStaffAuthError, normaliseRecoveryCode, normaliseTotp } from './staffAuthErrors'

describe('describeStaffAuthError', () => {
  it('never returns the server message text', () => {
    const f = describeStaffAuthError(new ApiError('invalid_mfa_code', 'oracle: code was close', 401), 'mfa')
    expect(f.message).not.toContain('oracle')
  })
  it('unknown code and non-ApiError collapse to one generic message', () => {
    const a = describeStaffAuthError(new ApiError('conflict', 'x', 409), 'mfa')
    const b = describeStaffAuthError(new Error('boom'), 'mfa')
    expect(a.message).toBe(b.message)
  })
  it('a bare 401 outside the password step is an expired session, not "wrong password"', () => {
    expect(describeStaffAuthError(new ApiError('unauthorized', 'x', 401), 'mfa')).toMatchObject({ effect: 'restart' })
    expect(describeStaffAuthError(new ApiError('unauthorized', 'x', 401), 'login').message).toContain('incorrect')
  })
  it('classifies effects', () => {
    expect(describeStaffAuthError(new ApiError('invalid_challenge', 'x', 401), 'mfa').effect).toBe('restart')
    expect(describeStaffAuthError(new ApiError('mfa_enrolment_expired', 'x', 409), 'enrol').effect).toBe('restart_enrolment')
    expect(describeStaffAuthError(new ApiError('invalid_mfa_code', 'x', 401), 'mfa').effect).toBe('stay')
    expect(describeStaffAuthError(new ApiError('service_unavailable', 'x', 503), 'mfa').effect).toBe('stay')
  })
  it('rate limit without Retry-After uses the fixed wording', () => {
    expect(describeStaffAuthError(new ApiError('rate_limited', 'x', 429), 'login').message).toBe(
      'Too many attempts. Wait a minute.',
    )
  })
})

describe('code normalisation', () => {
  it('totp strips spaces and dashes and demands six digits', () => {
    expect(normaliseTotp(' 123 456 ')).toBe('123456')
    expect(normaliseTotp('123-456')).toBe('123456')
    expect(normaliseTotp('12345')).toBeNull()
    expect(normaliseTotp('1234567')).toBeNull()
    expect(normaliseTotp('12345a')).toBeNull()
  })
  it('recovery code uppercases and re-dashes', () => {
    expect(normaliseRecoveryCode('abcde12345')).toBe('ABCDE-12345')
    expect(normaliseRecoveryCode(' abcde-12345 ')).toBe('ABCDE-12345')
    expect(normaliseRecoveryCode('abc')).toBeNull()
    expect(normaliseRecoveryCode('abcde-1234!')).toBeNull()
  })
})
