import { useId, useRef, useState } from 'react'
import { Select as SharedSelect, type SelectOption } from '../../../ui/Select'

/** 将共享下拉控件接入原生表单提交与必填校验，错误时将焦点交给可见按钮。 */
export function Select({
  id,
  label,
  name,
  value,
  defaultValue = '',
  onChange,
  options,
  required = false,
  disabled = false,
  placeholder,
  'aria-describedby': describedBy,
}: {
  id?: string
  label: string
  name?: string
  value?: string
  defaultValue?: string
  onChange?: (value: string) => void
  options: readonly SelectOption[]
  required?: boolean
  disabled?: boolean
  placeholder?: string
  'aria-describedby'?: string
}) {
  const generatedID = useId()
  const root = useRef<HTMLDivElement>(null)
  const [selection, setSelection] = useState(defaultValue)
  const [invalid, setInvalid] = useState(false)
  const current = value ?? selection
  const selected = options.find((option) => option.value === current && !option.disabled)
  const controlID = id ?? generatedID
  const hasError = invalid && required && !selected?.value && !disabled
  function choose(next: string) {
    setSelection(next)
    setInvalid(false)
    onChange?.(next)
  }
  return (
    <div ref={root} className="form-select">
      <SharedSelect
        id={controlID}
        label={label}
        value={current}
        options={options}
        onChange={choose}
        disabled={disabled}
        placeholder={placeholder}
        aria-required={required}
        aria-invalid={hasError}
        aria-describedby={
          [describedBy, hasError && `${controlID}-error`].filter(Boolean).join(' ') || undefined
        }
      />
      {(name || required) && (
        <input
          className="select-form-value"
          aria-hidden="true"
          tabIndex={-1}
          name={name}
          value={selected?.value ?? ''}
          required={required}
          disabled={disabled}
          onChange={(event) => choose(event.target.value)}
          onInvalid={(event) => {
            event.preventDefault()
            setInvalid(true)
            if (
              event.currentTarget.form?.querySelector('input:invalid, textarea:invalid') ===
              event.currentTarget
            )
              root.current?.querySelector<HTMLButtonElement>('[role="combobox"]')?.focus()
          }}
        />
      )}
      {hasError && (
        <small className="field-error" id={`${controlID}-error`}>
          请选择{label}
        </small>
      )}
    </div>
  )
}
