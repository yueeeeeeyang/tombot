import { useEffect, useId, useRef, useState } from 'react'
import { Archive, ArrowLeft, Check, CircleAlert, Monitor, RefreshCw, Upload } from 'lucide-react'
import { z } from 'zod'
import { upgradeStep } from './upgradeStep'
import { AdminClient } from '../api/client'
import { Alert, Dialog, LoadState, useQuery } from '../components'
import { Select } from '../components/Select'
import { nodeRelease, uploadNodeRelease } from './nodeReleaseUpload'
import './node-upgrade.css'

const status = z.object({
  releases: z.array(nodeRelease),
  jobs: z.array(
    z.object({
      id: z.string(),
      node: z.string(),
      state: z.string(),
      step: z.string().optional(),
      label: z.string(),
    }),
  ),
})
const states: Record<string, string> = {
  program_applied: '程序已生效',
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

function ReleaseFile({
  file,
  disabled,
  onChange,
}: {
  file?: File
  disabled: boolean
  onChange: (file?: File) => void
}) {
  const id = useId()
  return (
    <div className="upgrade-file">
      <label className="upgrade-file-picker">
        <input
          type="file"
          className="sr-only"
          aria-label="程序包或电脑镜像包"
          aria-describedby={id + '-name ' + id}
          accept=".tar.gz,.tgz,.gz"
          disabled={disabled}
          onChange={(event) => onChange(event.target.files?.[0])}
        />
        <span className="upgrade-file-icon">
          <Archive size={20} aria-hidden="true" />
        </span>
        <span className="upgrade-file-name">
          <strong>程序包或电脑镜像包</strong>
          <span id={id + '-name'} title={file?.name}>
            {file?.name ?? '例如 tomnode-linux-amd64-programs.tar.gz'}
          </span>
        </span>
        <span className="upgrade-file-action">{file ? '更换' : '选择文件'}</span>
      </label>
      <small id={id}>
        日常升级直接上传节点程序包，无需解压。首次部署或更新桌面环境时，另行上传电脑镜像包。
      </small>
    </div>
  )
}

export function NodeUpgradeDialog({
  client,
  nodes,
  onClose,
}: {
  client: AdminClient
  nodes: { id: string; name: string }[]
  onClose: () => void
}) {
  const {
    value,
    error: loadError,
    reload,
  } = useQuery(client, '/node-updates/status', status, { retainOnRefresh: true })
  const [view, setView] = useState<'dispatch' | 'upload'>()
  const uploading = view ? view === 'upload' : value?.releases.length === 0
  const [all, setAll] = useState(false)
  const [selected, setSelected] = useState<string[]>([])
  const [target, setTarget] = useState('')
  const [archive, setArchive] = useState<File>()
  const [busy, setBusy] = useState(false)
  const [uploadPercent, setUploadPercent] = useState(0)
  const [phase, setPhase] = useState<'hash' | 'upload' | 'verify'>('hash')
  const [notice, setNotice] = useState('')
  const [error, setError] = useState<unknown>()
  const pending = useRef<AbortController | null>(null)
  const versionID = useId()
  const scopeID = useId()
  const currentSelection = nodes.filter((node) => selected.includes(node.id)).map((node) => node.id)
  const hasTarget = value?.releases.some((release) => release.id === target)
  const canSubmit = uploading ? !!archive : hasTarget && (all || currentSelection.length > 0)
  useEffect(() => () => pending.current?.abort(), [])
  useEffect(() => {
    if (
      uploading ||
      busy ||
      !value?.jobs.some((job) => ['running', 'started', 'offline', 'queued'].includes(job.state))
    )
      return
    const timer = window.setTimeout(reload, 8000)
    return () => window.clearTimeout(timer)
  }, [reload, value, uploading, busy])

  function switchView(next: 'dispatch' | 'upload') {
    setError(undefined)
    setNotice('')
    setView(next)
  }
  async function submit() {
    if (pending.current || !canSubmit) return
    const controller = new AbortController()
    pending.current = controller
    setBusy(true)
    setError(undefined)
    setNotice('')
    try {
      if (uploading && archive) {
        setUploadPercent(0)
        setPhase('hash')
        const release = await uploadNodeRelease(
          client,
          archive,
          controller.signal,
          (percent, reading) => {
            setUploadPercent(percent)
            setPhase(reading)
          },
        )
        setTarget(release.id)
        setArchive(undefined)
        setView('dispatch')
        setNotice('发布包已上传，可选择节点下发升级。')
      } else {
        await client.request('/node-updates/dispatch', z.object({}), {
          method: 'POST',
          body: { release: target, nodes: all ? [] : currentSelection, all },
          signal: controller.signal,
        })
        setNotice('升级任务已下发，可在节点列表查看进度。')
      }
      reload()
    } catch (cause) {
      if (!controller.signal.aborted) setError(cause)
    } finally {
      pending.current = null
      if (!controller.signal.aborted) setBusy(false)
    }
  }
  return (
    <Dialog
      title={uploading ? '上传节点发布包' : '远程升级'}
      className="node-upgrade"
      onClose={onClose}
      busy={busy}
    >
      <div className="upgrade-body">
        {uploading ? (
          <ReleaseFile
            file={archive}
            disabled={busy}
            onChange={(file) => {
              setArchive(file)
              setError(undefined)
            }}
          />
        ) : !value ? (
          <LoadState error={loadError} onRetry={reload} />
        ) : (
          <>
            <p className="upgrade-impact">
              <CircleAlert size={16} aria-hidden="true" />
              <span>
                仅更新 bot-agent
                时，新会话使用新版。更新节点、基础代理或镜像会进入维护，已有电脑磁盘会保留。
              </span>
            </p>
            <div className="upgrade-version">
              <div className="upgrade-section-heading">
                <label htmlFor={versionID}>目标版本</label>
                <button
                  type="button"
                  className="text-button upgrade-upload-link"
                  disabled={busy}
                  onClick={() => switchView('upload')}
                >
                  <Upload size={14} />
                  上传新版本
                </button>
              </div>
              <Select
                id={versionID}
                label="目标版本"
                value={target}
                onChange={setTarget}
                disabled={busy || !value.releases.length}
                placeholder={value.releases.length ? '选择已上传的发布包' : '暂无已上传的发布包'}
                options={value.releases.map((r) => ({
                  value: r.id,
                  label:
                    r.label +
                    ' · ' +
                    r.platform +
                    (r.kind === 'programs'
                      ? ' · 节点程序'
                      : r.kind === 'image'
                        ? ' · 电脑镜像'
                        : ''),
                  description: r.id.slice(0, 8),
                }))}
              />
            </div>
            <fieldset className="upgrade-targets" disabled={busy}>
              <legend>目标节点</legend>
              <div className="upgrade-scope">
                <label>
                  <input
                    type="radio"
                    name={scopeID}
                    checked={!all}
                    onChange={() => setAll(false)}
                  />
                  手动选择
                </label>
                <label>
                  <input type="radio" name={scopeID} checked={all} onChange={() => setAll(true)} />
                  所有同架构节点
                </label>
              </div>
              {all && (
                <small className="upgrade-all-hint">包含其他分页中已启用更新器的同架构节点。</small>
              )}
              <div className="upgrade-nodes">
                <div className="upgrade-list-heading">
                  <span>本页节点</span>
                  {!all && nodes.length > 0 && (
                    <label>
                      <input
                        type="checkbox"
                        aria-label="选择本页全部节点"
                        checked={currentSelection.length === nodes.length}
                        ref={(input) => {
                          if (input)
                            input.indeterminate =
                              currentSelection.length > 0 && currentSelection.length < nodes.length
                        }}
                        onChange={(event) =>
                          setSelected(event.target.checked ? nodes.map((node) => node.id) : [])
                        }
                      />
                      全选
                    </label>
                  )}
                </div>
                {nodes.map((node) => {
                  const job = value.jobs.find((job) => job.node === node.id)
                  const Name = all ? 'span' : 'label'
                  return (
                    <div className="upgrade-node" key={node.id}>
                      <Name className="upgrade-node-name">
                        {all ? (
                          <Monitor size={15} aria-hidden="true" />
                        ) : (
                          <input
                            type="checkbox"
                            checked={selected.includes(node.id)}
                            onChange={(event) =>
                              setSelected(
                                event.target.checked
                                  ? [...currentSelection, node.id]
                                  : currentSelection.filter((id) => id !== node.id),
                              )
                            }
                          />
                        )}
                        {node.name}
                      </Name>
                      {job && (
                        <span
                          className={
                            'status' +
                            (['verification_failed', 'failed'].includes(job.state)
                              ? ' status--error'
                              : '')
                          }
                        >
                          {job.label} · {states[job.state] ?? job.state}
                          {job.step && <> · {upgradeStep(job.step)}</>}
                        </span>
                      )}
                    </div>
                  )
                })}
                {!nodes.length && <p className="upgrade-empty">本页暂无节点</p>}
              </div>
            </fieldset>
          </>
        )}
      </div>
      {error || notice || (busy && uploading && archive) ? (
        <div className="upgrade-feedback">
          <Alert error={error} />
          {busy && uploading && archive && archive.size > 0 && (
            <div className="upgrade-progress" role="status">
              {phase === 'verify'
                ? '正在校验升级包…'
                : (phase === 'hash' ? '正在读取文件' : '正在上传') + ' · ' + uploadPercent + '%'}
              <progress
                aria-label="发布包上传进度"
                value={phase === 'verify' ? undefined : uploadPercent}
                max={100}
              />
            </div>
          )}
          {notice && (
            <p role="status" className="upgrade-notice">
              <Check size={16} aria-hidden="true" />
              {notice}
            </p>
          )}
        </div>
      ) : null}
      <div className="dialog-actions upgrade-actions">
        {uploading ? (
          <button
            className="secondary-button"
            disabled={busy}
            onClick={() => switchView('dispatch')}
          >
            <ArrowLeft size={15} />
            返回
          </button>
        ) : (
          <button className="secondary-button" disabled={busy} onClick={reload}>
            <RefreshCw size={15} />
            刷新状态
          </button>
        )}
        <button
          className="primary-button"
          disabled={busy || !canSubmit}
          onClick={() => void submit()}
        >
          {uploading
            ? busy
              ? '正在处理…'
              : '上传发布包'
            : all
              ? '升级所有同架构节点'
              : '升级所选节点（' + currentSelection.length + '）'}
        </button>
      </div>
    </Dialog>
  )
}
