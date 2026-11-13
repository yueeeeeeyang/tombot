import { FormEvent, useState, type ReactNode } from 'react'
import { Avatar } from '../../components/Avatar'
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

/** 填写服务地址、在浏览器里登录、连接失败；没有服务端初始化步骤。 */
export function SessionGate({ session }: { session: Session }) {
  if (session.phase === 'loading') {
    return (
      <SessionFrame>
        <section className="session-card" aria-busy="true">
          <h1>正在连接服务</h1>
          <p role="status">正在确认这台服务是否可以访问。</p>
        </section>
      </SessionFrame>
    )
  }
  if (session.phase === 'address' || session.phase === 'offline') {
    return <ServerForm session={session} />
  }
  return <BrowserLogin session={session} />
}

function ServerForm({ session }: { session: Session }) {
  const [serverUrl, setServerUrl] = useState(session.serverUrl)
  const isOffline = session.phase === 'offline'

  function submit(event: FormEvent): void {
    event.preventDefault()
    void session.connect(serverUrl)
  }

  return (
    <SessionFrame>
      <form className="session-card" onSubmit={submit}>
        <Avatar preset="bombay" size="large" />
        <h1>{isOffline ? '无法连接服务' : '连接服务'}</h1>
        <p>
          {isOffline
            ? '没有连上这台服务。可以修改地址后再试。'
            : '桌面客户端只转发消息。请填写远端服务地址，例如 https://tom.example.com。'}
        </p>
        <label>
          服务地址
          <input
            value={serverUrl}
            autoFocus
            autoComplete="url"
            spellCheck={false}
            placeholder="https://tom.example.com"
            onChange={(event) => setServerUrl(event.target.value)}
          />
        </label>
        {session.message && (
          <p className="form-error" role="alert">
            {session.message}
          </p>
        )}
        <button className="primary-button" type="submit" disabled={session.pending}>
          {isOffline ? '重新连接' : '连接'}
        </button>
      </form>
    </SessionFrame>
  )
}

/**
 * 浏览器登录：口令只在系统浏览器里提交，桌面端等待一次性授权码。
 * 这样客户端不保存口令，也不接触动态验证码以外的任何凭据。
 */
function BrowserLogin({ session }: { session: Session }) {
  return (
    <SessionFrame>
      <section className="session-card">
        <Avatar preset="bombay" size="large" />
        <h1>登录 TomBot</h1>
        <p>点击后会在系统浏览器打开登录页。完成后会自动回到这里。</p>
        {session.message && (
          <p className="form-error" role="alert">
            {session.message}
          </p>
        )}
        <button
          className="primary-button"
          type="button"
          disabled={session.pending}
          onClick={() => void session.startBrowserLogin()}
        >
          {session.pending ? '等待浏览器登录' : '在浏览器中登录'}
        </button>
      </section>
    </SessionFrame>
  )
}
