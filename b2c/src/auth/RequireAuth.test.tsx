import { screen } from '@testing-library/react'
import { Route, Routes } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import { setSession } from './tokenStore'
import { makeTestJwt } from '../test/jwt'
import { renderWithProviders } from '../test/renderWithProviders'
import { RequireAuth } from './RequireAuth'

function ProtectedPage() {
  return <div>Protected content</div>
}

function LoginStub() {
  return <div>Login page</div>
}

describe('RequireAuth', () => {
  it('redirects an unauthenticated visitor to /login', async () => {
    renderWithProviders(
      <Routes>
        <Route path="/login" element={<LoginStub />} />
        <Route
          path="/protected"
          element={
            <RequireAuth>
              <ProtectedPage />
            </RequireAuth>
          }
        />
      </Routes>,
      { route: '/protected' },
    )

    expect(await screen.findByText('Login page')).toBeInTheDocument()
    expect(screen.queryByText('Protected content')).not.toBeInTheDocument()
  })

  it('renders the protected content for an authenticated visitor', async () => {
    setSession(makeTestJwt(), 'refresh-token-for-test')

    renderWithProviders(
      <Routes>
        <Route path="/login" element={<LoginStub />} />
        <Route
          path="/protected"
          element={
            <RequireAuth>
              <ProtectedPage />
            </RequireAuth>
          }
        />
      </Routes>,
      { route: '/protected' },
    )

    expect(await screen.findByText('Protected content')).toBeInTheDocument()
  })
})
