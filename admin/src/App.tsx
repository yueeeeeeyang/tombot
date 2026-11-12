import { useEffect, useState, useSyncExternalStore } from 'react'
import {
  Bot,
  Users,
  Boxes,
  SlidersHorizontal,
  ShieldCheck,
  LogOut,
  Menu,
  SunMoon,
} from 'lucide-react'
import { AdminClient, errorMessage } from './api/client'
import { Alert, Dialog, Field } from './components'
import { UsersPage } from './users/UsersPage'
import { ModelsPage, PoliciesPage } from './models/ModelsPage'
import { AdminActivation } from './auth/AdminActivation'

type Theme = 'system' | 'light' | 'dark'
function readTheme(): Theme {
  try {
    const value = localStorage.getItem('tombot-admin-theme')
    return value === 'light' || value === 'dark' ? value : 'system'
  } catch {
    return 'system'
  }
}
function ThemePicker() {
  const [theme, setTheme] = useState<Theme>(readTheme)
  const [storageError, setStorageError] = useState(false)
  useEffect(() => {
    const preference = matchMedia('(prefers-color-scheme: dark)')
    const apply = () => {
      document.documentElement.dataset.theme =
        theme === 'system' ? (preference.matches ? 'dark' : 'light') : theme
    }
    apply()
    preference.addEventListener('change', apply)
    return () => preference.removeEventListener('change', apply)
  }, [theme])
  return (
    <div className="theme-picker">
      <SunMoon size={16} />
      <label>
        <span className="sr-only">外观</span>
        <select
          value={theme}
          onChange={(event) => {
            const value = event.target.value as Theme
            setTheme(value)
            try {
              localStorage.setItem('tombot-admin-theme', value)
              setStorageError(false)
            } catch {
              setStorageError(true)
            }
          }}
        >
          <option value="system">跟随系统</option>
          <option value="light">浅色</option>
          <option value="dark">深色</option>
        </select>
      </label>
      {storageError && <small role="status">外观仅在本次页面生效</small>}
    </div>
  )
}

export function App({ client }: { client: AdminClient }) {
  const state = useSyncExternalStore(client.subscribe, client.snapshot)
  useEffect(() => {
    void client.restore()
  }, [client])
  if (state.phase === 'checking')
    return (
      <main className="auth-shell">
        <p role="status">正在检查管理会话…</p>
      </main>
    )
  if (state.phase === 'unavailable')
    return (
      <main className="auth-shell">
        <div className="auth-card">
          <h1>暂时无法连接管理服务</h1>
          <p role="alert">{state.message}</p>
          <button className="primary-button" onClick={() => void client.restore()}>
            重新连接
          </button>
        </div>
      </main>
    )
  if (state.phase === 'anonymous') return <Login client={client} />
  return <Workspace key={state.principal.session_id} client={client} />
}

function Login({ client }: { client: AdminClient }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>()
  const [activating, setActivating] = useState(false)
  const [notice, setNotice] = useState('')
  if (activating)
    return (
      <main className="auth-shell">
        <div className="auth-card activation-card">
          <AdminActivation
            client={client}
            onBack={(message) => {
              setActivating(false)
              setNotice(message)
            }}
          />
          <ThemePicker />
        </div>
      </main>
    )
  return (
    <main className="auth-shell">
      <div className="auth-card">
        <div className="brand-icon">
          <Bot size={28} />
        </div>
        <h1>TomBot 管理平台</h1>
        <p className="description">管理用户、机器人模型和共享电脑。</p>
        {notice && (
          <p className="notice" role="status">
            {notice}
          </p>
        )}
        <form
          onSubmit={async (event) => {
            event.preventDefault()
            if (busy) return
            const form = event.currentTarget
            const fields = new FormData(form)
            setBusy(true)
            setError(undefined)
            const password = form.elements.namedItem('password') as HTMLInputElement
            const code = form.elements.namedItem('code') as HTMLInputElement
            const submittedPassword = password.value
            const submittedCode = code.value
            password.value = ''
            code.value = ''
            try {
              await client.login(
                String(fields.get('username')).trim(),
                submittedPassword,
                submittedCode,
              )
            } catch (cause) {
              setError(cause)
            } finally {
              setBusy(false)
            }
          }}
        >
          <fieldset className="form-grid" disabled={busy}>
            <Field label="管理员用户名">
              <input name="username" required autoComplete="username" maxLength={64} />
            </Field>
            <Field label="密码">
              <input
                name="password"
                required
                type="password"
                autoComplete="current-password"
                maxLength={256}
              />
            </Field>
            <Field label="动态验证码">
              <input
                name="code"
                required
                inputMode="numeric"
                pattern="[0-9]{6}"
                autoComplete="one-time-code"
                maxLength={6}
              />
            </Field>
          </fieldset>
          <Alert error={error} />
          <button className="primary-button" disabled={busy}>
            {busy ? '正在登录…' : '登录管理平台'}
          </button>
        </form>
        <button
          type="button"
          className="secondary-button auth-secondary"
          disabled={busy}
          onClick={() => {
            setError(undefined)
            setNotice('')
            setActivating(true)
          }}
        >
          激活管理员账户
        </button>
        <ThemePicker />
      </div>
    </main>
  )
}

