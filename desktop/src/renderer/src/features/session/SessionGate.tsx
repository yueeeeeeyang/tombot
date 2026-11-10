import { FormEvent, useState, type ReactNode } from 'react'
import { Avatar } from '../../components/Avatar'
import type { BootstrapStep } from '../desktop/backend'
import type { useSession } from './useSession'

type Session = ReturnType<typeof useSession>

/**
 * macOS 的 hiddenInset 把红绿灯画在内容区左上角。
 * 门禁页整页可拖，这块和表单必须关掉拖动，否则窗口按钮和输入框都点不到。
 */
export function SessionFrame({ children }: { children: ReactNode }) {
  return (
    <main className="session-gate">
      <div className="session-traffic-lights" aria-hidden="true" />
      {children}
    </main>
  )
}

/** 初始化、密码验证、连接失败和引导进度。布局沿用现有表单和主按钮。 */
export function SessionGate({ session }: { session: Session }) {
  if (session.phase === 'loading') {
    return (
      <SessionFrame>
        <section className="session-card" aria-busy="true">
          <h1>正在连接服务</h1>
          <p role="status">正在确认这台服务是否已经初始化。</p>
        </section>
      </SessionFrame>
    )
  }
  if (session.phase === 'offline') {
    return (
      <SessionFrame>
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
      </SessionFrame>
    )
  }
  if (session.phase === 'login') return <LoginForm session={session} />
  if (session.phase === 'bootstrap') return <BootstrapView session={session} />
  return <SetupForm session={session} />
}

function BootstrapView({ session }: { session: Session }) {
  const steps = session.bootstrap?.steps ?? []
  const completed = steps.filter(
    (step) => step.status === 'ok' || step.status === 'degraded',
  ).length
  const current = steps.find((step) => step.status === 'running')
  const failed = session.bootstrap?.phase === 'failed'
  const notice = session.message || session.bootstrap?.error
  return (
    <SessionFrame>
      <section className="session-card" aria-busy={!failed}>
        <Avatar preset="bombay" size="large" />
        <h1>正在准备服务</h1>
        <p>字体和浏览器还没准备好。完成后才会进入工作空间。</p>
        <p role="status">{`当前阶段：${phaseLabel(session.bootstrap?.phase)}${current ? `，正在${current.title}` : ''}`}</p>
        <progress
          className="session-progress"
          aria-label="引导进度"
          max={Math.max(steps.length, 1)}
          value={completed}
        />
        <p>{steps.length === 0 ? '正在读取步骤' : `已完成 ${completed} / ${steps.length}`}</p>
        {steps.length > 0 && (
          <ol className="session-steps" aria-label="引导步骤">
            {steps.map((step) => (
              <StepRow key={step.id} step={step} />
            ))}
          </ol>
        )}
        {notice && (
          <p className="form-error" role="alert">
            {notice}
          </p>
        )}
        {failed && (
          <button
            className="primary-button"
            type="button"
            disabled={session.pending}
            onClick={() => void session.retryBootstrap()}
          >
            重新准备
          </button>
        )}
      </section>
    </SessionFrame>
  )
}

function StepRow({ step }: { step: BootstrapStep }) {
  return (
    <li className="session-step">
      <span className="session-step-row">
        <span>{step.title}</span>
        <span>{stepLabel(step.status)}</span>
      </span>
      {step.detail && <span className="session-step-detail">{step.detail}</span>}
    </li>
  )
}

function phaseLabel(phase: string | undefined): string {
  const labels: Record<string, string> = {
    pending: '等待启动',
    detecting: '检测环境',
    preparing: '准备依赖',
    selfcheck: '自检中',
    ready: '已就绪',
    failed: '引导失败',
  }
  if (!phase) return '正在读取'
  return labels[phase] ?? '进行中'
}

function stepLabel(status: string): string {
  const labels: Record<string, string> = {
    running: '进行中',
    ok: '已完成',
    failed: '失败',
    degraded: '需关注',
  }
  return labels[status] ?? status
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
    <SessionFrame>
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
    </SessionFrame>
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
    <SessionFrame>
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
    </SessionFrame>
  )
}
