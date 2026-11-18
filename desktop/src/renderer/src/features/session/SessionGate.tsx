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
  if (session.needsCertificateTrust && session.serverUrl) {
    return <BrowserLogin session={session} />
  }
  if (session.phase === 'address' || session.phase === 'offline' || !session.serverUrl) {
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
            : '请填写或确认管理员提供的服务端地址，然后继续登录。'}
        </p>
        <label>
          服务地址
          <input
            value={serverUrl}
            autoFocus
            autoComplete="url"
            spellCheck={false}
            placeholder="https://tom.example.com"
            onChange={(event) => {
              session.pauseRecovery()
              setServerUrl(event.target.value)
            }}
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
 * 口令不经过桌面进程；访问和刷新凭据只交给主进程保存。
 */
function BrowserLogin({ session }: { session: Session }) {
  return (
    <SessionFrame>
      <section className="session-card">
        <Avatar preset="bombay" size="large" />
        <h1>登录 TomBot</h1>
        <p>点击后会在系统浏览器打开登录页。完成后会自动回到这里。</p>
        <div className="session-server">
          <div className="session-server-address">
            <span>服务地址</span>
            <strong>{session.serverUrl}</strong>
          </div>
          <button
            className="text-button"
            type="button"
            disabled={session.pending}
            onClick={session.changeServer}
          >
            修改地址
          </button>
        </div>
        {session.message && (
          <p className="form-error" role="alert">
            {session.message}
          </p>
        )}
        {session.certificateNotice && <p role="status">{session.certificateNotice}</p>}
        {session.needsCertificateTrust ? (
          <CertificateForm session={session} />
        ) : (
          <button
            className="primary-button"
            type="button"
            disabled={session.pending}
            onClick={() => void session.startBrowserLogin()}
          >
            {session.pending ? '等待浏览器登录' : '在浏览器中登录'}
          </button>
        )}
      </section>
    </SessionFrame>
  )
}

/** 指纹必须来自管理员，不能把网络读到的指纹直接当作信任依据。 */
function CertificateForm({ session }: { session: Session }) {
  const [fingerprint, setFingerprint] = useState('')
  return (
    <form
      className="session-certificate"
      onSubmit={(event) => {
        event.preventDefault()
        void session.trustCertificate(fingerprint)
      }}
    >
      <p id="certificate-help">
        请向管理员获取 <code>tomd status</code> 显示的证书指纹。验证后仅信任上述服务地址。
      </p>
      <label>
        证书 SHA-256 指纹
        <input
          value={fingerprint}
          onChange={(event) => setFingerprint(event.target.value)}
          aria-describedby="certificate-help"
          autoComplete="off"
          spellCheck={false}
          placeholder="粘贴管理员提供的 64 位指纹"
          required
          maxLength={95}
          disabled={session.pending}
        />
      </label>
      <button
        className="primary-button"
        type="submit"
        disabled={session.pending || !fingerprint.trim()}
      >
        {session.pending ? '正在验证证书' : '验证并信任证书'}
      </button>
    </form>
  )
}
