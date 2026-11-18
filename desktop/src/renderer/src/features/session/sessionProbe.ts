import type { DesktopBackend, SessionSnapshot } from '../desktop/backend'

export const CONNECTION_CHECK_MS = 10_000

const TRANSIENT_SESSION_ERRORS = new Set([
  'OFFLINE',
  'CONNECTION_TIMEOUT',
  'CONNECTION_REFUSED',
  'CONNECTION_DNS',
  'SERVICE_UNAVAILABLE',
  'IDENTITY_UNAVAILABLE',
  'AUTH_RATE_LIMITED',
])

/** 识别网络错误和服务暂时不可用，区别需用户处理的失败。 */
export function isTransientSessionError(code: string | undefined): boolean {
  return code !== undefined && TRANSIENT_SESSION_ERRORS.has(code)
}

/** 读取主进程的会话快照，服务桥异常也要呈现为离线。 */
export async function readSession(backend: DesktopBackend): Promise<SessionSnapshot> {
  try {
    return await backend.session()
  } catch (error) {
    return {
      phase: 'offline',
      message: error instanceof Error ? error.message : '无法连接服务。请确认地址和网络后重试。',
      errorCode: error instanceof Error ? (error as Error & { code?: string }).code : undefined,
    }
  }
}

/** 串行探测会话。网络恢复或窗口聚焦时提前探测，回调返回 false 时停止。 */
export function watchSession(
  backend: DesktopBackend,
  onSnapshot: (snapshot: SessionSnapshot) => boolean,
  checkImmediately = true,
): () => void {
  let isStopped = false
  let isChecking = false
  let hasQueuedCheck = false
  let timer: ReturnType<typeof setTimeout> | undefined

  async function check(): Promise<void> {
    isChecking = true
    const snapshot = await readSession(backend)
    isChecking = false
    if (isStopped) return
    if (!onSnapshot(snapshot)) {
      stop()
      return
    }
    // 进行中的探测可能命中已经失效的连接，将多次唤醒合并为一次后续探测。
    timer = setTimeout(refresh, hasQueuedCheck ? 0 : CONNECTION_CHECK_MS)
    hasQueuedCheck = false
  }

  function refresh(): void {
    if (isStopped) return
    if (isChecking) {
      hasQueuedCheck = true
      return
    }
    clearTimeout(timer)
    void check()
  }

  function stop(): void {
    isStopped = true
    clearTimeout(timer)
    window.removeEventListener('online', refresh)
    window.removeEventListener('focus', refresh)
  }

  window.addEventListener('online', refresh)
  window.addEventListener('focus', refresh)
  if (checkImmediately) refresh()
  else timer = setTimeout(refresh, CONNECTION_CHECK_MS)
  return stop
}
