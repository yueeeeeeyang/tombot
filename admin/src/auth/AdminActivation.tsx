import { useEffect, useRef, useState, type FormEvent } from 'react'
import { ShieldCheck } from 'lucide-react'
import { AdminClient, ApiError } from '../api/client'
import type { Enrollment } from '../api/schemas'
import { Alert, Field } from '../components'

const unknownResult = '激活结果尚未确认，请使用刚设置的密码和验证器的新动态验证码尝试登录。'

/** 材料只存在于本次表单内；离开页面即取消请求并卸载，不能写入地址或浏览器存储。 */
export function AdminActivation({
  client,
  onBack,
}: {
  client: AdminClient
  onBack: (notice: string) => void
}) {
  const [enrollment, setEnrollment] = useState<{ token: string; setup: Enrollment }>()
  const [busy, setBusy] = useState(false)
  const [visible, setVisible] = useState(false)
  const [uncertain, setUncertain] = useState(false)
  const [error, setError] = useState<unknown>()
  const pending = useRef<{ controller: AbortController; isActivation: boolean } | null>(null)
  const form = useRef<HTMLFormElement>(null)
  const heading = useRef<HTMLHeadingElement>(null)

  useEffect(() => {
    heading.current?.focus()
  }, [enrollment, uncertain])

  useEffect(() => {
    // 页面进入历史缓存时也清除材料，防止后退恢复已经离开的登记画面。
    const clear = () => {
      const isActivation = pending.current?.isActivation ?? false
      pending.current?.controller.abort()
      pending.current = null
      form.current?.reset()
      setEnrollment(undefined)
      setVisible(false)
      setUncertain(isActivation)
      setError(undefined)
      setBusy(false)
    }
    window.addEventListener('pagehide', clear)
    return () => {
      window.removeEventListener('pagehide', clear)
      pending.current?.controller.abort()
    }
  }, [])

  function back() {
    const isActivation = pending.current?.isActivation ?? false
    pending.current?.controller.abort()
    onBack(isActivation || uncertain ? unknownResult : '')
  }

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (pending.current || uncertain) return
    const fields = new FormData(event.currentTarget)
    const token = enrollment?.token ?? String(fields.get('activation_token') ?? '').trim()
    const password = String(fields.get('password') ?? '')
    const confirmation = String(fields.get('confirmation') ?? '')
    const code = String(fields.get('code') ?? '')
    event.currentTarget.reset()
    setError(undefined)
    if (enrollment && (password !== confirmation || [...password].length < 12)) {
      setError(new ApiError('AUTH_INPUT_INVALID', '两次密码需一致，且至少包含 12 个字符。'))
      return
    }
    const controller = new AbortController()
    const isActivation = enrollment !== undefined
    pending.current = { controller, isActivation }
    setBusy(true)
    try {
      if (isActivation) {
        await client.activate(token, password, code, controller.signal)
        if (!controller.signal.aborted)
          onBack('账户已激活。请等待验证器生成新的动态验证码，再登录管理平台。')
      } else {
        const setup = await client.beginEnrollment(token, controller.signal)
        if (!controller.signal.aborted) setEnrollment({ token, setup })
      }
    } catch (cause) {
      if (controller.signal.aborted) return
      const rejected =
        cause instanceof ApiError &&
        [
          'AUTH_INPUT_INVALID',
          'AUTH_INVALID',
          'ACTIVATION_INVALID',
          'AUTH_FORBIDDEN',
          'AUTH_RATE_LIMITED',
        ].includes(cause.code)
      if (isActivation && !rejected) {
        // 回包不明确时不再提供重复提交按钮；核实登录才不会误把已消费的激活当作失败。
        setEnrollment(undefined)
        setVisible(false)
        setUncertain(true)
      }
      if (cause instanceof ApiError && cause.code === 'ACTIVATION_INVALID') {
        setEnrollment(undefined)
        setVisible(false)
      }
      setError(cause)
    } finally {
      if (pending.current?.controller === controller) {
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
        激活管理员账户
      </h1>
      {uncertain ? (
        <p className="notice" role="status">
          {unknownResult}
        </p>
      ) : (
        <>
          <p className="description">
            {enrollment
              ? '将账户添加到验证器，然后设置密码并完成验证。'
              : '输入部署人员或管理员提供的一次性激活材料，开始设置账户。'}
          </p>
          <ol className="activation-steps" aria-label="账户激活步骤">
            <li aria-current={!enrollment ? 'step' : undefined}>1 验证材料</li>
            <li aria-current={enrollment ? 'step' : undefined}>2 设置账户</li>
          </ol>
          <form ref={form} onSubmit={submit} key={enrollment ? 'setup' : 'material'}>
            <fieldset className="form-grid" disabled={busy}>
              {enrollment ? (
                <>
                  <section className="enrollment-setup" aria-label="验证器设置">
                    <h2>添加到验证器</h2>
                    <p>在验证器中手动添加账户，名称填写管理员用户名，再输入下方设置密钥。</p>
                    <Field
                      label="验证器设置密钥"
                      hint="时间型（TOTP），每 30 秒更新，6 位数字，SHA-1。"
                    >
                      <input
                        value={enrollment.setup.secret}
                        type={visible ? 'text' : 'password'}
                        readOnly
                        autoComplete="off"
                        spellCheck={false}
                      />
                    </Field>
                    <button
                      type="button"
                      className="text-button"
                      onClick={() => setVisible(!visible)}
                    >
                      {visible ? '隐藏设置密钥' : '显示设置密钥'}
                    </button>
                  </section>
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
                  <Field label="验证器动态码">
                    <input
                      name="code"
                      required
                      inputMode="numeric"
                      pattern="[0-9]{6}"
                      maxLength={6}
                      autoComplete="one-time-code"
                    />
                  </Field>
                </>
              ) : (
                <Field label="一次性激活材料" hint="材料仅用于本次激活，不会保存到浏览器。">
                  <input
                    name="activation_token"
                    type="password"
                    required
                    maxLength={256}
                    autoComplete="off"
                    spellCheck={false}
                  />
                </Field>
              )}
            </fieldset>
            <Alert error={error} />
            <button className="primary-button" disabled={busy}>
              {busy ? '正在提交…' : enrollment ? '激活账户' : '继续设置'}
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
