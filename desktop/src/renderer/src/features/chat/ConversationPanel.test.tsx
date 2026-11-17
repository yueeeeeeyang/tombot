import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { DesktopBackend, GroupDetail, ServerBubble } from '../desktop/backend'
import { MemoryBackend, memoryBackend } from '../desktop/memoryBackend'
import { createWorkspace } from '../workspace/model'
import { ConversationPanel } from './ConversationPanel'

const agents = createWorkspace().agents.slice(0, 2)
const group: GroupDetail = {
  id: 'group',
  name: '讨论组',
  revision: 1,
  avatar_presets: ['bombay', 'orange-tabby'],
  conversation_id: 'conversation',
  members: agents.map((cat) => ({ bot_id: cat.id, name: cat.name })),
}
type Send = NonNullable<DesktopBackend['sendConversation']>

afterEach(() => {
  vi.restoreAllMocks()
  window.desktop = { platform: 'linux', backend: memoryBackend }
})

type ListMessages = NonNullable<DesktopBackend['listConversationMessages']>

function openPanel(
  send: Send,
  listMessages: ListMessages = vi.fn(async () => ({ messages: [], hasMore: false })),
  activity?: DesktopBackend['conversationActivity'],
) {
  const onError = vi.fn()
  window.desktop = {
    platform: 'linux',
    backend: Object.assign(new MemoryBackend(), {
      groupDetail: vi.fn(async () => group),
      listConversationMessages: listMessages,
      sendConversation: send,
      conversationActivity: activity,
    }),
  }
  function Panel() {
    return (
      <ConversationPanel
        id={group.id}
        agents={agents}
        onError={onError}
        onNotice={vi.fn()}
        onGroupChange={vi.fn()}
      />
    )
  }
  render(<Panel />)
  return onError
}

describe('消息翻页', () => {
  it('按最早消息序号向前加载，加载完隐藏入口', async () => {
    const listMessages = vi
      .fn<
        (
          conversationId: string,
          before?: number,
        ) => Promise<{ messages: ServerBubble[]; hasMore: boolean }>
      >()
      .mockResolvedValueOnce({
        messages: [
          {
            id: 'latest',
            sequence: 40,
            role: 'assistant',
            speaker: agents[0]!.id,
            content: '较新的回复',
            createdAt: '2026-11-10T08:00:00.000Z',
            reactions: [],
            attachments: [],
          },
        ],
        hasMore: true,
      })
      .mockResolvedValueOnce({
        messages: [
          {
            id: 'older',
            sequence: 12,
            role: 'assistant',
            speaker: agents[1]!.id,
            content: '更早的回复',
            createdAt: '2026-11-09T08:00:00.000Z',
            reactions: [],
            attachments: [],
          },
        ],
        hasMore: false,
      })
    openPanel(
      vi.fn(async () => ({})),
      listMessages,
    )
    fireEvent.click(await screen.findByRole('button', { name: '加载更早的消息' }))
    await screen.findByText('更早的回复')
    // 第二页以第一页最早消息的序号为游标，服务端才能继续向前返回。
    expect(listMessages).toHaveBeenLastCalledWith('conversation', 40)
    expect(screen.queryByRole('button', { name: '加载更早的消息' })).toBeNull()
  })
})

