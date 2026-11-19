import manifest from '../../../../../package.json' with { type: 'json' }
import { GroupAvatar } from '../../components/GroupAvatar'
import type { GroupSummary } from '../desktop/backend'
import type { ConnectionStatus } from '../session/useConnectionStatus'
import { useRef, useState, type FormEvent } from 'react'
import { ulid } from '../../../../shared/ulid'
import { Check, HardDrive, Upload } from 'lucide-react'
import { Avatar } from '../../components/Avatar'
import { Dialog } from '../../components/Dialog'
import { Select } from '../../../../../../ui/Select'
import { CAT_AVATARS } from '../avatars/presets'
import { createCustomAvatar } from '../avatars/customAvatar'
import { SessionFrame } from '../session/SessionGate'
import { type Agent, type NewAgent, type AppWorkspace as Workspace } from './model'

interface RecruitCatFormProps {
  onCreate: (profile: NewAgent, requestId: string) => Promise<void>
  groups: string[]
  initialProfile?: NewAgent
  heading?: string
}

/** 招聘表单。弹窗和首次进入的整页共用，避免两套字段。 */
function RecruitCatForm({
  onCreate,
  groups,
  initialProfile,
  heading,
  onPendingChange,
}: RecruitCatFormProps & { onPendingChange?: (pending: boolean) => void }) {
  const [profile, setProfile] = useState<NewAgent>(
    initialProfile ?? {
      name: '',
      avatar: 'bombay',
      group: '未分组',
    },
  )
  const [error, setError] = useState('')
  const [isCustom, setCustom] = useState(Boolean(initialProfile?.customAvatar))
  const [isReadingAvatar, setReadingAvatar] = useState(false)
  const [isSubmitting, setSubmitting] = useState(false)
  const submitting = useRef(false)
  const request = useRef<{ profile: string; id: string } | null>(null)
  async function submit(event: FormEvent): Promise<void> {
    event.preventDefault()
    if (isReadingAvatar || submitting.current) return
    submitting.current = true
    setSubmitting(true)
    onPendingChange?.(true)
    setError('')
    try {
      if (isCustom && !profile.customAvatar) throw new Error('请先上传自定义猫咪头像。')
      const selected = isCustom
        ? profile
        : { name: profile.name, group: profile.group, avatar: profile.avatar }
      const encoded = JSON.stringify(selected)
      if (request.current?.profile !== encoded) request.current = { profile: encoded, id: ulid() }
      await onCreate(selected, request.current.id)
    } catch (failure) {
      setError((failure as Error).message)
    } finally {
      submitting.current = false
      setSubmitting(false)
      onPendingChange?.(false)
    }
  }
  return (
    <form onSubmit={submit} className="agent-form">
      {heading && <h1>{heading}</h1>}
      <p className="dialog-description">选一位猫咪搭档，一起开启下一个想法。</p>
      <fieldset className="recruit-fields" disabled={isSubmitting}>
        <div className="recruit-kind" role="group" aria-label="猫咪类型">
          <button type="button" aria-pressed={!isCustom} onClick={() => setCustom(false)}>
            预设猫咪
          </button>
          <button type="button" aria-pressed={isCustom} onClick={() => setCustom(true)}>
            自定义猫咪
          </button>
        </div>
        {isCustom ? (
          <div className="custom-cat-profile">
            <label className="custom-avatar-upload">
              {profile.customAvatar ? (
                <Avatar preset={profile.avatar} customAvatar={profile.customAvatar} size="large" />
              ) : (
                <Upload size={28} />
              )}
              <span>
                {isReadingAvatar
                  ? '正在处理图片…'
                  : profile.customAvatar
                    ? '更换头像'
                    : '上传猫咪头像'}
              </span>
              <small>PNG、JPEG 或 WebP，最大 4 MiB</small>
              <input
                type="file"
                accept="image/png,image/jpeg,image/webp"
                aria-label="上传猫咪头像"
                disabled={isReadingAvatar}
                onChange={(event) => {
                  const file = event.target.files?.[0]
                  event.target.value = ''
                  if (!file) return
                  setReadingAvatar(true)
                  setError('')
                  void createCustomAvatar(file)
                    .then((customAvatar) => setProfile((current) => ({ ...current, customAvatar })))
                    .catch((failure: Error) => setError(failure.message))
                    .finally(() => setReadingAvatar(false))
                }}
              />
            </label>
            <label>
              猫咪介绍
              <textarea
                maxLength={120}
                rows={3}
                placeholder="介绍它的性格、擅长的事情，或你希望它怎样与你相处"
                value={profile.description ?? ''}
                onChange={(event) => setProfile({ ...profile, description: event.target.value })}
              />
            </label>
          </div>
        ) : (
          <div className="avatar-picker">
            <div className="avatar-options" role="group" aria-label="预设猫咪头像">
              {CAT_AVATARS.map((preset) => (
                <button
                  key={preset.id}
                  type="button"
                  className="avatar-option"
                  onClick={() => setProfile({ ...profile, avatar: preset.id })}
                  aria-label={preset.name}
                  aria-pressed={profile.avatar === preset.id}
                >
                  <Avatar preset={preset.id} />
                  <span>{preset.name}</span>
                  {profile.avatar === preset.id && (
                    <Check className="avatar-option-check" size={11} />
                  )}
                </button>
              ))}
            </div>
          </div>
        )}
        <label>
          助手名称
          <input
            autoFocus
            value={profile.name}
            maxLength={24}
            placeholder="例如：灵感搭档"
            onChange={(event) => setProfile({ ...profile, name: event.target.value })}
            required
          />
        </label>
        <div className="recruit-selects">
          <div className="recruit-field">
            <span>分组</span>
            <Select
              label="分组"
              value={profile.group}
              onChange={(group) => setProfile({ ...profile, group })}
              options={[...new Set(['未分组', ...groups])].map((group) => ({
                value: group,
                label: group,
              }))}
            />
          </div>
        </div>
      </fieldset>
      {error && (
        <p role="alert" className="form-error">
          {error}
        </p>
      )}
      <button className="primary-button" type="submit" disabled={isReadingAvatar || isSubmitting}>
        {isSubmitting ? '正在招聘…' : '招聘猫咪'}
      </button>
      <p className="form-footnote">在对话中一起明确分工，猫咪会逐渐记住你的偏好和工作习惯。</p>
    </form>
  )
}

