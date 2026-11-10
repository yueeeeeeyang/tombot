import { useEffect, useState } from 'react'
import { getBackend } from '../desktop/backend'

export const CONNECTION_CHECK_MS = 10_000
export type ConnectionStatus = 'connected' | 'disconnected'

/** 工作空间保持打开；串行探测会话，断线和鉴权失效均标红，成功后自动恢复。 */
export function useConnectionStatus(): ConnectionStatus {
  const [status, setStatus] = useState<ConnectionStatus>('connected')
  const backend = getBackend()
  useEffect(() => {
    let disposed = false
    let checking = false
    let timer: ReturnType<typeof setTimeout>
    async function check(): Promise<void> {
      if (checking || disposed) return
      checking = true
      clearTimeout(timer)
      try {
        const session = await backend.session()
        if (!disposed) setStatus(session.phase === 'ready' ? 'connected' : 'disconnected')
      } catch {
        if (!disposed) setStatus('disconnected')
      } finally {
        checking = false
        if (!disposed) timer = setTimeout(() => void check(), CONNECTION_CHECK_MS)
      }
    }
    const offline = () => setStatus('disconnected')
    const refresh = () => void check()
    void check()
    window.addEventListener('offline', offline)
    window.addEventListener('online', refresh)
    window.addEventListener('focus', refresh)
    return () => {
      disposed = true
      clearTimeout(timer)
      window.removeEventListener('offline', offline)
      window.removeEventListener('online', refresh)
      window.removeEventListener('focus', refresh)
    }
  }, [backend])
  return status
}
