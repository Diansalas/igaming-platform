import { QueryClientProvider } from '@tanstack/react-query'
import { BrowserRouter } from 'react-router-dom'
import { AuthProvider } from '../auth/AuthContext'
import { BetSlipProvider } from '../features/betslip/BetSlipContext'
import { AppRoutes } from './AppRoutes'
import { queryClient } from './queryClient'

export function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <AuthProvider>
        <BetSlipProvider>
          <BrowserRouter>
            <AppRoutes />
          </BrowserRouter>
        </BetSlipProvider>
      </AuthProvider>
    </QueryClientProvider>
  )
}
