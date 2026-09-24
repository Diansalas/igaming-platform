import { useState, type FormEvent } from 'react'
import { Link, Navigate, useLocation } from 'react-router-dom'
import { Button } from '../components/Button'
import { TextInput } from '../components/TextInput'
import { ApiError } from '../api/types'
import { useAuth } from './AuthContext'

function errorMessageFor(err: unknown): string {
  if (err instanceof ApiError) {
    switch (err.code) {
      case 'conflict':
        return 'An account with this email already exists.'
      case 'not_found':
        return 'Unknown brand configuration.'
      case 'validation_error':
        return err.message || 'Please check the form and try again.'
      case 'network_error':
        return err.message
      default:
        return 'Registration failed. Please try again.'
    }
  }
  return 'Registration failed. Please try again.'
}

export function RegisterPage() {
  const { register, isAuthenticated } = useAuth()
  const location = useLocation()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [isSubmitting, setIsSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)

  if (isAuthenticated) {
    const from = (location.state as { from?: Location })?.from
    // A new account starts pending_verification; the account page is where
    // the player verifies their email before any money flow.
    return <Navigate to={from?.pathname ?? '/account'} replace />
  }

  async function onSubmit(e: FormEvent) {
    e.preventDefault()
    setError(null)
    setIsSubmitting(true)
    try {
      await register({ email: email.trim(), password })
    } catch (err) {
      setError(errorMessageFor(err))
    } finally {
      setIsSubmitting(false)
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-surface-alt px-4">
      <div className="w-full max-w-sm rounded-lg border border-border bg-surface p-6 shadow-sm">
        <h1 className="mb-1 text-lg font-semibold text-slate-900">Create an account</h1>
        <p className="mb-6 text-sm text-slate-500">Password must be at least 8 characters.</p>

        <form onSubmit={onSubmit} className="flex flex-col gap-4" noValidate>
          <label className="flex flex-col gap-1 text-sm">
            <span className="font-medium text-slate-700">Email</span>
            <TextInput
              type="email"
              required
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              autoComplete="username"
            />
          </label>
          <label className="flex flex-col gap-1 text-sm">
            <span className="font-medium text-slate-700">Password</span>
            <TextInput
              type="password"
              required
              minLength={8}
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete="new-password"
            />
          </label>

          {error && (
            <p role="alert" className="text-sm text-red-600">
              {error}
            </p>
          )}

          <Button type="submit" isLoading={isSubmitting} className="w-full">
            Create account
          </Button>
        </form>

        <p className="mt-4 text-center text-sm text-slate-500">
          Already have an account?{' '}
          <Link to="/login" className="text-brand-700 hover:underline">
            Log in
          </Link>
        </p>
      </div>
    </div>
  )
}
