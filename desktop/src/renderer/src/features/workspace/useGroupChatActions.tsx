import { useState, useEffect } from 'react'
import { Dialog } from '../../components/Dialog'
import { getBackend, type GroupSummary } from '../desktop/backend'
import { CatContextMenu, type CatMenuAnchor, type CatMenuAction } from './CatContextMenu'
import { removeSidebarAgent, setSidebarFlag, type SidebarPreferences } from './sidebarPreferences'

/** 群聊复用聊天菜单，服务端修改成功后更新目录，本机展示偏好保存失败时不生效。 */
export function useGroupChatActions({
  groups,
  categories,
  ids,
  preferences,
  update,
  onChanged,
  onSelect,
  onError,
  activeGroupId,
  onHide,
}: {
  activeGroupId?: string
  onHide?: (id: string) => void
  groups: GroupSummary[]
  categories: string[]
  ids: string[]
  preferences: SidebarPreferences
  update: (action: (previous: SidebarPreferences) => SidebarPreferences) => void
  onChanged: (group: GroupSummary, deleted?: boolean) => void
  onSelect: (id: string) => void
  onError: (message: string) => void
}) {
  const [anchor, setAnchor] = useState<CatMenuAnchor>()
  const [renamingId, setRenamingId] = useState<string>()
  const [deleting, setDeleting] = useState<GroupSummary>()
  const [moving, setMoving] = useState<GroupSummary>()
  const [category, setCategory] = useState('新分组')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState('')
  const target = groups.find((group) => group.id === anchor?.agentId)
  useEffect(
    () =>
      getBackend().watchEvents?.(
        (event) => {
          if (event.kind !== 'message.created') return
          const group = groups.find(
            (item) => item.conversation_id === event.payload.conversation_id,
          )
          if (!group || group.id === activeGroupId) return
          try {
            update((previous) => setSidebarFlag(previous, ids, group.id, 'unreadIds', true))
          } catch (failure) {
            onError((failure as Error).message)
          }
        },
        (failure) => onError(failure.message),
      ),
    [groups, ids, activeGroupId, update, onError],
  )
  function flag(id: string, key: 'pinnedIds' | 'unreadIds' | 'hiddenIds', enabled: boolean) {
    update((previous) => setSidebarFlag(previous, ids, id, key, enabled))
  }
  function move(group: GroupSummary, name: string) {
    update((previous) => ({
      ...setSidebarFlag(previous, ids, group.id, 'pinnedIds', false),
      chatCategories: { ...previous.chatCategories, [group.id]: name.trim() },
    }))
    setAnchor(undefined)
  }
  function action(action: CatMenuAction) {
    if (!target) return
    setAnchor(undefined)
    try {
      if (action === 'pin') flag(target.id, 'pinnedIds', !preferences.pinnedIds.includes(target.id))
      if (action === 'unread')
        flag(target.id, 'unreadIds', !preferences.unreadIds.includes(target.id))
      if (action === 'hide') {
        flag(target.id, 'hiddenIds', true)
        onHide?.(target.id)
        onError(`已隐藏「${target.name}」，可在“我的工作空间”恢复显示。`)
      }
      if (action === 'rename') {
        setRenamingId(target.id)
        onError('')
      }
      if (action === 'delete') {
        setDeleting(target)
        setError('')
      }
      if (action === 'copy')
        void navigator.clipboard
          .writeText(target.conversation_id)
          .then(() => onError('已复制对话 ID'))
          .catch(() => onError('复制失败，请检查剪贴板权限。'))
    } catch (failure) {
      onError((failure as Error).message)
    }
  }
  async function rename(id: string, name: string): Promise<boolean> {
    const group = groups.find((group) => group.id === id)
    try {
      if (!group || !getBackend().renameGroup) throw new Error('群聊不存在或服务不可用。')
      if (!name.trim()) throw new Error('请填写聊天名称。')
      const changed = await getBackend().renameGroup!(id, group.revision, name.trim())
      onChanged(changed)
      setRenamingId(undefined)
      return true
    } catch (failure) {
      onError((failure as Error).message)
      try {
        if (group && getBackend().groupDetail) onChanged(await getBackend().groupDetail!(id))
      } catch (refreshFailure) {
        onError(`${(failure as Error).message}；${(refreshFailure as Error).message}`)
      }
      return false
    }
  }
  async function remove() {
    if (!deleting || pending) return
    setPending(true)
    try {
      const remove = getBackend().deleteGroup
      if (!remove) throw new Error('群聊删除服务不可用。')
      await remove(deleting.id, deleting.revision)
      onChanged(deleting, true)
      setDeleting(undefined)
      update((previous) => removeSidebarAgent(previous, deleting.id))
    } catch (failure) {
      setError((failure as Error).message)
      onError((failure as Error).message)
    } finally {
      setPending(false)
    }
  }
  return {
    move: (id: string, name: string) => {
      const group = groups.find((group) => group.id === id)
      if (!group) throw new Error('群聊不存在')
      move(group, name)
    },
    openMenu: setAnchor,
    renamingId,
    rename,
    cancelRename: () => setRenamingId(undefined),
    select: (id: string) => {
      try {
        onSelect(id)
        flag(id, 'unreadIds', false)
      } catch (failure) {
        onError((failure as Error).message)
      }
    },
    overlays: (
      <>
        {anchor && target && (
          <CatContextMenu
            anchor={anchor}
            agent={{
              name: target.name,
              group: preferences.chatCategories?.[target.id] ?? '未分组',
            }}
            chatType="group"
            groups={[
              ...new Set([
                ...categories,
                ...Object.values(preferences.chatCategories ?? {}),
                '未分组',
              ]),
            ]}
            isLead={false}
            isPinned={preferences.pinnedIds.includes(target.id)}
            isUnread={preferences.unreadIds.includes(target.id)}
            onAction={action}
            onClose={() => setAnchor(undefined)}
            onMove={(name) => {
              try {
                move(target, name)
              } catch (failure) {
                onError((failure as Error).message)
              }
            }}
            onCreateGroup={() => {
              setMoving(target)
              setCategory('新分组')
              setError('')
              setAnchor(undefined)
            }}
          />
        )}
        {deleting && (
          <Dialog
            title={`删除「${deleting.name}」`}
            onClose={() => {
              if (!pending) setDeleting(undefined)
            }}
          >
            <p>群聊将从列表移除，已有聊天记录保留在服务端。</p>
            {error && <p role="alert">{error}</p>}
            <button
              type="button"
              className="secondary-button"
              disabled={pending}
              onClick={() => void remove()}
            >
              {pending ? '正在删除…' : '确认删除'}
            </button>
          </Dialog>
        )}
        {moving && (
          <Dialog title="新增分组" onClose={() => setMoving(undefined)}>
            <form
              onSubmit={(event) => {
                event.preventDefault()
                try {
                  move(moving, category)
                  setMoving(undefined)
                } catch (failure) {
                  setError((failure as Error).message)
                }
              }}
            >
              <label>
                分组名称
                <input
                  className="inline-name-input"
                  maxLength={24}
                  value={category}
                  onChange={(event) => setCategory(event.target.value)}
                />
              </label>
              {error && <p role="alert">{error}</p>}
              <button className="secondary-button" type="submit" disabled={!category.trim()}>
                创建并移入
              </button>
            </form>
          </Dialog>
        )}
      </>
    ),
  }
}
