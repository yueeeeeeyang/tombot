import { useCallback, useEffect, useState } from 'react'
import { normalizeServerAddress } from '../../../../shared/serverAddress'
import { getBackend, type SessionPhase, type SessionSnapshot } from '../desktop/backend'

/** 没有已保存地址时停在连接页。连不上必须离开加载，不能停在「正在连接服务」。 */
export function useSession() {
  const backend = getBackend()
  const syncSession = backend.trySessionSync?.() ?? null
  const [phase, setPhase] = useState<SessionPhase>(syncSession?.phase ?? 'loading')
  const [serverUrl, setServerUrl] = useState(syncSession?.serverUrl ?? '')
  const [message, setMessage] = useState(syncSession?.message ?? '')
  const [pending, setPending] = useState(false)
  const apply = useCallback((result: SessionSnapshot) => {
    setPhase(result.phase)
    setMessage(result.message ?? '')
    if (result.serverUrl !== undefined) setServerUrl(result.serverUrl)
  }, [])

  useEffect(() => {
    // 内存测试用同步会话，避免首屏闪一下加载。真实客户端没有这个方法，必须自己接住失败。
    if (backend.trySessionSync) return
    let cancelled = false
    void backend
      .session()
      .then((result) => {
        if (cancelled) return
        apply(result)
      })
      .catch((error: unknown) => {
        if (cancelled) return
        setPhase('offline')
        setMessage(
          error instanceof Error ? error.message : '无法连接服务。请确认地址和网络后重试。',
        )
      })
    return () => {
      cancelled = true
    }
  }, [apply, backend])

  async function submitSetup(input: { setupCode: string; password: string }): Promise<void> {
    setPending(true)
    setMessage('')
    try {
      await backend.setup(input)
      await backend.login(input.password)
      setPhase('ready')
    } catch (error) {
      setMessage((error as Error).message)
    } finally {
      setPending(false)
    }
  }

  async function submitLogin(password: string): Promise<void> {
    setPending(true)
    setMessage('')
    try {
      await backend.login(password)
      setPhase('ready')
    } catch (error) {
      setMessage((error as Error).message)
    } finally {
      setPending(false)
    }
  }

  async function connect(input: string): Promise<void> {
    let normalized: string
    try {
      normalized = normalizeServerAddress(input)
    } catch (error) {
      setMessage(error instanceof Error ? error.message : '请输入服务地址。')
      return
    }
    setPending(true)
    setMessage('')
    setServerUrl(normalized)
    setPhase('loading')
    try {
      apply(await backend.connect(normalized))
    } catch (error) {
      const coded = error as { code?: string; message?: string }
      if (coded.code === 'SERVER_URL_INVALID') {
        setPhase('address')
        setMessage(coded.message ?? '请输入服务地址。')
      } else {
        setPhase('offline')
        setMessage(coded.message || '无法连接服务。请确认地址和网络后重试。')
      }
    } finally {
      setPending(false)
    }
  }

  return { phase, serverUrl, message, pending, submitSetup, submitLogin, connect }
}
