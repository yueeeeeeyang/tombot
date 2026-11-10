import { MessageSquarePlus, Plus, Search, Settings, UserPlus } from 'lucide-react'
import {
  useLayoutEffect,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
  type MouseEvent as ReactMouseEvent,
} from 'react'
import workspaceCat from '../../assets/cats/workspace-cat.png'
import { Avatar } from '../../components/Avatar'
import { InlineNameInput } from '../../components/InlineNameInput'
import type { CatMenuAnchor } from './CatContextMenu'
import type { Agent, AppWorkspace as Workspace } from './model'
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
  chatGroups,
  selected,
  onOpenGroup,
  onCreateGroup,
  serverUrl,
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
  chatGroups: Array<{ id: string; name: string }>
  selected: { kind: 'agent' | 'group'; id: string }
  onOpenGroup: (id: string) => void
  onCreateGroup: () => void
  serverUrl: string
}) {
  const serverHost = hostOf(serverUrl)
  const assistant =
    workspace.agents.find((agent) => agent.id === 'assistant') ??
    workspace.agents.find((agent) => agent.id === workspace.activeAgentId) ??
    workspace.agents[0]
  if (!assistant) return null
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
            <CreateMenu
              className="round-button"
              onRecruit={onCreate}
              onCreateGroup={onCreateGroup}
            />
          </>
        )}
      </div>
      <div className="pinned-area">
        {pinnedAgents.map((agent) => (
          <PinnedAgent
            key={agent.id}
            agent={agent}
            isAssistant={agent.id === assistant.id}
            isActive={selected.kind === 'agent' && workspace.activeAgentId === agent.id}
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
                  isActive={selected.kind === 'agent' && workspace.activeAgentId === agent.id}
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
        {chatGroups.length > 0 && (
          <section className="agent-group">
            <h2>群聊</h2>
            {chatGroups.map((group) => (
              <button
                key={group.id}
                type="button"
                className={`agent-button ${selected.kind === 'group' && selected.id === group.id ? 'is-active' : ''}`}
                onClick={() => onOpenGroup(group.id)}
              >
                <span className="agent-copy">
                  <span className="agent-name">{group.name}</span>
                  <span className="agent-preview">多只猫咪一起聊</span>
                </span>
              </button>
            ))}
          </section>
        )}
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
            <CreateMenu
              className="icon-button"
              onRecruit={onCreate}
              onCreateGroup={onCreateGroup}
            />
          </div>
        )}
        <button
          className="profile-button"
          onClick={onWorkspace}
          aria-label="我的工作空间"
          title="我的工作空间"
        >
          <span className="profile-avatar">
            <img src={workspaceCat} alt="" draggable={false} />
          </span>
          <span className="profile-copy">
            <span>我的工作空间</span>
            {serverHost && (
              <small>
                <i />
                {serverHost}
              </small>
            )}
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

/** 左下角只展示已连接服务的主机名，避免再写成单机模式。 */
function hostOf(serverUrl: string): string {
  if (!serverUrl) return ''
  try {
    return new URL(serverUrl).host
  } catch {
    return ''
  }
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
    : lastMessage?.dmNotice
      ? lastMessage.content
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

/** 原来的新建按钮现在先弹出菜单，再进入招聘或建群。 */
function CreateMenu({
  className,
  onRecruit,
  onCreateGroup,
}: {
  className: string
  onRecruit: () => void
  onCreateGroup: () => void
}) {
  const button = useRef<HTMLButtonElement>(null)
  const menu = useRef<HTMLDivElement>(null)
  const [open, setOpen] = useState(false)
  useLayoutEffect(() => {
    if (!open) return
    if (menu.current == null || button.current == null) return
    const element: HTMLDivElement = menu.current
    const trigger: HTMLButtonElement = button.current
    if (typeof element.showPopover === 'function') element.showPopover()
    const bounds = trigger.getBoundingClientRect()
    const box = element.getBoundingClientRect()
    element.style.left = `${Math.max(8, Math.min(bounds.right - box.width, innerWidth - box.width - 8))}px`
    element.style.top = `${Math.max(8, Math.min(bounds.bottom + 6, innerHeight - box.height - 8))}px`
    element.querySelector<HTMLButtonElement>('button')?.focus({ preventScroll: true })
    function closeOutside(event: PointerEvent): void {
      if (
        event.target instanceof Node &&
        !element.contains(event.target) &&
        !trigger.contains(event.target)
      )
        setOpen(false)
    }
    function onKey(event: KeyboardEvent): void {
      if (event.key !== 'Escape') return
      event.preventDefault()
      setOpen(false)
      trigger.focus()
    }
    document.addEventListener('pointerdown', closeOutside, true)
    window.addEventListener('keydown', onKey)
    return () => {
      if (typeof element.hidePopover === 'function') element.hidePopover()
      document.removeEventListener('pointerdown', closeOutside, true)
      window.removeEventListener('keydown', onKey)
    }
  }, [open])
  return (
    <>
      <button
        ref={button}
        className={className}
        type="button"
        aria-label="新建"
        aria-haspopup="menu"
        aria-expanded={open}
        title="新建"
        onClick={() => setOpen((current) => !current)}
      >
        <Plus size={21} strokeWidth={1.6} />
      </button>
      {open && (
        <div
          ref={menu}
          className="cat-context-menu"
          popover="manual"
          role="menu"
          aria-label="新建"
          onKeyDown={(event) => {
            if (event.key !== 'Escape') return
            event.preventDefault()
            setOpen(false)
            button.current?.focus()
          }}
        >
          <button
            type="button"
            role="menuitem"
            onClick={() => {
              button.current?.focus()
              setOpen(false)
              onRecruit()
            }}
          >
            <UserPlus size={16} />
            <span>招聘猫咪</span>
          </button>
          <button
            type="button"
            role="menuitem"
            onClick={() => {
              button.current?.focus()
              setOpen(false)
              onCreateGroup()
            }}
          >
            <MessageSquarePlus size={16} />
            <span>创建群聊</span>
          </button>
        </div>
      )}
    </>
  )
}
