import { useCallback, useEffect, useRef, useState } from 'react'
import { normalizeServerAddress } from '../../../../shared/serverAddress'
import { getBackend, type SessionPhase, type SessionSnapshot } from '../desktop/backend'
import { isTransientSessionError, readSession, watchSession } from './sessionProbe'

/** 证书与其他明确拒绝需要用户处理，后台只探测暂时不可用的会话。 */
function isTemporarySessionFailure({
  phase,
  errorCode,
}: Pick<SessionSnapshot, 'phase' | 'errorCode'>): boolean {
  return phase === 'offline' && (!errorCode || isTransientSessionError(errorCode))
}

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
  const signingOut = useRef(false)
  const stopSessionRecovery = useRef<(() => void) | undefined>(undefined)
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
    void readSession(backend).then((result) => {
      if (!cancelled) apply(result)
    })
    return () => {
      cancelled = true
    }
  }, [apply, backend])

  const canRecover =
    Boolean(serverUrl) && !pending && isTemporarySessionFailure({ phase, errorCode })
  useEffect(() => {
    if (!canRecover) return
    const stop = watchSession(
      backend,
      (result) => {
        apply(result)
        return isTemporarySessionFailure(result)
      },
      false,
    )
    stopSessionRecovery.current = stop
    return () => {
      stop()
      stopSessionRecovery.current = undefined
    }
  }, [apply, backend, canRecover, serverUrl])

  /** 用户开始修改地址时停掉已发出的探测的回调，避免迟到结果替换正在编辑的页面。 */
  function pauseRecovery(): void {
    stopSessionRecovery.current?.()
  }

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
    pauseRecovery()
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

  /** 撤销会话后重新确认登录状态；撤销失败也可能已清除本地凭据，不能继续展示工作空间。 */
  async function signOut(): Promise<void> {
    if (signingOut.current) return
    signingOut.current = true
    setPending(true)
    setMessage('')
    let failure = ''
    try {
      await backend.logout()
    } catch (error) {
      failure = error instanceof Error ? error.message : '退出登录失败，请重试。'
    }
    try {
      apply(await backend.session())
      if (failure) setMessage(failure)
    } catch (error) {
      setPhase('offline')
      setMessage(error instanceof Error ? error.message : '无法确认登录状态，请重新连接。')
    } finally {
      signingOut.current = false
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
    pauseRecovery,
    changeServer,
    signOut,
  }
}
