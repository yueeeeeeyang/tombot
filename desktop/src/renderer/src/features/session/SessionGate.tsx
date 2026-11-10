import { FormEvent, useState } from 'react'
import { Avatar } from '../../components/Avatar'
import type { useSession } from './useSession'

type Session = ReturnType<typeof useSession>

/** 初始化、密码验证和连接失败。布局沿用现有表单和主按钮，不改工作空间界面。 */
export function SessionGate({ session }: { session: Session }) {
  if (session.phase === 'loading') {
    return (
      <main className="session-gate">
        <section className="session-card" aria-busy="true">
          <h1>正在连接服务</h1>
          <p role="status">正在确认这台服务是否已经初始化。</p>
        </section>
      </main>
    )
  }
  if (session.phase === 'offline') {
    return (
      <main className="session-gate">
        <section className="session-card">
          <Avatar preset="bombay" size="large" />
          <h1>无法连接服务</h1>
          <p>TomBot 需要本机服务才能打开工作空间。</p>
          {session.message && (
            <p className="form-error" role="alert">
              {session.message}
            </p>
          )}
          <button
            className="primary-button"
            type="button"
            disabled={session.pending}
            onClick={() => void session.retry()}
          >
            重新连接
          </button>
        </section>
      </main>
    )
  }
  if (session.phase === 'login') return <LoginForm session={session} />
  return <SetupForm session={session} />
}

function SetupForm({ session }: { session: Session }) {
  const [setupCode, setSetupCode] = useState('')
  const [password, setPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [localError, setLocalError] = useState('')

  function submit(event: FormEvent): void {
    event.preventDefault()
    if (!setupCode.trim()) {
      setLocalError('请输入初始化码。')
      return
    }
    if ([...password].length < 12) {
      setLocalError('密码至少 12 个字符。')
      return
    }
    if (password !== confirmPassword) {
      setLocalError('两次输入的密码不一致。')
      return
    }
    setLocalError('')
    void session.submitSetup({ setupCode: setupCode.trim(), password })
  }

  return (
    <main className="session-gate">
      <form className="session-card" onSubmit={submit}>
        <Avatar preset="bombay" size="large" />
        <h1>初始化 TomBot</h1>
        <p>在服务启动日志里找到一次性初始化码。密码至少 12 位，只保存在服务端。</p>
        <label>
          初始化码
          <input
            value={setupCode}
            autoComplete="off"
            spellCheck={false}
            onChange={(event) => setSetupCode(event.target.value)}
          />
        </label>
        <label>
          密码
          <input
            type="password"
            value={password}
            autoComplete="new-password"
            onChange={(event) => setPassword(event.target.value)}
          />
        </label>
        <label>
          确认密码
          <input
            type="password"
            value={confirmPassword}
            autoComplete="new-password"
            onChange={(event) => setConfirmPassword(event.target.value)}
          />
        </label>
        {(localError || session.message) && (
          <p className="form-error" role="alert">
            {localError || session.message}
          </p>
        )}
        <button className="primary-button" type="submit" disabled={session.pending}>
          完成初始化
        </button>
      </form>
    </main>
  )
}

function LoginForm({ session }: { session: Session }) {
  const [password, setPassword] = useState('')
  const [localError, setLocalError] = useState('')

  function submit(event: FormEvent): void {
    event.preventDefault()
    if (!password) {
      setLocalError('请输入密码。')
      return
    }
    setLocalError('')
    void session.submitLogin(password)
  }

  return (
    <main className="session-gate">
      <form className="session-card" onSubmit={submit}>
        <Avatar preset="bombay" size="large" />
        <h1>验证密码</h1>
        <p>这台服务已经初始化。输入管理员密码后即可打开工作空间。</p>
        <label>
          密码
          <input
            type="password"
            value={password}
            autoComplete="current-password"
            onChange={(event) => setPassword(event.target.value)}
          />
        </label>
        {(localError || session.message) && (
          <p className="form-error" role="alert">
            {localError || session.message}
          </p>
        )}
        <button className="primary-button" type="submit" disabled={session.pending}>
          进入工作空间
        </button>
      </form>
    </main>
  )
}
