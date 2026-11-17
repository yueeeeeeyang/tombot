import type { ConversationPreview, GroupSummary, UserProfile } from '../desktop/backend'
import { GroupAvatar } from '../../components/GroupAvatar'
import { ChevronRight, MessageSquarePlus, Plus, Search, Settings, UserPlus } from 'lucide-react'
import {
  useLayoutEffect,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
  type DragEvent,
  type MouseEvent as ReactMouseEvent,
} from 'react'
import type { ConnectionStatus } from '../session/useConnectionStatus'
import workspaceCat from '../../assets/cats/workspace-cat.png'
import { Avatar } from '../../components/Avatar'
import { InlineNameInput } from '../../components/InlineNameInput'
import type { CatMenuAnchor } from './CatContextMenu'
import type { Agent, AppWorkspace as Workspace } from './model'
import { messagePreview } from './messagePreview'
import { useCollapsedGroups } from './useCollapsedGroups'
import type { SidebarPreferences } from './sidebarPreferences'

/** 显示置顶区、分组助手与当前会话入口；右键菜单只上报目标与坐标。 */
export function Sidebar({
  userProfile,
  workspace,
  preferences,
  previews = {},
  onMove,
  isCollapsed,
  renamingAgentId,
  editingGroupName,
  onAgentMenu,
  onGroupMenu,
  renamingChatId,
  onRenameChat,
  onCancelRenameChat,
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
  connectionStatus,
  onError,
}: {
  userProfile?: UserProfile
  workspace: Workspace
  preferences: SidebarPreferences
  previews?: Record<string, ConversationPreview>
  onMove?: (kind: 'agent' | 'group', id: string, category: string) => void
  isCollapsed: boolean
  renamingAgentId?: string
  editingGroupName?: string
  onGroupMenu?: (anchor: CatMenuAnchor) => void
  renamingChatId?: string
  onRenameChat?: (id: string, name: string) => Promise<boolean>
  onCancelRenameChat?: () => void
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
  chatGroups: GroupSummary[]
  selected: { kind: 'agent' | 'group'; id: string }
  onOpenGroup: (id: string) => void
  onCreateGroup: () => void
  serverUrl: string
  connectionStatus: ConnectionStatus
  onError: (message: string) => void
}) {
  const { collapsed, toggle, error: collapseError } = useCollapsedGroups(onError)
  const serverHost = hostOf(serverUrl)
  const displayName = userProfile?.displayName || userProfile?.username || '用户信息'
  // 主猫由服务端 leadAgentId 固定。当前会话不能顶上去，否则点击会和置顶位交换。
  const lead = workspace.agents.find((agent) => agent.id === workspace.leadAgentId)
  const { hiddenIds, pinnedIds, unreadIds } = preferences
  const pinnedAgents = [
    ...(lead ? [lead] : []),
    ...pinnedIds.flatMap((id) => {
      if (id === lead?.id || hiddenIds.includes(id)) return []
      const agent = workspace.agents.find((candidate) => candidate.id === id)
      return agent ? [agent] : []
    }),
  ]
  const visibleAgents = workspace.agents.filter(
    (agent) => agent.id !== lead?.id && !hiddenIds.includes(agent.id),
  )
  const unpinnedAgents = visibleAgents.filter((agent) => !pinnedIds.includes(agent.id))
  const visibleChats = chatGroups.filter((group) => !hiddenIds.includes(group.id))
  const unpinnedChats = visibleChats.filter((group) => !pinnedIds.includes(group.id))
  const chatCategory = (id: string) => preferences.chatCategories?.[id] ?? '未分组'
  const groups = [
    ...new Set([
      ...unpinnedAgents.map((agent) => agent.group),
      ...unpinnedChats.map((group) => chatCategory(group.id)),
      '未分组',
    ]),
  ]
  const [dragged, setDragged] = useState<{ kind: 'agent' | 'group'; id: string }>()
  const [dropCategory, setDropCategory] = useState<string>()
  function beginDrag(event: DragEvent<HTMLButtonElement>, kind: 'agent' | 'group', id: string) {
    if (!onMove || id === workspace.leadAgentId) {
      event.preventDefault()
      return
    }
    setDragged({ kind, id })
    event.dataTransfer.effectAllowed = 'move'
    event.dataTransfer.setData('application/x-tombot-chat', JSON.stringify({ kind, id }))
    const ghost = document.createElement('div')
    ghost.className = 'sidebar-drag-preview'
    const avatar = event.currentTarget.querySelector('.avatar, .group-avatar')
    if (avatar) ghost.append(avatar.cloneNode(true))
    const label = document.createElement('span')
    label.className = 'sidebar-drag-label'
    label.textContent =
      kind === 'agent'
        ? (workspace.agents.find((agent) => agent.id === id)?.name ?? '')
        : (chatGroups.find((group) => group.id === id)?.name ?? '')
    ghost.append(label)
    document.body.append(ghost)
    event.dataTransfer.setDragImage(ghost, 48, 48)
    requestAnimationFrame(() => ghost.remove())
  }
  function endDrag() {
    setDragged(undefined)
    setDropCategory(undefined)
  }
  function chatButton(group: GroupSummary, pinned = false) {
    const css = pinned ? 'pinned-agent' : 'agent-button'
    if (group.id === renamingChatId && onRenameChat && onCancelRenameChat)
      return (
        <div className={`${css} is-editing`} key={group.id}>
          <GroupAvatar presets={group.avatar_presets} />
          <InlineNameInput
            initialName={group.name}
            label="聊天名称"
            maxLength={48}
            onCommit={(name) => onRenameChat(group.id, name)}
            onCancel={onCancelRenameChat}
          />
        </div>
      )
    return (
      <button
        key={group.id}
        draggable={!!onMove}
        onDragStart={(event) => beginDrag(event, 'group', group.id)}
        onDragEnd={endDrag}
        type="button"
        className={`${css} ${selected.kind === 'group' && selected.id === group.id ? 'is-active' : ''}`}
        aria-label={`${group.name}${unreadIds.includes(group.id) ? '，未读' : ''}`}
        onClick={() => onOpenGroup(group.id)}
        onContextMenu={(event) => {
          event.preventDefault()
          onGroupMenu?.({
            agentId: group.id,
            x: event.clientX,
            y: event.clientY,
            trigger: event.currentTarget,
          })
        }}
        onKeyDown={(event) => {
          if (event.key === 'ContextMenu' || (event.shiftKey && event.key === 'F10')) {
            event.preventDefault()
            const box = event.currentTarget.getBoundingClientRect()
            onGroupMenu?.({
              agentId: group.id,
              x: box.left,
              y: box.bottom,
              trigger: event.currentTarget,
            })
          }
        }}
      >
        <GroupAvatar presets={group.avatar_presets} size={pinned ? 'large' : undefined} />
        <span className={pinned ? 'pinned-name' : 'agent-copy'}>
          <span className="agent-name">{group.name}</span>
          {!pinned && (
            <span className="agent-preview">
              {previews[group.conversation_id]?.content || '暂无消息'}
            </span>
          )}
        </span>
        {unreadIds.includes(group.id) && <span className="agent-unread-dot" aria-hidden="true" />}
      </button>
    )
  }

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
            isAssistant={agent.id === workspace.leadAgentId}
            onDragStart={(event) => beginDrag(event, 'agent', agent.id)}
            onDragEnd={endDrag}
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
        {visibleChats
          .filter((group) => pinnedIds.includes(group.id))
          .map((group) => chatButton(group, true))}
      </div>
      <nav className="agent-groups" aria-label="助手列表">
        {collapseError && <p role="alert">{collapseError}</p>}
        {groups.map((group) => {
          const groupAgents = unpinnedAgents.filter((agent) => agent.group === group)
          const groupChats = unpinnedChats.filter((chat) => chatCategory(chat.id) === group)
          const groupCount = groupAgents.length + groupChats.length
          const isGroupCollapsed = collapsed.includes(`cats:${group}`)
          return (
            <section
              className={`agent-group${dropCategory === group ? ' is-drop-target' : ''}`}
              key={group}
              onDragOver={(event) => {
                if (dragged) {
                  event.preventDefault()
                  event.dataTransfer.dropEffect = 'move'
                  setDropCategory(group)
                }
              }}
              onDragLeave={(event) => {
                if (!event.currentTarget.contains(event.relatedTarget as Node | null))
                  setDropCategory(undefined)
              }}
              onDrop={(event) => {
                if (!dragged) return
                event.preventDefault()
                onMove?.(dragged.kind, dragged.id, group)
                endDrag()
              }}
            >
              {editingGroupName === group ? (
                <InlineNameInput
                  initialName={group}
                  label="分组名称"
                  className="agent-group-name-input"
                  onCommit={(name) => onRenameGroup(group, name)}
                  onCancel={onCancelRenameGroup}
                />
              ) : (
                <h2 aria-label={group}>
                  <button
                    className="group-toggle"
                    type="button"
                    aria-label={`${isGroupCollapsed ? '展开' : '收起'}${group}分组`}
                    aria-description={
                      isGroupCollapsed
                        ? groupChats.length
                          ? `${groupCount} 个聊天`
                          : `${groupCount} 只猫咪`
                        : undefined
                    }
                    aria-expanded={!isGroupCollapsed}
                    onClick={() => toggle(`cats:${group}`)}
                  >
                    <span className="group-name">{group}</span>
                    {isGroupCollapsed && (
                      <span className="group-count" aria-hidden="true">
                        {groupCount}
                      </span>
                    )}
                    <ChevronRight size={13} aria-hidden="true" />
                  </button>
                </h2>
              )}
              <div hidden={!isCollapsed && isGroupCollapsed && editingGroupName !== group}>
                {groupAgents.map((agent) => (
                  <AgentButton
                    key={agent.id}
                    agent={agent}
                    preview={agent.conversationId ? previews[agent.conversationId] : undefined}
                    onDragStart={(event) => beginDrag(event, 'agent', agent.id)}
                    onDragEnd={endDrag}
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
                {groupChats.map((chat) => chatButton(chat))}
              </div>
            </section>
          )
        })}
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
          aria-label="用户信息"
          title={displayName}
        >
          <span className="profile-avatar">
            <img src={userProfile?.avatar || workspaceCat} alt="" draggable={false} />
          </span>
          <span className="profile-copy">
            <span>{displayName}</span>
            {serverHost && (
              <small>
                <i
                  className={`server-status server-status--${connectionStatus}`}
                  role="status"
                  aria-label={connectionStatus === 'connected' ? '服务端已连接' : '服务端连接异常'}
                  title={
                    connectionStatus === 'connected'
                      ? '服务端已连接'
                      : '服务端连接异常，正在尝试重新连接'
                  }
                />
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
  onDragStart,
  onDragEnd,
}: {
  agent: Agent
  isAssistant: boolean
  onDragStart: (event: DragEvent<HTMLButtonElement>) => void
  onDragEnd: () => void
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
          customAvatar={agent.customAvatar}
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
      draggable={!isAssistant}
      onDragStart={onDragStart}
      onDragEnd={onDragEnd}
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
        customAvatar={agent.customAvatar}
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
  preview: serverPreview,
  onDragStart,
  onDragEnd,
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
  preview?: ConversationPreview
  onDragStart: (event: DragEvent<HTMLButtonElement>) => void
  onDragEnd: () => void
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
    : lastMessage &&
        (lastMessage.status !== 'sent' ||
          !serverPreview ||
          (lastMessage.sequence ?? 0) >= serverPreview.sequence)
      ? messagePreview(lastMessage)
      : serverPreview?.content || agent.description
  if (isRenaming) {
    return (
      <div className="agent-button is-editing">
        <Avatar
          preset={agent.avatar}
          customAvatar={agent.customAvatar}
          agentId={agent.id}
          tracksPointer
        />
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
      draggable
      onDragStart={onDragStart}
      onDragEnd={onDragEnd}
      className={`agent-button ${isActive ? 'is-active' : ''}`}
      aria-label={isUnread ? `${agent.name}，有未读消息` : agent.name}
      title={agent.name}
      onClick={() => onSelect(agent.id)}
      onContextMenu={onContextMenu}
      onKeyDown={onMenuKeyDown}
      aria-current={isActive ? 'page' : undefined}
    >
      <Avatar
        preset={agent.avatar}
        customAvatar={agent.customAvatar}
        agentId={agent.id}
        tracksPointer
      />
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
