import { Outlet } from 'react-router-dom'
import { BetSlip } from '../features/betslip/BetSlip'
import { useBetSlip } from '../features/betslip/BetSlipContext'
import { BottomNav } from './BottomNav'
import { TopBar } from './TopBar'

/** Floating mobile-only toggle for the bet slip bottom sheet - only rendered once a selection exists, so it never occupies space for a visitor who hasn't picked anything yet. */
function BetSlipToggle() {
  const { selection, isOpen, toggle } = useBetSlip()
  if (!selection || isOpen) return null

  return (
    <button
      type="button"
      onClick={toggle}
      className="fixed bottom-16 right-4 z-30 rounded-full bg-brand-600 px-4 py-3 text-sm font-medium text-white shadow-lg lg:hidden"
    >
      Bet slip (1)
    </button>
  )
}

export function AppLayout() {
  return (
    <div className="flex h-screen flex-col">
      <TopBar />
      <div className="flex flex-1 overflow-hidden">
        <main className="flex-1 overflow-y-auto p-4 sm:p-6">
          <Outlet />
        </main>
        <BetSlip />
      </div>
      <BetSlipToggle />
      <BottomNav />
    </div>
  )
}
