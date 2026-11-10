import { useCallback, useEffect, useState, type CSSProperties } from 'react'
import { installAgentActivityEvents, setAgentActivity } from './features/avatars/activity'
import { preloadCatFrames } from './features/avatars/catMotion'
import { AlertCircle, X } from 'lucide-react'
import { Sidebar } from './features/workspace/Sidebar'
import { CatSidePanel } from './features/chat/CatSidePanel'
import { ChatPanel } from './features/chat/ChatPanel'
import {
  ConversationPanel,
  DmThreadDialog,
  GroupCreateDialog,
} from './features/chat/ConversationPanel'
import { useWorkspace } from './features/workspace/useWorkspace'
import {
  addAgent,
  insertDmNotice,
  saveMessage,
  setAgentComputerUrl,
  setAgentModel,
  toggleReaction,
  updateDraft,
  type NewAgent,
} from './features/workspace/model'
import { RecruitCatDialog, WorkspaceDialog } from './features/workspace/WorkspaceDialogs'
import { getBackend } from './features/desktop/backend'
import type { DmNotice, MessageAttachment } from './features/workspace/model'
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
import { SettingsDialog } from './features/settings/SettingsDialog'
import { useTheme } from './features/settings/useTheme'
import { SidebarResizer, SIDEBAR_WIDTH } from './features/workspace/SidebarResizer'
import { type SettingsSection } from './features/settings/settings'
import { useServerSettings } from './features/settings/useServerSettings'
import { getModelOptions } from './features/models/modelOptions'
import { useSession } from './features/session/useSession'
import { SessionGate } from './features/session/SessionGate'

type OpenDialog = 'create' | 'search' | 'settings' | 'group' | 'workspace' | null
type Surface = { kind: 'agent' | 'group'; id: string }

/** 未验证身份时只显示初始化或密码页，避免把预设工作空间当成已连接。 */
export default function App() {
  const session = useSession()
  useTheme()
  if (session.phase !== 'ready') return <SessionGate session={session} />
  return <WorkspaceApp />
}

