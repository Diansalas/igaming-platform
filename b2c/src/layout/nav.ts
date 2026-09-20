export interface NavItem {
  to: string
  label: string
  /** Only shown when authenticated. */
  requiresAuth?: boolean
}

export const NAV_ITEMS: NavItem[] = [
  { to: '/', label: 'Home' },
  { to: '/sports', label: 'Sports' },
  { to: '/casino', label: 'Casino', requiresAuth: true },
  { to: '/account/bets', label: 'My bets', requiresAuth: true },
  { to: '/casino/history', label: 'Casino history', requiresAuth: true },
  { to: '/account', label: 'Account', requiresAuth: true },
]

export function visibleNavItems(isAuthenticated: boolean): NavItem[] {
  return NAV_ITEMS.filter((item) => !item.requiresAuth || isAuthenticated)
}
