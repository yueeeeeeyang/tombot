import { useConversationPreviews } from './features/workspace/useConversationPreviews'
import { parseChatFilePath } from '../../shared/chatFiles'
import { useGroupChatActions } from './features/workspace/useGroupChatActions'
import { ulid } from '../../shared/ulid'
import type { GroupSummary } from './features/desktop/backend'
import { useCallback, useEffect, useRef, useState, type CSSProperties } from 'react'
import { installAgentActivityEvents } from './features/avatars/activity'
import { preloadCatFrames } from './features/avatars/catMotion'
import { AlertCircle, X } from 'lucide-react'
import { Sidebar } from './features/workspace/Sidebar'
import { CatSidePanel } from './features/chat/CatSidePanel'
import { ChatImagePreviewProvider } from './features/chat/ChatImagePreviewProvider'
import { ChatPanel } from './features/chat/ChatPanel'
import { useWorkActivity } from './features/chat/useWorkActivity'
import {
  ConversationPanel,
  DmThreadDialog,
  GroupCreateDialog,
} from './features/chat/ConversationPanel'
import { useWorkspace } from './features/workspace/useWorkspace'
import { saveMessage, updateDraft, type NewAgent } from './features/workspace/model'
import {
  FirstCatPage,
  RecruitCatDialog,
  WorkspaceDialog,
} from './features/workspace/WorkspaceDialogs'
import { getBackend } from './features/desktop/backend'
import { previewAttachment } from './features/chat/MessageFile'
import { attachmentKind } from '../../shared/attachmentMedia'
import type { DmNotice } from './features/workspace/model'
import {
  CatContextMenu,
  type CatMenuAction,
  type CatMenuAnchor,
} from './features/workspace/CatContextMenu'
import { DeleteCatDialog } from './features/workspace/CatActionDialogs'
import {
  createGroupForAgent,
  deleteAgent,
  getAgentGroups,
  getNextGroupName,
  moveAgent,
  renameAgent,
  renameAgentGroup,
} from './features/workspace/agentActions'
import { useSidebarPreferences } from './features/workspace/useSidebarPreferences'
import { removeSidebarAgent, setSidebarFlag } from './features/workspace/sidebarPreferences'

import { SearchDialog } from './features/search/SearchDialog'
import { ClientUpdates } from './features/settings/ClientUpdates'
import { SettingsDialog } from './features/settings/SettingsDialog'
import { useTheme } from './features/settings/useTheme'
import { SidebarResizer, SIDEBAR_WIDTH } from './features/workspace/SidebarResizer'
import { useLanguage } from './features/settings/useLanguage'
import { useConnectionStatus } from './features/session/useConnectionStatus'
import { useUserProfile } from './features/session/useUserProfile'
import { useSession } from './features/session/useSession'
import { SessionFrame, SessionGate } from './features/session/SessionGate'

type OpenDialog = 'create' | 'search' | 'settings' | 'group' | 'workspace' | null
type Surface = { kind: 'agent' | 'group'; id: string }

/** 未连接、未验证或主机引导未完成时只显示门禁页，避免把预设工作空间当成已连接。 */
export default function App() {
  const session = useSession()
  useTheme()
  return (
    <ClientUpdates
      key={session.phase === 'login' || session.phase === 'ready' ? session.serverUrl : ''}
      signedIn={session.phase === 'ready'}
      connected={!!session.serverUrl && (session.phase === 'login' || session.phase === 'ready')}
    >
      {session.phase === 'ready' ? (
        <ChatImagePreviewProvider>
          <WorkspaceApp session={session} />
        </ChatImagePreviewProvider>
      ) : (
        <SessionGate session={session} />
      )}
    </ClientUpdates>
  )
}

