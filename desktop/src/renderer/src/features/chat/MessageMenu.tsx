import { Copy, Hash, Reply, RotateCw, SmilePlus, Trash2 } from 'lucide-react'
import { useLayoutEffect, useRef, useState, type KeyboardEvent as ReactKeyboardEvent } from 'react'
import type { ChatMessage } from '../workspace/model'
import { MORE_REACTION_EMOJIS, QUICK_REACTION_EMOJIS } from '../workspace/reactions'

const VIEWPORT_MARGIN = 8

export type MessageMenuAnchor = {
  messageId: string
  x: number
  y: number
  trigger: HTMLElement
}

type MenuItem = {
  key: string
  label: string
  Icon: typeof Reply
  onSelect: () => void
  danger?: boolean
  separator?: boolean
}

/** 把菜单限制在窗口内；弹出层展开后按实际尺寸回退，避免贴边时被裁切。 */
function placeMenu(element: HTMLElement, x: number, y: number): void {
  const bounds = element.getBoundingClientRect()
  const left = Math.max(
    VIEWPORT_MARGIN,
    Math.min(x, window.innerWidth - bounds.width - VIEWPORT_MARGIN),
  )
  const top = Math.max(
    VIEWPORT_MARGIN,
    Math.min(y, window.innerHeight - bounds.height - VIEWPORT_MARGIN),
  )
  element.style.left = `${left}px`
  element.style.top = `${top}px`
}

function focusOffset(buttons: HTMLButtonElement[], direction: 1 | -1): void {
  const current = buttons.findIndex((button) => button === document.activeElement)
  const next =
    current === -1
      ? direction === 1
        ? 0
        : buttons.length - 1
      : (current + direction + buttons.length) % buttons.length
  buttons[next]?.focus()
}

/** 消息右键菜单：快捷表情、回复、复制，以及原来放在气泡底下的删除和重试。 */
export function MessageMenu({
  anchor,
  message,
  onReact,
  onReply,
  onCopy,
  onCopyRequestId,
  onRetry,
  onDelete,
  onClose,
}: {
  anchor: MessageMenuAnchor
  message: ChatMessage
  onReact: (emoji: string) => void
  onReply: () => void
  onCopy: () => void
  onCopyRequestId: () => void
  onRetry: () => void
  onDelete: () => void
  onClose: () => void
}) {
  const menu = useRef<HTMLDivElement>(null)
  const [isMoreOpen, setMoreOpen] = useState(false)
  const reacted = new Set(message.reactions.map((reaction) => reaction.emoji))

  useLayoutEffect(() => {
    const element = menu.current!
    element.showPopover()
    placeMenu(element, anchor.x, anchor.y)
    element
      .querySelector<HTMLButtonElement>('button:not(:disabled)')
      ?.focus({ preventScroll: true })
    function closeOutside(event: PointerEvent): void {
      if (event.target instanceof Node && element.contains(event.target)) return
      onClose()
    }
    function closeOnScroll(event: Event): void {
      if (event.target instanceof Node && element.contains(event.target)) return
      onClose()
    }
    document.addEventListener('pointerdown', closeOutside, true)
    document.addEventListener('scroll', closeOnScroll, true)
    window.addEventListener('resize', onClose)
    window.addEventListener('blur', onClose)
    return () => {
      element.hidePopover()
      document.removeEventListener('pointerdown', closeOutside, true)
      document.removeEventListener('scroll', closeOnScroll, true)
      window.removeEventListener('resize', onClose)
      window.removeEventListener('blur', onClose)
      if (anchor.trigger.isConnected) anchor.trigger.focus({ preventScroll: true })
    }
  }, [anchor, onClose])

  useLayoutEffect(() => {
    const element = menu.current
    if (!element) return
    placeMenu(element, anchor.x, anchor.y)
  }, [isMoreOpen, anchor])

  function onKeyDown(event: ReactKeyboardEvent<HTMLDivElement>): void {
    if (event.key === 'Escape' || event.key === 'Tab') {
      event.preventDefault()
      onClose()
      return
    }
    const current = event.target
    if (
      (event.key === 'ArrowLeft' || event.key === 'ArrowRight') &&
      current instanceof HTMLElement &&
      current.closest('.reaction-strip, .emoji-grid')
    ) {
      event.preventDefault()
      const container = current.closest('.reaction-strip, .emoji-grid')
      if (!(container instanceof HTMLElement)) return
      focusOffset(
        [...container.querySelectorAll<HTMLButtonElement>('button:not(:disabled)')],
        event.key === 'ArrowRight' ? 1 : -1,
      )
      return
    }
    if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) return
    event.preventDefault()
    const buttons = [
      ...event.currentTarget.querySelectorAll<HTMLButtonElement>('button:not(:disabled)'),
    ]
    if (event.key === 'Home') {
      buttons[0]?.focus()
      return
    }
    if (event.key === 'End') {
      buttons.at(-1)?.focus()
      return
    }
    focusOffset(buttons, event.key === 'ArrowDown' ? 1 : -1)
  }

  function select(action: () => void): void {
    action()
    onClose()
  }

  const canRetry = message.status === 'unsent' || message.status === 'failed'
  const items: MenuItem[] = [
    { key: 'reply', label: '回复', Icon: Reply, onSelect: onReply },
    { key: 'copy', label: '复制', Icon: Copy, onSelect: onCopy },
    ...(message.requestId
      ? [{ key: 'request', label: '复制请求 ID', Icon: Hash, onSelect: onCopyRequestId }]
      : []),
    ...(canRetry ? [{ key: 'retry', label: '重试', Icon: RotateCw, onSelect: onRetry }] : []),
    {
      key: 'delete',
      label: '删除',
      Icon: Trash2,
      onSelect: onDelete,
      danger: true,
      separator: true,
    },
  ]

  return (
    <div
      ref={menu}
      className="message-menu"
      role="menu"
      aria-label="消息操作"
      popover="manual"
      onContextMenu={(event) => event.preventDefault()}
      onKeyDown={onKeyDown}
    >
      <div className="reaction-strip" role="group" aria-label="快捷表情">
        {QUICK_REACTION_EMOJIS.map((emoji) => {
          const isSelected = reacted.has(emoji)
          return (
            <button
              key={emoji}
              type="button"
              aria-pressed={isSelected}
              aria-label={isSelected ? `取消回应${emoji}` : `回应${emoji}`}
              onClick={() => select(() => onReact(emoji))}
            >
              {emoji}
            </button>
          )
        })}
        <button
          type="button"
          aria-label="更多表情"
          aria-expanded={isMoreOpen}
          onClick={() => setMoreOpen((open) => !open)}
        >
          <SmilePlus size={16} />
        </button>
      </div>
      {isMoreOpen && (
        <div className="emoji-grid" role="group" aria-label="更多表情">
          {MORE_REACTION_EMOJIS.map((emoji) => {
            const isSelected = reacted.has(emoji)
            return (
              <button
                key={emoji}
                type="button"
                aria-pressed={isSelected}
                aria-label={isSelected ? `取消回应${emoji}` : `回应${emoji}`}
                onClick={() => select(() => onReact(emoji))}
              >
                {emoji}
              </button>
            )
          })}
        </div>
      )}
      {items.map((item) => (
        <div key={item.key}>
          {item.separator && <div className="message-menu-separator" role="separator" />}
          <button
            type="button"
            role="menuitem"
            className={item.danger ? 'is-danger' : undefined}
            onClick={() => select(item.onSelect)}
          >
            <item.Icon size={15} strokeWidth={1.7} />
            <span>{item.label}</span>
          </button>
        </div>
      ))}
    </div>
  )
}