describe('群聊持久消息同步', () => {
  it('重开时恢复输入状态，有成员正在输入时优先显示，完成后整条显示', async () => {
    let publish!: Parameters<NonNullable<DesktopBackend['watchEvents']>>[0]
    vi.spyOn(MemoryBackend.prototype, 'watchEvents').mockImplementation((listener) => {
      publish = listener
      return () => undefined
    })
    const activity = vi
      .fn<NonNullable<DesktopBackend['conversationActivity']>>()
      .mockResolvedValue({
        runs: [
          { id: 'queued', bot_id: agents[0]!.id, status: 'queued', progress_summary: '' },
          { id: 'typing', bot_id: agents[1]!.id, status: 'running', progress_summary: '正在输入…' },
        ],
      })
    const list = vi.fn<ListMessages>().mockResolvedValue({ messages: [], hasMore: false })
    openPanel(
      vi.fn(async () => ({})),
      list,
      activity,
    )
    expect(await screen.findByText('正在输入…')).toBeInTheDocument()
    expect(screen.getByRole('log')).not.toHaveTextContent('核对完成')
    activity.mockResolvedValue({ runs: [] })
    list.mockResolvedValue({
      messages: [
        {
          id: 'reply',
          sequence: 1,
          role: 'assistant',
          speaker: agents[1]!.id,
          content: '核对完成，可以使用。',
          createdAt: new Date().toISOString(),
          reactions: [],
          attachments: [],
        },
      ],
      hasMore: false,
    })
    await act(async () =>
      publish({
        sequence: 1,
        kind: 'group.run.finished',
        resource_id: 'typing',
        payload: { conversation_id: 'conversation' },
        created_at: new Date().toISOString(),
      }),
    )
    expect(await screen.findByText('核对完成，可以使用。')).toBeInTheDocument()
    expect(screen.queryByText('正在输入…')).toBeNull()
  })
  it('交错到达的成员消息分别显示，受理后可继续发言并保留引用与用户提及', async () => {
    const send = vi.fn<Send>().mockResolvedValue({ acceptedMessageId: 'accepted' })
    const list = vi.fn<ListMessages>().mockResolvedValue({ messages: [], hasMore: false })
    let publish!: Parameters<NonNullable<DesktopBackend['watchEvents']>>[0]
    vi.spyOn(MemoryBackend.prototype, 'watchEvents').mockImplementation((listener) => {
      publish = listener
      return () => undefined
    })
    const error = openPanel(send, list)

    fireEvent.change(await screen.findByRole('textbox', { name: '给讨论组发消息' }), {
      target: { value: '一起看看' },
    })
    fireEvent.click(screen.getByRole('button', { name: '发送' }))
    await waitFor(() => expect(screen.getByRole('textbox')).toHaveValue(''))
    expect(send.mock.calls[0]?.[5]).toMatch(/^[0-9A-Z]{26}$/)
    const messages: ServerBubble[] = agents.map((agent, index) => ({
      id: `reply-${index}`,
      sequence: index + 1,
      role: 'assistant',
      speaker: agent.id,
      content: `第${index + 1}位回复`,
      createdAt: new Date().toISOString(),
      reactions: [],
      attachments: [],
      mentions: index === 1 ? [{ id: 'user', name: '用户' }] : [],
    }))
    list.mockResolvedValue({ messages, hasMore: false })
    await act(async () =>
      publish({
        sequence: 1,
        kind: 'message.created',
        resource_id: 'reply-1',
        payload: { conversation_id: 'conversation' },
        created_at: new Date().toISOString(),
      }),
    )
    const first = screen.getByText('第1位回复').closest('.message-bubble') as HTMLElement
    const second = screen.getByText('第2位回复').closest('.message-bubble') as HTMLElement
    expect(within(first).getByText(agents[0]!.name)).toBeInTheDocument()
    expect(within(second).getByText(agents[1]!.name)).toBeInTheDocument()
    expect(screen.getByText('@你')).toBeInTheDocument()
    fireEvent.click(
      within(second.closest('.message-line') as HTMLElement).getByRole('button', { name: '回复' }),
    )
    fireEvent.change(screen.getByRole('textbox'), { target: { value: '解释第二点' } })
    fireEvent.click(screen.getByRole('button', { name: '发送' }))
    await waitFor(() => expect(send).toHaveBeenCalledTimes(2))
    expect(send.mock.calls[1]?.[3]).toBe('reply-1')
    await waitFor(() => expect(screen.getByRole('textbox')).toHaveValue(''))
    expect(screen.getAllByText('第1位回复')).toHaveLength(1)
    expect(error).not.toHaveBeenCalled()
  })
  it('发送失败保留草稿，同一消息重试沿用幂等键', async () => {
    const send = vi
      .fn<Send>()
      .mockRejectedValueOnce(new Error('连接中断'))
      .mockResolvedValue({ acceptedMessageId: 'accepted' })
    const error = openPanel(send)
    fireEvent.change(await screen.findByRole('textbox', { name: '给讨论组发消息' }), {
      target: { value: '保留这条消息' },
    })
    fireEvent.click(screen.getByRole('button', { name: '发送' }))
    await waitFor(() => expect(error).toHaveBeenCalledWith('连接中断'))
    expect(screen.getByRole('textbox')).toHaveValue('保留这条消息')
    fireEvent.click(screen.getByRole('button', { name: '发送' }))
    await waitFor(() => expect(send).toHaveBeenCalledTimes(2))
    expect(send.mock.calls[1]?.[5]).toBe(send.mock.calls[0]?.[5])
    await waitFor(() => expect(screen.getByRole('textbox')).toHaveValue(''))
  })
})

it('新建空群聊保留输入框并移除顶部提示及分隔线', async () => {
  openPanel(vi.fn(async () => ({})))
  await screen.findByRole('log', { name: '讨论组' })
  expect(screen.queryByText('在下面说一句，群里的猫咪会一起回应。')).not.toBeInTheDocument()
  expect(document.querySelector('.saved-messages .local-note')).toBeNull()
  expect(screen.getByRole('textbox')).toBeInTheDocument()
})
