import { useState, type FormEvent } from 'react'
import { Navigate, useLocation } from 'react-router-dom'
import { Button } from '../components/Button'
import { ApiError } from '../api/types'
import { useAuth } from './AuthContext'

function errorMessageFor(err: unknown): string {
  if (err instanceof ApiError) {
    switch (err.code) {
      case 'rate_limited':
        return 'Too many failed attempts. Try again later.'
      case 'unauthorized':
        return 'Invalid email or password.'
      case 'forbidden':
        return 'This account cannot currently log in.'
      case 'not_found':
        return 'Unknown tenant.'
      case 'validation_error':
        return err.message || 'Please check the form and try again.'
      case 'network_error':
        return err.message
      default:
        return 'Login failed. Please try again.'
    }
  }
  return 'Login failed. Please try again.'
}

export function LoginPage() {
  const { login, isAuthenticated } = useAuth()
  const location = useLocation()
  const [tenantSlug, setTenantSlug] = useState('')
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [isSubmitting, setIsSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  if (isAuthenticated) {
    const from = (location.state as { from?: Location })?.from
    return <Navigate to={from?.pathname ?? '/'} replace />
  }

  async function onSubmit(e: FormEvent) {
    e.preventDefault()
    setError(null)
    setIsSubmitting(true)
    try {
      await login({ tenantSlug: tenantSlug.trim() || undefined, email: email.trim(), password })
    } catch (err) {
      setError(errorMessageFor(err))
    } finally {
      setIsSubmitting(false)
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-surface-alt px-4">
      <div className="w-full max-w-sm rounded-lg border border-border bg-surface p-6 shadow-sm">
        <h1 className="mb-1 text-lg font-semibold text-slate-900">Operator Back Office</h1>
        <p className="mb-6 text-sm text-slate-500">Sign in with your staff credentials.</p>

        <form onSubmit={onSubmit} className="flex flex-col gap-4" noValidate>
          <label className="flex flex-col gap-1 text-sm">
            <span className="font-medium text-slate-700">Tenant slug</span>
            <input
              type="text"
              className="rounded-md border border-border px-3 py-2 text-sm shadow-sm focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
              value={tenantSlug}
              onChange={(e) => setTenantSlug(e.target.value)}
              placeholder="Leave blank for platform admin"
              autoComplete="organization"
            />
          </label>
          <label className="flex flex-col gap-1 text-sm">
            <span className="font-medium text-slate-700">Email</span>
            <input
              type="email"
              required
              className="rounded-md border border-border px-3 py-2 text-sm shadow-sm focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              autoComplete="username"
            />
          </label>
          <label className="flex flex-col gap-1 text-sm">
            <span className="font-medium text-slate-700">Password</span>
            <input
              type="password"
              required
              className="rounded-md border border-border px-3 py-2 text-sm shadow-sm focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete="current-password"
            />
          </label>

          {error && (
            <p role="alert" className="text-sm text-red-600">
              {error}
            </p>
          )}

          <Button type="submit" isLoading={isSubmitting} className="w-full">
            Sign in
          </Button>
        </form>
      </div>
    </div>
  )
}
