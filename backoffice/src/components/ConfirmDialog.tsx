import { useState, type ReactNode } from 'react'
import { Button } from './Button'
import { Modal } from './Modal'

interface ConfirmDialogProps {
  title: string
  description?: ReactNode
  confirmLabel?: string
  cancelLabel?: string
  /** 'danger' for destructive/money-moving actions (reject, suspend, reject withdrawal). */
  variant?: 'primary' | 'danger'
  /** When true, a reason-code text field is required before Confirm is enabled - used for
   *  every action CLAUDE.md requires a reason code for (suspend, reinstate, reject, etc.). */
  requireReason?: boolean
  reasonLabel?: string
  reasonPlaceholder?: string
  isSubmitting?: boolean
  errorMessage?: string | null
  onConfirm: (reason: string) => void
  onCancel: () => void
}

export function ConfirmDialog({
  title,
  description,
  confirmLabel = 'Confirm',
  cancelLabel = 'Cancel',
  variant = 'primary',
  requireReason = false,
  reasonLabel = 'Reason code',
  reasonPlaceholder,
  isSubmitting = false,
  errorMessage,
  onConfirm,
  onCancel,
}: ConfirmDialogProps) {
  const [reason, setReason] = useState('')
  const reasonMissing = requireReason && reason.trim() === ''

  return (
    <Modal title={title} onClose={onCancel}>
      <div className="flex flex-col gap-4">
        {description && <div className="text-sm text-slate-600">{description}</div>}
        {requireReason && (
          <label className="flex flex-col gap-1 text-sm">
            <span className="font-medium text-slate-700">{reasonLabel}</span>
            <textarea
              className="rounded-md border border-border px-3 py-2 text-sm shadow-sm focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
              rows={2}
              value={reason}
              placeholder={reasonPlaceholder}
              onChange={(e) => setReason(e.target.value)}
              autoFocus
            />
          </label>
        )}
        {errorMessage && <p className="text-sm text-red-600">{errorMessage}</p>}
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={onCancel} disabled={isSubmitting}>
            {cancelLabel}
          </Button>
          <Button
            variant={variant === 'danger' ? 'danger' : 'primary'}
            onClick={() => onConfirm(reason.trim())}
            disabled={reasonMissing || isSubmitting}
            isLoading={isSubmitting}
          >
            {confirmLabel}
          </Button>
        </div>
      </div>
    </Modal>
  )
}
