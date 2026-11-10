import { Trash2 } from 'lucide-react'
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'

export interface CatRoutine {
  id: string
  name: string
  cron: string
  prompt: string
  is_enabled: boolean
  revision: number
}

type RoutineMenuAnchor = {
  routine: CatRoutine
  x: number
  y: number
  trigger: HTMLElement
}

/** 定时任务只保留启用开关和与猫咪菜单一致的右键删除。创建和改期由 agent 在对话里完成。 */
export function RoutinePane({
  agentId,
  onLoad,
  onToggle,
  onDelete,
}: {
  agentId: string
  onLoad: (agentId: string) => Promise<CatRoutine[]>
  onToggle: (routine: CatRoutine) => Promise<void>
  onDelete: (routine: CatRoutine) => Promise<void>
}) {
  const [items, setItems] = useState<CatRoutine[]>([])
  const [error, setError] = useState('')
  const [menu, setMenu] = useState<RoutineMenuAnchor | null>(null)

  useEffect(() => {
    let cancelled = false
    void onLoad(agentId).then(
      (next) => {
        if (!cancelled) {
          setItems(Array.isArray(next) ? next : [])
          setError('')
        }
      },
      (failure: Error) => {
        if (!cancelled) setError(failure.message)
      },
    )
    return () => {
      cancelled = true
    }
  }, [agentId, onLoad])

  async function refresh(): Promise<void> {
    const next = await onLoad(agentId)
    setItems(Array.isArray(next) ? next : [])
  }

  const closeMenu = useCallback(() => setMenu(null), [])

  return (
    <div
      className="cat-panel-body"
      role="tabpanel"
      id="cat-panel-routine"
      aria-labelledby="cat-tab-routine"
    >
      {items.length === 0 && !error ? (
        <p className="cat-panel-empty" role="status">
          还没有定时任务
        </p>
      ) : null}
      <ul className="routine-list">
        {items.map((item) => (
          <li
            key={item.id}
            onContextMenu={(event) => {
              event.preventDefault()
              setMenu({
                routine: item,
                x: event.clientX,
                y: event.clientY,
                trigger: event.currentTarget,
              })
            }}
          >
            <div className="routine-row">
              <div>
                <strong>{item.name}</strong>
                <p>{item.prompt}</p>
              </div>
              <button
                type="button"
                role="switch"
                className="setting-switch"
                aria-checked={item.is_enabled}
                aria-label={item.is_enabled ? `暂停${item.name}` : `启用${item.name}`}
                onClick={() => {
                  void onToggle(item)
                    .then(refresh)
                    .then(() => setError(''))
                    .catch((failure: Error) => setError(failure.message))
                }}
              >
                <span />
              </button>
            </div>
          </li>
        ))}
      </ul>
      {menu ? (
        <RoutineMenu
          anchor={menu}
          onClose={closeMenu}
          onDelete={() => {
            const routine = menu.routine
            closeMenu()
            void onDelete(routine)
              .then(refresh)
              .then(() => setError(''))
              .catch((failure: Error) => setError(failure.message))
          }}
        />
      ) : null}
      {error ? <p role="alert">{error}</p> : null}
    </div>
  )
}

function RoutineMenu({
  anchor,
  onDelete,
  onClose,
}: {
  anchor: RoutineMenuAnchor
  onDelete: () => void
  onClose: () => void
}) {
  const menu = useRef<HTMLDivElement>(null)
  useLayoutEffect(() => {
    if (menu.current == null) return
    const element: HTMLDivElement = menu.current
    if (typeof element.showPopover === 'function') element.showPopover()
    const bounds = element.getBoundingClientRect()
    element.style.left = `${Math.max(8, Math.min(anchor.x, innerWidth - bounds.width - 8))}px`
    element.style.top = `${Math.max(8, Math.min(anchor.y, innerHeight - bounds.height - 8))}px`
    element.querySelector<HTMLButtonElement>('button')?.focus({ preventScroll: true })
    function closeOutside(event: PointerEvent): void {
      if (event.target instanceof Node && !element.contains(event.target)) onClose()
    }
    document.addEventListener('pointerdown', closeOutside, true)
    window.addEventListener('resize', onClose)
    window.addEventListener('blur', onClose)
    return () => {
      if (typeof element.hidePopover === 'function') element.hidePopover()
      document.removeEventListener('pointerdown', closeOutside, true)
      window.removeEventListener('resize', onClose)
      window.removeEventListener('blur', onClose)
      if (anchor.trigger.isConnected) anchor.trigger.focus({ preventScroll: true })
    }
  }, [anchor, onClose])

  return (
    <div
      ref={menu}
      className="cat-context-menu"
      popover="manual"
      role="menu"
      aria-label="定时任务"
      onKeyDown={(event) => {
        if (event.key === 'Escape') {
          event.preventDefault()
          onClose()
        }
      }}
    >
      <button type="button" role="menuitem" className="is-danger" onClick={onDelete}>
        <Trash2 size={16} />
        <span>删除</span>
      </button>
    </div>
  )
}
