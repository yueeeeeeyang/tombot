import { Plus, Search, Settings } from 'lucide-react'
import type { KeyboardEvent as ReactKeyboardEvent, MouseEvent as ReactMouseEvent } from 'react'
import { Avatar } from '../../components/Avatar'
import { InlineNameInput } from '../../components/InlineNameInput'
import { WORKSPACE_AVATAR } from '../avatars/presets'
import type { CatMenuAnchor } from './CatContextMenu'
import type { Agent, CurrentWorkspace as Workspace } from './model'
import type { SidebarPreferences } from './sidebarPreferences'

/** 显示置顶区、分组助手与当前会话入口；右键菜单只上报目标与坐标。 */
export function Sidebar({
  workspace,
  preferences,
  isCollapsed,
  renamingAgentId,
  editingGroupName,
  onAgentMenu,
  onRenameAgent,
  onCancelRenameAgent,
  onRenameGroup,
  onCancelRenameGroup,
  onWorkspace,
  onSelect,
  onSearch,
  onCreate,
  onSettings,
}: {
  workspace: Workspace
  preferences: SidebarPreferences
  isCollapsed: boolean
  renamingAgentId?: string
  editingGroupName?: string
  onAgentMenu: (anchor: CatMenuAnchor) => void
  onRenameAgent: (id: string, name: string) => boolean
  onCancelRenameAgent: () => void
  onRenameGroup: (previousName: string, name: string) => boolean
  onCancelRenameGroup: () => void
  onWorkspace: () => void
  onSelect: (id: string) => void
  onSearch: () => void
  onCreate: () => void
  onSettings: () => void
}) {
  const assistant = workspace.agents.find((agent) => agent.id === 'assistant')!
  const { hiddenIds, pinnedIds, unreadIds } = preferences
  const pinnedAgents = [
    assistant,
    ...pinnedIds.flatMap((id) => {
      if (id === assistant.id || hiddenIds.includes(id)) return []
      const agent = workspace.agents.find((candidate) => candidate.id === id)
      return agent ? [agent] : []
    }),
  ]
  const visibleAgents = workspace.agents.filter(
    (agent) => agent.id !== assistant.id && !hiddenIds.includes(agent.id),
  )
  const unpinnedAgents = visibleAgents.filter((agent) => !pinnedIds.includes(agent.id))
  const groups = [...new Set(unpinnedAgents.map((agent) => agent.group))]

  function openAgentMenu(event: ReactMouseEvent<HTMLButtonElement>, agentId: string): void {
    event.preventDefault()
    onAgentMenu({ agentId, x: event.clientX, y: event.clientY, trigger: event.currentTarget })
  }
  function openAgentMenuFromKeyboard(
    event: ReactKeyboardEvent<HTMLButtonElement>,
    agentId: string,
  ): void {
    if (event.key !== 'ContextMenu' && !(event.shiftKey && event.key === 'F10')) return
    event.preventDefault()
    const bounds = event.currentTarget.getBoundingClientRect()
    onAgentMenu({
      agentId,
      x: bounds.left + bounds.width / 2,
      y: bounds.bottom,
      trigger: event.currentTarget,
    })
  }
  function isUnread(agent: Agent): boolean {
    return unreadIds.includes(agent.id)
  }

  return (
    <aside className="sidebar" aria-label="助手侧边栏">
      <div className="sidebar-toolbar">
        <span className="window-drag-space" />
        {!isCollapsed && (
          <>
            <button
              className="round-button"
              onClick={onSearch}
              aria-label="搜索助手和消息"
              title="搜索 · ⌘K"
            >
              <Search size={18} strokeWidth={1.7} />
            </button>
            <button
              className="round-button"
              onClick={onCreate}
              aria-label="招聘猫咪"
              title="招聘猫咪"
            >
              <Plus size={21} strokeWidth={1.6} />
            </button>
          </>
        )}
      </div>
      <div className="pinned-area">
        {pinnedAgents.map((agent) => (
          <PinnedAgent
            key={agent.id}
            agent={agent}
            isAssistant={agent.id === assistant.id}
            isActive={workspace.activeAgentId === agent.id}
            isUnread={isUnread(agent)}
            isRenaming={agent.id === renamingAgentId}
            onSelect={onSelect}
            onContextMenu={(event) => openAgentMenu(event, agent.id)}
            onMenuKeyDown={(event) => openAgentMenuFromKeyboard(event, agent.id)}
            onRename={onRenameAgent}
            onCancelRename={onCancelRenameAgent}
          />
        ))}
      </div>
      <nav className="agent-groups" aria-label="助手列表">
        {groups.map((group) => (
          <section className="agent-group" key={group}>
            {editingGroupName === group ? (
              <InlineNameInput
                initialName={group}
                label="分组名称"
                className="agent-group-name-input"
                onCommit={(name) => onRenameGroup(group, name)}
                onCancel={onCancelRenameGroup}
              />
            ) : (
              <h2>{group}</h2>
            )}
            {unpinnedAgents
              .filter((agent) => agent.group === group)
              .map((agent) => (
                <AgentButton
                  key={agent.id}
                  agent={agent}
                  isActive={workspace.activeAgentId === agent.id}
                  isUnread={isUnread(agent)}
                  isRenaming={agent.id === renamingAgentId}
                  onSelect={onSelect}
                  onContextMenu={(event) => openAgentMenu(event, agent.id)}
                  onMenuKeyDown={(event) => openAgentMenuFromKeyboard(event, agent.id)}
                  onRename={onRenameAgent}
                  onCancelRename={onCancelRenameAgent}
                />
              ))}
          </section>
        ))}
      </nav>
      <div className="sidebar-footer">
        {isCollapsed && (
          <div className="rail-actions">
            <button
              className="icon-button"
              onClick={onSearch}
              aria-label="搜索助手和消息"
              title="搜索"
            >
              <Search size={18} />
            </button>
            <button
              className="icon-button"
              onClick={onCreate}
              aria-label="招聘猫咪"
              title="招聘猫咪"
            >
              <Plus size={21} />
            </button>
          </div>
        )}
        <button
          className="profile-button"
          onClick={onWorkspace}
          aria-label="我的工作空间"
          title="我的工作空间"
        >
          <Avatar preset={WORKSPACE_AVATAR.id} tracksPointer />
          <span className="profile-copy">
            <span>我的工作空间</span>
            <small>
              <i />
              本地模式
            </small>
          </span>
        </button>
        <button
          className="round-button settings-button"
          onClick={onSettings}
          aria-label="打开设置"
          title="设置"
        >
          <Settings size={18} strokeWidth={1.7} />
        </button>
      </div>
    </aside>
  )
}

