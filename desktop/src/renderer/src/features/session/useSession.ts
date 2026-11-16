import { useCallback, useEffect, useState } from 'react'
import { normalizeServerAddress } from '../../../../shared/serverAddress'
import { getBackend, type SessionPhase, type SessionSnapshot } from '../desktop/backend'

/** 先确认服务地址，再让用户在系统浏览器里完成登录；未登录不进入工作空间。 */
export function useSession() {
  const backend = getBackend()
  const syncSession = backend.trySessionSync?.() ?? null
  const [phase, setPhase] = useState<SessionPhase>(syncSession?.phase ?? 'loading')
  const [serverUrl, setServerUrl] = useState(syncSession?.serverUrl ?? '')
  const [message, setMessage] = useState(syncSession?.message ?? '')
  const [errorCode, setErrorCode] = useState(syncSession?.errorCode ?? '')
  const [certificateNotice, setCertificateNotice] = useState('')
  const [pending, setPending] = useState(false)
  const apply = useCallback((result: SessionSnapshot) => {
    setPhase(result.phase)
    setMessage(result.message ?? '')
    setErrorCode(result.errorCode ?? '')
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

  /** 打开系统浏览器登录；授权完成后客户端自动拿到会话并进入工作空间。 */
  async function startBrowserLogin(): Promise<void> {
    setPending(true)
    setMessage('')
    setCertificateNotice('')
    try {
      apply(await backend.browserLogin())
    } catch (error) {
      setMessage((error as Error).message)
      setErrorCode((error as { code?: string }).code ?? '')
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
    setErrorCode('')
    setCertificateNotice('')
    setServerUrl(normalized)
    setPhase('loading')
    try {
      apply(await backend.connect(normalized))
    } catch (error) {
      const coded = error as { code?: string; message?: string }
      setErrorCode(coded.code ?? '')
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

  /** 登录进行中不能切换服务，避免回调与接收授权码的服务不一致。 */
  function changeServer(): void {
    if (pending) return
    setMessage('')
    setErrorCode('')
    setCertificateNotice('')
    setPhase('address')
  }

  /** 只把用户核对的指纹交给主进程；成功后重新检查会话，不自动重发登录请求。 */
  async function trustCertificate(fingerprint: string): Promise<void> {
    if (pending) return
    setPending(true)
    try {
      await backend.trustServerCertificate(serverUrl, fingerprint)
      const result = await backend.connect(serverUrl)
      apply(result)
      setCertificateNotice(result.phase === 'login' ? '证书已验证并保存，可以继续登录。' : '')
    } catch (error) {
      setMessage((error as Error).message)
    } finally {
      setPending(false)
    }
  }

  /** 本地退出：服务端会撤销会话，本地凭据随即清除。 */
  async function signOut(): Promise<void> {
    if (!backend.logout) return
    setPending(true)
    try {
      await backend.logout()
      apply(await backend.session())
    } catch (error) {
      setMessage((error as Error).message)
    } finally {
      setPending(false)
    }
  }

  return {
    phase,
    serverUrl,
    message,
    needsCertificateTrust: [
      'CERTIFICATE_UNTRUSTED',
      'CERTIFICATE_CHANGED',
      'CERTIFICATE_STORE_INVALID',
      'CERTIFICATE_EXPIRED',
      'CERTIFICATE_HOSTNAME',
    ].includes(errorCode),
    certificateNotice,
    trustCertificate,
    pending,
    startBrowserLogin,
    connect,
    changeServer,
    signOut,
  }
}
