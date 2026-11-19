import {
  cloneElement,
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
  type ReactElement,
  type ReactNode,
  type RefObject,
} from 'react'
import { X } from 'lucide-react'
import type { z } from 'zod'
import { AdminClient, ApiError, errorMessage, isVerificationCanceled } from './api/client'

export function Alert({ error }: { error: unknown }) {
  if (!error || isVerificationCanceled(error)) return null
  return (
    <div className="form-error" role="alert">
      {errorMessage(error)}
      {error instanceof ApiError && error.requestID && <small>请求编号：{error.requestID}</small>}
    </div>
  )
}
export function Field({
  label,
  children,
  hint,
}: {
  label: string
  children: ReactElement<{ id?: string; 'aria-describedby'?: string }>
  hint?: string
}) {
  const id = useId()
  return (
    <div className="field">
      <label htmlFor={id}>{label}</label>
      {cloneElement(children, { id, ...(hint ? { 'aria-describedby': `${id}-hint` } : {}) })}
      {hint && <small id={`${id}-hint`}>{hint}</small>}
    </div>
  )
}
export function Dialog({
  title,
  onClose,
  children,
  busy = false,
  className = '',
  returnFocusRef,
}: {
  title: string
  onClose: () => void
  children: ReactNode
  busy?: boolean
  className?: string
  returnFocusRef?: RefObject<HTMLElement | null>
}) {
  const ref = useRef<HTMLDialogElement>(null)
  useLayoutEffect(() => {
    const element = ref.current
    const trigger = returnFocusRef?.current ?? document.activeElement
    element?.showModal()
    return () => {
      element?.close()
      if (trigger instanceof HTMLElement && trigger.isConnected)
        trigger.focus({ preventScroll: true })
    }
  }, [returnFocusRef])
  return (
    <dialog
      ref={ref}
      className={`dialog ${className}`}
      aria-label={title}
      onCancel={(event) => {
        event.preventDefault()
        if (!busy) onClose()
      }}
    >
      <div className="dialog-heading">
        <h2>{title}</h2>
        <button
          type="button"
          className="icon-button"
          onClick={onClose}
          disabled={busy}
          aria-label="关闭弹窗"
        >
          <X size={18} />
        </button>
      </div>
      {children}
    </dialog>
  )
}

export function useQuery<T>(
  client: AdminClient,
  path: string,
  schema: z.ZodType<T>,
  { retainOnRefresh = false } = {},
) {
  const [revision, setRevision] = useState(0)
  const [result, setResult] = useState<{
    path: string
    revision: number
    value?: T
    error?: unknown
  }>()
  const reload = useCallback(() => setRevision((current) => current + 1), [])
  useEffect(() => {
    const controller = new AbortController()
    client.request(path, schema, { signal: controller.signal }).then(
      (value) => {
        if (!controller.signal.aborted) setResult({ path, revision, value })
      },
      (error: unknown) => {
        if (!controller.signal.aborted) setResult({ path, revision, error })
      },
    )
    return () => controller.abort()
  }, [client, path, schema, revision])
  const current = result?.path === path && result.revision === revision
  return {
    value: current || (retainOnRefresh && result?.path === path) ? result?.value : undefined,
    error: current ? result.error : undefined,
    reload,
  }
}

export function LoadState({ error, onRetry }: { error: unknown; onRetry: () => void }) {
  return error ? (
    <div className="empty-state">
      <Alert error={error} />
      <button className="secondary-button" onClick={onRetry}>
        重新读取
      </button>
    </div>
  ) : (
    <p role="status" className="empty-state">
      正在读取…
    </p>
  )
}

export const statusNames: Record<string, string> = {
  active: '正常',
  enabled: '已启用',
  disabled: '已停用',
  provisioning: '正在创建电脑',
  provision_failed: '创建失败',
  pending_activation: '待激活',
  deleting: '删除中',
  deleted: '已删除',
  pending: '等待创建',
  running: '正在创建',
  reconciling: '正在核对资源',
  compensating: '正在回收资源',
  succeeded: '已完成',
  failed: '失败，资源已回收',
  quarantined: '已隔离，等待核对',
  ready: '就绪',
  bootstrapping: '引导中',
  draining: '停止接单',
  offline: '离线',
  error: '异常',
  stopped: '已停止',
  starting: '启动中',
  stopping: '停止中',
}
export function Status({ value }: { value: string | null }) {
  return (
    <span
      className={`status ${value && ['failed', 'quarantined', 'error', 'provision_failed'].includes(value) ? 'status--error' : ''}`}
    >
      {value ? (statusNames[value] ?? '状态待确认') : '尚未供给'}
    </span>
  )
}
export function Pagination({
  before,
  onBefore,
  last,
  count,
}: {
  before: string
  onBefore: (value: string) => void
  last: string | undefined
  count: number
}) {
  if (count === 0 && !before) return null
  return (
    <div className={`pagination${count === 0 ? ' pagination--empty' : ''}`}>
      {count > 0 && <span>本页 {count} 条</span>}
      {before && (
        <button type="button" className="secondary-button" onClick={() => onBefore('')}>
          返回首页
        </button>
      )}
      {count === 50 && last && (
        <button type="button" className="secondary-button" onClick={() => onBefore(last)}>
          下一页
        </button>
      )}
    </div>
  )
}
export function pagePath(path: string, before: string) {
  return `${path}?limit=50${before ? `&before=${encodeURIComponent(before)}` : ''}`
}