function Workspace({ client }: { client: AdminClient }) {
  const [page, setPage] = useState<'users' | 'models' | 'policies'>('users')
  const [menu, setMenu] = useState(false)
  const [verifying, setVerifying] = useState(false)
  const [error, setError] = useState<unknown>()
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState(false)
  async function sessionAction(action: () => Promise<void>, message: string) {
    setBusy(true)
    setError(undefined)
    setNotice('')
    try {
      await action()
      setNotice(message)
    } catch (cause) {
      setError(cause)
    } finally {
      setBusy(false)
    }
  }
  const navigation = [
    { id: 'users', title: '用户', icon: Users },
    { id: 'models', title: '模型供应商', icon: Boxes },
    { id: 'policies', title: '模型策略', icon: SlidersHorizontal },
  ] as const
  return (
    <div className={`admin-shell ${menu ? 'navigation-open' : ''}`}>
      <aside className="navigation">
        <a className="brand" href="/admin/">
          <Bot size={24} />
          <span>
            TomBot<small>管理平台</small>
          </span>
        </a>
        <nav aria-label="管理导航">
          {navigation.map(({ id, title, icon: Icon }) => (
            <button
              key={id}
              aria-current={page === id ? 'page' : undefined}
              onClick={() => {
                setPage(id)
                setMenu(false)
              }}
            >
              <Icon size={18} />
              {title}
            </button>
          ))}
        </nav>
        <div className="navigation-footer">
          <ThemePicker />
          <button
            className="text-button"
            disabled={busy}
            onClick={() => void sessionAction(() => client.logout(), '')}
          >
            <LogOut size={16} />
            退出登录
          </button>
        </div>
      </aside>
      <div className="workspace">
        <header className="topbar">
          <button
            className="icon-button mobile-menu"
            aria-label="切换导航"
            aria-expanded={menu}
            onClick={() => setMenu(!menu)}
          >
            <Menu size={19} />
          </button>
          <span>服务端管理</span>
          <div className="actions">
            <button
              className="text-button"
              disabled={busy}
              onClick={() => void sessionAction(() => client.refresh(), '管理会话已续期')}
            >
              续期会话
            </button>
            <button className="secondary-button" onClick={() => setVerifying(true)}>
              <ShieldCheck size={15} />
              验证管理身份
            </button>
          </div>
        </header>
        <main id="main" className="page-content">
          <Alert error={error} />
          {notice && (
            <p role="status" className="notice">
              {notice}
            </p>
          )}
          {page === 'users' ? (
            <UsersPage client={client} />
          ) : page === 'models' ? (
            <ModelsPage client={client} />
          ) : (
            <PoliciesPage client={client} />
          )}
        </main>
      </div>
      {verifying && (
        <Verification
          client={client}
          onClose={() => setVerifying(false)}
          onVerified={() => {
            setVerifying(false)
            setNotice('管理身份已验证，请重新确认需要执行的操作')
          }}
        />
      )}
    </div>
  )
}

function Verification({
  client,
  onClose,
  onVerified,
}: {
  client: AdminClient
  onClose: () => void
  onVerified: () => void
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  return (
    <Dialog title="验证管理身份" onClose={onClose} busy={busy}>
      <p className="description">
        修改账户和模型配置前，需要近期验证密码及动态验证码。验证成功后请重新确认原操作。
      </p>
      <form
        onSubmit={async (event) => {
          event.preventDefault()
          if (busy) return
          const form = event.currentTarget
          const fields = new FormData(form)
          form.reset()
          setBusy(true)
          setError('')
          try {
            await client.verify(String(fields.get('password')), String(fields.get('code')))
            onVerified()
          } catch (cause) {
            setError(errorMessage(cause))
          } finally {
            setBusy(false)
          }
        }}
      >
        <fieldset disabled={busy} className="form-grid">
          <Field label="当前密码">
            <input type="password" name="password" required autoComplete="current-password" />
          </Field>
          <Field label="动态验证码">
            <input
              name="code"
              required
              pattern="[0-9]{6}"
              inputMode="numeric"
              maxLength={6}
              autoComplete="one-time-code"
            />
          </Field>
        </fieldset>
        {error && (
          <p role="alert" className="form-error">
            {error}
          </p>
        )}
        <button className="primary-button" disabled={busy}>
          {busy ? '正在验证…' : '确认身份'}
        </button>
      </form>
    </Dialog>
  )
}
