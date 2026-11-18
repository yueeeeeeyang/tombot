import { Eye, Maximize2, Minimize2, Monitor, X } from 'lucide-react'
import { lazy, Suspense, useCallback, useEffect, useRef, useState } from 'react'
import type { ConsoleBackend, DesktopConsole } from '../../../../shared/console'
import { ulid } from '../../../../shared/ulid'
import { Dialog } from '../../components/Dialog'
import { getBackend } from '../desktop/backend'
const ComputerScreen = lazy(() =>
  import('./ComputerScreen').then((module) => ({ default: module.ComputerScreen })),
)

function consoleBackend(): ConsoleBackend {
  const backend = getBackend()
  if (
    !backend.openConsole ||
    !backend.consoleStatus ||
    !backend.closeConsole ||
    !backend.controlConsole ||
    !backend.connectConsole
  )
    throw new Error('当前客户端未连接电脑服务。')
  return backend as ConsoleBackend
}
const labels: Record<DesktopConsole['state'], string> = {
  opening: '正在启动电脑画面',
  viewing: '仅观看',
  waiting: '正在等待猫咪交接',
  pausing: '正在暂停猫咪的操作',
  active: '用户正在操作，猫咪已暂停',
  resuming: '正在恢复猫咪的任务',
  closed: '观看已结束',
  failed: '电脑连接失败',
}
type ConsoleAction = 'connect' | 'control' | 'close'

function isLive(session: DesktopConsole | undefined): session is DesktopConsole {
  return Boolean(session && session.state !== 'closed' && session.state !== 'failed')
}

function logUnconfirmedClose(session: DesktopConsole): void {
  console.warn('关闭电脑观看未确认，将等待服务端租约到期。', {
    botId: session.bot_id,
    consoleId: session.id,
  })
}

/** 面板离开后由短租约兜底，释放失败需留下可定位的状态。 */
async function closeAbandonedConsole(session: DesktopConsole): Promise<void> {
  try {
    await consoleBackend().closeConsole(session.id)
  } catch {
    logUnconfirmedClose(session)
  }
}

