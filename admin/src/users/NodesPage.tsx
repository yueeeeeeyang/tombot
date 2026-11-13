import { useRef, useState } from 'react'
import { Plus, RefreshCw, Server } from 'lucide-react'
import { AdminClient } from '../api/client'
import { nodesSchema } from '../api/schemas'
import { z } from 'zod'
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

// 登记响应与目录查询共用同一结构；节点地址只来自部署配置，页面不提供该字段。
const catalogSchema = z.object({
  node: z.object({
    id: z.string(),
    name: z.string(),
    status: z.string(),
    capacity: z.number().int(),
    reserved: z.number().int(),
    generation: z.number().int(),
    last_seen_at: z.string().nullable().optional(),
    verified_image_id: z.string().nullable().optional(),
    capability_expires_at: z.string().nullable().optional(),
    verification_status: z.string().optional(),
    verification_error: z.string().nullable().optional(),
  }),
  image: z.object({ id: z.string(), reference: z.string(), is_enabled: z.boolean() }),
})

export function NodesPage({ client }: { client: AdminClient }) {
  const [before, setBefore] = useState('')
  const nodes = useQuery(client, pagePath('/computer-nodes', before), nodesSchema)
  const [registering, setRegistering] = useState(false)
  const [catalog, setCatalog] = useState<z.infer<typeof catalogSchema> | null>(null)
  return (
    <>
      <header className="page-heading">
        <div>
          <h1>电脑节点</h1>
          <p>登记本机部署节点与电脑镜像；登记不代表节点已就绪，就绪取决于心跳与能力验收。</p>
        </div>
        <div className="actions">
          <button className="icon-button" aria-label="刷新节点" onClick={nodes.reload}>
            <RefreshCw size={17} />
          </button>
          <button className="primary-button" onClick={() => setRegistering(true)}>
            <Plus size={16} />
            登记节点与镜像
          </button>
        </div>
      </header>
      {!nodes.value ? (
        <LoadState error={nodes.error} onRetry={nodes.reload} />
      ) : nodes.value.nodes.length === 0 ? (
        <div className="empty-state">
          <Server size={28} />
          <h2>暂无节点</h2>
          <p>先在部署机完成 Docker 引导，再从这里登记节点与固定镜像。</p>
        </div>
      ) : (
        <div className="table-card">
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>节点</th>
                  <th>状态</th>
                  <th>容量</th>
                  <th>已预留</th>
                </tr>
              </thead>
              <tbody>
                {nodes.value.nodes.map((node) => (
                  <tr key={node.id}>
                    <td>
                      <strong>{node.name}</strong>
                      <small>{node.id}</small>
                    </td>
                    <td>
                      <Status value={node.status} />
                    </td>
                    <td>{node.capacity}</td>
                    <td>{node.reserved}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <Pagination
            before={before}
            onBefore={setBefore}
            count={nodes.value.nodes.length}
            last={nodes.value.nodes.at(-1)?.id}
          />
        </div>
      )}
      {catalog && (
        <Dialog title="节点目录" onClose={() => setCatalog(null)}>
          <dl className="detail-list">
            <dt>节点</dt>
            <dd>
              <code>{catalog.node.name}</code>
            </dd>
            <dt>状态</dt>
            <dd>
              <Status value={catalog.node.status} />
            </dd>
            <dt>镜像</dt>
            <dd>
              <code>{catalog.image.reference}</code>
            </dd>
          </dl>
          <div className="form-actions">
            <button type="button" className="primary-button" onClick={() => setCatalog(null)}>
              关闭
            </button>
          </div>
        </Dialog>
      )}
      {registering && (
        <RegisterNode
          client={client}
          onClose={() => setRegistering(false)}
          onRegistered={(value) => {
            setRegistering(false)
            setCatalog(value)
            nodes.reload()
          }}
        />
      )}
    </>
  )
}

function RegisterNode({
  client,
  onClose,
  onRegistered,
}: {
  client: AdminClient
  onClose: () => void
  onRegistered: (value: z.infer<typeof catalogSchema>) => void
}) {
  const [error, setError] = useState<unknown>()
  const [busy, setBusy] = useState(false)
  const form = useRef<HTMLFormElement>(null)
  async function submit() {
    if (busy || !form.current?.reportValidity()) return
    const fields = new FormData(form.current)
    setBusy(true)
    setError(undefined)
    try {
      onRegistered(
        await client.request('/computer-nodes', catalogSchema, {
          method: 'POST',
          body: {
            name: String(fields.get('name')).trim(),
            capacity: Number(fields.get('capacity')),
            image_id: String(fields.get('image_id')).trim(),
            image_reference: String(fields.get('image_reference')).trim(),
          },
          timeout: 15_000,
        }),
      )
    } catch (cause) {
      setError(cause)
    } finally {
      setBusy(false)
    }
  }
  return (
    <Dialog title="登记节点与镜像" onClose={onClose} busy={busy}>
      <p className="description">
        节点标识与连接地址取自服务部署配置；镜像摘要必须与节点实际固定的一致。
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
          <Field label="节点名称">
            <input name="name" required maxLength={120} autoComplete="off" />
          </Field>
          <Field label="容量" hint="允许同时运行的电脑数量上限">
            <input name="capacity" type="number" required min={1} max={10000} defaultValue={10} />
          </Field>
          <Field label="镜像标识" hint="部署登记的镜像 ULID">
            <input name="image_id" required pattern="[0-9A-HJKMNP-TV-Z]{26}" maxLength={26} />
          </Field>
          <Field label="镜像引用" hint="包含固定摘要的镜像地址">
            <input name="image_reference" required maxLength={512} />
          </Field>
          <div className="form-actions">
            <button type="button" className="secondary-button" onClick={onClose}>
              取消
            </button>
            <button type="submit" className="primary-button" disabled={busy}>
              登记
            </button>
          </div>
        </fieldset>
      </form>
    </Dialog>
  )
}
