import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import App from './App'
import type { DeliveryResult } from './features/desktop/backend'
import { memoryBackend } from './features/desktop/memoryBackend'
import { createWorkspace, type ChatMessage } from './features/workspace/model'

const content = '消息预览回归'

/** 使用非置顶猫咪，让测试同时覆盖实际侧栏条目与搜索结果。 */
function loadPreview(message?: Partial<ChatMessage>, draft = ''): void {
  const workspace = createWorkspace()
  const agent = workspace.agents.find((item) => item.id === 'developer')!
  workspace.activeAgentId = agent.id
  agent.draft = draft
  agent.description = '猫咪介绍'
  agent.messages = message
    ? [
        {
          id: 'preview-message',
          role: 'assistant',
          content,
          createdAt: '2026-11-16T01:00:00Z',
          status: 'sent',
          reactions: [],
          ...message,
        },
      ]
    : []
  memoryBackend.loadFixture({ workspace, isModelPolicyAvailable: true })
}

function sidebarPreview(): Element | null {
  return screen.getByRole('button', { name: '游戏程序' }).querySelector('.agent-preview')
}

describe('侧栏与搜索消息预览', () => {
  it.each<{ role: ChatMessage['role']; status: ChatMessage['status']; prefix: string }>([
    { role: 'assistant', status: 'sent', prefix: '' },
    { role: 'user', status: 'sent', prefix: '' },
    { role: 'user', status: 'unsent', prefix: '未发送：' },
    { role: 'user', status: 'failed', prefix: '发送失败：' },
  ])('$role 消息在 $status 状态使用正确摘要，重开后保持一致', async ({ role, status, prefix }) => {
    loadPreview({ role, status })
    const user = userEvent.setup()
    const view = render(<App />)
    expect(sidebarPreview()?.textContent).toBe(prefix + content)
    view.unmount()
    render(<App />)
    expect(sidebarPreview()?.textContent).toBe(prefix + content)
    await user.click(screen.getByRole('button', { name: '搜索助手和消息' }))
    const searchPreview = () =>
      screen.getByRole('option', { name: /游戏程序/ }).querySelector('small')?.textContent
    expect(searchPreview()).toBe(prefix + content)
    await user.type(screen.getByRole('combobox', { name: '搜索内容' }), content)
    expect(searchPreview()).toBe(prefix + content)
  })

  it('私聊通知保留正文，不增加本地发送状态标签', async () => {
    loadPreview({
      role: 'assistant',
      dmNotice: {
        direction: 'received',
        peerId: 'artist',
        peerName: '游戏美术',
        conversationId: 'conversation',
      },
    })
    const user = userEvent.setup()
    render(<App />)
    expect(sidebarPreview()?.textContent).toBe(content)
    await user.click(screen.getByRole('button', { name: '搜索助手和消息' }))
    expect(
      screen.getByRole('option', { name: /游戏程序/ }).querySelector('small')?.textContent,
    ).toBe(content)
  })

  it('没有消息时显示介绍，输入草稿优先于历史摘要且清空后恢复', async () => {
    loadPreview()
    const user = userEvent.setup()
    render(<App />)
    expect(sidebarPreview()?.textContent).toBe('猫咪介绍')
    const input = screen.getByRole('textbox', { name: '给游戏程序发消息' })
    await user.type(input, '尚在编辑')
    expect(sidebarPreview()?.textContent).toBe('草稿：尚在编辑')
    await user.clear(input)
    expect(sidebarPreview()?.textContent).toBe('猫咪介绍')
  })

  it('有草稿时侧栏显示草稿，搜索历史仍显示匹配消息的实际状态', async () => {
    loadPreview({ role: 'assistant', status: 'sent' }, '尚在编辑')
    const user = userEvent.setup()
    render(<App />)
    expect(sidebarPreview()?.textContent).toBe('草稿：尚在编辑')
    await user.click(screen.getByRole('button', { name: '搜索助手和消息' }))
    expect(
      screen.getByRole('option', { name: /游戏程序/ }).querySelector('small')?.textContent,
    ).toBe('草稿：尚在编辑')
    await user.type(screen.getByRole('combobox', { name: '搜索内容' }), content)
    expect(
      screen.getByRole('option', { name: /游戏程序/ }).querySelector('small')?.textContent,
    ).toBe(content)
  })

  it('服务受理后立即移除未发送前缀，后续 bot 回复与重命名预览不再误标', async () => {
    loadPreview()
    let finish!: (result: DeliveryResult) => void
    vi.spyOn(memoryBackend, 'deliverMessage').mockImplementation(
      () =>
        new Promise((resolve) => {
          finish = resolve
        }),
    )
    const user = userEvent.setup()
    render(<App />)
    await user.type(screen.getByRole('textbox', { name: '给游戏程序发消息' }), content)
    await user.click(screen.getByRole('button', { name: '发送' }))
    await waitFor(() => expect(finish).toBeDefined())
    expect(sidebarPreview()?.textContent).toBe('未发送：' + content)
    await act(async () =>
      finish({
        acceptedMessageId: 'accepted-message',
        messages: [
          {
            id: 'accepted-message',
            sequence: 1,
            role: 'user',
            speaker: '',
            content,
            createdAt: '2026-11-16T01:00:00Z',
            reactions: [],
            attachments: [],
          },
          {
            id: 'bot-reply',
            sequence: 2,
            role: 'assistant',
            speaker: 'developer',
            content: '收到，我会处理',
            createdAt: '2026-11-16T01:00:01Z',
            reactions: [],
            attachments: [],
          },
        ],
      }),
    )
    expect(sidebarPreview()?.textContent).toBe('收到，我会处理')
    fireEvent.contextMenu(screen.getByRole('button', { name: '游戏程序' }))
    await user.click(screen.getByRole('menuitem', { name: '重命名猫咪' }))
    const editing = screen.getByRole('textbox', { name: '猫咪名称' }).closest('.agent-button')!
    expect(editing.querySelector('.agent-preview')?.textContent).toBe('收到，我会处理')
    await user.keyboard('{Escape}')
    expect(sidebarPreview()?.textContent).toBe('收到，我会处理')
  })
})
