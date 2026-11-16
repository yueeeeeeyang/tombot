import { useEffect, useRef, useState } from 'react'
import { AdminClient, isVerificationCanceled } from '../api/client'
import { nodeDetailSchema } from '../api/schemas'
import { Alert, Dialog, Field, LoadState, useQuery } from '../components'
import { Select } from '../components/Select'

export function NodeEditDialog({
  client,
  nodeID,
  onClose,
  onSaved,
}: {
  client: AdminClient
  nodeID: string
  onClose: () => void
  onSaved: () => void
}) {
  const query = useQuery(client, `/computer-nodes/${nodeID}`, nodeDetailSchema)
  const node = query.value
  const [busy, setBusy] = useState(false)
  const [attempted, setAttempted] = useState(false)
  const [error, setError] = useState<unknown>()
  const request = useRef<AbortController | null>(null)
  useEffect(() => () => request.current?.abort(), [])
  return (
    <Dialog title="编辑节点" onClose={onClose} busy={busy}>
      <p className="description">
        修改节点名称、准入容量和接收设置，节点身份与连接信息由安装程序管理。
      </p>
      {!node ? (
        <LoadState error={query.error} onRetry={query.reload} />
      ) : (
        <form
          className="node-editor"
          onSubmit={async (event) => {
            event.preventDefault()
            if (busy || attempted) return
            const fields = new FormData(event.currentTarget)
            const controller = new AbortController()
            request.current = controller
            setBusy(true)
            setAttempted(true)
            setError(undefined)
            try {
              await client.request(`/computer-nodes/${node.id}`, nodeDetailSchema, {
                method: 'PUT',
                body: {
                  name: String(fields.get('name')).trim(),
                  capacity: Number(fields.get('capacity')),
                  is_draining: fields.get('admission') === 'paused',
                  configuration_version: node.configuration_version,
                },
                signal: controller.signal,
              })
              if (!controller.signal.aborted) onSaved()
            } catch (cause) {
              if (!controller.signal.aborted) {
                setError(cause)
                if (isVerificationCanceled(cause)) setAttempted(false)
              }
            } finally {
              if (!controller.signal.aborted) setBusy(false)
            }
          }}
        >
          <Alert error={error} />
          <Field label="节点名称">
            <input name="name" defaultValue={node.name} maxLength={120} required disabled={busy} />
          </Field>
          <div className="form-grid">
            <Field label="准入容量" hint={`当前已预留 ${node.reserved} 台，不能低于已预留数量。`}>
              <input
                name="capacity"
                type="number"
                min={Math.max(1, node.reserved)}
                max={10000}
                step={1}
                defaultValue={node.capacity}
                required
                disabled={busy}
              />
            </Field>
            <Field label="接收新电脑">
              <Select
                name="admission"
                label="接收新电脑"
                defaultValue={node.is_draining ? 'paused' : 'enabled'}
                options={[
                  { value: 'enabled', label: '允许接收' },
                  { value: 'paused', label: '暂停接收' },
                ]}
                disabled={busy}
              />
            </Field>
          </div>
          <p className="description">
            容量仅控制新电脑准入，不会增加宿主硬件资源。暂停接收不停止已有电脑；恢复接收后需要重新确认能力。
          </p>
          <dl className="node-properties">
            <div>
              <dt>节点标识</dt>
              <dd>
                <code>{node.id}</code>
              </dd>
            </div>
            <div>
              <dt>节点地址（只读）</dt>
              <dd>
                <code>{node.url ?? '尚未绑定'}</code>
              </dd>
            </div>
          </dl>
          <div className="dialog-actions">
            <button className="secondary-button" type="button" onClick={onClose} disabled={busy}>
              取消
            </button>
            {attempted && !busy && !isVerificationCanceled(error) ? (
              <button
                className="primary-button"
                type="button"
                onClick={() => {
                  setAttempted(false)
                  setError(undefined)
                  query.reload()
                }}
              >
                重新读取当前配置
              </button>
            ) : (
              <button className="primary-button" disabled={busy}>
                {busy ? '正在保存…' : '保存修改'}
              </button>
            )}
          </div>
        </form>
      )}
    </Dialog>
  )
}
