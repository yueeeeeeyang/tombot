import { useState, type FormEvent } from 'react'
import { Check, CheckCheck, Copy, Download, HardDrive } from 'lucide-react'
import { Avatar } from '../../components/Avatar'
import { Dialog } from '../../components/Dialog'
import { Select, type SelectOption } from '../../components/Select'
import { CAT_AVATARS } from '../avatars/presets'
import { exportConversation, type Agent, type NewAgent, type Workspace } from './model'

/** 创建本地助手；验证错误保留表单输入，成功后由父组件关闭弹窗。 */
export function RecruitCatDialog({
  onClose,
  onCreate,
  groups,
  modelOptions,
  initialProfile,
  onModelSettings,
}: {
  onClose: () => void
  onCreate: (profile: NewAgent, model: SelectOption) => void
  groups: string[]
  modelOptions: readonly SelectOption[]
  initialProfile?: NewAgent
  onModelSettings: (profile: NewAgent) => void
}) {
  const [profile, setProfile] = useState<NewAgent>(
    initialProfile ?? {
      name: '',
      description: '',
      avatar: 'bombay',
      group: '未分组',
    },
  )
  const [error, setError] = useState('')
  const [modelValue, setModelValue] = useState(modelOptions[0]?.value ?? '')
  function submit(event: FormEvent): void {
    event.preventDefault()
    try {
      const model = modelOptions.find((option) => option.value === modelValue)
      if (!model) throw new Error('请选择有效模型')
      onCreate(profile, model)
    } catch (failure) {
      setError((failure as Error).message)
    }
  }
  return (
    <Dialog title="招聘猫咪" variant="recruit" onClose={onClose}>
      <p className="dialog-description">选一位猫咪搭档，一起开启下一个想法。</p>
      <form onSubmit={submit} className="agent-form">
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
        <label>
          一句话介绍 <span className="optional-label">选填</span>
          <textarea
            value={profile.description}
            maxLength={120}
            rows={2}
            placeholder="你想和这位搭档一起做什么？"
            onChange={(event) => setProfile({ ...profile, description: event.target.value })}
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
          <div className="recruit-field">
            <span>模型</span>
            <Select
              label="模型"
              value={modelValue}
              onChange={setModelValue}
              options={modelOptions}
              placeholder="暂无已配置模型"
            />
          </div>
        </div>
        {modelOptions.length === 0 && (
          <div className="empty-model-note">
            <span>先在模型设置中填写模型名称，再招聘猫咪。</span>
            <button type="button" onClick={() => onModelSettings(profile)}>
              前往模型设置
            </button>
          </div>
        )}
        {error && (
          <p role="alert" className="form-error">
            {error}
          </p>
        )}
        <button className="primary-button" type="submit" disabled={modelOptions.length === 0}>
          招聘猫咪
        </button>
        <p className="form-footnote">猫咪资料保存在本机，模型选择暂仅预览。</p>
      </form>
    </Dialog>
  )
}

/** 仅导出当前助手的用户内容；剪贴板失败显示错误且允许重试。 */
export function ShareDialog({ agent, onClose }: { agent: Agent; onClose: () => void }) {
  const [isCopied, setCopied] = useState(false)
  const [error, setError] = useState('')
  const hasContent = agent.messages.length > 0 || Boolean(agent.draft.trim())
  async function copy(): Promise<void> {
    try {
      await navigator.clipboard.writeText(exportConversation(agent))
      setCopied(true)
      setError('')
    } catch {
      setError('复制失败，请检查剪贴板权限，或将草稿导出为文件。')
    }
  }
  function download(): void {
    const url = URL.createObjectURL(
      new Blob([exportConversation(agent)], { type: 'text/markdown;charset=utf-8' }),
    )
    const anchor = document.createElement('a')
    anchor.href = url
    const safeName = [...agent.name]
      .map((character) =>
        character.charCodeAt(0) < 32 || '<>:"/\\|?*'.includes(character) ? '_' : character,
      )
      .join('')
    anchor.download = `TomBot-${safeName}-草稿.md`
    anchor.click()
    setTimeout(() => URL.revokeObjectURL(url), 1000)
  }
  return (
    <Dialog title="把想法带走" onClose={onClose}>
      <p className="dialog-description">导出与「{agent.name}」的本地草稿，随时接着思考。</p>
      <div className="export-summary">
        <Avatar preset={agent.avatar} />
        <div>
          <strong>{agent.name}</strong>
          <p>
            {agent.messages.length} 条草稿{agent.draft.trim() ? ' · 包含正在编辑的内容' : ''}
          </p>
        </div>
      </div>
      <div className="share-actions">
        <button className="secondary-button" disabled={!hasContent} onClick={() => void copy()}>
          {isCopied ? <CheckCheck size={16} /> : <Copy size={16} />}
          {isCopied ? '已复制' : '复制内容'}
        </button>
        <button className="primary-button" disabled={!hasContent} onClick={download}>
          <Download size={16} />
          导出 Markdown
        </button>
      </div>
      {!hasContent && <p className="form-footnote">还没有可导出的内容，先写下你的第一个想法吧。</p>}
      {error && (
        <p className="form-error" role="alert">
          {error}
        </p>
      )}
      <p className="form-footnote">文件仅在本机导出。云端分享将在服务端接入后提供。</p>
    </Dialog>
  )
}

/** 查看助手的本地资料和连接状态。 */
export function AgentDetailsDialog({
  agent,
  onClose,
  onExport,
  model,
}: {
  agent: Agent
  onClose: () => void
  onExport: () => void
  model?: SelectOption
}) {
  return (
    <Dialog title="你的搭档" onClose={onClose}>
      <div className="settings-brand">
        <Avatar preset={agent.avatar} size="large" starred={agent.id === 'assistant'} />
        <h3>{agent.name}</h3>
        <p>{agent.description || '期待和你一起开启下一个想法。'}</p>
        <span>{agent.group}</span>
      </div>
      <div className="agent-model-summary">
        <span>模型</span>
        <strong>{model?.label ?? '未配置'}</strong>
        <small>当前窗口预览，服务端接入后启用</small>
      </div>
      <div className="details-note">
        <span className="connection-dot" />
        <p>助手已添加到本地工作空间。连接服务端后，可以为它配置模型与工具。</p>
      </div>
      <div className="share-actions">
        <button className="secondary-button" onClick={onExport}>
          <Download size={15} />
          导出草稿
        </button>
        <button className="primary-button" onClick={onClose}>
          回到会话
        </button>
      </div>
    </Dialog>
  )
}

/** 展示工作空间概况，和独立的外观及模型设置入口分开。 */
export function WorkspaceDialog({
  workspace,
  hiddenAgents,
  onUnhide,
  onClose,
}: {
  workspace: Workspace
  hiddenAgents: Agent[]
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
        <span>版本 0.1.0</span>
      </div>
      <div className="workspace-stats">
        <div>
          <strong>{workspace.agents.length}</strong>
          <span>位助手</span>
        </div>
        <div>
          <strong>{messageCount}</strong>
          <span>条本地草稿</span>
        </div>
      </div>
      <div className="workspace-status">
        <HardDrive size={18} />
        <div>
          <strong>本地工作空间</strong>
          <p>助手、消息与输入草稿保存在当前设备</p>
        </div>
        <span>已启用</span>
      </div>
      <div className="workspace-status">
        <span className="connection-dot" />
        <div>
          <strong>服务端连接</strong>
          <p>对话、文件上传与语音功能等待服务端接入</p>
        </div>
        <span>未连接</span>
      </div>
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
                <Avatar preset={agent.avatar} />
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
