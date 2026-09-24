import { useId, type InputHTMLAttributes, type ReactNode, type SelectHTMLAttributes } from 'react'
import type { SelectOption } from './Select'

const INPUT_CLASSES =
  'rounded-md border border-border bg-white px-3 py-2 text-sm shadow-sm focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500'

interface TextInputProps extends InputHTMLAttributes<HTMLInputElement> {
  label: string
  help?: ReactNode
}

/** Labelled text input; help text is linked via aria-describedby so it never pollutes the accessible name. */
export function TextInput({ label, help, className = '', id, ...rest }: TextInputProps) {
  const generated = useId()
  const inputId = id ?? generated
  const helpId = `${inputId}-help`
  return (
    <div className="flex flex-col gap-1 text-sm">
      <label htmlFor={inputId} className="font-medium text-slate-700">
        {label}
      </label>
      <input id={inputId} aria-describedby={help ? helpId : undefined} className={`${INPUT_CLASSES} ${className}`} {...rest} />
      {help && (
        <span id={helpId} className="text-xs text-slate-500">
          {help}
        </span>
      )}
    </div>
  )
}

interface SelectFieldProps extends SelectHTMLAttributes<HTMLSelectElement> {
  label: string
  options: SelectOption[]
  help?: ReactNode
}

export function SelectField({ label, options, help, className = '', id, ...rest }: SelectFieldProps) {
  const generated = useId()
  const selectId = id ?? generated
  const helpId = `${selectId}-help`
  return (
    <div className="flex flex-col gap-1 text-sm">
      <label htmlFor={selectId} className="font-medium text-slate-700">
        {label}
      </label>
      <select id={selectId} aria-describedby={help ? helpId : undefined} className={`${INPUT_CLASSES} ${className}`} {...rest}>
        {options.map((opt) => (
          <option key={opt.value} value={opt.value}>
            {opt.label}
          </option>
        ))}
      </select>
      {help && (
        <span id={helpId} className="text-xs text-slate-500">
          {help}
        </span>
      )}
    </div>
  )
}

interface CheckboxProps extends Omit<InputHTMLAttributes<HTMLInputElement>, 'type'> {
  label: string
}

export function Checkbox({ label, ...rest }: CheckboxProps) {
  return (
    <label className="flex items-center gap-2 text-sm text-slate-700">
      <input type="checkbox" className="h-4 w-4 rounded border-border text-brand-600 focus:ring-brand-500" {...rest} />
      <span>{label}</span>
    </label>
  )
}

/** Splits a comma/whitespace-separated list into trimmed, non-empty entries. */
export function parseList(value: string): string[] {
  return value
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter((s) => s !== '')
}
