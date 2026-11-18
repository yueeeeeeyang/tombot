import { useEffect, useRef, useState } from 'react'
import { Plus, RefreshCw, Server } from 'lucide-react'
import { Link } from 'react-router'
import { NodeUpgradeDialog } from './NodeUpgradeDialog'
import { NodeStatus } from './NodeDetailPage'
import { NodeEditDialog } from './NodeEditDialog'
import { AdminClient } from '../api/client'
import { nodesSchema } from '../api/schemas'
import { z } from 'zod'
import { Alert, Dialog, Field, LoadState, Pagination, pagePath, useQuery } from '../components'

const grantSchema = z.object({
  id: z.string(),
  token: z.string().min(1).max(2048),
  expires_at: z.string(),
})

export function NodesPage({ client }: { client: AdminClient }) {
  const [before, setBefore] = useState('')
  const nodes = useQuery(client, pagePath('/computer-nodes', before), nodesSchema)
  const [upgrading, setUpgrading] = useState(false)
  const [registering, setRegistering] = useState(false)
  const [editingNodeID, setEditingNodeID] = useState<string | null>(null)
  return (
    <>
      <header className="page-heading">
        <div>
          <h1>电脑节点</h1>
          <p>节点启动后自动接入；通过心跳和能力验收后可用于创建电脑。</p>
        </div>
        <div className="actions">
          <button className="icon-button" aria-label="刷新节点" onClick={nodes.reload}>
            <RefreshCw size={17} />
          </button>
          <button className="secondary-button" onClick={() => setUpgrading(true)}>
            远程升级
          </button>
          <button className="primary-button" onClick={() => setRegistering(true)}>
            <Plus size={16} />
            生成接入凭据
          </button>
        </div>
      </header>
      {!nodes.value ? (
        <LoadState error={nodes.error} onRetry={nodes.reload} />
      ) : nodes.value.nodes.length === 0 ? (
        <div className="empty-state">
          <Server size={28} />
          <h2>暂无节点</h2>
          <p>生成接入凭据，在节点安装时粘贴，启动后会自动出现在这里。</p>
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
                  <th>操作</th>
                </tr>
              </thead>
              <tbody>
                {nodes.value.nodes.map((node) => (
                  <tr key={node.id}>
                    <td>
                      <Link className="node-name" to={`/computer-nodes/${node.id}`}>
                        {node.name}
                      </Link>
                      <small>{node.id}</small>
                    </td>
                    <td>
                      <NodeStatus status={node.status} verification={node.verification_status} />
                    </td>
                    <td>{node.capacity}</td>
                    <td>{node.reserved}</td>
                    <td>
                      <div className="actions">
                        <Link className="text-button" to={`/computer-nodes/${node.id}`}>
                          详情
                        </Link>
                        <button className="text-button" onClick={() => setEditingNodeID(node.id)}>
                          编辑
                        </button>
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
            count={nodes.value.nodes.length}
            last={nodes.value.nodes.at(-1)?.id}
          />
        </div>
      )}
      {editingNodeID && (
        <NodeEditDialog
          client={client}
          nodeID={editingNodeID}
          onClose={() => setEditingNodeID(null)}
          onSaved={() => {
            setEditingNodeID(null)
            nodes.reload()
          }}
        />
      )}
      {upgrading && (
        <NodeUpgradeDialog
          client={client}
          nodes={nodes.value?.nodes ?? []}
          onClose={() => setUpgrading(false)}
        />
      )}
      {registering && <EnrollmentToken client={client} onClose={() => setRegistering(false)} />}
    </>
  )
}

function EnrollmentToken({ client, onClose }: { client: AdminClient; onClose: () => void }) {
  const [error, setError] = useState<unknown>()
  const [busy, setBusy] = useState(false)
  const [grant, setGrant] = useState<z.infer<typeof grantSchema> | null>(null)
  const [copied, setCopied] = useState(false)
  const [copying, setCopying] = useState(false)
  const [copyFailed, setCopyFailed] = useState(false)
  const resources = useRef<{ controller?: AbortController; isDisposed: boolean }>({
    isDisposed: false,
  })
  useEffect(() => {
    const current = resources.current
    current.isDisposed = false
    const clear = () => {
      current.controller?.abort()
      setGrant(null)
      setCopied(false)
      setCopyFailed(false)
      setBusy(false)
      setCopying(false)
    }
    window.addEventListener('pagehide', clear)
    return () => {
      current.controller?.abort()
      current.isDisposed = true
      window.removeEventListener('pagehide', clear)
    }
  }, [])
  async function issue() {
    if (busy) return
    const controller = new AbortController()
    resources.current.controller = controller
    setBusy(true)
    setError(undefined)
    try {
      const value = await client.request('/computer-nodes/enrollment-tokens', grantSchema, {
        method: 'POST',
        body: {},
        signal: controller.signal,
        timeout: 15_000,
      })
      if (controller.signal.aborted) return
      setGrant(value)
    } catch (cause) {
      if (!controller.signal.aborted) setError(cause)
    } finally {
      if (!controller.signal.aborted) setBusy(false)
    }
  }
  async function copy() {
    if (!grant || copying) return
    const controller = resources.current.controller
    setCopying(true)
    setCopied(false)
    setCopyFailed(false)
    try {
      await navigator.clipboard.writeText(grant.token)
      if (!resources.current.isDisposed && !controller?.signal.aborted) setCopied(true)
    } catch {
      if (!resources.current.isDisposed && !controller?.signal.aborted) setCopyFailed(true)
    } finally {
      if (!resources.current.isDisposed) setCopying(false)
    }
  }
  return (
    <Dialog title="生成接入凭据" onClose={onClose} busy={busy}>
      <p className="description">
        每台新节点使用一份凭据，24
        小时内有效，首次接入后自动绑定节点身份。已接入节点重启无需重新生成。
      </p>
      <Alert error={error} />
      {copyFailed && (
        <p className="form-error" role="alert">
          复制失败，请选中凭据码手动复制。
        </p>
      )}
      {grant && (
        <>
          <Field label="接入凭据码">
            <textarea
              className="enrollment-token"
              value={grant.token}
              readOnly
              rows={4}
              spellCheck={false}
              autoComplete="off"
              onFocus={(event) => event.currentTarget.select()}
            />
          </Field>
          <p className="description">
            有效期至：{new Date(grant.expires_at).toLocaleString()}
            。请复制保存，关闭后无法再次查看。
          </p>
          <p className="description">
            完整解压离线安装包，运行 sudo ./tomnode install
            并粘贴接入码。无需填写服务地址、证书指纹或镜像参数。
          </p>
        </>
      )}
      <div className="dialog-actions">
        <button type="button" className="secondary-button" onClick={onClose} disabled={busy}>
          关闭
        </button>
        {grant ? (
          <button
            type="button"
            className="primary-button"
            disabled={copying}
            onClick={() => void copy()}
          >
            {copying ? '正在复制…' : copied ? '已复制' : '复制凭据码'}
          </button>
        ) : (
          <button
            type="button"
            className="primary-button"
            disabled={busy}
            onClick={() => void issue()}
          >
            {busy ? '正在生成…' : '生成凭据'}
          </button>
        )}
      </div>
    </Dialog>
  )
}
