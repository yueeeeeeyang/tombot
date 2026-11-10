import { useEffect, useState } from 'react'
import { getBackend, type BootstrapStatus, type SessionPhase } from '../desktop/backend'

/** 先完成初始化和密码验证，再等主机引导。引导未完成时不进入工作空间。 */
export function useSession() {
  const backend = getBackend()
  const peeked = backend.trySessionSync?.() ?? null
  const [phase, setPhase] = useState<SessionPhase>(peeked ?? 'loading')
  const [bootstrap, setBootstrap] = useState<BootstrapStatus | undefined>(
    backend.tryBootstrapSync?.(),
  )
  const [message, setMessage] = useState('')
  const [pending, setPending] = useState(false)

  useEffect(() => {
    // 同步快照已经给出引导进度时不再补拉一次，否则晚到的旧快照会盖住后续事件。
    if (peeked) return
    let cancelled = false
    void backend
      .session()
      .then((result) => {
        if (cancelled) return
        setPhase(result.phase)
        setMessage(result.message ?? '')
        setBootstrap(result.bootstrap)
      })
      .catch((error: Error) => {
        if (cancelled) return
        setPhase('offline')
        setMessage(error.message)
      })
    return () => {
      cancelled = true
    }
  }, [backend, peeked])

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
      await refresh()
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
      await refresh()
    } catch (error) {
      setMessage((error as Error).message)
    } finally {
      setPending(false)
    }
  }

  async function retry(): Promise<void> {
    setPending(true)
    setPhase('loading')
    setMessage('')
    try {
      await refresh()
    } catch (error) {
      setPhase('offline')
      setMessage((error as Error).message)
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

  async function refresh(): Promise<void> {
    const result = await backend.session()
    setPhase(result.phase)
    setMessage(result.message ?? '')
    setBootstrap(result.bootstrap)
  }

  const visibleMessage =
    phase === 'bootstrap' && !backend.watchBootstrap ? '无法订阅引导进度。' : message

  return {
    phase,
    bootstrap,
    message: visibleMessage,
    pending,
    submitSetup,
    submitLogin,
    retry,
    retryBootstrap,
  }
}
