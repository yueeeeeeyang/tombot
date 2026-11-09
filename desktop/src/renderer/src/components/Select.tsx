import { Check, ChevronDown } from 'lucide-react'
import { useEffect, useId, useLayoutEffect, useRef, useState, type KeyboardEvent } from 'react'

export interface SelectOption<Value extends string = string> {
  value: Value
  label: string
  description?: string
  disabled?: boolean
}

/** 自绘单选菜单；弹出层进入顶层避免被弹窗裁剪，支持键盘、读屏、禁用项和外部关闭。 */
export function Select<Value extends string>({
  label,
  value,
  options,
  onChange,
  disabled = false,
  placeholder = '请选择',
  className = '',
}: {
  label: string
  value: Value
  options: readonly SelectOption<Value>[]
  onChange: (value: Value) => void
  disabled?: boolean
  placeholder?: string
  className?: string
}) {
  const id = useId()
  const trigger = useRef<HTMLButtonElement>(null)
  const menu = useRef<HTMLDivElement>(null)
  const search = useRef({ text: '', time: 0 })
  const [isOpen, setOpen] = useState(false)
  const [activeIndex, setActiveIndex] = useState(-1)
  const selected = options.find((option) => option.value === value)
  const isDisabled = disabled || !options.some((option) => !option.disabled)

  function open(): void {
    if (isDisabled) return
    const selectedIndex = options.findIndex((option) => option.value === value && !option.disabled)
    setActiveIndex(
      selectedIndex >= 0 ? selectedIndex : options.findIndex((option) => !option.disabled),
    )
    search.current = { text: '', time: 0 }
    setOpen(true)
  }

  function choose(index: number): void {
    const option = options[index]
    if (!option || option.disabled) return
    onChange(option.value)
    setOpen(false)
    trigger.current?.focus({ preventScroll: true })
  }

  useLayoutEffect(() => {
    const popup = menu.current
    const button = trigger.current
    if (!isOpen || !popup || !button) return
    popup.showPopover()
    function position(): void {
      if (!popup || !button) return
      const bounds = button.getBoundingClientRect()
      const margin = 12
      const gap = 6
      const width = Math.min(Math.max(bounds.width, 190), window.innerWidth - margin * 2)
      const below = window.innerHeight - bounds.bottom - margin - gap
      const above = bounds.top - margin - gap
      const opensAbove = below < Math.min(popup.scrollHeight, 260) && above > below
      const height = Math.max(0, Math.min(260, opensAbove ? above : below))
      popup.style.width = `${width}px`
      popup.style.maxHeight = `${height}px`
      popup.style.left = `${Math.max(margin, Math.min(bounds.right - width, window.innerWidth - width - margin))}px`
      popup.style.top = opensAbove ? 'auto' : `${bounds.bottom + gap}px`
      popup.style.bottom = opensAbove ? `${window.innerHeight - bounds.top + gap}px` : 'auto'
    }
    position()
    window.addEventListener('resize', position)
    document.addEventListener('scroll', position, true)
    function closeOutside(event: PointerEvent): void {
      if (
        event.target instanceof Node &&
        !popup?.contains(event.target) &&
        !button?.contains(event.target)
      )
        setOpen(false)
    }
    document.addEventListener('pointerdown', closeOutside, true)
    return () => {
      popup.hidePopover()
      window.removeEventListener('resize', position)
      document.removeEventListener('scroll', position, true)
      document.removeEventListener('pointerdown', closeOutside, true)
    }
  }, [isOpen])

  useEffect(() => {
    if (isOpen)
      menu.current
        ?.querySelector(`[data-index="${activeIndex}"]`)
        ?.scrollIntoView({ block: 'nearest' })
  }, [isOpen, activeIndex])

  function handleKey(event: KeyboardEvent<HTMLButtonElement>): void {
    if (event.nativeEvent.isComposing || event.metaKey || event.ctrlKey || event.altKey) return
    if (event.key === 'Escape' && isOpen) {
      event.preventDefault()
      event.stopPropagation()
      setOpen(false)
    } else if (event.key === 'Tab') {
      setOpen(false)
    } else if (['Enter', ' '].includes(event.key)) {
      event.preventDefault()
      if (isOpen) choose(activeIndex)
      else open()
    } else if (['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) {
      event.preventDefault()
      if (!isOpen) {
        open()
        return
      }
      const enabled = options
        .map((option, index) => (option.disabled ? -1 : index))
        .filter((index) => index >= 0)
      const current = enabled.indexOf(activeIndex)
      const next =
        event.key === 'Home'
          ? 0
          : event.key === 'End'
            ? enabled.length - 1
            : (current + (event.key === 'ArrowDown' ? 1 : -1) + enabled.length) % enabled.length
      setActiveIndex(enabled[next] ?? -1)
    } else if (event.key.length === 1) {
      event.preventDefault()
      if (!isOpen) open()
      const now = Date.now()
      const query =
        (now - search.current.time < 700 ? search.current.text : '') + event.key.toLocaleLowerCase()
      search.current = { text: query, time: now }
      const match = options.findIndex(
        (option) => !option.disabled && option.label.toLocaleLowerCase().startsWith(query),
      )
      if (match >= 0) setActiveIndex(match)
    }
  }

  return (
    <div className={`custom-select ${className}`}>
      <button
        ref={trigger}
        type="button"
        role="combobox"
        className="select-trigger"
        aria-label={label}
        aria-haspopup="listbox"
        aria-expanded={isOpen}
        aria-controls={isOpen ? id : undefined}
        aria-activedescendant={isOpen && activeIndex >= 0 ? `${id}-${activeIndex}` : undefined}
        disabled={isDisabled}
        onClick={() => (isOpen ? setOpen(false) : open())}
        onKeyDown={handleKey}
        onBlur={() => setOpen(false)}
      >
        <span>{selected?.label ?? placeholder}</span>
        <ChevronDown size={14} aria-hidden="true" />
      </button>
      <div
        ref={menu}
        id={id}
        className="select-menu"
        role="listbox"
        aria-label={label}
        popover="manual"
        hidden={!isOpen}
      >
        {options.map((option, index) => (
          <div
            id={`${id}-${index}`}
            key={option.value}
            data-index={index}
            role="option"
            aria-label={option.label}
            aria-describedby={option.description ? `${id}-${index}-description` : undefined}
            aria-selected={option.value === value}
            aria-disabled={option.disabled || undefined}
            className={`select-option ${index === activeIndex ? 'is-highlighted' : ''}`}
            onPointerMove={() => !option.disabled && setActiveIndex(index)}
            onPointerDown={(event) => event.preventDefault()}
            onClick={() => choose(index)}
          >
            <span>
              <span>{option.label}</span>
              {option.description && (
                <small id={`${id}-${index}-description`}>{option.description}</small>
              )}
            </span>
            {option.value === value && <Check size={14} aria-hidden="true" />}
          </div>
        ))}
      </div>
    </div>
  )
}
