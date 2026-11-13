import { useRef, useState } from 'react'
import { Plus, RefreshCw, ShieldCheck } from 'lucide-react'
import { AdminClient } from '../api/client'
import { administratorSetupSchema, administratorsSchema } from '../api/schemas'
import {
  Alert,
  Dialog,
  Field,
  LoadState,
  Pagination,
  Status,
  pagePath,
  useQuery,
} from '../components'
import type { AdministratorSetup } from '../api/schemas'

// 管理员账号只用于平台管理：不分配电脑，也不出现在桌面客户端。
export function AdministratorsPage({ client }: { client: AdminClient }) {
  const [before, setBefore] = useState('')
  const administrators = useQuery(client, pagePath('/administrators', before), administratorsSchema)
  const [creating, setCreating] = useState(false)
  const [setup, setSetup] = useState<AdministratorSetup | null>(null)
  return (
    <>
      <header className="page-heading">
        <div>
          <h1>管理员</h1>
          <p>管理员只做平台管理，不拥有电脑，也不登录桌面客户端。</p>
        </div>
        <div className="actions">
          <button className="icon-button" aria-label="刷新管理员" onClick={administrators.reload}>
            <RefreshCw size={17} />
          </button>
          <button className="primary-button" onClick={() => setCreating(true)}>
            <Plus size={16} />
            创建管理员
          </button>
        </div>
      </header>
      {!administrators.value ? (
        <LoadState error={administrators.error} onRetry={administrators.reload} />
      ) : administrators.value.administrators.length === 0 ? (
        <div className="empty-state">
          <ShieldCheck size={28} />
          <h2>暂无管理员</h2>
          <p>首位管理员在登录页创建，这里用于追加管理账号。</p>
        </div>
      ) : (
        <div className="table-card">
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>管理员</th>
                  <th>账户</th>
                </tr>
              </thead>
              <tbody>
                {administrators.value.administrators.map((administrator) => (
                  <tr key={administrator.id}>
                    <td>
                      <strong>{administrator.display_name}</strong>
                      <small>{administrator.username}</small>
                    </td>
                    <td>
                      <Status value={administrator.status} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <Pagination
            before={before}
            onBefore={setBefore}
            count={administrators.value.administrators.length}
            last={administrators.value.administrators.at(-1)?.id}
          />
        </div>
      )}
      {creating && (
        <CreateAdministrator
          client={client}
          onClose={() => setCreating(false)}
          onCreated={(value) => {
            setCreating(false)
            setSetup(value)
            administrators.reload()
          }}
        />
      )}
      {setup && <SetupDialog setup={setup} onClose={() => setSetup(null)} />}
    </>
  )
}

function CreateAdministrator({
  client,
  onClose,
  onCreated,
}: {
  client: AdminClient
  onClose: () => void
  onCreated: (value: AdministratorSetup) => void
}) {
  const [error, setError] = useState<unknown>()
  const [busy, setBusy] = useState(false)
  const form = useRef<HTMLFormElement>(null)
  async function submit() {
    if (busy || !form.current?.reportValidity()) return
    const fields = new FormData(form.current)
    const password = String(fields.get('password'))
    if (password !== String(fields.get('confirmation'))) {
      setError(new Error('两次密码需一致。'))
      return
    }
    setBusy(true)
    setError(undefined)
    try {
      onCreated(
        await client.request('/administrators', administratorSetupSchema, {
          method: 'POST',
          body: {
            username: String(fields.get('username')).trim(),
            display_name: String(fields.get('display_name')).trim(),
            password,
          },
          timeout: 20_000,
        }),
      )
    } catch (cause) {
      setError(cause)
    } finally {
      setBusy(false)
    }
  }
  return (
    <Dialog title="创建管理员" onClose={onClose} busy={busy}>
      <p className="description">
        账号创建后立即可登录。动态验证器密钥只在创建成功后显示一次，请当面交给本人。
      </p>
      <Alert error={error} />
      <form
        ref={form}
        onSubmit={(event) => {
          event.preventDefault()
          void submit()
        }}
      >
        <fieldset disabled={busy} className="form-grid">
          <Field label="管理员用户名" hint="小写字母、数字、点、下划线和短横线。">
            <input
              name="username"
              required
              minLength={3}
              maxLength={64}
              pattern="[a-z0-9_.\-]{3,64}"
              autoComplete="off"
              spellCheck={false}
            />
          </Field>
          <Field label="显示名称">
            <input name="display_name" required maxLength={120} />
          </Field>
          <Field label="初始密码" hint="至少 12 个字符。">
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
        <div className="dialog-actions">
          <button type="button" className="secondary-button" onClick={onClose}>
            取消
          </button>
          <button type="submit" className="primary-button" disabled={busy}>
            {busy ? '正在创建…' : '创建'}
          </button>
        </div>
      </form>
    </Dialog>
  )
}

// SetupDialog 只展示一次验证器密钥；关闭即清除，服务端不提供再次读取。
function SetupDialog({ setup, onClose }: { setup: AdministratorSetup; onClose: () => void }) {
  const [visible, setVisible] = useState(false)
  return (
    <Dialog title="管理员已创建" onClose={onClose}>
      <p className="description">
        请把用户名、初始密码和下面的验证器密钥交给本人。密钥关闭后无法再次查看。
      </p>
      <dl className="detail-list">
        <dt>用户名</dt>
        <dd>
          <code>{setup.username}</code>
        </dd>
      </dl>
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
      <div className="dialog-actions">
        <button type="button" className="primary-button" onClick={onClose}>
          我已记录，关闭
        </button>
      </div>
    </Dialog>
  )
}
