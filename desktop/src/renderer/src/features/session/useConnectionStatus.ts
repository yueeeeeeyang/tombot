import { useEffect, useEffectEvent, useState } from 'react'
import { getBackend } from '../desktop/backend'
import { watchSession } from './sessionProbe'

export type ConnectionStatus = 'connected' | 'disconnected'

/** 工作空间保持打开；串行探测会话，断线和鉴权失效均标红，成功后自动恢复。 */
export function useConnectionStatus(onReady?: () => void): ConnectionStatus {
  const [status, setStatus] = useState<ConnectionStatus>('connected')
  const backend = getBackend()
  const notifyReady = useEffectEvent(() => onReady?.())
  useEffect(() => {
    const stop = watchSession(backend, (session) => {
      setStatus(session.phase === 'ready' ? 'connected' : 'disconnected')
      if (session.phase === 'ready') notifyReady()
      return true
    })
    const offline = () => setStatus('disconnected')
    window.addEventListener('offline', offline)
    return () => {
      stop()
      window.removeEventListener('offline', offline)
    }
  }, [backend])
  return status
}
