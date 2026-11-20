import { useEffect, useState, type FormEvent } from 'react'
import { z } from 'zod'
import { AdminClient, ApiError } from '../api/client'
import { nodesSchema, type User } from '../api/schemas'
import { Alert, Dialog, Field, LoadState, Pagination, pagePath, useQuery } from '../components'
import { Select } from '../components/Select'

const gib = 1024 ** 3
const settingsSchema = z.object({
  node_id: z.string(),
  image_id: z.string(),
  resources: z.object({
    cpu_millis: z.number(),
    memory_bytes: z.number(),
    disk_bytes: z.number(),
    process_limit: z.number(),
  }),
})
const userEditSchema = z.object({
  username: z.string(),
  display_name: z.string(),
  has_avatar: z.boolean(),
  node_name: z.string(),
  image_reference: z.string(),
  revision: z.number().int(),
  generation: z.number().int(),
  limits: z.object({
    concurrent_model_calls: z.number(),
    concurrent_desktops: z.number(),
    queued_runs: z.number(),
    monthly_token_limit: z.string().nullable(),
  }),
  computer: settingsSchema,
  progress: z
    .object({
      stage: z.enum(['checking', 'copying', 'installing']),
      copied: z.number(),
      total: z.number(),
      failure: z.string().nullable(),
      paused: z.boolean(),
      target: settingsSchema,
    })
    .nullable(),
})
type Settings = z.infer<typeof userEditSchema>
type Props = { client: AdminClient; id: string; onClose: () => void }

