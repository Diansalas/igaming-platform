import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { server } from '../test/mswServer'
import { renderWithProviders } from '../test/renderWithProviders'
import { LoginPage } from './LoginPage'

describe('LoginPage', () => {
  it('logs in successfully and stores a session', async () => {
    renderWithProviders(<LoginPage />, { route: '/login' })
    const user = userEvent.setup()

    await user.type(screen.getByLabelText('Email'), 'player@example.com')
    await user.type(screen.getByLabelText('Password'), 'correct-password')
    await user.click(screen.getByRole('button', { name: 'Sign in' }))

    // On success, LoginPage's own isAuthenticated check redirects away
    // from the form - the clearest observable proof from a component
    // test that a session was established.
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Sign in' })).not.toBeInTheDocument())
  })

  it('shows a real server error for invalid credentials, never assuming success', async () => {
    renderWithProviders(<LoginPage />, { route: '/login' })
    const user = userEvent.setup()

    await user.type(screen.getByLabelText('Email'), 'player@example.com')
    await user.type(screen.getByLabelText('Password'), 'wrong-password')
    await user.click(screen.getByRole('button', { name: 'Sign in' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('Invalid email or password.')
    // Still on the login form - not redirected.
    expect(screen.getByRole('button', { name: 'Sign in' })).toBeInTheDocument()
  })

  it('shows a validation error message returned by the server', async () => {
    server.use(
      http.post('/v1/auth/login', () =>
        HttpResponse.json({ code: 'validation_error', message: 'brand_slug is required' }, { status: 400 }),
      ),
    )
    renderWithProviders(<LoginPage />, { route: '/login' })
    const user = userEvent.setup()

    await user.type(screen.getByLabelText('Email'), 'player@example.com')
    await user.type(screen.getByLabelText('Password'), 'whatever')
    await user.click(screen.getByRole('button', { name: 'Sign in' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('brand_slug is required')
  })
})
