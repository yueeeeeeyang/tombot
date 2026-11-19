import {
  createContext,
  useContext,
  useCallback,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from 'react'
import { Dialog } from '../../components/Dialog'
import { getBackend, type ClientUpdate } from '../desktop/backend'

type UpdateState = {
  release: ClientUpdate | null
  status: string
  error: string
  busy: boolean
  check: () => void
  install: () => void
  download: () => void
}
const UpdateContext = createContext<UpdateState | null>(null)
const checkInterval = 6 * 60 * 60 * 1000

/** 在已连接的源站启动和恢复联网时检查；登录与工作区共用一次检查及提醒状态。 */
export function ClientUpdates({
  connected,
  signedIn = false,
  children,
}: {
  connected: boolean
  signedIn?: boolean
  children: ReactNode
}) {
  const backend = getBackend()
  const [release, setRelease] = useState<ClientUpdate | null>(null)
  const [status, setStatus] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [showDialog, setShowDialog] = useState(false)
  const announced = useRef('')
  const pending = useRef(false)
  const lifecycle = useRef<AbortController | null>(null)
  const check = useCallback(
    async (notify: boolean) => {
      if (pending.current) return
      if (!connected) {
        setError('请先连接服务，再检查更新。')
        return
      }
      const signal = lifecycle.current?.signal
      pending.current = true
      setBusy(true)
      setStatus('正在检查更新…')
      setError('')
      try {
        const latest = await backend.checkClientUpdate()
        if (signal?.aborted) return
        setRelease(latest)
        setStatus(latest ? '' : '当前已是最新版本')
        if (latest && notify && announced.current !== latest.id) {
          announced.current = latest.id
          setShowDialog(true)
        }
      } catch (cause) {
        if (!signal?.aborted) {
          setStatus('')
          setError(cause instanceof Error ? cause.message : '检查更新失败，请重试。')
        }
      } finally {
        if (!signal?.aborted) {
          pending.current = false
          setBusy(false)
        }
      }
    },
    [backend, connected],
  )
  useEffect(() => {
    const controller = new AbortController()
    lifecycle.current = controller
    return () => {
      controller.abort()
      pending.current = false
    }
  }, [])
  useEffect(() => {
    let active = true
    const automatic = () => {
      if (active && connected) void check(true)
    }
    void Promise.resolve().then(automatic)
    const timer = window.setInterval(automatic, checkInterval)
    window.addEventListener('online', automatic)
    const unsubscribe = backend.watchUpdateRequest(() => {
      setShowDialog(true)
      void check(false)
    })
    return () => {
      active = false
      window.clearInterval(timer)
      window.removeEventListener('online', automatic)
      unsubscribe()
    }
  }, [backend, check, connected, signedIn])
  async function update(install: boolean) {
    if (!release || pending.current) return
    const signal = lifecycle.current?.signal
    pending.current = true
    setBusy(true)
    setStatus(install ? '正在下载并校验，安装完成后将自动重启…' : '正在下载安装包…')
    setError('')
    try {
      if (install) {
        await backend.installClientUpdate(release.id, (message) => {
          if (!signal?.aborted) setStatus(message)
        })
        if (!signal?.aborted) setStatus('正在安装并重启客户端…')
      } else {
        const saved = await backend.downloadClientUpdate(release.id)
        if (!signal?.aborted) setStatus(saved ? '安装包已保存，打开后即可升级。' : '')
      }
    } catch (cause) {
      if (!signal?.aborted) {
        setStatus('')
        setError(cause instanceof Error ? cause.message : '升级操作失败，请重试。')
      }
    } finally {
      if (!signal?.aborted) {
        pending.current = false
        setBusy(false)
      }
    }
  }
  return (
    <UpdateContext.Provider
      value={{
        release,
        error,
        status,
        busy,
        check: () => void check(false),
        download: () => void update(false),
        install: () => void update(true),
      }}
    >
      {children}
      {showDialog && (
        <Dialog
          title={release ? '发现客户端新版本' : '客户端更新'}
          onClose={() => setShowDialog(false)}
        >
          <UpdatePanel onDismiss={() => setShowDialog(false)} />
        </Dialog>
      )}
    </UpdateContext.Provider>
  )
}

export function UpdatePanel({ onDismiss }: { onDismiss?: () => void }) {
  const state = useContext(UpdateContext)
  if (!state) throw new Error('客户端更新状态不可用。')
  return (
    <div className="client-update">
      {state.release && (
        <>
          <strong>TomBot {state.release.version}</strong>
          <p className="client-update-notes">{state.release.notes}</p>
        </>
      )}
      {state.release?.installUnavailable && <p>{state.release.installUnavailable}</p>}
      {state.error && (
        <p role="alert" className="form-error">
          {state.error}
        </p>
      )}
      {state.status && <p role="status">{state.status}</p>}
      <div className="client-update-actions">
        {state.release && (
          <>
            <button
              className="primary-button"
              onClick={state.install}
              disabled={state.busy || !!state.release.installUnavailable}
            >
              立即升级
            </button>
            <button className="secondary-button" onClick={state.download} disabled={state.busy}>
              下载安装包
            </button>
          </>
        )}
        {(!onDismiss || !state.release || state.error) && (
          <button className="secondary-button" onClick={state.check} disabled={state.busy}>
            检查更新
          </button>
        )}
        {onDismiss && (
          <button className="secondary-button" onClick={onDismiss}>
            {state.release ? '稍后再说' : '关闭'}
          </button>
        )}
      </div>
    </div>
  )
}
