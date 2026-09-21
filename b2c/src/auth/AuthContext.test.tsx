import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { Route, Routes } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import { AccountPage } from '../features/account/AccountPage'
import { makeTestJwt } from '../test/jwt'
import { server } from '../test/mswServer'
import { renderWithProviders } from '../test/renderWithProviders'
import { RequireAuth } from './RequireAuth'
import { getAccessToken, getRefreshToken, setSession } from './tokenStore'

function LoginStub() {
  return <div>Login page</div>
}

function renderAccount() {
  return renderWithProviders(
    <Routes>
      <Route path="/login" element={<LoginStub />} />
      <Route
        path="/account"
        element={
          <RequireAuth>
            <AccountPage />
          </RequireAuth>
        }
      />
    </Routes>,
    { route: '/account' },
  )
}

describe('session expiry mid-session', () => {
  it('attempts a silent refresh on a 401, and cleanly redirects to /login (no loop, no stuck screen) when the refresh token is also revoked', async () => {
    setSession(makeTestJwt(), 'refresh-token-for-test')

    let refreshCalls = 0
    server.use(
      // The bootstrap-on-load refresh (AuthContext's own effect) succeeds
      // once, establishing a normal session - modeling "already logged in
      // for a while". The SECOND refresh, triggered reactively by
      // apiFetch's own 401 handling once a real API call's access token
      // has expired, is the one that is revoked - this is what a player
      // whose session was later revoked (or whose refresh token expired)
      // actually experiences mid-session, not at page load.
      http.post('/v1/auth/refresh', () => {
        refreshCalls += 1
        if (refreshCalls === 1) {
          return HttpResponse.json({
            access_token: makeTestJwt(),
            refresh_token: 'refresh-token-2',
            token_type: 'Bearer',
            expires_in: 900,
          })
        }
        return HttpResponse.json({ code: 'unauthorized', message: 'refresh token revoked' }, { status: 401 })
      }),
      http.get('/v1/me', () => HttpResponse.json({ code: 'unauthorized', message: 'access token expired' }, { status: 401 })),
      http.get('/v1/me/wallets', () =>
        HttpResponse.json({ code: 'unauthorized', message: 'access token expired' }, { status: 401 }),
      ),
    )

    renderAccount()

    expect(await screen.findByText('Login page')).toBeInTheDocument()
    // Not just a navigation - the client-side session is actually gone,
    // so a stale token is never reused on a later request or replayed on
    // a client-side back navigation.
    expect(getAccessToken()).toBeNull()
    expect(getRefreshToken()).toBeNull()
  })
})

describe('logout', () => {
  it('clears all local session state and calls the backend revocation endpoint (not just a client-side navigation)', async () => {
    setSession(makeTestJwt(), 'refresh-token-to-revoke')

    let revokedWith: string | null = null
    server.use(
      http.post('/v1/auth/logout', async ({ request }) => {
        const body = (await request.json()) as { refresh_token: string }
        revokedWith = body.refresh_token
        return new HttpResponse(null, { status: 204 })
      }),
    )

    renderAccount()
    const user = userEvent.setup()

    await screen.findByText('My account')
    // AuthProvider's own bootstrap-on-load effect (see AuthContext.tsx)
    // silently rotates the refresh token once via the default /v1/auth/
    // refresh handler before this point - capture whatever token is
    // actually current right before logging out, rather than assuming
    // the original one set above is still live.
    const currentRefreshToken = getRefreshToken()
    expect(currentRefreshToken).not.toBeNull()

    await user.click(screen.getByRole('button', { name: 'Log out' }))

    expect(await screen.findByText('Login page')).toBeInTheDocument()
    expect(getAccessToken()).toBeNull()
    expect(getRefreshToken()).toBeNull()
    await waitFor(() => expect(revokedWith).toBe(currentRefreshToken))
  })

  it('still clears local session state even if the backend revocation call fails (never traps the user in a signed-in UI)', async () => {
    setSession(makeTestJwt(), 'refresh-token-for-test')
    server.use(http.post('/v1/auth/logout', () => HttpResponse.error()))

    renderAccount()
    const user = userEvent.setup()

    await screen.findByText('My account')
    await user.click(screen.getByRole('button', { name: 'Log out' }))

    expect(await screen.findByText('Login page')).toBeInTheDocument()
    expect(getAccessToken()).toBeNull()
    expect(getRefreshToken()).toBeNull()
  })
})