/** 关闭观看不会取消任务，接管的恢复必须等待服务端确认，断线则由短租约回收。 */
export function ComputerPane({ agentId, agentName }: { agentId: string; agentName: string }) {
  const [isOpen, setOpen] = useState(false)
  const [isFullscreen, setFullscreen] = useState(false)
  const [isScreenReady, setScreenReady] = useState(false)
  const [hasScreenFailed, setScreenFailed] = useState(false)
  const [session, setSession] = useState<DesktopConsole>()
  const [error, setError] = useState('')
  const [fullscreenError, setFullscreenError] = useState('')
  const [busy, setBusy] = useState(false)
  const ownsFullscreen = useRef(false)
  const current = useRef<DesktopConsole>(undefined)
  const action = useRef<ConsoleAction>(undefined)
  const openRequestId = useRef('')
  const visible = useRef(false)
  const revision = useRef(0)
  const needsRelease = useRef(false)
  const mounted = useRef(true)
  const onScreenReady = useCallback(() => setScreenReady(true), [])

  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
      const active = current.current
      if (isLive(active) && action.current !== 'close') void closeAbandonedConsole(active)
    }
  }, [agentId])
  useEffect(() => {
    if (!isOpen) return
    const update = () =>
      setFullscreen(ownsFullscreen.current && Boolean(document.fullscreenElement))
    document.addEventListener('fullscreenchange', update)
    return () => {
      document.removeEventListener('fullscreenchange', update)
      if (ownsFullscreen.current && document.fullscreenElement) {
        void document.exitFullscreen().catch(() => {
          console.warn('退出电脑全屏未确认。', { botId: agentId })
        })
      }
      ownsFullscreen.current = false
    }
  }, [agentId, isOpen])

  const id = isLive(session) ? session.id : undefined
  useEffect(() => {
    if (!id) return
    let stopped = false
    let timer: ReturnType<typeof setTimeout> | undefined
    async function poll() {
      const startedAt = revision.current
      try {
        const status = await consoleBackend().consoleStatus(id!)
        if (stopped) return
        // 本地动作发出前读到的状态不能覆盖接管或关闭的结果。
        if (!action.current && startedAt === revision.current) {
          current.current = status
          setSession(status)
          if (!isLive(status)) return
        }
      } catch (failure) {
        if (stopped) return
        if (!action.current && startedAt === revision.current)
          setError(failure instanceof Error ? failure.message : '读取电脑状态失败。')
      }
      if (!stopped) timer = setTimeout(() => void poll(), 1000)
    }
    void poll()
    return () => {
      stopped = true
      clearTimeout(timer)
    }
  }, [id])

  function begin(nextAction: ConsoleAction): boolean {
    if (action.current) return false
    revision.current++
    action.current = nextAction
    setBusy(true)
    return true
  }

  function end(): void {
    action.current = undefined
    if (mounted.current) setBusy(false)
  }

  function apply(next: DesktopConsole): void {
    current.current = next
    if (mounted.current) setSession(next)
  }

  function hideViewer(): void {
    visible.current = false
    setFullscreen(false)
    setFullscreenError('')
    setOpen(false)
  }

  async function connect() {
    if (!begin('connect')) return
    setScreenReady(false)
    setScreenFailed(false)
    setError('')
    current.current = undefined
    setSession(undefined)
    try {
      // 响应丢失时复用申请标识，防止重试创建另一份观看租约。
      openRequestId.current ||= ulid()
      const next = await consoleBackend().openConsole(agentId, openRequestId.current)
      openRequestId.current = ''
      if (!mounted.current || !visible.current) {
        // 点击关闭时申请可能还没有返回 ID，拿到后再释放该次观看。
        if (isLive(next)) await closeAbandonedConsole(next)
        return
      }
      apply(next)
    } catch (failure) {
      if (mounted.current && visible.current)
        setError(failure instanceof Error ? failure.message : '连接电脑失败。')
    } finally {
      end()
    }
  }

  function openViewer(): void {
    if (visible.current || action.current || isLive(current.current)) return
    visible.current = true
    setOpen(true)
    void connect()
  }

  async function control(takeover: boolean) {
    const active = current.current
    if (!isLive(active) || !begin('control')) return
    setError('')
    try {
      apply(await consoleBackend().controlConsole(active.id, takeover))
    } catch (failure) {
      if (mounted.current && !needsRelease.current)
        setError(failure instanceof Error ? failure.message : '切换电脑控制失败。')
    } finally {
      end()
      if (needsRelease.current && mounted.current) {
        needsRelease.current = false
        void finish(false)
      }
    }
  }

  async function finish(shouldDismiss: boolean) {
    const active = current.current
    if (!isLive(active)) {
      if (shouldDismiss) hideViewer()
      return
    }
    if (!begin('close')) return
    try {
      apply(await consoleBackend().closeConsole(active.id))
      if (shouldDismiss && mounted.current) hideViewer()
    } catch (failure) {
      if (mounted.current)
        setError(failure instanceof Error ? failure.message : '关闭电脑未确认，请重试。')
      else logUnconfirmedClose(active)
    } finally {
      end()
    }
  }

  function failScreen(message: string): void {
    setScreenReady(false)
    setScreenFailed(true)
    setError(message)
    if (action.current === 'control') needsRelease.current = true
    else if (action.current !== 'close') void finish(false)
  }

  async function toggleFullscreen(): Promise<void> {
    setFullscreenError('')
    try {
      if (document.fullscreenElement) await document.exitFullscreen()
      else {
        ownsFullscreen.current = true
        await document.documentElement.requestFullscreen()
        if (!visible.current) await document.exitFullscreen()
      }
    } catch {
      ownsFullscreen.current = Boolean(document.fullscreenElement)
      if (mounted.current && visible.current) setFullscreenError('切换全屏失败，请重试。')
    }
  }

  const isLiveSession = isLive(session)
  const canShow = isLiveSession && session.state !== 'opening' && !hasScreenFailed
  const canRelease = session && ['waiting', 'pausing', 'active'].includes(session.state)
  const isDismissible = !busy || !isLiveSession
  const title = `${agentName}的电脑`
  const viewerError = error || fullscreenError
  const status = session && labels[session.state]
  const placeholder =
    (hasScreenFailed && '电脑画面已断开') || status || (busy ? '正在连接电脑…' : '电脑连接失败')
  return (
    <div className="computer-pane">
      <button
        type="button"
        className="computer-card"
        aria-label={title}
        aria-haspopup="dialog"
        aria-disabled={busy || isLiveSession}
        onClick={openViewer}
      >
        <span className="computer-card-copy">
          <strong>{title}</strong>
          <span role="status">{status || (busy ? '正在连接' : '未连接')}</span>
        </span>
        <span className="computer-preview" aria-hidden="true">
          <Monitor size={24} strokeWidth={1.5} />
        </span>
      </button>
      {isOpen && (
        <Dialog
          title={title}
          variant="computer"
          isDismissible={isDismissible}
          onClose={() => void finish(true)}
        >
          <div className="computer-viewer">
            <header className="computer-toolbar">
              <button
                type="button"
                aria-label={isFullscreen ? '退出全屏' : '全屏查看'}
                onClick={() => void toggleFullscreen()}
              >
                {isFullscreen ? <Minimize2 size={18} /> : <Maximize2 size={18} />}
              </button>
              <button
                type="button"
                aria-label="关闭观看"
                disabled={!isDismissible}
                onClick={() => void finish(true)}
              >
                <X size={20} />
              </button>
            </header>
            <div className="computer-display">
              {canShow ? (
                <Suspense
                  fallback={
                    <p className="computer-placeholder" role="status">
                      正在加载电脑查看器…
                    </p>
                  }
                >
                  <ComputerScreen
                    backend={consoleBackend()}
                    consoleId={session.id}
                    name={agentName}
                    isControlling={session.state === 'active' && isScreenReady && !busy && !error}
                    onConnected={onScreenReady}
                    onError={failScreen}
                  />
                </Suspense>
              ) : (
                <p className="computer-placeholder" role="status">
                  {placeholder}
                </p>
              )}
            </div>
            <footer className="computer-footer">
              {viewerError && <p role="alert">{viewerError}</p>}
              {canShow && (
                <div className="computer-actions">
                  <span className="computer-view-mode" role="status">
                    <Eye size={15} aria-hidden="true" />
                    {labels[session.state]}
                  </span>
                  {session.state === 'viewing' && (
                    <button
                      type="button"
                      disabled={busy || !isScreenReady}
                      title="网站登录状态与同一用户的其他猫咪共享"
                      onClick={() => void control(true)}
                    >
                      取得控制权
                    </button>
                  )}
                  {canRelease && (
                    <button type="button" disabled={busy} onClick={() => void control(false)}>
                      交还控制权
                    </button>
                  )}
                </div>
              )}
              {!isLiveSession && !busy && (
                <div className="computer-actions">
                  <button type="button" onClick={() => void connect()}>
                    重新连接
                  </button>
                </div>
              )}
            </footer>
          </div>
        </Dialog>
      )}
    </div>
  )
}
