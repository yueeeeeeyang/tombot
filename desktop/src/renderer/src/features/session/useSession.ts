import { useCallback, useEffect, useState } from 'react'
import { normalizeServerAddress } from '../../../../shared/serverAddress'
import {
  getBackend,
  type BootstrapStatus,
  type SessionPhase,
  type SessionSnapshot,
} from '../desktop/backend'

/** 先确认服务地址和身份，再等主机引导。引导未完成时不进入工作空间。 */
export function useSession() {
  const backend = getBackend()
  const syncSession = backend.trySessionSync?.() ?? null
  const [phase, setPhase] = useState<SessionPhase>(syncSession?.phase ?? 'loading')
  const [serverUrl, setServerUrl] = useState(syncSession?.serverUrl ?? '')
  const [bootstrap, setBootstrap] = useState<BootstrapStatus | undefined>(syncSession?.bootstrap)
  const [message, setMessage] = useState(syncSession?.message ?? '')
  const [pending, setPending] = useState(false)
  const apply = useCallback((result: SessionSnapshot) => {
    setPhase(result.phase)
    setMessage(result.message ?? '')
    if (result.serverUrl !== undefined) setServerUrl(result.serverUrl)
    setBootstrap(result.bootstrap)
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

  useEffect(() => {
    if (phase !== 'bootstrap' || !backend.watchBootstrap) return
    let cancelled = false
    // 必须作为方法调用，内存实现依赖 this；拆成变量再调用会丢掉接收者。
    const stop = backend.watchBootstrap(
      (status) => {
        if (cancelled) return
        setBootstrap(status)
        setMessage(status.phase === 'failed' ? (status.error ?? '') : '')
        if (status.phase === 'ready') setPhase('ready')
      },
      (error) => {
        if (cancelled) return
        setMessage(error.message)
      },
    )
    return () => {
      cancelled = true
      stop()
    }
  }, [backend, phase])

  async function submitSetup(input: { setupCode: string; password: string }): Promise<void> {
    setPending(true)
    setMessage('')
    try {
      await backend.setup(input)
      await backend.login(input.password)
      apply(await backend.session())
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
      apply(await backend.session())
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

  async function retryBootstrap(): Promise<void> {
    if (!backend.startBootstrap) {
      setMessage('无法重新准备。')
      return
    }
    setPending(true)
    setMessage('')
    try {
      // 接口接受后就返回，结果由引导事件推进，不能把 202 当成已经完成。
      await backend.startBootstrap()
    } catch (error) {
      setMessage((error as Error).message)
    } finally {
      setPending(false)
    }
  }

  const visibleMessage =
    phase === 'bootstrap' && !backend.watchBootstrap ? '无法订阅引导进度。' : message

  return {
    phase,
    serverUrl,
    bootstrap,
    message: visibleMessage,
    pending,
    submitSetup,
    submitLogin,
    connect,
    retryBootstrap,
  }
}
