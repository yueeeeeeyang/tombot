import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { expect, it, vi } from 'vitest'
import App from './App'
import { memoryBackend } from './features/desktop/memoryBackend'
import { createWorkspace } from './features/workspace/model'
import { SIDEBAR_STORAGE_KEY } from './features/workspace/sidebarPreferences'

const chat = {
  id: 'group',
  name: '项目讨论',
  revision: 1,
  conversation_id: 'group-chat',
  avatar_presets: ['bombay', 'persian'],
}
function setup() {
  const workspace = createWorkspace()
  const cat = workspace.agents.find((agent) => agent.id === 'developer')!
  cat.conversationId = 'cat-chat'
  cat.messages = []
  memoryBackend.loadFixture({ workspace, isModelPolicyAvailable: true })
  vi.spyOn(memoryBackend, 'listGroups').mockResolvedValue([chat])
  vi.spyOn(memoryBackend, 'loadConversationPreviews').mockImplementation(async (ids) =>
    ids.map((id) => ({
      conversation_id: id,
      sequence: 5,
      content: id === 'group-chat' ? '最新群消息' : '最新机器人消息',
    })),
  )
}
it('启动无需打开会话即可展示摘要，群聊默认未分组，重开仍恢复', async () => {
  setup()
  const view = render(<App />)
  await waitFor(() =>
    expect(screen.getByRole('button', { name: '项目讨论' })).toHaveTextContent('最新群消息'),
  )
  expect(screen.getByRole('button', { name: '游戏程序' })).toHaveTextContent('最新机器人消息')
  expect(screen.queryByText('多只猫咪一起聊')).toBeNull()
  expect(screen.queryByRole('heading', { name: '群聊' })).toBeNull()
  expect(
    within(screen.getByRole('button', { name: '项目讨论' }).closest('section')!).getByRole(
      'heading',
      { name: '未分组' },
    ),
  ).toBeInTheDocument()
  view.unmount()
  render(<App />)
  await waitFor(() =>
    expect(screen.getByRole('button', { name: '项目讨论' })).toHaveTextContent('最新群消息'),
  )
})
it('拖动机器人和群聊修改分组并高亮整个目标，保存后重开保持', async () => {
  setup()
  const view = render(<App />)
  const groupRow = await screen.findByRole('button', { name: '项目讨论' })
  const target = screen.getByRole('heading', { name: '游戏开发' }).closest('section')!
  const transfer = { setData: vi.fn(), setDragImage: vi.fn(), effectAllowed: '', dropEffect: '' }
  fireEvent.dragStart(groupRow, { dataTransfer: transfer })
  fireEvent.dragOver(target, { dataTransfer: transfer })
  expect(target).toHaveClass('is-drop-target')
  fireEvent.drop(target, { dataTransfer: transfer })
  expect(target).not.toHaveClass('is-drop-target')
  expect(within(target).getByRole('button', { name: '项目讨论' })).toBeInTheDocument()
  expect(JSON.parse(localStorage.getItem(SIDEBAR_STORAGE_KEY)!).chatCategories.group).toBe(
    '游戏开发',
  )
  const ungrouped = screen.getByRole('heading', { name: '未分组' }).closest('section')!
  fireEvent.dragStart(screen.getByRole('button', { name: '游戏程序' }), { dataTransfer: transfer })
  fireEvent.dragOver(ungrouped, { dataTransfer: transfer })
  fireEvent.drop(ungrouped, { dataTransfer: transfer })
  expect(within(ungrouped).getByRole('button', { name: '游戏程序' })).toBeInTheDocument()
  expect(screen.getByRole('button', { name: '小黑' })).toHaveAttribute('draggable', 'false')
  view.unmount()
  render(<App />)
  await waitFor(() =>
    expect(
      within(screen.getByRole('heading', { name: '游戏开发' }).closest('section')!).getByRole(
        'button',
        { name: '项目讨论' },
      ),
    ).toBeInTheDocument(),
  )
})
it('外部文件拖拽不能修改分组', async () => {
  setup()
  render(<App />)
  await screen.findByRole('button', { name: '项目讨论' })
  fireEvent.drop(screen.getByRole('heading', { name: '游戏开发' }).closest('section')!, {
    dataTransfer: { getData: () => '{"id":"group","kind":"group"}' },
  })
  expect(localStorage.getItem(SIDEBAR_STORAGE_KEY)).toBeNull()
})
