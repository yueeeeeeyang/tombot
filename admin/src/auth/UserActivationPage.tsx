import { useEffect, useRef, useState, type FormEvent } from 'react'
import { AdminClient, ApiError } from '../api/client'
import { Alert, Field } from '../components'
import { BrandAvatar } from '../components/BrandAvatar'
import { ThemePicker } from '../components/ThemePicker'

export const userActivationPath = '/activate'

/** 公开激活入口独立于管理会话，激活材料只从表单读取，不从地址或浏览器存储恢复。 */
export function UserActivationPage({ client }: { client: AdminClient }) {
  const [status, setStatus] = useState<'editing' | 'succeeded' | 'uncertain'>('editing')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>()
  const pending = useRef<AbortController | null>(null)
  const form = useRef<HTMLFormElement>(null)
  const heading = useRef<HTMLHeadingElement>(null)

  useEffect(() => {
    const previousTitle = document.title
    const element = form.current
    document.title = '激活账户 · TomBot'
    const clear = () => {
      // 离开后可能从页面缓存恢复；已发出的写入不能被当作失败重新提交。
      if (pending.current) setStatus('uncertain')
      pending.current?.abort()
      pending.current = null
      element?.reset()
      setBusy(false)
      setError(undefined)
    }
    window.addEventListener('pagehide', clear)
    return () => {
      window.removeEventListener('pagehide', clear)
      pending.current?.abort()
      element?.reset()
      document.title = previousTitle
    }
  }, [])

  useEffect(() => {
    heading.current?.focus()
  }, [status])

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (pending.current || status !== 'editing') return
    const fields = new FormData(event.currentTarget)
    const token = String(fields.get('activation_token') ?? '').trim()
    const password = String(fields.get('password') ?? '')
    const confirmation = String(fields.get('confirmation') ?? '')
    setError(undefined)
    if (!token) {
      setError(new ApiError('AUTH_INPUT_INVALID', '请输入管理员提供的激活码。'))
      return
    }
    if ([...password].length < 12 || new TextEncoder().encode(password).length > 1024) {
      setError(new ApiError('AUTH_INPUT_INVALID', '密码至少包含 12 个字符，最多 1024 字节。'))
      return
    }
    if (password !== confirmation) {
      setError(new ApiError('AUTH_INPUT_INVALID', '两次输入的密码不一致，请重新确认。'))
      return
    }
    event.currentTarget.reset()
    const controller = new AbortController()
    pending.current = controller
    setBusy(true)
    try {
      await client.activate(token, password, controller.signal)
      if (!controller.signal.aborted) setStatus('succeeded')
    } catch (cause) {
      if (controller.signal.aborted) return
      if (cause instanceof ApiError && cause.code === 'ACTIVATION_INVALID') {
        setError(
          new ApiError(
            cause.code,
            '激活码无效、已过期或已使用。若尚未激活，请联系管理员重新签发；若已激活，请直接登录。',
            cause.requestID,
          ),
        )
      } else {
        const rejected =
          cause instanceof ApiError &&
          [
            'AUTH_INPUT_INVALID',
            'AUTH_RATE_LIMITED',
            'ORIGIN_INVALID',
            'HTTPS_REQUIRED',
            'SOURCE_INVALID',
          ].includes(cause.code)
        if (!rejected) setStatus('uncertain')
        setError(cause)
      }
    } finally {
      if (pending.current === controller) {
        pending.current = null
        setBusy(false)
      }
    }
  }

  return (
    <main className="auth-shell">
      <div className="auth-card activation-card">
        <div className="brand-icon">
          <BrandAvatar />
        </div>
        <h1 ref={heading} tabIndex={-1}>
          {status === 'succeeded'
            ? '账户已激活'
            : status === 'uncertain'
              ? '激活结果待确认'
              : '激活 TomBot 账户'}
        </h1>
        {status === 'succeeded' ? (
          <p className="notice" role="status">
            密码已设置，激活码已失效。使用管理员提供的用户名和刚设置的密码登录用户平台，也可在
            TomBot 客户端发起登录。
          </p>
        ) : status === 'uncertain' ? (
          <>
            <p className="notice" role="status">
              未收到明确的完成结果。请先使用用户名和刚设置的密码尝试登录用户平台。若仍提示未激活，请联系管理员确认账户状态并重新签发激活码。
            </p>
            <Alert error={error} />
          </>
        ) : (
          <>
            <p className="description">输入管理员提供的一次性激活码，设置你的登录密码。</p>
            <form ref={form} onSubmit={submit}>
              <fieldset className="form-grid" disabled={busy}>
                <Field label="激活码" hint="激活码仅可使用一次；过期后请联系管理员重新签发。">
                  <input
                    name="activation_token"
                    required
                    maxLength={256}
                    autoComplete="off"
                    autoCapitalize="none"
                    spellCheck={false}
                  />
                </Field>
                <Field label="设置密码" hint="至少 12 个字符。">
                  <input
                    name="password"
                    type="password"
                    required
                    maxLength={1024}
                    autoComplete="new-password"
                  />
                </Field>
                <Field label="确认密码">
                  <input
                    name="confirmation"
                    type="password"
                    required
                    maxLength={1024}
                    autoComplete="new-password"
                  />
                </Field>
              </fieldset>
              <Alert error={error} />
              <button className="primary-button" disabled={busy}>
                {busy ? '正在激活…' : '激活账户'}
              </button>
            </form>
          </>
        )}
        <a className="text-button auth-secondary" href="/login">
          {status === 'succeeded' ? '登录用户平台' : '返回用户登录'}
        </a>
        <ThemePicker />
      </div>
    </main>
  )
}
