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

    await user.type(screen.getByLabelText('Email'), 'admin@example.com')
    await user.type(screen.getByLabelText('Password'), 'correct-password')
    await user.click(screen.getByRole('button', { name: 'Sign in' }))

    // On success, LoginPage's own isAuthenticated check redirects away
    // from the form - the clearest observable proof from a component
    // test that a session was established.
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Sign in' })).not.toBeInTheDocument())
  })

  it('shows an error message for invalid credentials', async () => {
    renderWithProviders(<LoginPage />, { route: '/login' })
    const user = userEvent.setup()

    await user.type(screen.getByLabelText('Email'), 'admin@example.com')
    await user.type(screen.getByLabelText('Password'), 'wrong-password')
    await user.click(screen.getByRole('button', { name: 'Sign in' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('Invalid email or password.')
  })

  it('shows a lockout message when the API rate-limits the attempt', async () => {
    server.use(
      http.post('/v1/staff/auth/login', () =>
        HttpResponse.json({ code: 'rate_limited', message: 'too many failed attempts' }, { status: 429 }),
      ),
    )
    renderWithProviders(<LoginPage />, { route: '/login' })
    const user = userEvent.setup()

    await user.type(screen.getByLabelText('Email'), 'admin@example.com')
    await user.type(screen.getByLabelText('Password'), 'whatever')
    await user.click(screen.getByRole('button', { name: 'Sign in' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('Too many failed attempts. Try again later.')
  })
})
