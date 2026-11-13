import { useEffect, useRef, useState, type FormEvent } from 'react'
import { ShieldCheck } from 'lucide-react'
import { AdminClient, ApiError } from '../api/client'
import type { AdministratorSetup } from '../api/schemas'
import { Alert, Field } from '../components'

const unknownResult = '创建结果尚未确认，请尝试使用刚才设置的用户名、密码和验证器密钥登录。'

/**
 * 首位管理员只在平台还没有任何管理员时创建：由创建者当场设定口令，
 * 动态验证器密钥只在这里展示一次，服务端不保存明文也不再提供读取。
 */
export function AdminSetup({
  client,
  onBack,
}: {
  client: AdminClient
  onBack: (notice: string) => void
}) {
  const [setup, setSetup] = useState<AdministratorSetup>()
  const [uncertain, setUncertain] = useState(false)
  const [busy, setBusy] = useState(false)
  const [visible, setVisible] = useState(false)
  const [error, setError] = useState<unknown>()
  const pending = useRef<AbortController | null>(null)
  const form = useRef<HTMLFormElement>(null)
  const heading = useRef<HTMLHeadingElement>(null)

  useEffect(() => {
    heading.current?.focus()
  }, [setup, uncertain])

  useEffect(() => {
    const clear = () => {
      pending.current?.abort()
      pending.current = null
      form.current?.reset()
      setSetup(undefined)
      setVisible(false)
      setError(undefined)
      setBusy(false)
    }
    window.addEventListener('pagehide', clear)
    return () => {
      window.removeEventListener('pagehide', clear)
      pending.current?.abort()
    }
  }, [])

  function back() {
    pending.current?.abort()
    onBack(uncertain ? unknownResult : '')
  }

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (pending.current) return
    const fields = new FormData(event.currentTarget)
    const username = String(fields.get('username') ?? '').trim()
    const displayName = String(fields.get('display_name') ?? '').trim()
    const password = String(fields.get('password') ?? '')
    const confirmation = String(fields.get('confirmation') ?? '')
    event.currentTarget.reset()
    setError(undefined)
    if (password !== confirmation || [...password].length < 12) {
      setError(new ApiError('AUTH_INPUT_INVALID', '两次密码需一致，且至少包含 12 个字符。'))
      return
    }
    const controller = new AbortController()
    pending.current = controller
    setBusy(true)
    try {
      const created = await client.createFirstAdministrator(
        username,
        displayName,
        password,
        controller.signal,
      )
      if (!controller.signal.aborted) setSetup(created)
    } catch (cause) {
      if (controller.signal.aborted) return
      const rejected =
        cause instanceof ApiError &&
        ['AUTH_INPUT_INVALID', 'ADMINISTRATOR_CONFLICT', 'AUTH_RATE_LIMITED'].includes(cause.code)
      // 回包不明确时不再提供重复提交按钮：再次提交可能建出第二个账号或撞上冲突。
      if (!rejected) setUncertain(true)
      setError(cause)
    } finally {
      if (pending.current === controller) {
        pending.current = null
        setBusy(false)
      }
    }
  }

  return (
    <>
      <div className="brand-icon">
        <ShieldCheck size={28} />
      </div>
      <h1 ref={heading} tabIndex={-1}>
        创建首位管理员
      </h1>
      {uncertain ? (
        <p className="notice" role="status">
          {unknownResult}
        </p>
      ) : setup ? (
        <>
          <p className="description">
            账号已创建。以下设置密钥只显示这一次，请立即记录并添加到验证器，然后返回登录。
          </p>
          <section className="enrollment-setup" aria-label="验证器设置">
            <h2>添加到验证器</h2>
            <p>
              在验证器中手动添加账户，名称填写 <strong>{setup.username}</strong>
              ，再输入下方设置密钥。
            </p>
            <Field label="验证器设置密钥" hint="时间型（TOTP），每 30 秒更新，6 位数字，SHA-1。">
              <input
                value={setup.secret}
                type={visible ? 'text' : 'password'}
                readOnly
                autoComplete="off"
                spellCheck={false}
              />
            </Field>
            <button type="button" className="text-button" onClick={() => setVisible(!visible)}>
              {visible ? '隐藏设置密钥' : '显示设置密钥'}
            </button>
            <p className="notice" role="status">
              关闭本页后无法再次查看该密钥；丢失只能由其他管理员重建账号。
            </p>
          </section>
        </>
      ) : (
        <>
          <p className="description">
            平台还没有管理员账号。创建后即可登录管理平台，之后的管理员在该平台内添加。
          </p>
          <form ref={form} onSubmit={submit}>
            <fieldset className="form-grid" disabled={busy}>
              <Field label="管理员用户名">
                <input
                  name="username"
                  required
                  minLength={3}
                  maxLength={64}
                  pattern="[a-z0-9_.\-]{3,64}"
                  autoComplete="username"
                  spellCheck={false}
                />
              </Field>
              <Field label="显示名称">
                <input name="display_name" required maxLength={120} />
              </Field>
              <Field label="设置密码" hint="至少 12 个字符。">
                <input
                  name="password"
                  type="password"
                  required
                  minLength={12}
                  maxLength={256}
                  autoComplete="new-password"
                />
              </Field>
              <Field label="确认密码">
                <input
                  name="confirmation"
                  type="password"
                  required
                  minLength={12}
                  maxLength={256}
                  autoComplete="new-password"
                />
              </Field>
            </fieldset>
            <Alert error={error} />
            <button className="primary-button" disabled={busy}>
              {busy ? '正在创建…' : '创建管理员'}
            </button>
          </form>
        </>
      )}
      {uncertain && <Alert error={error} />}
      <button type="button" className="secondary-button auth-secondary" onClick={back}>
        返回登录
      </button>
    </>
  )
}
