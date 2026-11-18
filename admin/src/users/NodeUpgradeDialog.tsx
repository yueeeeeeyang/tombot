import { useEffect, useRef, useState } from 'react'
import { z } from 'zod'
import { AdminClient } from '../api/client'
import { Alert, Dialog, Field, LoadState, useQuery } from '../components'

const release = z.object({ id: z.string(), label: z.string(), platform: z.string() })
const status = z.object({
  releases: z.array(release),
  jobs: z.array(
    z.object({ id: z.string(), node: z.string(), state: z.string(), label: z.string() }),
  ),
})
const signature = z.object({ payload: z.string(), signature: z.string() })
const states: Record<string, string> = {
  superseded: '节点已切换其他镜像',
  queued: '等待领取',
  offline: '等待节点上线',
  running: '升级中',
  started: '控制服务已启动，等待验收',
  ready: '验收就绪',
  verification_failed: '能力验收失败',
  busy: '节点忙碌，未停机',
  unsupported: '未启用更新器或架构不匹配',
  failed: '升级失败，可重新下发相同包',
}
const chunkSize = 4 * 1024 * 1024
export function NodeUpgradeDialog({
  client,
  nodes,
  onClose,
}: {
  client: AdminClient
  nodes: { id: string; name: string }[]
  onClose: () => void
}) {
  const query = useQuery(client, '/node-updates/status', status)
  const [all, setAll] = useState(false)
  const [selected, setSelected] = useState<string[]>([])
  const [target, setTarget] = useState('')
  const [archive, setArchive] = useState<File>()
  const [descriptor, setDescriptor] = useState<File>()
  const [busy, setBusy] = useState(false)
  const [notice, setNotice] = useState('')
  const [error, setError] = useState<unknown>()
  const pending = useRef<AbortController | null>(null)
  useEffect(() => () => pending.current?.abort(), [])
  const reload = query.reload
  useEffect(() => {
    if (
      !query.value?.jobs.some((j) => ['running', 'started', 'offline', 'queued'].includes(j.state))
    )
      return
    const timer = window.setTimeout(reload, 8000)
    return () => window.clearTimeout(timer)
  }, [reload, query.value])
  async function submit(upload: boolean) {
    if (busy) return
    const controller = new AbortController()
    pending.current = controller
    const signal = controller.signal
    setError(undefined)
    setBusy(true)
    setNotice('')
    try {
      if (upload) {
        if (!archive || !descriptor || descriptor.size > 600 * 1024)
          throw new Error('请选择完整节点包和配套签名清单')
        const envelope = signature.parse(JSON.parse(await descriptor.text()))
        const manifest = z
          .object({ size: z.number().int().positive() })
          .parse(JSON.parse(atob(envelope.payload)))
        if (archive.size !== manifest.size) throw new Error('所选节点包大小与签名清单不一致')
        const r = await client.request('/node-updates/releases', release, {
          method: 'POST',
          body: envelope,
          signal,
        })
        const count = Math.ceil(archive.size / chunkSize)
        for (let i = 0; i < count; i++) {
          setNotice('正在上传 ' + (i + 1) + ' / ' + count + ' 个分片')
          await client.request('/node-updates/parts/' + r.id + '/' + i, z.object({}), {
            method: 'PUT',
            body: archive.slice(i * chunkSize, (i + 1) * chunkSize),
            signal,
            timeout: 30000,
          })
        }
        setTarget(r.id)
        setNotice('上传完成，可以选择节点下发')
      } else {
        await client.request('/node-updates/dispatch', z.object({}), {
          method: 'POST',
          body: { release: target, nodes: all ? [] : selected, all },
          signal,
        })
        setNotice('已提交，请查看每台节点的结果')
      }
      reload()
    } catch (cause) {
      if (!signal.aborted) setError(cause)
    } finally {
      if (!signal.aborted) setBusy(false)
    }
  }
  return (
    <Dialog title="远程升级" onClose={onClose} busy={busy}>
      <p className="description">节点服务会重启，当前会话可能中断。已有电脑和磁盘会保留。</p>
      <Alert error={error} />
      <details>
        <summary>上传新的发布包</summary>
        <Field label="完整离线包">
          <input
            type="file"
            accept=".gz,.tgz"
            disabled={busy}
            onChange={(e) => setArchive(e.target.files?.[0])}
          />
        </Field>
        <Field label="发布签名清单">
          <input
            type="file"
            accept=".json"
            disabled={busy}
            onChange={(e) => setDescriptor(e.target.files?.[0])}
          />
        </Field>
        <button
          className="secondary-button"
          disabled={busy || !descriptor || !archive}
          onClick={() => void submit(true)}
        >
          上传发布包
        </button>
      </details>
      {!query.value ? (
        <LoadState error={query.error} onRetry={reload} />
      ) : (
        <>
          <Field label="目标版本">
            <select value={target} disabled={busy} onChange={(e) => setTarget(e.target.value)}>
              <option value="">选择发布包</option>
              {query.value.releases.map((r) => (
                <option key={r.id} value={r.id}>
                  {r.label} · {r.platform} · {r.id.slice(0, 8)}
                </option>
              ))}
            </select>
          </Field>
          <fieldset disabled={busy}>
            <legend>目标节点</legend>
            <label>
              <input type="checkbox" checked={all} onChange={(e) => setAll(e.target.checked)} />
              所有同架构更新节点（含其他分页）
            </label>
            <label>
              <input
                type="checkbox"
                disabled={all}
                checked={nodes.length > 0 && selected.length === nodes.length}
                onChange={(e) => setSelected(e.target.checked ? nodes.map((n) => n.id) : [])}
              />
              选择本页全部节点
            </label>
            {nodes.map((n) => {
              const job = query.value?.jobs.find((j) => j.node === n.id)
              return (
                <div key={n.id}>
                  <label>
                    <input
                      type="checkbox"
                      disabled={all}
                      checked={selected.includes(n.id)}
                      onChange={(e) =>
                        setSelected(
                          e.target.checked
                            ? [...selected, n.id]
                            : selected.filter((id) => id !== n.id),
                        )
                      }
                    />{' '}
                    {n.name}
                  </label>
                  {job && (
                    <small>
                      {job.label} · {states[job.state] ?? job.state}
                    </small>
                  )}
                </div>
              )
            })}
          </fieldset>
          <div className="actions">
            <button className="secondary-button" disabled={busy} onClick={reload}>
              刷新状态
            </button>
            <button
              className="primary-button"
              disabled={busy || !target || (!all && !selected.length)}
              onClick={() => void submit(false)}
            >
              {all ? '升级所有同架构节点' : '升级所选节点（' + selected.length + '）'}
            </button>
          </div>
        </>
      )}
      {notice && <p role="status">{notice}</p>}
    </Dialog>
  )
}
