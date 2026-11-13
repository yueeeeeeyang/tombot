import { useEffect, useRef, useState } from 'react'
import { Plus, RefreshCw, Users } from 'lucide-react'
import { AdminClient, ApiError } from '../api/client'
import {
  activationSchema,
  imagesSchema,
  nodesSchema,
  operationResult,
  usersSchema,
  type Operation,
} from '../api/schemas'
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

const GIB = 1024 ** 3
type Creation = {
  username: string
  display_name: string
  node_id: string
  image_id: string
  resources: { cpu_millis: number; memory_bytes: number; disk_bytes: number; process_limit: number }
  idempotency_key: string
}

export function UsersPage({ client }: { client: AdminClient }) {
  const [before, setBefore] = useState('')
  const users = useQuery(client, pagePath('/users', before), usersSchema)
  const [creating, setCreating] = useState(false)
  const [operationID, setOperationID] = useState<string | null>(null)
  const [activationUser, setActivationUser] = useState<string | null>(null)
  return (
    <>
      <header className="page-heading">
        <div>
          <h1>用户</h1>
          <p>每位用户拥有独立账户和一台共享电脑。</p>
        </div>
        <div className="actions">
          <button className="icon-button" aria-label="刷新用户" onClick={users.reload}>
            <RefreshCw size={17} />
          </button>
          <button className="primary-button" onClick={() => setCreating(true)}>
            <Plus size={16} />
            创建用户
          </button>
        </div>
      </header>
      {!users.value ? (
        <LoadState error={users.error} onRetry={users.reload} />
      ) : users.value.users.length === 0 ? (
        <div className="empty-state">
          <Users size={28} />
          <h2>暂无用户</h2>
          <p>创建账户后，服务器会同步准备用户电脑。</p>
          <Pagination before={before} onBefore={setBefore} count={0} last={undefined} />
        </div>
      ) : (
        <div className="table-card">
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>用户</th>
                  <th>账户</th>
                  <th>电脑</th>
                  <th>操作</th>
                </tr>
              </thead>
              <tbody>
                {users.value.users.map((user) => (
                  <tr key={user.id}>
                    <td>
                      <strong>{user.display_name}</strong>
                      <small>{user.username}</small>
                    </td>
                    <td>
                      <Status value={user.status} />
                    </td>
                    <td>
                      <Status value={user.computer_status} />
                    </td>
                    <td>
                      <div className="actions">
                        {user.creation_operation_id && (
                          <button
                            className="text-button"
                            onClick={() => setOperationID(user.creation_operation_id)}
                          >
                            创建进度
                          </button>
                        )}
                        {user.status === 'pending_activation' && (
                          <button
                            className="text-button"
                            onClick={() => setActivationUser(user.id)}
                          >
                            签发激活材料
                          </button>
                        )}
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <Pagination
            before={before}
            onBefore={setBefore}
            count={users.value.users.length}
            last={users.value.users.at(-1)?.id}
          />
        </div>
      )}
      {creating && (
        <CreateUser
          client={client}
          onClose={() => setCreating(false)}
          onOperation={(operation) => {
            setCreating(false)
            setOperationID(operation.id)
            users.reload()
          }}
        />
      )}
      {operationID && (
        <OperationDialog
          client={client}
          id={operationID}
          onClose={() => {
            setOperationID(null)
            users.reload()
          }}
        />
      )}
      {activationUser && (
        <ActivationDialog
          client={client}
          userID={activationUser}
          onClose={() => setActivationUser(null)}
        />
      )}
    </>
  )
}

function CreateUser({
  client,
  onClose,
  onOperation,
}: {
  client: AdminClient
  onClose: () => void
  onOperation: (operation: Operation) => void
}) {
  const [nodeBefore, setNodeBefore] = useState('')
  const [imageBefore, setImageBefore] = useState('')
  const nodes = useQuery(client, pagePath('/computer-nodes', nodeBefore), nodesSchema)
  const images = useQuery(client, pagePath('/computer-images', imageBefore), imagesSchema)
  const [error, setError] = useState<unknown>()
  const [busy, setBusy] = useState(false)
  const [uncertain, setUncertain] = useState(false)
  const request = useRef<Creation | null>(null)
  const form = useRef<HTMLFormElement>(null)
  async function submit() {
    if (busy) return
    if (!request.current) {
      if (!form.current?.reportValidity()) return
      const fields = new FormData(form.current)
      request.current = {
        username: String(fields.get('username')).trim(),
        display_name: String(fields.get('display_name')).trim(),
        node_id: String(fields.get('node_id')),
        image_id: String(fields.get('image_id')),
        idempotency_key: crypto.randomUUID(),
        resources: {
          cpu_millis: Number(fields.get('cpu')) * 1000,
          memory_bytes: Number(fields.get('memory')) * GIB,
          disk_bytes: Number(fields.get('disk')) * GIB,
          process_limit: Number(fields.get('processes')),
        },
      }
    }
    setBusy(true)
    setError(undefined)
    try {
      onOperation(
        (
          await client.request('/users', operationResult, {
            method: 'POST',
            body: request.current,
            timeout: 65_000,
          })
        ).operation,
      )
    } catch (cause) {
      if (cause instanceof ApiError && cause.operation) {
        onOperation(cause.operation)
        return
      }
      setError(cause)
      // 传输失败可能发生在服务端提交后；保留原请求和幂等键，由管理员显式查询同一次创建。
      if (!(
        cause instanceof ApiError &&
        [
          'INPUT_INVALID',
          'CREATE_CONFLICT',
          'CAPACITY_UNAVAILABLE',
          'IMAGE_UNAVAILABLE',
          'AUTH_FORBIDDEN',
          'CSRF_INVALID',
        ].includes(cause.code)
      ))
        setUncertain(true)
      else {
        request.current = null
        setUncertain(false)
      }
    } finally {
      setBusy(false)
    }
  }
  return (
    <Dialog title="创建用户与电脑" onClose={onClose} busy={busy || uncertain}>
      <p className="description">创建成功时，用户的终端、浏览器和文件功能必须已通过健康检查。</p>
      <form
        ref={form}
        onSubmit={(event) => {
          event.preventDefault()
          void submit()
        }}
      >
        <fieldset disabled={busy || uncertain} className="form-grid">
          <Field label="用户名">
            <input name="username" required minLength={3} maxLength={64} autoComplete="off" />
          </Field>
          <Field label="显示名称">
            <input name="display_name" required maxLength={120} />
          </Field>
          <Field label="电脑节点">
            <select name="node_id" required defaultValue="">
              <option value="" disabled>
                选择可用节点
              </option>
              {nodes.value?.nodes.map((node) => (
                <option
                  key={node.id}
                  value={node.id}
                  disabled={node.status !== 'ready' || node.reserved >= node.capacity}
                >
                  {node.name} · {node.reserved}/{node.capacity}
                  {node.status !== 'ready' ? ' · 未就绪' : ''}
                </option>
              ))}
            </select>
          </Field>
          {nodes.value && (
            <Pagination
              before={nodeBefore}
              onBefore={setNodeBefore}
              count={nodes.value.nodes.length}
              last={nodes.value.nodes.at(-1)?.id}
            />
          )}
          <Field label="电脑镜像">
            <select name="image_id" required defaultValue="">
              <option value="" disabled>
                选择已启用镜像
              </option>
              {images.value?.images.map((image) => (
                <option key={image.id} value={image.id} disabled={!image.is_enabled}>
                  {image.reference}
                </option>
              ))}
            </select>
          </Field>
          {images.value && (
            <Pagination
              before={imageBefore}
              onBefore={setImageBefore}
              count={images.value.images.length}
              last={images.value.images.at(-1)?.id}
            />
          )}
          <div className="field-pair">
            <Field label="CPU 核数">
              <input
                name="cpu"
                type="number"
                required
                min="0.1"
                max="256"
                step="0.1"
                defaultValue="1"
              />
            </Field>
            <Field label="内存 GiB">
              <input
                name="memory"
                type="number"
                required
                min="0.125"
                max="1024"
                step="0.125"
                defaultValue="2"
              />
            </Field>
          </div>
          <div className="field-pair">
            <Field label="磁盘 GiB">
              <input
                name="disk"
                type="number"
                required
                min="0.5"
                max="102400"
                step="0.5"
                defaultValue="10"
              />
            </Field>
            <Field label="进程上限">
              <input
                name="processes"
                type="number"
                required
                min="16"
                max="4096"
                step="1"
                defaultValue="256"
              />
            </Field>
          </div>
        </fieldset>
        <Alert error={nodes.error || images.error || error} />
        {Boolean(nodes.error || images.error) && (
          <button
            type="button"
            className="secondary-button"
            onClick={() => {
              nodes.reload()
              images.reload()
            }}
          >
            重新读取电脑资源
          </button>
        )}
        {uncertain && (
          <p role="status" className="notice">
            创建结果尚未确认。继续核对会复用原请求，不会重复创建账户。
          </p>
        )}
        <div className="dialog-actions">
          <button type="button" className="secondary-button" disabled={busy} onClick={onClose}>
            {uncertain ? '关闭并从用户列表查询' : '取消'}
          </button>
          <button className="primary-button" disabled={busy || !nodes.value || !images.value}>
            {busy ? '正在创建用户与电脑…' : uncertain ? '核对原创建请求' : '创建用户与电脑'}
          </button>
        </div>
      </form>
    </Dialog>
  )
}

function OperationDialog({
  client,
  id,
  onClose,
}: {
  client: AdminClient
  id: string
  onClose: () => void
}) {
  const [operation, setOperation] = useState<Operation>()
  const [error, setError] = useState<unknown>()
  const [busy, setBusy] = useState(false)
  const [revision, setRevision] = useState(0)
  useEffect(() => {
    if (busy) return
    const controller = new AbortController()
    let timer: ReturnType<typeof setTimeout> | undefined
    let attempts = 0
    async function read() {
      try {
        const result = await client.request(`/user-creation-operations/${id}`, operationResult, {
          signal: controller.signal,
        })
        if (controller.signal.aborted) return
        setOperation((current) =>
          current && Date.parse(current.updated_at) > Date.parse(result.operation.updated_at)
            ? current
            : result.operation,
        )
        if (
          ['pending', 'running', 'reconciling', 'compensating'].includes(result.operation.status) &&
          ++attempts < 120
        )
          timer = setTimeout(() => void read(), 2000)
      } catch (cause) {
        if (!controller.signal.aborted) setError(cause)
      }
    }
    void read()
    return () => {
      controller.abort()
      clearTimeout(timer)
    }
  }, [client, id, revision, busy])
  async function change(action: 'retry' | 'reconcile') {
    setBusy(true)
    setError(undefined)
    try {
      setOperation(
        (
          await client.request(`/user-creation-operations/${id}/${action}`, operationResult, {
            method: 'POST',
            body: {},
            timeout: 65_000,
          })
        ).operation,
      )
      setRevision((value) => value + 1)
    } catch (cause) {
      if (cause instanceof ApiError && cause.operation) setOperation(cause.operation)
      setError(cause)
    } finally {
      setBusy(false)
    }
  }
  const stages: Record<string, string> = {
    reserved: '资源已预留',
    inspecting: '核对电脑资源',
    provisioning: '创建容器并执行健康检查',
    completed: '电脑已就绪，账户等待激活',
    compensated: '失败资源已回收',
    quarantined: '资源状态未确认，禁止重复创建',
    reconciling: '核对节点结果',
    compensating: '回收失败资源',
  }
  return (
    <Dialog title="用户创建进度" onClose={onClose} busy={busy}>
      <Alert error={error} />
      {operation ? (
        <>
          <Status value={operation.status} />
          <h3>{stages[operation.stage] ?? '等待服务端更新进度'}</h3>
          <p className="description">
            第 {operation.attempt} 次创建。只有电脑全部就绪后，才可签发账户激活材料。
          </p>
          <dl>
            <dt>操作编号</dt>
            <dd>{operation.id}</dd>
            <dt>最近更新</dt>
            <dd>{new Date(operation.updated_at).toLocaleString('zh-CN')}</dd>
          </dl>
          {operation.error_code && <p className="notice">失败分类：{operation.error_code}</p>}
        </>
      ) : (
        <p role="status">正在读取进度…</p>
      )}
      <div className="dialog-actions">
        <button
          className="secondary-button"
          disabled={busy}
          onClick={() => {
            setError(undefined)
            setRevision((value) => value + 1)
          }}
        >
          刷新进度
        </button>
        {operation?.status === 'failed' && (
          <button className="primary-button" disabled={busy} onClick={() => void change('retry')}>
            重试原创建操作
          </button>
        )}
        {operation?.status === 'quarantined' && (
          <button
            className="primary-button"
            disabled={busy}
            onClick={() => void change('reconcile')}
          >
            核对并清理隔离资源
          </button>
        )}
      </div>
    </Dialog>
  )
}

function ActivationDialog({
  client,
  userID,
  onClose,
}: {
  client: AdminClient
  userID: string
  onClose: () => void
}) {
  const [activation, setActivation] = useState<{ activation_token: string; expires_at: string }>()
  const [error, setError] = useState<unknown>()
  const [busy, setBusy] = useState(false)
  async function issue() {
    setBusy(true)
    setError(undefined)
    try {
      setActivation(
        await client.request(`/users/${userID}/activation`, activationSchema, {
          method: 'POST',
          body: {},
        }),
      )
    } catch (cause) {
      setError(cause)
    } finally {
      setBusy(false)
    }
  }
  return (
    <Dialog title="用户激活材料" onClose={onClose} busy={busy}>
      <p className="description">
        新签发会使此前尚未使用的激活材料失效。材料仅在本次弹窗中展示，请通过可信渠道交给该用户。
      </p>
      <Alert error={error} />
      {activation ? (
        <>
          <Field label="一次性激活材料">
            <textarea
              readOnly
              value={activation.activation_token}
              autoComplete="off"
              spellCheck={false}
            />
          </Field>
          <p className="description">
            有效期至 {new Date(activation.expires_at).toLocaleString('zh-CN')}
          </p>
          <button className="primary-button" onClick={onClose}>
            已保存，关闭展示
          </button>
        </>
      ) : (
        <button className="primary-button" disabled={busy} onClick={() => void issue()}>
          {busy ? '正在签发…' : '签发新的激活材料'}
        </button>
      )}
    </Dialog>
  )
}