/** 组合工作空间界面并协调领域操作；错误始终向用户展示。 */
function WorkspaceApp() {
  const { workspace, commit, deliver, error, isBlocked, ready } = useWorkspace()
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
  const {
    draft: settingsDraft,
    setDraft: setSettingsDraft,
    apiKey,
    setApiKey,
    hasApiKey,
    error: settingsError,
  } = useServerSettings()
  const [isCatPanelOpen, setCatPanelOpen] = useState(false)
  const [surface, setSurface] = useState<Surface>({ kind: 'agent', id: '' })
  const [chatGroups, setChatGroups] = useState<Array<{ id: string; name: string }>>([])
  const [dmThread, setDmThread] = useState<{ notice: DmNotice; selfId: string }>()
  const [liveReply, setLiveReply] = useState<{ agentId: string; content: string }>()

  function startDelivery(agentId: string, messageId: string, quietWhenUnconfigured: boolean): void {
    if (settingsDraft.baseUrl.trim() && settingsDraft.modelName.trim()) {
      setAgentActivity(agentId, 'working')
    }
    let processLine = ''
    let reply = ''
    void deliver(agentId, messageId, (streamEvent) => {
      if (
        streamEvent.type === 'dm' &&
        streamEvent.messageId &&
        streamEvent.peerId &&
        streamEvent.conversationId
      ) {
        const notice: DmNotice = {
          direction: streamEvent.direction ?? 'sent',
          peerId: streamEvent.peerId,
          peerName: streamEvent.peerName ?? streamEvent.peerId,
          conversationId: streamEvent.conversationId,
        }
        commit((previous) =>
          insertDmNotice(previous, agentId, {
            ...notice,
            id: streamEvent.messageId!,
            createdAt: new Date().toISOString(),
          }),
        )
        return
      }
      if (!settingsDraft.isStreaming) return
      if (streamEvent.type === 'process') processLine = streamEvent.text
      else if (streamEvent.type === 'delta') reply += streamEvent.text
      const content = [processLine, reply].filter((part) => part !== '').join('\n')
      setLiveReply({ agentId, content })
    }).then((result) => {
      setLiveReply(undefined)
      setAgentActivity(agentId, 'idle')
      void window.desktop?.backend
        .listGroups?.()
        .then(setChatGroups)
        .catch(() => undefined)
      if (result.code === 'MODEL_NOT_READY' && quietWhenUnconfigured) return
      if (result.message) setNotice(result.message)
    })
  }
  const [recruitProfile, setRecruitProfile] = useState<NewAgent>()
  const [returnToRecruit, setReturnToRecruit] = useState(false)
  const [settingsSection, setSettingsSection] = useState<SettingsSection>('general')
  const agent = workspace.agents.find((candidate) => candidate.id === workspace.activeAgentId)!
  const agentIds = workspace.agents.map((item) => item.id)
  const menuAgent = menuAnchor
    ? workspace.agents.find((candidate) => candidate.id === menuAnchor.agentId)
    : undefined
  const deletingAgent = deletingAgentId
    ? workspace.agents.find((candidate) => candidate.id === deletingAgentId)
    : undefined
  const hiddenAgents = workspace.agents.filter((candidate) =>
    preferences.hiddenIds.includes(candidate.id),
  )
  const bannerMessage = error || themeError || sidebarError || settingsError || notice
  const closeMenu = useCallback(() => setMenuAnchor(undefined), [])

  // 活动只由传输层事件驱动；本地草稿没有进行中的请求，不能在这里写入 working。
  useEffect(() => installAgentActivityEvents(), [])
  const refreshRooms = useCallback(() => {
    const backend = window.desktop?.backend
    void backend
      ?.listGroups?.()
      .then((rows) => setChatGroups(rows))
      .catch(() => undefined)
  }, [])
  useEffect(() => {
    if (ready) refreshRooms()
  }, [ready, refreshRooms])
  // 帧没解码完不切换图片，失败时保持睁眼原图，错误已在预加载里记录。
  useEffect(() => {
    preloadCatFrames().catch(() => undefined)
  }, [])

  useEffect(() => {
    function handleShortcut(event: KeyboardEvent): void {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'k') {
        event.preventDefault()
        setMenuAnchor(undefined)
        setReturnToRecruit(false)
        setOpenDialog('search')
      }
    }
    window.addEventListener('keydown', handleShortcut)
    return () => window.removeEventListener('keydown', handleShortcut)
  }, [])

  useEffect(() => {
    if (!error || isBlocked) return
    function preventLoss(event: BeforeUnloadEvent): void {
      event.preventDefault()
    }
    window.addEventListener('beforeunload', preventLoss)
    return () => window.removeEventListener('beforeunload', preventLoss)
  }, [error, isBlocked])

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
  function openSettings(section: SettingsSection = 'general', fromRecruit = false): void {
    setReturnToRecruit(fromRecruit)
    setSettingsSection(section)
    setOpenDialog('settings')
  }
  function createAgent(profile: NewAgent, model: { label: string }): void {
    const id = crypto.randomUUID()
    commit((previous) => addAgent(previous, profile, id, model.label))
    setRecruitProfile(undefined)
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
        const isPinned = target.id === 'assistant' || preferences.pinnedIds.includes(target.id)
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
            commit((previous) => ({ ...previous, activeAgentId: 'assistant' }))
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
      commit((previous) => moveAgent(previous, target.id, group))
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

  if (!ready) {
    return (
      <main className="session-gate">
        <p role="status">正在打开工作空间</p>
      </main>
    )
  }

  return (
    <div
      className={`app-shell ${sidebarWidth === SIDEBAR_WIDTH.collapsed ? 'sidebar-is-collapsed' : ''} ${isResizing ? 'is-resizing' : ''}`}
      style={{ '--sidebar-width': `${sidebarWidth}px` } as CSSProperties}
    >
      <Sidebar
        workspace={workspace}
        preferences={preferences}
        isCollapsed={sidebarWidth === SIDEBAR_WIDTH.collapsed}
        renamingAgentId={renamingAgentId}
        editingGroupName={editingGroupName}
        onAgentMenu={setMenuAnchor}
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
        selected={
          surface.kind === 'agent' ? { kind: 'agent', id: workspace.activeAgentId } : surface
        }
        onOpenGroup={(id) => {
          setSurface({ kind: 'group', id })
          setCatPanelOpen(false)
        }}
        onCreateGroup={() => setOpenDialog('group')}
      />
      <SidebarResizer width={sidebarWidth} onChange={setSidebarWidth} onResizing={setResizing} />
      <div className="chat-stage">
        {surface.kind !== 'agent' ? (
          <ConversationPanel
            key={surface.id}
            id={surface.id}
            agents={workspace.agents}
            onNotice={setNotice}
            onError={setNotice}
          />
        ) : (
          <ChatPanel
            key={agent.id}
            agent={
              liveReply?.agentId === agent.id
                ? {
                    ...agent,
                    messages: [
                      ...agent.messages,
                      {
                        id: 'live-reply',
                        role: 'assistant',
                        content: liveReply.content,
                        createdAt: new Date().toISOString(),
                        status: 'sent',
                        reactions: [],
                      },
                    ],
                  }
                : agent
            }
            isBlocked={isBlocked}
            isDetailsOpen={isCatPanelOpen}
            onDraft={(draft) =>
              runAction(() => {
                commit((previous) => updateDraft(previous, agent.id, draft))
              })
            }
            onSave={(replyToId, attachments) => {
              try {
                const messageId = crypto.randomUUID()
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
                startDelivery(agent.id, messageId, true)
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
              runAction(() => {
                commit((previous) => toggleReaction(previous, agent.id, messageId, emoji))
              })
            }
            onRetry={(messageId) => {
              startDelivery(agent.id, messageId, false)
            }}
            onDetails={() => setCatPanelOpen((open) => !open)}
            onNotice={setNotice}
            onError={setNotice}
            onUpload={async (file) => {
              const uploaded = await getBackend().uploadAttachment?.(
                {
                  name: file.name,
                  type: file.type,
                  bytes: new Uint8Array(await file.arrayBuffer()),
                },
                { botId: agent.id },
              )
              if (!uploaded) throw new Error('还不能添加附件。')
              const kind: MessageAttachment['kind'] =
                file.type === 'image/png' || file.type === 'image/jpeg' ? 'image' : 'file'
              return {
                name: file.name,
                path: uploaded.path,
                sha256: uploaded.sha256,
                mediaType: file.type,
                kind,
              }
            }}
            onPreview={(attachment) =>
              getBackend().previewAttachment?.(attachment.path, agent.id) ?? Promise.resolve('')
            }
            peers={workspace.agents}
            onOpenDm={(notice) => setDmThread({ notice, selfId: agent.id })}
          />
        )}
        {surface.kind === 'agent' && isCatPanelOpen && (
          <CatSidePanel
            agent={agent}
            onClose={() => setCatPanelOpen(false)}
            onModelChange={(modelId) =>
              runAction(() => {
                commit((previous) => setAgentModel(previous, agent.id, modelId))
              })
            }
            onComputerUrl={(computerUrl) =>
              runAction(() => {
                commit((previous) => setAgentComputerUrl(previous, agent.id, computerUrl))
              })
            }
            onPrepareComputer={() => {
              const start = window.desktop?.backend.startBootstrap
              if (!start) return
              void start().catch((failure: Error) => setNotice(failure.message))
            }}
            onConnectComputer={() => {
              const open = window.desktop?.backend.openComputer
              if (!open) return
              void open(agent.id)
                .then((url) => {
                  runAction(() => {
                    commit((previous) => setAgentComputerUrl(previous, agent.id, url))
                  })
                })
                .catch((failure: Error) => setNotice(failure.message))
            }}
          />
        )}
      </div>
      {menuAnchor && menuAgent && (
        <CatContextMenu
          anchor={menuAnchor}
          agent={menuAgent}
          groups={getAgentGroups(workspace)}
          isPinned={menuAgent.id === 'assistant' || preferences.pinnedIds.includes(menuAgent.id)}
          isUnread={preferences.unreadIds.includes(menuAgent.id)}
          onAction={handleMenuAction}
          onMove={handleMoveAgent}
          onCreateGroup={handleCreateGroup}
          onClose={closeMenu}
        />
      )}
      {bannerMessage && openDialog !== 'settings' && (
        <div role="alert" className="error-banner">
          <AlertCircle size={16} />
          <span>{bannerMessage}</span>
          {error && !isBlocked ? (
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
            !error &&
            !themeError &&
            !sidebarError && (
              <button aria-label="关闭提示" onClick={() => setNotice('')}>
                <X size={16} />
              </button>
            )
          )}
        </div>
      )}
      {openDialog === 'create' && (
        <RecruitCatDialog
          onClose={() => {
            setOpenDialog(null)
            setRecruitProfile(undefined)
          }}
          onCreate={createAgent}
          initialProfile={recruitProfile}
          onModelSettings={(profile) => {
            setRecruitProfile(profile)
            openSettings('models', true)
          }}
          groups={[...new Set(workspace.agents.map((item) => item.group))]}
          modelOptions={getModelOptions(settingsDraft)}
        />
      )}
      {openDialog === 'search' && (
        <SearchDialog
          agents={workspace.agents}
          activeAgentId={workspace.activeAgentId}
          onSettings={openSettings}
          onClose={() => setOpenDialog(null)}
          onSelect={selectAgent}
        />
      )}
      {openDialog === 'settings' && (
        <SettingsDialog
          onClose={() => {
            setOpenDialog(returnToRecruit ? 'create' : null)
            setReturnToRecruit(false)
          }}
          theme={theme}
          onThemeChange={setTheme}
          themeError={themeError}
          section={settingsSection}
          onSectionChange={setSettingsSection}
          draft={settingsDraft}
          onChange={setSettingsDraft}
          apiKey={apiKey}
          hasApiKey={hasApiKey}
          onApiKeyChange={setApiKey}
        />
      )}
      {openDialog === 'workspace' && (
        <WorkspaceDialog
          workspace={workspace}
          hiddenAgents={hiddenAgents}
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
          onCreate={async (name, memberIds) => {
            const created = await getBackend().createGroup?.(name, memberIds)
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
