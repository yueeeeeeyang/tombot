import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, describe, it, expect, vi } from 'vitest'
import { useGroupChatActions } from './useGroupChatActions'
import { createSidebarPreferences } from './sidebarPreferences'
import { InlineNameInput } from '../../components/InlineNameInput'
import { MemoryBackend, memoryBackend } from '../desktop/memoryBackend'
import type { GroupDetail } from '../desktop/backend'
const initial: GroupDetail = {
  id: 'group',
  name: '讨论组',
  revision: 1,
  conversation_id: 'conversation',
  avatar_presets: ['bombay'],
  members: [{ bot_id: 'a', name: '小黑' }],
}
function open() {
  const rename = vi.fn(async (_id: string, revision: number, name: string) => ({
    ...initial,
    revision: revision + 1,
    name,
  }))
  const remove = vi.fn(async () => undefined)
  window.desktop = {
    platform: 'linux',
    backend: Object.assign(new MemoryBackend(), {
      renameGroup: rename,
      deleteGroup: remove,
      groupDetail: vi.fn(async () => ({ ...initial, revision: 7 })),
    }),
  }
  const notice = vi.fn()
  function Harness() {
    const [groups, setGroups] = useState([initial])
    const [prefs, setPrefs] = useState(createSidebarPreferences())
    const actions = useGroupChatActions({
      groups,
      categories: ['工作'],
      ids: ['group'],
      preferences: prefs,
      update: (fn) => setPrefs(fn),
      onError: notice,
      onSelect: vi.fn(),
      onChanged: (group, deleted) =>
        setGroups((current) => (deleted ? [] : current.map((item) => ({ ...item, ...group })))),
    })
    return (
      <>
        <output>{JSON.stringify(prefs)}</output>
        {groups.map((group) => (
          <div key={group.id}>
            {actions.renamingId === group.id ? (
              <InlineNameInput
                initialName={group.name}
                label="聊天名称"
                onCommit={(name) => actions.rename(group.id, name)}
                onCancel={actions.cancelRename}
              />
            ) : (
              <button
                onContextMenu={(event) => {
                  event.preventDefault()
                  actions.openMenu({ agentId: group.id, x: 0, y: 0, trigger: event.currentTarget })
                }}
              >
                {group.name}
              </button>
            )}
          </div>
        ))}
        {actions.overlays}
      </>
    )
  }
  render(<Harness />)
  return { rename, remove, notice }
}
function menu(name = '讨论组') {
  fireEvent.contextMenu(screen.getByRole('button', { name }))
}
afterEach(() => {
  vi.restoreAllMocks()
  window.desktop = { platform: 'linux', backend: memoryBackend }
})
describe('群聊共用菜单', () => {
  it('置顶、未读、移动和隐藏使用同一套菜单', () => {
    open()
    menu()
    expect(screen.getByRole('menuitem', { name: '重命名聊天' })).toBeInTheDocument()
    expect(screen.queryByRole('menuitem', { name: '重命名猫咪' })).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('menuitem', { name: '置顶' }))
    expect(screen.getByRole('status')).toHaveTextContent('"pinnedIds":["group"]')
    menu()
    fireEvent.click(screen.getByRole('menuitem', { name: '标为未读' }))
    expect(screen.getByRole('status')).toHaveTextContent('"unreadIds":["group"]')
    menu()
    fireEvent.click(screen.getByRole('menuitem', { name: '移至' }))
    fireEvent.click(screen.getByRole('menuitemradio', { name: '工作' }))
    expect(screen.getByRole('status')).toHaveTextContent('"group":"工作"')
    menu()
    fireEvent.click(screen.getByRole('menuitem', { name: '从侧边栏隐藏' }))
    expect(screen.getByRole('status')).toHaveTextContent('"hiddenIds":["group"]')
  })
  it('非当前群收到消息时标为未读', () => {
    open()
    act(() => {
      ;(window.desktop!.backend as MemoryBackend).publishEvent({
        sequence: 1,
        kind: 'message.created',
        resource_id: 'message',
        payload: { conversation_id: 'conversation' },
        created_at: '2026-11-17T00:00:00Z',
      })
    })
    expect(screen.getByRole('status')).toHaveTextContent('"unreadIds":["group"]')
  })
  it('重命名成功后更新目录，删除使用真实版本', async () => {
    const { rename, remove } = open()
    menu()
    fireEvent.click(screen.getByRole('menuitem', { name: '重命名聊天' }))
    fireEvent.change(screen.getByRole('textbox'), { target: { value: ' 新讨论组 ' } })
    fireEvent.keyDown(screen.getByRole('textbox'), { key: 'Enter' })
    await screen.findByRole('button', { name: '新讨论组' })
    expect(rename).toHaveBeenCalledWith('group', 1, '新讨论组')
    menu('新讨论组')
    fireEvent.click(screen.getByRole('menuitem', { name: '删除' }))
    fireEvent.click(screen.getByRole('button', { name: '确认删除' }))
    await waitFor(() => expect(remove).toHaveBeenCalledWith('group', 2))
    await waitFor(() => expect(screen.queryByRole('button', { name: '新讨论组' })).toBeNull())
  })
  it('失败保留名称并刷新版本，重复 Enter 不会重复提交', async () => {
    const { rename, notice } = open()
    let reject!: (error: Error) => void
    rename.mockImplementation(
      () =>
        new Promise((_resolve, fail) => {
          reject = fail
        }),
    )
    menu()
    fireEvent.click(screen.getByRole('menuitem', { name: '重命名聊天' }))
    const input = screen.getByRole('textbox')
    fireEvent.change(input, { target: { value: '待保存' } })
    fireEvent.keyDown(input, { key: 'Enter' })
    fireEvent.keyDown(input, { key: 'Enter' })
    expect(rename).toHaveBeenCalledTimes(1)
    await act(async () => reject(new Error('版本冲突')))
    expect(notice).toHaveBeenCalledWith('版本冲突')
    expect(input).toHaveValue('待保存')
    fireEvent.keyDown(input, { key: 'Enter' })
    expect(rename).toHaveBeenLastCalledWith('group', 7, '待保存')
    await act(async () => reject(new Error('仍然失败')))
  })
})