/** 置顶卡片在重命名时切换为原位输入，避免跳转弹窗。 */
function PinnedAgent({
  agent,
  isAssistant,
  isActive,
  isUnread,
  isRenaming,
  onSelect,
  onContextMenu,
  onMenuKeyDown,
  onRename,
  onCancelRename,
}: {
  agent: Agent
  isAssistant: boolean
  isActive: boolean
  isUnread: boolean
  isRenaming: boolean
  onSelect: (id: string) => void
  onContextMenu: (event: ReactMouseEvent<HTMLButtonElement>) => void
  onMenuKeyDown: (event: ReactKeyboardEvent<HTMLButtonElement>) => void
  onRename: (id: string, name: string) => boolean
  onCancelRename: () => void
}) {
  if (isRenaming) {
    return (
      <div className="pinned-agent is-editing">
        <Avatar
          preset={agent.avatar}
          agentId={agent.id}
          tracksPointer
          size="large"
          starred={isAssistant}
        />
        <InlineNameInput
          initialName={agent.name}
          label="猫咪名称"
          className="pinned-name-input"
          onCommit={(name) => onRename(agent.id, name)}
          onCancel={onCancelRename}
        />
        {isUnread && <span className="agent-unread-dot" aria-hidden="true" />}
      </div>
    )
  }
  return (
    <button
      className={`pinned-agent ${isActive ? 'is-active' : ''}`}
      onClick={() => onSelect(agent.id)}
      onContextMenu={onContextMenu}
      onKeyDown={onMenuKeyDown}
      aria-label={isUnread ? `${agent.name}，有未读消息` : agent.name}
      title={agent.name}
      aria-pressed={isActive}
    >
      <Avatar
        preset={agent.avatar}
        agentId={agent.id}
        tracksPointer
        size="large"
        starred={isAssistant}
      />
      <span className="pinned-name">{agent.name}</span>
      {isUnread && <span className="agent-unread-dot" aria-hidden="true" />}
    </button>
  )
}

/** 展示助手摘要；重命名时在原位切换为输入框。 */
function AgentButton({
  agent,
  isActive,
  isUnread,
  isRenaming,
  onSelect,
  onContextMenu,
  onMenuKeyDown,
  onRename,
  onCancelRename,
}: {
  agent: Agent
  isActive: boolean
  isUnread: boolean
  isRenaming: boolean
  onSelect: (id: string) => void
  onContextMenu: (event: ReactMouseEvent<HTMLButtonElement>) => void
  onMenuKeyDown: (event: ReactKeyboardEvent<HTMLButtonElement>) => void
  onRename: (id: string, name: string) => boolean
  onCancelRename: () => void
}) {
  const lastMessage = agent.messages.at(-1)
  const preview = agent.draft
    ? `草稿：${agent.draft}`
    : lastMessage
      ? `未发送：${lastMessage.content}`
      : agent.description
  if (isRenaming) {
    return (
      <div className="agent-button is-editing">
        <Avatar preset={agent.avatar} agentId={agent.id} tracksPointer />
        <span className="agent-copy">
          <InlineNameInput
            initialName={agent.name}
            label="猫咪名称"
            className="agent-name-input"
            onCommit={(name) => onRename(agent.id, name)}
            onCancel={onCancelRename}
          />
          <span className="agent-preview">{preview}</span>
        </span>
        {isUnread && <span className="agent-unread-dot" aria-hidden="true" />}
      </div>
    )
  }
  return (
    <button
      className={`agent-button ${isActive ? 'is-active' : ''}`}
      aria-label={isUnread ? `${agent.name}，有未读消息` : agent.name}
      title={agent.name}
      onClick={() => onSelect(agent.id)}
      onContextMenu={onContextMenu}
      onKeyDown={onMenuKeyDown}
      aria-current={isActive ? 'page' : undefined}
    >
      <Avatar preset={agent.avatar} agentId={agent.id} tracksPointer />
      <span className="agent-copy">
        <span className="agent-name">{agent.name}</span>
        <span className="agent-preview">{preview}</span>
      </span>
      {isUnread && <span className="agent-unread-dot" aria-hidden="true" />}
    </button>
  )
}
