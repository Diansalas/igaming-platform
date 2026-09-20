import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { renderWithProviders } from '../test/renderWithProviders'
import { RegisterPage } from './RegisterPage'

describe('RegisterPage', () => {
  it('registers successfully and stores a session', async () => {
    renderWithProviders(<RegisterPage />, { route: '/register' })
    const user = userEvent.setup()

    await user.type(screen.getByLabelText('Email'), 'new-player@example.com')
    await user.type(screen.getByLabelText('Password'), 'a-strong-password')
    await user.click(screen.getByRole('button', { name: 'Create account' }))

    await waitFor(() => expect(screen.queryByRole('button', { name: 'Create account' })).not.toBeInTheDocument())
  })

  it('shows the real server error when the email is already taken', async () => {
    renderWithProviders(<RegisterPage />, { route: '/register' })
    const user = userEvent.setup()

    await user.type(screen.getByLabelText('Email'), 'taken@example.com')
    await user.type(screen.getByLabelText('Password'), 'a-strong-password')
    await user.click(screen.getByRole('button', { name: 'Create account' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('An account with this email already exists.')
    expect(screen.getByRole('button', { name: 'Create account' })).toBeInTheDocument()
  })
})
