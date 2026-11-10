import { useCallback, useEffect, useState, type CSSProperties } from 'react'
import { installAgentActivityEvents } from './features/avatars/activity'
import { preloadCatFrames } from './features/avatars/catMotion'
import { AlertCircle, X } from 'lucide-react'
import { Sidebar } from './features/workspace/Sidebar'
import { CatSidePanel } from './features/chat/CatSidePanel'
import { ChatPanel } from './features/chat/ChatPanel'
import { useWorkspace } from './features/workspace/useWorkspace'
import {
  addAgent,
  saveMessage,
  setAgentComputerUrl,
  setAgentModel,
  toggleReaction,
  updateDraft,
  type NewAgent,
} from './features/workspace/model'
import {
  RecruitCatDialog,
  ShareDialog,
  WorkspaceDialog,
} from './features/workspace/WorkspaceDialogs'
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
import { createSettingsDraft, type SettingsSection } from './features/settings/settings'
import { getModelOptions } from './features/models/modelOptions'

type OpenDialog = 'create' | 'search' | 'settings' | 'share' | 'workspace' | null

/** 组合工作空间界面并协调领域操作；错误始终向用户展示。 */
export default function App() {
  const { workspace, commit, error, isBlocked } = useWorkspace()
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
  const [settingsDraft, setSettingsDraft] = useState(createSettingsDraft)
  const [isCatPanelOpen, setCatPanelOpen] = useState(false)
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
  const bannerMessage = error || themeError || sidebarError || notice
  const closeMenu = useCallback(() => setMenuAnchor(undefined), [])

  // 活动只由传输层事件驱动；本地草稿没有进行中的请求，不能在这里写入 working。
  useEffect(() => installAgentActivityEvents(), [])
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
  function createAgent(profile: NewAgent): void {
    const id = crypto.randomUUID()
    commit((previous) => addAgent(previous, profile, id))
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
      />
      <SidebarResizer width={sidebarWidth} onChange={setSidebarWidth} onResizing={setResizing} />
      <div className="chat-stage">
        <ChatPanel
          key={agent.id}
          agent={agent}
          isBlocked={isBlocked}
          isDetailsOpen={isCatPanelOpen}
          onDraft={(draft) =>
            runAction(() => {
              commit((previous) => updateDraft(previous, agent.id, draft))
            })
          }
          onSave={(replyToId) => {
            try {
              commit((previous) =>
                saveMessage(
                  previous,
                  agent.id,
                  crypto.randomUUID(),
                  new Date().toISOString(),
                  replyToId,
                ),
              )
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
          onRetry={() => setNotice('服务端尚未接入，暂时无法重新发送。')}
          onDetails={() => setCatPanelOpen((open) => !open)}
          onNotice={setNotice}
          onError={setNotice}
        />
        {isCatPanelOpen && (
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
            onExport={() => setOpenDialog('share')}
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
      {openDialog === 'share' && <ShareDialog agent={agent} onClose={() => setOpenDialog(null)} />}
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
