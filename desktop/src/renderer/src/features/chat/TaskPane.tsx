import { MoreHorizontal, Trash2 } from 'lucide-react'
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
import type { CatTask } from '../../../../shared/tasks'

type TaskMenuAnchor = { task: CatTask; x: number; y: number; trigger: HTMLElement }

/** 任务随对话事件刷新；轮询补足断线，旧回包和卸载后的结果不能覆盖新状态。 */
export function TaskPane({
  agentId,
  onLoad,
  onToggle,
  onDelete,
  onWatch,
}: {
  agentId: string
  onLoad: (agentId: string) => Promise<CatTask[]>
  onToggle: (task: CatTask) => Promise<void>
  onDelete: (task: CatTask) => Promise<void>
  onWatch?: (agentId: string, onChange: () => void) => () => void
}) {
  const [items, setItems] = useState<CatTask[]>([])
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(true)
  const [pending, setPending] = useState<string | null>(null)
  const [menu, setMenu] = useState<TaskMenuAnchor | null>(null)
  const mounted = useRef(false)
  const sequence = useRef({ value: 0 })
  const mutation = useRef(false)
  const refresh = useCallback(async () => {
    const request = ++sequence.current.value
    try {
      const next = await onLoad(agentId)
      if (mounted.current && request === sequence.current.value) {
        setItems(next)
        setError('')
      }
    } catch (failure) {
      if (mounted.current && request === sequence.current.value)
        setError(failure instanceof Error ? failure.message : '任务读取失败，请重试。')
    } finally {
      if (mounted.current && request === sequence.current.value) setLoading(false)
    }
  }, [agentId, onLoad])
  useEffect(() => {
    mounted.current = true
    const initial = setTimeout(() => {
      void refresh()
    }, 0)
    const requests = sequence.current
    const unwatch = onWatch?.(agentId, () => {
      void refresh()
    })
    const timer = setInterval(() => {
      void refresh()
    }, 10_000)
    return () => {
      mounted.current = false
      requests.value++
      clearTimeout(initial)
      clearInterval(timer)
      unwatch?.()
    }
  }, [agentId, refresh, onWatch])
  const closeMenu = useCallback(() => setMenu(null), [])
  async function change(task: CatTask, action: (task: CatTask) => Promise<void>): Promise<void> {
    if (mutation.current) return
    mutation.current = true
    setPending(task.id)
    closeMenu()
    try {
      await action(task)
      await refresh()
    } catch (failure) {
      // 重新读取冲突后的权威状态，但保留操作失败提示，不自动重发修改。
      await refresh()
      if (mounted.current)
        setError(failure instanceof Error ? failure.message : '任务操作失败，请刷新后重试。')
    } finally {
      mutation.current = false
      if (mounted.current) setPending(null)
    }
  }
  return (
    <div className="task-pane" aria-busy={loading || pending !== null}>
      {loading ? (
        <p className="cat-panel-empty" role="status">
          正在读取任务…
        </p>
      ) : items.length === 0 && !error ? (
        <div className="cat-panel-empty" role="status">
          <strong>还没有任务</strong>
          <span>告诉猫咪要做什么、什么时候做，就能添加任务。</span>
        </div>
      ) : null}
      <ul className="task-list">
        {items.map((task) => (
          <li
            key={task.id}
            onContextMenu={(event) => {
              event.preventDefault()
              if (!pending)
                setMenu({
                  task: task,
                  x: event.clientX,
                  y: event.clientY,
                  trigger: event.currentTarget,
                })
            }}
          >
            <div className="task-row">
              <strong>{task.name}</strong>
              <button
                type="button"
                role="switch"
                className="task-toggle"
                disabled={pending !== null}
                aria-checked={task.is_enabled}
                aria-label={task.is_enabled ? `禁用${task.name}` : `启用${task.name}`}
                onClick={() => {
                  void change(task, onToggle)
                }}
              >
                <span />
              </button>
              <button
                type="button"
                className="task-menu-trigger"
                aria-label={`管理${task.name}`}
                disabled={pending !== null}
                aria-haspopup="menu"
                onClick={(event) => {
                  const bounds = event.currentTarget.getBoundingClientRect()
                  setMenu({
                    task: task,
                    x: bounds.left,
                    y: bounds.bottom,
                    trigger: event.currentTarget,
                  })
                }}
              >
                <MoreHorizontal size={16} />
              </button>
            </div>
            <p className="task-prompt">{task.prompt}</p>
            <p className="task-schedule">
              {task.next_run_at
                ? `下次：${formatTime(task.next_run_at, task.timezone)}`
                : task.last_run_at && !task.cron
                  ? '一次性任务已结束'
                  : '已禁用'}
              <span>
                {task.cron ? '重复任务' : '一次性任务'} · {task.timezone}
              </span>
            </p>
            {task.last_run_at ? (
              <p className="task-result">
                最近：{statusText(task.last_status)} · {formatTime(task.last_run_at, task.timezone)}
                {task.last_delivery_status === 'waiting' ? (
                  <span>结果已就绪，将在本轮对话结束后发送。</span>
                ) : null}
                {task.last_error ? <span>本次任务未完成，可在对话中查看或请猫咪调整。</span> : null}
              </p>
            ) : null}
          </li>
        ))}
      </ul>
      {menu ? (
        <TaskMenu
          anchor={menu}
          onClose={closeMenu}
          onDelete={() => {
            void change(menu.task, onDelete)
          }}
        />
      ) : null}
      {error ? (
        <div className="task-error" role="alert">
          <span>{error}</span>
          <button
            type="button"
            disabled={pending !== null}
            onClick={() => {
              void refresh()
            }}
          >
            重新读取
          </button>
        </div>
      ) : null}
    </div>
  )
}
function formatTime(value: string, timezone: string): string {
  return new Intl.DateTimeFormat('zh-CN', {
    timeZone: timezone,
    month: 'numeric',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  }).format(new Date(value))
}
function statusText(status: string): string {
  const labels: Record<string, string> = {
    queued: '等待执行',
    running: '执行中',
    canceling: '正在停止',
    succeeded: '已完成',
    failed: '执行失败',
    canceled: '已停止',
    unknown: '结果待确认',
  }
  return labels[status] ?? '状态待确认'
}

function TaskMenu({
  anchor,
  onDelete,
  onClose,
}: {
  anchor: TaskMenuAnchor
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
      aria-label="任务"
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