/** 创建服务端助手；等待回执期间保持弹窗，失败时保留输入与重试标识。 */
export function RecruitCatDialog({
  onClose,
  ...props
}: RecruitCatFormProps & { onClose: () => void }) {
  const [isPending, setPending] = useState(false)
  return (
    <Dialog title="招聘猫咪" variant="recruit" onClose={onClose} isDismissible={!isPending}>
      <RecruitCatForm {...props} onPendingChange={setPending} />
    </Dialog>
  )
}

/** 工作空间还没有猫咪时的整页。不能跳过，招聘成功后才进入主界面。 */
export function FirstCatPage(props: RecruitCatFormProps) {
  return (
    <SessionFrame>
      <section className="session-page" aria-label="招聘第一只猫咪">
        <RecruitCatForm {...props} heading="招聘第一只猫咪" />
      </section>
    </SessionFrame>
  )
}

/** 展示工作空间概况，与通用设置入口分开。 */
export function WorkspaceDialog({
  workspace,
  hiddenAgents,
  hiddenChats = [],
  onUnhideChat,
  connectionStatus,
  onUnhide,
  onClose,
}: {
  workspace: Workspace
  hiddenAgents: Agent[]
  hiddenChats?: GroupSummary[]
  onUnhideChat?: (id: string) => void
  connectionStatus: ConnectionStatus
  onUnhide: (id: string) => void
  onClose: () => void
}) {
  const messageCount = workspace.agents.reduce((count, agent) => count + agent.messages.length, 0)
  return (
    <Dialog title="我的工作空间" onClose={onClose}>
      <div className="settings-brand">
        <Avatar preset="bombay" size="large" starred />
        <h3>TomBot</h3>
        <p>让每个想法，都有一个好搭档。</p>
        <span>版本 {manifest.version}</span>
      </div>
      <div className="workspace-stats">
        <div>
          <strong>{workspace.agents.length}</strong>
          <span>位助手</span>
        </div>
        <div>
          <strong>{messageCount}</strong>
          <span>条消息</span>
        </div>
      </div>
      <div className="workspace-status">
        <HardDrive size={18} />
        <div>
          <strong>本机偏好</strong>
          <p>主题、侧栏置顶和隐藏保存在当前设备</p>
        </div>
        <span>已启用</span>
      </div>
      <div className="workspace-status">
        <span className={`connection-dot connection-dot--${connectionStatus}`} />
        <div>
          <strong>服务端连接</strong>
          <p>助手和消息由服务管理。</p>
        </div>
        <span>{connectionStatus === 'connected' ? '已连接' : '连接异常'}</span>
      </div>
      {hiddenChats.length > 0 && (
        <section className="workspace-hidden" aria-label="已隐藏的群聊">
          <h3>已隐藏的群聊</h3>
          {hiddenChats.map((group) => (
            <div className="workspace-hidden-item" key={group.id}>
              <GroupAvatar presets={group.avatar_presets} />
              <strong>{group.name}</strong>
              <button
                type="button"
                aria-label={`恢复显示${group.name}`}
                onClick={() => onUnhideChat?.(group.id)}
              >
                恢复显示
              </button>
            </div>
          ))}
        </section>
      )}
      {hiddenAgents.length > 0 && (
        <section className="workspace-hidden" aria-label="已隐藏的助手">
          <div className="workspace-hidden-heading">
            <h3>已隐藏的助手</h3>
            <span>{hiddenAgents.length}</span>
          </div>
          <p>这些助手不会出现在侧边栏，恢复后回到原分组或置顶区。</p>
          <div className="workspace-hidden-list">
            {hiddenAgents.map((agent) => (
              <div className="workspace-hidden-item" key={agent.id}>
                <Avatar
                  preset={agent.avatar}
                  customAvatar={agent.customAvatar}
                  agentId={agent.id}
                />
                <div>
                  <strong>{agent.name}</strong>
                  <span>{agent.group}</span>
                </div>
                <button
                  type="button"
                  aria-label={`恢复显示${agent.name}`}
                  onClick={() => onUnhide(agent.id)}
                >
                  恢复显示
                </button>
              </div>
            ))}
          </div>
        </section>
      )}
    </Dialog>
  )
}
