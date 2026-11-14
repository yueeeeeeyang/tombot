import { useEffect, useId, useRef, useState } from 'react'
import { Check, ChevronUp, LogOut, Monitor, Moon, Sun } from 'lucide-react'
import type { Principal } from '../api/schemas'
import { themeOptions, useTheme } from './ThemePicker'
import catAvatar from '../../../desktop/src/renderer/src/assets/cats/frames/bombay.png'

/** 账户入口常驻侧栏；菜单支持键盘导航、点击外部关闭和焦点返回。 */
export function AccountMenu({
  principal,
  busy,
  onLogout,
}: {
  principal: Principal
  busy: boolean
  onLogout: () => void
}) {
  const [open, setOpen] = useState(false)
  const { theme, setTheme, storageError } = useTheme()
  const root = useRef<HTMLDivElement>(null)
  const trigger = useRef<HTMLButtonElement>(null)
  const menu = useRef<HTMLDivElement>(null)
  const id = useId()
  const startAtEnd = useRef(false)
  const icons = { system: Monitor, light: Sun, dark: Moon }
  function close() {
    setOpen(false)
    trigger.current?.focus()
  }
  useEffect(() => {
    if (!open) return
    const items = menu.current?.querySelectorAll<HTMLButtonElement>(
      '[role^="menuitem"]:not(:disabled)',
    )
    items?.[startAtEnd.current ? items.length - 1 : 0]?.focus()
    const outside = (event: PointerEvent) => {
      if (event.target instanceof Node && !root.current?.contains(event.target)) setOpen(false)
    }
    document.addEventListener('pointerdown', outside)
    return () => document.removeEventListener('pointerdown', outside)
  }, [open])
  return (
    <div className="account-control" ref={root}>
      <button
        ref={trigger}
        id={id}
        type="button"
        className="account-trigger"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={`${id}-menu`}
        disabled={busy}
        aria-label={`账户菜单：${principal.display_name}（${principal.username}）`}
        onClick={() => {
          startAtEnd.current = false
          setOpen(!open)
        }}
        onKeyDown={(event) => {
          if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
            event.preventDefault()
            startAtEnd.current = event.key === 'ArrowUp'
            setOpen(true)
          }
        }}
      >
        <img src={catAvatar} alt="当前用户头像" width={36} height={36} />
        <span className="account-identity">
          <strong title={principal.display_name}>{principal.display_name}</strong>
          <small title={principal.username}>@{principal.username}</small>
        </span>
        <ChevronUp size={16} className={open ? 'account-chevron-open' : ''} />
      </button>
      {open && (
        <div
          ref={menu}
          id={`${id}-menu`}
          className="account-menu"
          role="menu"
          aria-labelledby={id}
          onBlur={(event) => {
            if (event.relatedTarget instanceof Node && !root.current?.contains(event.relatedTarget))
              setOpen(false)
          }}
          onKeyDown={(event) => {
            if (event.key === 'Escape') {
              event.preventDefault()
              event.stopPropagation()
              close()
              return
            }
            if (event.key === 'Tab') {
              close()
              return
            }
            const items = Array.from(
              menu.current?.querySelectorAll<HTMLButtonElement>(
                '[role^="menuitem"]:not(:disabled)',
              ) ?? [],
            )
            const index = items.indexOf(document.activeElement as HTMLButtonElement)
            const next =
              event.key === 'ArrowDown'
                ? (index + 1) % items.length
                : event.key === 'ArrowUp'
                  ? (index + items.length - 1) % items.length
                  : event.key === 'Home'
                    ? 0
                    : event.key === 'End'
                      ? items.length - 1
                      : -1
            if (next >= 0) {
              event.preventDefault()
              items[next]?.focus()
            }
          }}
        >
          <div className="account-menu-label">外观</div>
          {themeOptions.map((option) => {
            const Icon = icons[option.value]
            return (
              <button
                key={option.value}
                type="button"
                role="menuitemradio"
                aria-checked={theme === option.value}
                tabIndex={-1}
                onClick={() => {
                  setTheme(option.value)
                  close()
                }}
              >
                <Icon size={16} />
                <span>{option.label}</span>
                {theme === option.value && <Check size={15} />}
              </button>
            )
          })}
          <div className="account-menu-divider" role="separator" />
          <button
            type="button"
            role="menuitem"
            tabIndex={-1}
            disabled={busy}
            onClick={() => {
              close()
              onLogout()
            }}
          >
            <LogOut size={16} />
            <span>退出登录</span>
          </button>
        </div>
      )}
      {storageError && <small role="status">外观仅在本次页面生效</small>}
    </div>
  )
}
