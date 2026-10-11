import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { LoginPage } from '../auth/LoginPage'
import { RegisterPage } from '../auth/RegisterPage'
import { ApiError } from '../api/types'
import { server } from '../test/mswServer'
import { renderWithProviders } from '../test/renderWithProviders'
import { SITE_UNAVAILABLE_MESSAGE, isSiteUnavailableError } from './SiteUnavailable'

const LEGAL_OR_LEAKY = /licen|launch|pending|suspend|closed|regulat|legal|exist|unknown brand/i

async function submit(buttonName: string) {
  const user = userEvent.setup()
  await user.type(screen.getByLabelText('Email'), 'player@example.com')
  await user.type(screen.getByLabelText('Password'), 'a-strong-password')
  await user.click(screen.getByRole('button', { name: buttonName }))
}

describe('neutral site-unavailable state (ADR 0112 7.3)', () => {
  it('classifies only the not-launched 404 and the not-accepting 409', () => {
    expect(isSiteUnavailableError(new ApiError('not_found', 'unknown brand', 404))).toBe(true)
    expect(isSiteUnavailableError(new ApiError('BRAND_NOT_ACCEPTING_REGISTRATIONS', 'x', 409))).toBe(true)
    expect(isSiteUnavailableError(new ApiError('conflict', 'duplicate', 409))).toBe(false)
    expect(isSiteUnavailableError(new ApiError('unauthorized', 'bad', 401))).toBe(false)
    expect(isSiteUnavailableError(new Error('boom'))).toBe(false)
  })

  it('login 404 shows the neutral message with no brand-existence or legal wording', async () => {
    server.use(http.post('/v1/auth/login', () => HttpResponse.json({ code: 'not_found', message: 'unknown brand' }, { status: 404 })))
    renderWithProviders(<LoginPage />, { route: '/login' })
    await submit('Sign in')
    const el = await screen.findByRole('status')
    expect(el).toHaveTextContent(SITE_UNAVAILABLE_MESSAGE)
    expect(document.body.textContent).not.toMatch(LEGAL_OR_LEAKY)
    expect(screen.queryByRole('button', { name: 'Sign in' })).not.toBeInTheDocument()
  })

  it('register 404 and register 409 BRAND_NOT_ACCEPTING_REGISTRATIONS show the same neutral message', async () => {
    server.use(http.post('/v1/auth/register', () => HttpResponse.json({ code: 'not_found', message: 'unknown brand' }, { status: 404 })))
    const first = renderWithProviders(<RegisterPage />, { route: '/register' })
    await submit('Create account')
    expect(await screen.findByRole('status')).toHaveTextContent(SITE_UNAVAILABLE_MESSAGE)
    first.unmount()

    server.use(
      http.post('/v1/auth/register', () =>
        HttpResponse.json({ code: 'BRAND_NOT_ACCEPTING_REGISTRATIONS', message: 'brand not accepting registrations' }, { status: 409 }),
      ),
    )
    renderWithProviders(<RegisterPage />, { route: '/register' })
    await submit('Create account')
    const el = await screen.findByRole('status')
    expect(el).toHaveTextContent(SITE_UNAVAILABLE_MESSAGE)
    expect(document.body.textContent).not.toMatch(LEGAL_OR_LEAKY)
  })

  it('a duplicate-email 409 conflict is NOT treated as unavailable', async () => {
    renderWithProviders(<RegisterPage />, { route: '/register' })
    const user = userEvent.setup()
    await user.type(screen.getByLabelText('Email'), 'taken@example.com')
    await user.type(screen.getByLabelText('Password'), 'a-strong-password')
    await user.click(screen.getByRole('button', { name: 'Create account' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('already exists')
    expect(screen.queryByText(SITE_UNAVAILABLE_MESSAGE)).not.toBeInTheDocument()
  })
})
