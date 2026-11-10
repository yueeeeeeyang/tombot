import { useEffect, useState } from 'react'
import { getBackend, type SessionPhase } from '../desktop/backend'

/** 管理初始化、密码验证和离线重试。工作空间只在验证通过后挂载。 */
export function useSession() {
  const backend = getBackend()
  const peeked = backend.trySessionSync?.() ?? null
  const [phase, setPhase] = useState<SessionPhase>(peeked ?? 'loading')
  const [message, setMessage] = useState('')
  const [pending, setPending] = useState(false)

  useEffect(() => {
    if (peeked) return
    let cancelled = false
    void backend.session().then((result) => {
      if (cancelled) return
      setPhase(result.phase)
      setMessage(result.message ?? '')
    })
    return () => {
      cancelled = true
    }
  }, [backend, peeked])

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

  async function retry(): Promise<void> {
    setPending(true)
    setPhase('loading')
    setMessage('')
    try {
      const result = await backend.session()
      setPhase(result.phase)
      setMessage(result.message ?? '')
    } catch (error) {
      setPhase('offline')
      setMessage((error as Error).message)
    } finally {
      setPending(false)
    }
  }

  return { phase, message, pending, submitSetup, submitLogin, retry }
}