export function UserStatusDialog({
  client,
  user,
  onClose,
}: {
  client: AdminClient
  user: User
  onClose: () => void
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>()
  const enabled = user.status === 'disabled'
  const action = enabled ? '启用' : '禁用'
  async function submit() {
    setBusy(true)
    try {
      await client.request(`/users/${user.id}/status`, z.object({ enabled: z.literal(enabled) }), {
        method: 'PUT',
        body: { enabled, revision: user.revision },
      })
      onClose()
    } catch (e) {
      setError(e)
    } finally {
      setBusy(false)
    }
  }
  return (
    <Dialog title={action + '用户'} busy={busy} onClose={onClose}>
      <p>
        {enabled
          ? `启用后，${user.username} 可以重新登录。`
          : `禁用后，${user.username} 的登录会话将失效。账户资料和电脑数据会保留。`}
      </p>
      <Alert error={error} />
      <div className="dialog-actions">
        <button className="secondary-button" disabled={busy} onClick={onClose}>
          取消
        </button>
        <button className="primary-button" disabled={busy} onClick={() => void submit()}>
          {'确认' + action}
        </button>
      </div>
    </Dialog>
  )
}

export function UserEditDialog({ client, id, onClose }: Props) {
  const query = useQuery(client, `/users/${id}/settings`, userEditSchema, { retainOnRefresh: true })
  const [busy, setBusy] = useState(false)
  const active = Boolean(query.value?.progress && !query.value.progress.paused)
  const reload = query.reload
  useEffect(() => {
    if (!active) return
    const timer = setInterval(reload, 5000)
    return () => clearInterval(timer)
  }, [active, reload])
  return (
    <Dialog title="编辑禁用用户" onClose={onClose} busy={busy}>
      {query.error || !query.value ? (
        <LoadState error={query.error} onRetry={reload} />
      ) : (
        <EditForm
          key={query.value.revision + '-' + query.value.generation}
          value={query.value}
          client={client}
          id={id}
          onClose={onClose}
          reload={reload}
          busy={busy}
          setBusy={setBusy}
        />
      )}
    </Dialog>
  )
}

const stages = {
  checking: '检查节点和原电脑',
  copying: '复制持久盘',
  installing: '核验目标磁盘并启动电脑',
}

const failures: Record<string, string> = {
  storage_capacity_unavailable: '目标节点空间不足，无法同时保留原盘并分配目标盘。',
  disk_copy_resize_failed: '目标盘调整容量失败，请核对目标容量及节点磁盘日志。',
  disk_copy_fsck_failed: '目标盘文件系统检查失败，原盘已保留。',
  disk_copy_digest_mismatch: '复制块校验不一致，尚未切换电脑。',
  disk_source_not_frozen: '原盘尚未确认离线，复制已停止。',
  disk_source_changed: '原电脑身份或运行代次发生变化。',
  node_call_unconfirmed: '尚未收到节点回执，请检查节点连接。',
  disk_target_in_use: '目标节点上的电脑实例正在使用。',
  target_probe_failed: '目标电脑尚未通过运行能力检查。',
}

function EditForm({
  value,
  client,
  id,
  onClose,
  reload,
  busy,
  setBusy,
}: Props & {
  value: Settings
  reload: () => void
  busy: boolean
  setBusy: (busy: boolean) => void
}) {
  const config = value.progress?.target ?? value.computer
  const [before, setBefore] = useState('')
  const nodes = useQuery(client, pagePath('/computer-nodes', before), nodesSchema)
  const [node, setNode] = useState(config.node_id)
  const [image, setImage] = useState(config.image_id)
  const [error, setError] = useState<unknown>()
  const [resetAvatar, setResetAvatar] = useState(false)
  const [file, setFile] = useState<File | null>(null)
  const progress = value.progress
  const selected = nodes.value?.nodes.find((n) => n.id === node)
  const [nodeName, setNodeName] = useState(
    config.node_id === value.computer.node_id ? value.node_name : config.node_id,
  )
  const [imageName, setImageName] = useState(
    config.image_id === value.computer.image_id ? value.image_reference : config.image_id,
  )
  const opts =
    nodes.value?.nodes.map((n) => ({
      value: n.id,
      label: n.name,
      disabled:
        n.id !== config.node_id &&
        (n.status !== 'ready' || n.reserved >= n.capacity || !n.image?.is_enabled),
    })) ?? []
  if (!opts.some((n) => n.value === node))
    opts.unshift({ value: node, label: nodeName, disabled: false })
  const images = [
    { value: image, label: selected?.image?.id === image ? selected.image.reference : imageName },
  ]
  if (selected?.image?.is_enabled && selected.image.id !== image)
    images.push({ value: selected.image.id, label: selected.image.reference })
  if (node === config.node_id && !images.some((i) => i.value === config.image_id))
    images.push({
      value: config.image_id,
      label: config.image_id === value.computer.image_id ? value.image_reference : config.image_id,
    })
  async function save(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (busy) return
    const fields = new FormData(event.currentTarget)
    setError(undefined)
    setBusy(true)
    try {
      const monthly = String(fields.get('monthly') ?? '').trim()
      if (monthly && (!/^[1-9]\d{0,18}$/.test(monthly) || BigInt(monthly) > 9223372036854775807n))
        throw new ApiError('INPUT_INVALID', '每月 Token 上限须为正整数')
      let avatar: string | undefined
      if (file && !resetAvatar) {
        if (file.size > 2 * 1024 ** 2 || file.size === 0)
          throw new ApiError('INPUT_INVALID', '头像文件大小须在 1 字节至 2 MiB 之间')
        avatar = await new Promise<string>((resolve, reject) => {
          const reader = new FileReader()
          reader.onerror = () => reject(new ApiError('INPUT_INVALID', '头像读取失败'))
          reader.onload = () => resolve(String(reader.result).split(',')[1]!)
          reader.readAsDataURL(file)
        })
      }
      const result = await client.request(`/users/${id}/settings`, userEditSchema, {
        method: 'PUT',
        body: {
          revision: value.revision,
          generation: value.generation,
          display_name: fields.get('display_name'),
          avatar,
          remove_avatar: resetAvatar,
          limits: {
            concurrent_model_calls: Number(fields.get('models')),
            concurrent_desktops: Number(fields.get('desktops')),
            queued_runs: Number(fields.get('queued')),
            monthly_token_limit: monthly || null,
          },
          computer: progress
            ? progress.target
            : {
                node_id: node,
                image_id: image,
                resources: {
                  cpu_millis: Number(fields.get('cpu')) * 1000,
                  memory_bytes: Number(fields.get('memory')) * gib,
                  disk_bytes: Number(fields.get('disk')) * gib,
                  process_limit: Number(fields.get('processes')),
                },
              },
        },
      })
      if (result.progress) reload()
      else onClose()
    } catch (e) {
      setError(e)
    } finally {
      setBusy(false)
    }
  }
  return (
    <form onSubmit={save}>
      {progress && (
        <div role="status" className="notice">
          {stages[progress.stage]}
          {progress.stage === 'copying' &&
            ` · ${((100 * progress.copied) / progress.total).toFixed(1)}%`}
          {progress.failure &&
            ` · ${failures[progress.failure] ?? '节点未确认当前操作。'}（${progress.failure}）`}
          {progress.paused && ' · 已暂停，可重试原迁移'}
        </div>
      )}
      <fieldset disabled={busy} className="form-grid">
        <div className="field-pair">
          <Field label="用户名">
            <input readOnly value={value.username} />
          </Field>
          <Field label="显示名称">
            <input name="display_name" required defaultValue={value.display_name} maxLength={120} />
          </Field>
        </div>
        <Field
          label="头像"
          hint={
            (value.has_avatar ? '已设置头像。' : '使用默认头像。') +
            '可上传 512 × 512 像素以内的 PNG 或 JPEG，最大 2 MiB。'
          }
        >
          <input
            type="file"
            accept="image/png,image/jpeg"
            disabled={resetAvatar}
            onChange={(e) => setFile(e.target.files?.[0] ?? null)}
          />
        </Field>
        {value.has_avatar && (
          <label>
            <input
              type="checkbox"
              checked={resetAvatar}
              onChange={(e) => setResetAvatar(e.target.checked)}
            />
            恢复默认头像
          </label>
        )}
        <div className="field-pair">
          <Field label="模型并发上限">
            <input
              type="number"
              name="models"
              required
              min={1}
              max={100}
              defaultValue={value.limits.concurrent_model_calls}
            />
          </Field>
          <Field label="桌面并发上限">
            <input
              type="number"
              name="desktops"
              required
              min={1}
              max={64}
              defaultValue={value.limits.concurrent_desktops}
            />
          </Field>
        </div>
        <div className="field-pair">
          <Field label="排队任务上限">
            <input
              type="number"
              name="queued"
              required
              min={1}
              max={10000}
              defaultValue={value.limits.queued_runs}
            />
          </Field>
          <Field label="每月 Token 上限" hint="留空表示不限。">
            <input
              inputMode="numeric"
              name="monthly"
              pattern="[1-9][0-9]*"
              maxLength={19}
              defaultValue={value.limits.monthly_token_limit ?? ''}
            />
          </Field>
        </div>
      </fieldset>
      <fieldset disabled={busy || Boolean(progress)} className="form-grid">
        <Field label="电脑节点">
          <Select
            label="电脑节点"
            name="node_id"
            value={node}
            disabled={busy || Boolean(progress)}
            options={opts}
            onChange={(id) => {
              const n = nodes.value?.nodes.find((option) => option.id === id)
              if (!n) return
              setNode(id)
              setNodeName(n.name)
              setImage(id === config.node_id ? config.image_id : n.image!.id)
              setImageName(
                n.id === value.computer.node_id
                  ? value.image_reference
                  : (n.image?.reference ?? config.image_id),
              )
            }}
          />
        </Field>
        {nodes.value && (before || nodes.value.nodes.length === 50) && (
          <Pagination
            before={before}
            onBefore={setBefore}
            count={nodes.value.nodes.length}
            last={nodes.value.nodes.at(-1)?.id}
          />
        )}
        <Field label="电脑镜像" hint="可选择目标节点已验收的镜像。">
          <Select
            label="电脑镜像"
            name="image_id"
            value={image}
            options={images}
            disabled={busy || Boolean(progress)}
            onChange={setImage}
          />
        </Field>
        <div className="field-pair">
          <Field label="CPU 核数">
            <input
              type="number"
              required
              name="cpu"
              min={0.1}
              max={256}
              step="any"
              defaultValue={config.resources.cpu_millis / 1000}
            />
          </Field>
          <Field label="内存 GiB">
            <input
              type="number"
              required
              name="memory"
              min={0.125}
              max={1024}
              step="any"
              defaultValue={config.resources.memory_bytes / gib}
            />
          </Field>
        </div>
        <div className="field-pair">
          <Field label="磁盘 GiB">
            <input
              type="number"
              required
              name="disk"
              min={0.5}
              max={102400}
              step="any"
              defaultValue={config.resources.disk_bytes / gib}
            />
          </Field>
          <Field label="进程上限">
            <input
              type="number"
              required
              name="processes"
              min={16}
              max={4096}
              defaultValue={config.resources.process_limit}
            />
          </Field>
        </div>
      </fieldset>
      <Alert error={error || nodes.error} />
      {(Boolean(error) || Boolean(nodes.error)) && (
        <button
          type="button"
          className="text-button"
          onClick={() => {
            nodes.reload()
            reload()
          }}
        >
          重新读取
        </button>
      )}
      <div className="dialog-actions">
        <button type="button" className="secondary-button" disabled={busy} onClick={onClose}>
          关闭
        </button>
        <button className="primary-button" disabled={busy}>
          {progress?.paused ? '保存并重试原迁移' : '保存修改'}
        </button>
      </div>
    </form>
  )
}
