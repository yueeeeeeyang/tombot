import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { DesktopBackend, GroupDetail, ServerBubble } from '../desktop/backend'
import { MemoryBackend, memoryBackend } from '../desktop/memoryBackend'
import { createWorkspace } from '../workspace/model'
import { ConversationPanel } from './ConversationPanel'
import { useWorkActivity } from './useWorkActivity'

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
  window.desktop = { platform: 'linux', backend: memoryBackend }
})

function openPanel(send: Send, listMessages = vi.fn(async (): Promise<ServerBubble[]> => [])) {
  const onError = vi.fn()
  window.desktop = {
    platform: 'linux',
    backend: Object.assign(new MemoryBackend(), {
      groupDetail: vi.fn(async () => group),
      listConversationMessages: listMessages,
      sendConversation: send,
    }),
  }
  function Panel() {
    const { workStates, receiveActivity, clearActivity } = useWorkActivity()
    return (
      <ConversationPanel
        workStates={workStates}
        onExecutionEvent={receiveActivity}
        onExecutionEnd={clearActivity}
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

describe('群聊流式回复', () => {
  it('交错文字按消息与成员分别累加，完成后替换为持久化内容', async () => {
    let finish!: () => void
    let publish!: NonNullable<Parameters<Send>[5]>
    const send: Send = vi.fn(async (_id, _content, _files, _reply, _mentions, onEvent) => {
      publish = onEvent!
      await new Promise<void>((resolve) => {
        finish = resolve
      })
    })
    const listMessages = vi.fn(async (): Promise<ServerBubble[]> => [])
    const error = openPanel(send, listMessages)
    fireEvent.change(await screen.findByRole('textbox', { name: '给讨论组发消息' }), {
      target: { value: '一起看看' },
    })
    fireEvent.click(screen.getByRole('button', { name: '发送' }))
    await waitFor(() => expect(publish).toBeDefined())
    expect(screen.queryByText('正在思考…')).toBeNull()
    act(() =>
      publish({
        type: 'activity',
        text: '',
        botId: agents[0]!.id,
        runId: 'first-run',
        isWorking: true,
      }),
    )
    expect(
      screen.getByText('正在思考…').closest('.work-status')!.querySelectorAll('.avatar'),
    ).toHaveLength(1)
    act(() => {
      publish({ type: 'delta', text: '第一', messageId: 'first', botId: agents[0]!.id })
      publish({ type: 'delta', text: '第二位回复', messageId: 'second', botId: agents[1]!.id })
      publish({ type: 'delta', text: '位回复', messageId: 'first', botId: agents[0]!.id })
    })
    const first = screen.getByText('第一位回复').closest('.message-bubble')! as HTMLElement
    const second = screen.getByText('第二位回复').closest('.message-bubble')! as HTMLElement
    expect(within(first).getByText(agents[0]!.name)).toBeInTheDocument()
    expect(within(second).getByText(agents[1]!.name)).toBeInTheDocument()
    expect(first).not.toHaveTextContent('第二位回复')
    expect(screen.getByRole('button', { name: '发送' })).toBeDisabled()
    listMessages.mockResolvedValue([
      {
        id: 'first',
        role: 'assistant',
        speaker: agents[0]!.id,
        content: '第一位回复',
        createdAt: new Date().toISOString(),
        reactions: [],
        attachments: [],
      },
      {
        id: 'second',
        role: 'assistant',
        speaker: agents[1]!.id,
        content: '第二位回复',
        createdAt: new Date().toISOString(),
        reactions: [],
        attachments: [],
      },
    ])
    await act(async () => finish())
    expect(screen.getAllByText('第一位回复')).toHaveLength(1)
    expect(screen.getAllByText('第二位回复')).toHaveLength(1)
    expect(screen.getByRole('textbox')).toHaveValue('')
    expect(error).not.toHaveBeenCalled()
    expect(screen.queryByText('正在思考…')).toBeNull()
  })

  it('缺少发言标识时提示错误，保留草稿并停止等待动画', async () => {
    const send: Send = vi.fn(async (_id, _content, _files, _reply, _mentions, onEvent) => {
      onEvent?.({ type: 'delta', text: '无法归属的回复' })
    })
    const error = openPanel(send)
    fireEvent.change(await screen.findByRole('textbox', { name: '给讨论组发消息' }), {
      target: { value: '保留这条消息' },
    })
    fireEvent.click(screen.getByRole('button', { name: '发送' }))
    await waitFor(() => expect(error).toHaveBeenCalledWith('群消息缺少发言成员信息。'))
    expect(screen.getByRole('textbox')).toHaveValue('保留这条消息')
    expect(screen.queryByText('无法归属的回复')).toBeNull()
    expect(screen.queryByText('正在思考…')).toBeNull()
    expect(screen.getByRole('button', { name: '发送' })).toBeEnabled()
  })
})