/** 组合工作空间界面并协调领域操作；错误始终向用户展示。 */
function WorkspaceApp({ session }: { session: ReturnType<typeof useSession> }) {
  const { serverUrl } = session
  const { workStates, receiveActivity, clearActivity } = useWorkActivity()
  const {
    workspace,
    commit,
    create,
    deliver,
    react,
    error,
    isBlocked,
    ready,
    retryLoadingWorkspace,
    historyPages,
    loadEarlier,
    hasUnsavedChanges,
    dismissError,
  } = useWorkspace(receiveActivity, clearActivity)
  const connectionStatus = useConnectionStatus(retryLoadingWorkspace)
  const {
    profile: userProfile,
    error: profileError,
    dismissError: dismissProfileError,
  } = useUserProfile()
  const { preferences, update: updatePreferences, error: sidebarError } = useSidebarPreferences()
  const [openDialog, setOpenDialog] = useState<OpenDialog>(null)
  const [sidebarWidth, setSidebarWidth] = useState<number>(SIDEBAR_WIDTH.default)
  const [isResizing, setResizing] = useState(false)
  const { theme, setTheme, error: themeError } = useTheme()
  const [notice, setNotice] = useState('')
  const [menuAnchor, setMenuAnchor] = useState<CatMenuAnchor>()
  const [renamingAgentId, setRenamingAgentId] = useState<string>()
  const [editingGroupName, setEditingGroupName] = useState<string>()
  const [deletingAgentId, setDeletingAgentId] = useState<string>()
  const { language, setLanguage, error: languageError } = useLanguage()
  const [isCatPanelOpen, setCatPanelOpen] = useState(false)
  const [surface, setSurface] = useState<Surface>({ kind: 'agent', id: '' })
  const [chatGroups, setChatGroups] = useState<GroupSummary[]>([])
  const conversationPreviews = useConversationPreviews(
    ready
      ? [
          ...workspace.agents.flatMap((agent) =>
            agent.conversationId ? [agent.conversationId] : [],
          ),
          ...chatGroups.map((group) => group.conversation_id),
        ]
      : [],
    setNotice,
  )
  const [dmThread, setDmThread] = useState<{ notice: DmNotice; selfId: string }>()
  const [pendingMessages, setPendingMessages] = useState<Record<string, string[]>>({})

  function startDelivery(agentId: string, content: string, messageId: string): void {
    const source = crypto.randomUUID()
    setPendingMessages((current) => ({
      ...current,
      [agentId]: [...(current[agentId] ?? []), messageId],
    }))
    void deliver(agentId, content, messageId, (streamEvent) => {
      receiveActivity(source, streamEvent)
    })
      .then((result) => {
        if (result.message) setNotice(result.message)
        refreshRooms()
      })
      .catch((failure: Error) => setNotice(failure.message))
      .finally(() => {
        setPendingMessages((current) => {
          const next = { ...current }
          next[agentId] = next[agentId]?.filter((id) => id !== messageId) ?? []
          return next
        })
        clearActivity(source)
      })
  }
  const agent = workspace.agents.find((candidate) => candidate.id === workspace.activeAgentId)
  const agentIds = [
    ...workspace.agents.map((item) => item.id),
    ...chatGroups.map((group) => group.id),
  ]
  const menuAgent = menuAnchor
    ? workspace.agents.find((candidate) => candidate.id === menuAnchor.agentId)
    : undefined
  const deletingAgent = deletingAgentId
    ? workspace.agents.find((candidate) => candidate.id === deletingAgentId)
    : undefined
  const hiddenAgents = workspace.agents.filter((candidate) =>
    preferences.hiddenIds.includes(candidate.id),
  )
  const bannerMessage =
    error || profileError || themeError || sidebarError || languageError || notice
  const closeMenu = useCallback(() => setMenuAnchor(undefined), [])
  const sidebarCategories = [
    ...new Set([...getAgentGroups(workspace), ...Object.values(preferences.chatCategories ?? {})]),
  ]
  const groupActions = useGroupChatActions({
    activeGroupId: surface.kind === 'group' ? surface.id : undefined,
    onHide: (id) => {
      if (surface.kind === 'group' && surface.id === id)
        setSurface({ kind: 'agent', id: workspace.activeAgentId })
    },
    groups: chatGroups,
    categories: sidebarCategories,
    ids: agentIds,
    preferences,
    update: updatePreferences,
    onError: setNotice,
    onSelect: (id) => {
      setSurface({ kind: 'group', id })
      setCatPanelOpen(false)
    },
    onChanged: (group, deleted) => {
      setChatGroups((current) =>
        deleted
          ? current.filter((item) => item.id !== group.id)
          : current.map((item) => (item.id === group.id ? group : item)),
      )
      if (deleted && surface.kind === 'group' && surface.id === group.id)
        setSurface({ kind: 'agent', id: workspace.activeAgentId })
    },
  })

  // 活动只由传输层事件驱动；本地草稿没有进行中的请求，不能在这里写入 working。
  useEffect(() => installAgentActivityEvents(), [])
  const refreshRooms = useCallback(() => {
    const backend = window.desktop?.backend
    void backend
      ?.listGroups?.()
      .then((rows) => setChatGroups(rows))
      .catch((failure: Error) => setNotice(failure.message))
  }, [])
  useEffect(() => {
    if (ready && !isBlocked) refreshRooms()
  }, [ready, isBlocked, refreshRooms])
  // 帧没解码完不切换图片，失败时保持睁眼原图，错误已在预加载里记录。
  useEffect(() => {
    preloadCatFrames().catch(() => undefined)
  }, [])

  useEffect(() => {
    function handleShortcut(event: KeyboardEvent): void {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'k') {
        event.preventDefault()
        setMenuAnchor(undefined)
        setOpenDialog('search')
      }
    }
    window.addEventListener('keydown', handleShortcut)
    return () => window.removeEventListener('keydown', handleShortcut)
  }, [])

  useEffect(() => {
    if (!hasUnsavedChanges || isBlocked) return
    function preventLoss(event: BeforeUnloadEvent): void {
      event.preventDefault()
    }
    window.addEventListener('beforeunload', preventLoss)
    return () => window.removeEventListener('beforeunload', preventLoss)
  }, [hasUnsavedChanges, isBlocked])

  function runAction(action: () => void): void {
    try {
      action()
    } catch (failure) {
      setNotice((failure as Error).message)
    }
  }
  function unpinAgent(id: string): void {
    if (!preferences.pinnedIds.includes(id)) return
    updatePreferences((previous) => setSidebarFlag(previous, agentIds, id, 'pinnedIds', false))
  }
  function selectAgent(id: string): void {
    setSurface({ kind: 'agent', id })
    runAction(() => {
      commit((previous) => ({ ...previous, activeAgentId: id }))
      setOpenDialog(null)
      if (preferences.unreadIds.includes(id)) {
        updatePreferences((previous) => setSidebarFlag(previous, agentIds, id, 'unreadIds', false))
      }
    })
  }
  function openSettings(): void {
    setOpenDialog('settings')
  }
  const uploadIds = useRef(new WeakMap<File, string>())

  async function createAgent(profile: NewAgent, requestId: string): Promise<void> {
    const id = await create(profile, requestId)
    setSurface({ kind: 'agent', id })
    setCatPanelOpen(false)
    // v2 没有独立的欢迎语回合：新建机器人后直接进入空会话，由用户发起第一句。
    setOpenDialog(null)
  }
  async function copyConversationId(target: { id: string; name: string }): Promise<void> {
    try {
      await navigator.clipboard.writeText(target.id)
      setNotice(`已复制「${target.name}」的对话 ID：${target.id}`)
    } catch {
      setNotice('复制对话 ID 失败，请检查剪贴板权限后重试。')
    }
  }
  function handleMenuAction(action: CatMenuAction): void {
    const target = menuAgent
    if (!target) return
    closeMenu()
    switch (action) {
      case 'pin': {
        const isPinned =
          target.id === workspace.leadAgentId || preferences.pinnedIds.includes(target.id)
        runAction(() => {
          updatePreferences((previous) =>
            setSidebarFlag(previous, agentIds, target.id, 'pinnedIds', !isPinned),
          )
        })
        break
      }
      case 'unread': {
        const isUnread = preferences.unreadIds.includes(target.id)
        runAction(() => {
          updatePreferences((previous) =>
            setSidebarFlag(previous, agentIds, target.id, 'unreadIds', !isUnread),
          )
        })
        break
      }
      case 'rename':
        setNotice('')
        setEditingGroupName(undefined)
        setRenamingAgentId(target.id)
        break
      case 'copy':
        void copyConversationId(target)
        break
      case 'hide':
        setRenamingAgentId(undefined)
        setEditingGroupName(undefined)
        runAction(() => {
          if (workspace.activeAgentId === target.id) {
            const fallback = workspace.agents.find((agent) => agent.id !== target.id)?.id
            if (fallback) {
              commit((previous) => ({ ...previous, activeAgentId: fallback }))
            }
          }
          updatePreferences((previous) =>
            setSidebarFlag(previous, agentIds, target.id, 'hiddenIds', true),
          )
          setNotice(`已从侧边栏隐藏「${target.name}」，可在“我的工作空间”恢复显示。`)
        })
        break
      case 'delete':
        setRenamingAgentId(undefined)
        setEditingGroupName(undefined)
        setDeletingAgentId(target.id)
        break
    }
  }
  function handleMoveAgent(group: string): void {
    const target = menuAgent
    if (!target) return
    closeMenu()
    runAction(() => {
      unpinAgent(target.id)
      commit((previous) => moveAgent(previous, target.id, group, sidebarCategories))
      setRenamingAgentId(undefined)
      setEditingGroupName(undefined)
      setNotice(`已将「${target.name}」移至「${group}」`)
    })
  }
  function handleCreateGroup(): void {
    const target = menuAgent
    if (!target) return
    closeMenu()
    setNotice('')
    runAction(() => {
      const group = getNextGroupName(workspace)
      unpinAgent(target.id)
      commit((previous) => createGroupForAgent(previous, target.id, group))
      setRenamingAgentId(undefined)
      setEditingGroupName(group)
    })
  }
  function renameCurrentAgent(id: string, name: string): boolean {
    try {
      commit((previous) => renameAgent(previous, id, name))
      setRenamingAgentId(undefined)
      setNotice('')
      return true
    } catch (failure) {
      setNotice((failure as Error).message)
      return false
    }
  }
  function renameCurrentGroup(previousName: string, name: string): boolean {
    try {
      commit((previous) => renameAgentGroup(previous, previousName, name))
      updatePreferences((previous) => ({
        ...previous,
        chatCategories: Object.fromEntries(
          Object.entries(previous.chatCategories ?? {}).map(([id, category]) => [
            id,
            category === previousName ? name.trim() : category,
          ]),
        ),
      }))
      setEditingGroupName(undefined)
      setNotice('')
      return true
    } catch (failure) {
      setNotice((failure as Error).message)
      return false
    }
  }
  function deleteCurrentAgent(): void {
    const target = deletingAgent
    if (!target) return
    commit((previous) => deleteAgent(previous, target.id))
    let preferenceError = ''
    try {
      updatePreferences((previous) => removeSidebarAgent(previous, target.id))
    } catch (failure) {
      preferenceError = (failure as Error).message
    }
    setDeletingAgentId(undefined)
    setEditingGroupName(undefined)
    setNotice(preferenceError || `已删除「${target.name}」`)
  }
  function showHiddenAgent(id: string): void {
    runAction(() => {
      const target = workspace.agents.find((candidate) => candidate.id === id)
      if (!target) throw new Error('猫咪不存在')
      updatePreferences((previous) => setSidebarFlag(previous, agentIds, id, 'hiddenIds', false))
      setNotice(`已恢复显示「${target.name}」`)
    })
  }

  const settingsDialog = openDialog === 'settings' && (
    <SettingsDialog
      onClose={() => {
        setOpenDialog(null)
      }}
      theme={theme}
      onThemeChange={setTheme}
      themeError={themeError}
      userProfile={userProfile}
      profileError={profileError}
      isSigningOut={session.pending}
      signOutError={session.message}
      onSignOut={session.signOut}
      language={language}
      onLanguageChange={setLanguage}
      languageError={languageError}
    />
  )
  if (!ready) {
    return (
      <SessionFrame>
        <p role="status">正在打开工作空间</p>
      </SessionFrame>
    )
  }
  if (isBlocked && !agent) {
    return (
      <SessionFrame>
        <section className="session-card">
          <h1>暂时无法打开工作空间</h1>
          <p className="form-error" role="alert">
            {error}
          </p>
        </section>
      </SessionFrame>
    )
  }
  if (!agent) {
    return (
      <>
        <FirstCatPage
          onCreate={createAgent}
          groups={[...new Set(workspace.agents.map((item) => item.group))]}
        />
        {settingsDialog}
      </>
    )
  }

  const composerNotice = bannerMessage && openDialog !== 'settings' && (
    <div role="alert" className="error-banner">
      <span className="error-banner-icon" aria-hidden="true">
        <AlertCircle size={16} />
      </span>
      <span className="error-banner-text">{bannerMessage}</span>
      {error && !isBlocked && hasUnsavedChanges ? (
        <button
          onClick={() =>
            runAction(() => {
              commit((previous) => previous)
            })
          }
        >
          重试保存
        </button>
      ) : (
        !isBlocked &&
        (error || profileError || (!themeError && !sidebarError && !languageError)) && (
          <button
            aria-label="关闭提示"
            onClick={() => {
              if (error) dismissError()
              else if (profileError) dismissProfileError()
              else setNotice('')
            }}
          >
            <X size={16} />
          </button>
        )
      )}
    </div>
  )

  return (
    <div
      className={`app-shell ${sidebarWidth === SIDEBAR_WIDTH.collapsed ? 'sidebar-is-collapsed' : ''} ${isResizing ? 'is-resizing' : ''}`}
      style={{ '--sidebar-width': `${sidebarWidth}px` } as CSSProperties}
    >
      <Sidebar
        userProfile={userProfile}
        workspace={workspace}
        preferences={preferences}
        isCollapsed={sidebarWidth === SIDEBAR_WIDTH.collapsed}
        renamingAgentId={renamingAgentId}
        editingGroupName={editingGroupName}
        onAgentMenu={(anchor) => {
          groupActions.openMenu(undefined)
          setMenuAnchor(anchor)
        }}
        onGroupMenu={(anchor) => {
          closeMenu()
          groupActions.openMenu(anchor)
        }}
        renamingChatId={groupActions.renamingId}
        onRenameChat={groupActions.rename}
        onCancelRenameChat={groupActions.cancelRename}
        onRenameAgent={renameCurrentAgent}
        onCancelRenameAgent={() => setRenamingAgentId(undefined)}
        onRenameGroup={renameCurrentGroup}
        onCancelRenameGroup={() => setEditingGroupName(undefined)}
        onWorkspace={() => setOpenDialog('workspace')}
        onSelect={selectAgent}
        onSearch={() => setOpenDialog('search')}
        onCreate={() => setOpenDialog('create')}
        onSettings={() => openSettings()}
        chatGroups={chatGroups}
        previews={conversationPreviews}
        onMove={(kind, id, category) =>
          runAction(() => {
            if (kind === 'group') groupActions.move(id, category)
            else {
              const next = moveAgent(workspace, id, category, sidebarCategories)
              unpinAgent(id)
              commit(() => next)
            }
          })
        }
        selected={
          surface.kind === 'agent' ? { kind: 'agent', id: workspace.activeAgentId } : surface
        }
        onOpenGroup={groupActions.select}
        onCreateGroup={() => setOpenDialog('group')}
        serverUrl={serverUrl}
        connectionStatus={connectionStatus}
        onError={setNotice}
      />
      <SidebarResizer width={sidebarWidth} onChange={setSidebarWidth} onResizing={setResizing} />
      <div className="chat-stage">
        {surface.kind !== 'agent' ? (
          <ConversationPanel
            userProfile={userProfile}
            composerNotice={composerNotice}
            key={surface.id}
            id={surface.id}
            name={chatGroups.find((group) => group.id === surface.id)?.name}
            agents={workspace.agents}
            onNotice={setNotice}
            onError={setNotice}
            onGroupChange={(group) =>
              setChatGroups((current) =>
                current.map((item) => (item.id === group.id ? group : item)),
              )
            }
          />
        ) : (
          <ChatPanel
            composerNotice={composerNotice}
            key={agent.id}
            agent={agent}
            isBlocked={isBlocked}
            workSummary={workStates[agent.id]?.summary}
            pendingMessageIds={pendingMessages[agent.id]}
            hasEarlierMessages={historyPages[agent.id]?.hasMore}
            isLoadingEarlier={historyPages[agent.id]?.loading}
            onLoadEarlier={() => {
              void loadEarlier(agent.id)
            }}
            isDetailsOpen={isCatPanelOpen}
            onDraft={(draft) =>
              runAction(() => {
                commit((previous) => updateDraft(previous, agent.id, draft))
              })
            }
            onSave={(replyToId, attachments) => {
              try {
                const messageId = ulid()
                // 保存会清空草稿，投递内容必须在提交前取出。
                const content =
                  agent.draft.trim() || attachments?.map((item) => item.name).join('、') || ''
                const saved = commit((previous) =>
                  saveMessage(
                    previous,
                    agent.id,
                    messageId,
                    new Date().toISOString(),
                    replyToId,
                    attachments,
                  ),
                )
                if (!saved) return false
                startDelivery(agent.id, content, messageId)
                return true
              } catch (failure) {
                setNotice((failure as Error).message)
                return false
              }
            }}
            onDelete={(messageId) =>
              runAction(() => {
                commit((previous) => ({
                  ...previous,
                  agents: previous.agents.map((item) =>
                    item.id === agent.id
                      ? {
                          ...item,
                          messages: item.messages.filter((message) => message.id !== messageId),
                        }
                      : item,
                  ),
                }))
              })
            }
            onReact={(messageId, emoji) =>
              void react(agent.id, messageId, emoji).then(
                () =>
                  setNotice((previous) => (previous.startsWith('表情更新失败：') ? '' : previous)),
                (failure: Error) => setNotice(`表情更新失败：${failure.message}`),
              )
            }
            onRetry={(messageId) => {
              const failed = agent.messages.find((message) => message.id === messageId)
              if (!failed) {
                setNotice('找不到这条消息，请重新输入。')
                return
              }
              startDelivery(agent.id, failed.content, messageId)
            }}
            onDetails={() => setCatPanelOpen((open) => !open)}
            onNotice={setNotice}
            onError={setNotice}
            onUpload={async (file) => {
              const requestId = uploadIds.current.get(file) ?? ulid()
              uploadIds.current.set(file, requestId)
              const uploaded = await getBackend().uploadAttachment?.(
                {
                  name: file.name,
                  type: file.type,
                  bytes: new Uint8Array(await file.arrayBuffer()),
                  requestId,
                },
                { botId: agent.id },
              )
              if (!uploaded) throw new Error('还不能添加附件。')
              return {
                name: file.name,
                path: uploaded.path,
                sha256: uploaded.sha256,
                mediaType: file.type,
                kind: attachmentKind(file.name, file.type),
              }
            }}
            onDownload={async (attachment) => {
              const { conversationId, fileId } = parseChatFilePath(attachment.path)
              if (await getBackend().downloadChatFile(conversationId, fileId))
                setNotice('文件已保存。')
            }}
            onPreview={previewAttachment}
            peers={workspace.agents}
            onOpenDm={(notice) => setDmThread({ notice, selfId: agent.id })}
          />
        )}
        {surface.kind === 'agent' && isCatPanelOpen && (
          <CatSidePanel
            agent={agent}
            isLead={agent.id === workspace.leadAgentId}
            onClose={() => setCatPanelOpen(false)}
          />
        )}
      </div>
      {groupActions.overlays}
      {menuAnchor && menuAgent && (
        <CatContextMenu
          anchor={menuAnchor}
          agent={menuAgent}
          chatType="bot"
          groups={sidebarCategories}
          isLead={menuAgent.id === workspace.leadAgentId}
          isPinned={
            menuAgent.id === workspace.leadAgentId || preferences.pinnedIds.includes(menuAgent.id)
          }
          isUnread={preferences.unreadIds.includes(menuAgent.id)}
          onAction={handleMenuAction}
          onMove={handleMoveAgent}
          onCreateGroup={handleCreateGroup}
          onClose={closeMenu}
        />
      )}
      {openDialog === 'create' && (
        <RecruitCatDialog
          onClose={() => {
            setOpenDialog(null)
          }}
          onCreate={createAgent}
          groups={[...new Set(workspace.agents.map((item) => item.group))]}
        />
      )}
      {openDialog === 'search' && (
        <SearchDialog
          agents={workspace.agents}
          activeAgentId={workspace.activeAgentId}
          onSettings={openSettings}
          onCreate={() => setOpenDialog('create')}
          onCreateGroup={() => setOpenDialog('group')}
          onClose={() => setOpenDialog(null)}
          onSelect={selectAgent}
        />
      )}
      {settingsDialog}
      {openDialog === 'workspace' && (
        <WorkspaceDialog
          workspace={workspace}
          hiddenAgents={hiddenAgents}
          hiddenChats={chatGroups.filter((group) => preferences.hiddenIds.includes(group.id))}
          onUnhideChat={(id) =>
            runAction(() =>
              updatePreferences((previous) =>
                setSidebarFlag(previous, agentIds, id, 'hiddenIds', false),
              ),
            )
          }
          connectionStatus={connectionStatus}
          onUnhide={showHiddenAgent}
          onClose={() => setOpenDialog(null)}
        />
      )}
      {dmThread && (
        <DmThreadDialog
          notice={dmThread.notice}
          self={workspace.agents.find((item) => item.id === dmThread.selfId) ?? agent}
          agents={workspace.agents}
          onClose={() => setDmThread(undefined)}
          onError={setNotice}
        />
      )}
      {openDialog === 'group' && (
        <GroupCreateDialog
          agents={workspace.agents}
          onClose={() => setOpenDialog(null)}
          onCreate={async (name, memberIds, requestId) => {
            const created = await getBackend().createGroup?.(name, memberIds, requestId)
            if (!created) throw new Error('还不能创建群聊。')
            refreshRooms()
            setSurface({ kind: 'group', id: created.id })
            setOpenDialog(null)
          }}
        />
      )}
      {deletingAgent && (
        <DeleteCatDialog
          agent={deletingAgent}
          onDelete={deleteCurrentAgent}
          onClose={() => setDeletingAgentId(undefined)}
        />
      )}
    </div>
  )
}
