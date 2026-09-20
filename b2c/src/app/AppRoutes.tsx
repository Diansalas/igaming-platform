import { Route, Routes } from 'react-router-dom'
import { LoginPage } from '../auth/LoginPage'
import { RegisterPage } from '../auth/RegisterPage'
import { RequireAuth } from '../auth/RequireAuth'
import { AccountPage } from '../features/account/AccountPage'
import { BetHistoryPage } from '../features/bets/BetHistoryPage'
import { DepositPage } from '../features/deposit/DepositPage'
import { EventDetailPage } from '../features/sportsbook/EventDetailPage'
import { SportsListPage } from '../features/sportsbook/SportsListPage'
import { AppLayout } from '../layout/AppLayout'
import { HomePage } from './HomePage'
import { NotFoundPage } from './NotFoundPage'

export function AppRoutes() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route path="/register" element={<RegisterPage />} />

      <Route element={<AppLayout />}>
        <Route index element={<HomePage />} />

        {/* Public catalogue browse - no authentication required. */}
        <Route path="sports" element={<SportsListPage />} />
        <Route path="sports/events/:id" element={<EventDetailPage />} />

        <Route
          path="account"
          element={
            <RequireAuth>
              <AccountPage />
            </RequireAuth>
          }
        />
        <Route
          path="account/deposit"
          element={
            <RequireAuth>
              <DepositPage />
            </RequireAuth>
          }
        />
        <Route
          path="account/bets"
          element={
            <RequireAuth>
              <BetHistoryPage />
            </RequireAuth>
          }
        />

        <Route path="*" element={<NotFoundPage />} />
      </Route>
    </Routes>
  )
}
