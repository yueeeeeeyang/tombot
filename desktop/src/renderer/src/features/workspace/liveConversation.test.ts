import { act, renderHook, waitFor } from '@testing-library/react'
import { afterEach, expect, it, vi } from 'vitest'
import type {
  AgentConversation,
  DeliveryResult,
  DesktopBackend,
  ServerBubble,
} from '../desktop/backend'
import { memoryBackend } from '../desktop/memoryBackend'
import { useWorkActivity } from '../chat/useWorkActivity'
import { createWorkspace, saveMessage, updateDraft } from './model'
import { useWorkspace } from './useWorkspace'

const desktop = window.desktop

afterEach(() => {
  window.desktop = desktop
  vi.restoreAllMocks()
})

it('工作进度只更新活动，晚到的历史快照不能覆盖新的运行状态', async () => {
  const { result, backend } = setup({
    messages: [bubble('history', '历史', 1)],
    hasMore: false,
    runs: [],
  })
  await waitFor(() => expect(result.current.workspace.agents[0]?.messages).toHaveLength(1))
  const messages = result.current.workspace.agents[0]!.messages
  let finish!: (page: AgentConversation) => void
  vi.mocked(backend.loadAgentConversation!).mockImplementationOnce(
    () =>
      new Promise((resolve) => {
        finish = resolve
      }),
  )
  act(() =>
    memoryBackend.publishEvent({
      sequence: 1,
      kind: 'message.created',
      resource_id: 'history',
      payload: {},
      created_at: '',
    }),
  )
  await waitFor(() => expect(finish).toBeDefined())
  const requests = vi.mocked(backend.loadAgentConversation!).mock.calls.length
  act(() => {
    for (const kind of ['run.started', 'run.progress'])
      memoryBackend.publishEvent({
        sequence: kind === 'run.started' ? 2 : 3,
        kind,
        resource_id: 'run',
        payload: { bot_id: 'assistant', text: '正在核对' },
        created_at: '',
      })
  })
  expect(result.current.workStates.assistant?.summary).toBe('正在核对')
  expect(backend.loadAgentConversation).toHaveBeenCalledTimes(requests)
  await act(async () =>
    finish({ messages: [bubble('history', '历史', 1)], hasMore: false, runs: [] }),
  )
  expect(result.current.workStates.assistant?.summary).toBe('正在核对')
  expect(result.current.workspace.agents[0]!.messages).toBe(messages)
})

it('消息推送先于回执且回执丢失时，只保留已确认的气泡，重试也不重复', async () => {
  const page: AgentConversation = { messages: [], hasMore: false, runs: [] }
  const { result, backend } = setup(page)
  await waitFor(() => expect(backend.loadAgentConversation).toHaveBeenCalled())
  let fail!: (error: Error) => void
  backend.deliverMessage = vi.fn(
    () =>
      new Promise<DeliveryResult>((_resolve, reject) => {
        fail = reject
      }),
  )
  let delivery!: Promise<unknown>
  act(() => {
    result.current.commit((w) =>
      saveMessage(
        updateDraft(w, 'assistant', '消息'),
        'assistant',
        'request',
        '2026-11-16T01:00:00Z',
      ),
    )
    delivery = result.current.deliver('assistant', '消息', 'request')
  })
  await waitFor(() => expect(fail).toBeDefined())
  page.messages = [{ ...bubble('server', '消息', 1), requestId: 'request' }]
  act(() =>
    memoryBackend.publishEvent({
      sequence: 1,
      kind: 'message.created',
      resource_id: 'server',
      payload: {},
      created_at: '',
    }),
  )
  await waitFor(() => expect(result.current.workspace.agents[0]?.messages[0]?.status).toBe('sent'))
  expect(result.current.workspace.agents[0]!.messages).toHaveLength(1)
  await act(async () => {
    fail(new Error('发送回执丢失'))
    await delivery
  })
  expect(result.current.workspace.agents[0]?.messages).toHaveLength(1)
  backend.deliverMessage = vi.fn(async () => ({ acceptedMessageId: 'server' }))
  await act(async () => {
    await result.current.deliver('assistant', '消息', 'request')
  })
  expect(result.current.workspace.agents[0]?.messages).toHaveLength(1)
  expect(result.current.workspace.agents[0]?.messages[0]).toMatchObject({
    id: 'server',
    requestId: 'request',
    status: 'sent',
  })
})

function bubble(id: string, content: string, sequence: number): ServerBubble {
  return {
    id,
    content,
    sequence,
    role: 'user',
    speaker: '',
    createdAt: `2026-11-16T01:00:${String(sequence).padStart(2, '0')}Z`,
    reactions: [],
    attachments: [],
  }
}
function setup(page: AgentConversation) {
  memoryBackend.loadFixture({ workspace: createWorkspace(), isModelPolicyAvailable: true })
  const backend = Object.create(memoryBackend) as DesktopBackend
  backend.loadAgentConversation = vi.fn(async () => page)
  window.desktop = { ...desktop!, backend }
  const hook = renderHook(() => {
    const activity = useWorkActivity()
    return { ...useWorkspace(activity.receiveActivity, activity.clearActivity), ...activity }
  })
  return { ...hook, backend }
}

it('表情确认与旧历史乱序时保留最新版本，快速操作串行且独立于草稿保存', async () => {
  const original = bubble('original', '原消息', 1)
  const page: AgentConversation = { messages: [original], hasMore: false, runs: [] }
  const { result, backend } = setup(page)
  await waitFor(() => expect(result.current.workspace.agents[0]?.messages).toHaveLength(1))
  const save = vi.spyOn(backend, 'saveWorkspace')
  let revision = 0
  backend.setReaction = vi.fn(async (_id, _emoji, isActive) => ({
    ...original,
    reactions: isActive ? [{ emoji: '👍', count: 1 }] : [],
    reactionRevision: ++revision,
  }))
  await act(async () => {
    await result.current.react('assistant', 'original', '👍')
  })
  act(() =>
    memoryBackend.publishEvent({
      sequence: 1,
      kind: 'message.reactions_changed',
      resource_id: 'original',
      payload: { bot_id: 'assistant' },
      created_at: '',
    }),
  )
  await waitFor(() => expect(backend.loadAgentConversation).toHaveBeenCalledTimes(2))
  expect(result.current.workspace.agents[0]?.messages[0]?.reactions).toEqual([
    { emoji: '👍', count: 1 },
  ])
  expect(save).not.toHaveBeenCalled()
  expect(result.current.hasUnsavedChanges).toBe(false)
  await act(async () => {
    await Promise.all([
      result.current.react('assistant', 'original', '👍'),
      result.current.react('assistant', 'original', '👍'),
    ])
  })
  expect(vi.mocked(backend.setReaction).mock.calls.map((call) => call[2])).toEqual([
    true,
    false,
    true,
  ])
  expect(result.current.workspace.agents[0]?.messages[0]?.reactionRevision).toBe(3)
})

it('表情推送成功但回执丢失后，点击取消先核实旧请求再提交新意图', async () => {
  const original = bubble('original', '原消息', 1)
  const { result, backend } = setup({ messages: [original], hasMore: false, runs: [] })
  await waitFor(() => expect(result.current.workspace.agents[0]?.messages).toHaveLength(1))
  const confirmed = { ...original, reactionRevision: 1, reactions: [{ emoji: '👍', count: 1 }] }
  backend.setReaction = vi
    .fn()
    .mockRejectedValueOnce(new Error('回执丢失'))
    .mockResolvedValueOnce(confirmed)
    .mockResolvedValueOnce({ ...original, reactionRevision: 2 })
  await act(async () => {
    await expect(result.current.react('assistant', 'original', '👍')).rejects.toThrow('回执丢失')
    result.current.receive([{ id: 'assistant', messages: [{ ...confirmed, status: 'sent' }] }])
  })
  await act(async () => result.current.react('assistant', 'original', '👍'))
  const calls = vi.mocked(backend.setReaction).mock.calls
  expect(calls).toHaveLength(3)
  expect(calls[1]).toEqual(calls[0])
  expect(calls[2]?.[2]).toBe(false)
  expect(calls[2]?.[3]).not.toBe(calls[0]?.[3])
  expect(result.current.workspace.agents[0]?.messages[0]?.reactions).toEqual([])
})

it('未发送消息不能标记表情，网络失败保留幂等请求供显式重试', async () => {
  const original = bubble('original', '原消息', 1)
  const { result, backend } = setup({ messages: [original], hasMore: false, runs: [] })
  await waitFor(() => expect(result.current.workspace.agents[0]?.messages).toHaveLength(1))
  backend.setReaction = vi
    .fn()
    .mockRejectedValueOnce(new Error('回执丢失'))
    .mockResolvedValueOnce({
      ...original,
      reactionRevision: 1,
      reactions: [{ emoji: '👍', count: 1 }],
    })
  await act(async () => {
    await expect(result.current.react('assistant', 'original', '👍')).rejects.toThrow('回执丢失')
  })
  expect(result.current.hasUnsavedChanges).toBe(false)
  await act(async () => {
    await result.current.react('assistant', 'original', '👍')
  })
  expect(vi.mocked(backend.setReaction).mock.calls[0]).toEqual(
    vi.mocked(backend.setReaction).mock.calls[1],
  )
  act(() =>
    result.current.commit((w) =>
      saveMessage(
        updateDraft(w, 'assistant', '草稿'),
        'assistant',
        'local',
        new Date().toISOString(),
      ),
    ),
  )
  await act(async () => {
    await expect(result.current.react('assistant', 'local', '👍')).rejects.toThrow(
      '请等待消息发送成功',
    )
  })
  expect(backend.setReaction).toHaveBeenCalledTimes(2)
})

it('重开与历史分页无需加载原消息也能恢复回复摘要', async () => {
  const replyTo = { messageId: 'outside-page', excerpt: '来自更早消息的摘要' }
  const page: AgentConversation = {
    messages: [{ ...bubble('reply', '接着讨论', 9), replyTo }],
    hasMore: true,
    runs: [],
  }
  const { result, backend, unmount } = setup(page)
  await waitFor(() =>
    expect(result.current.workspace.agents[0]?.messages[0]?.replyTo).toEqual(replyTo),
  )
  vi.mocked(backend.loadAgentConversation!).mockResolvedValueOnce({
    messages: [{ ...bubble('older-reply', '更早的回复', 8), replyTo }],
    hasMore: false,
    runs: [],
  })
  await act(async () => result.current.loadEarlier('assistant'))
  expect(result.current.workspace.agents[0]?.messages.map((message) => message.replyTo)).toEqual([
    replyTo,
    replyTo,
  ])
  unmount()
  const reopened = setup(page)
  await waitFor(() =>
    expect(reopened.result.current.workspace.agents[0]?.messages[0]?.replyTo).toEqual(replyTo),
  )
})

it('回复发送失败后重试仍使用原消息标识与同一个幂等键', async () => {
  const page: AgentConversation = {
    messages: [bubble('original', '原消息', 1)],
    hasMore: false,
    runs: [],
  }
  const { result, backend } = setup(page)
  await waitFor(() => expect(result.current.workspace.agents[0]?.messages).toHaveLength(1))
  backend.deliverMessage = vi
    .fn()
    .mockRejectedValueOnce(new Error('连接中断'))
    .mockResolvedValueOnce({ acceptedMessageId: 'reply' })
  act(() =>
    result.current.commit((w) =>
      saveMessage(
        updateDraft(w, 'assistant', '这是回复'),
        'assistant',
        'request',
        new Date().toISOString(),
        'original',
      ),
    ),
  )
  await act(async () => {
    await result.current.deliver('assistant', '这是回复', 'request')
  })
  expect(result.current.workspace.agents[0]?.messages[1]?.replyTo?.messageId).toBe('original')
  await act(async () => {
    await result.current.deliver('assistant', '这是回复', 'request')
  })
  expect(backend.deliverMessage).toHaveBeenCalledTimes(2)
  for (const call of vi.mocked(backend.deliverMessage).mock.calls) {
    expect(call).toEqual(['assistant', '这是回复', 'request', expect.any(Function), 'original'])
  }
  expect(result.current.workspace.agents[0]?.messages[1]).toMatchObject({
    id: 'reply',
    status: 'sent',
    replyTo: { messageId: 'original', excerpt: '原消息' },
  })
})

it('原消息尚未受理时明确拒绝引用发送，确认后引用自动更新', async () => {
  const page: AgentConversation = { messages: [], hasMore: false, runs: [] }
  const { result, backend } = setup(page)
  await waitFor(() => expect(backend.loadAgentConversation).toHaveBeenCalled())
  backend.deliverMessage = vi.fn(async () => ({ acceptedMessageId: 'reply' }))
  act(() =>
    result.current.commit((w) => {
      const original = saveMessage(
        updateDraft(w, 'assistant', '原消息'),
        'assistant',
        'local',
        new Date().toISOString(),
      )
      return saveMessage(
        updateDraft(original, 'assistant', '回复'),
        'assistant',
        'request',
        new Date().toISOString(),
        'local',
      )
    }),
  )
  await act(async () => {
    expect(await result.current.deliver('assistant', '回复', 'request')).toMatchObject({
      code: 'DELIVER_FAILED',
      message: '请等待原消息发送成功后再回复。',
    })
  })
  expect(backend.deliverMessage).not.toHaveBeenCalled()
  act(() =>
    result.current.receive([
      {
        id: 'assistant',
        messages: [
          {
            id: 'confirmed',
            requestId: 'local',
            sequence: 1,
            role: 'user',
            content: '原消息',
            createdAt: new Date().toISOString(),
            status: 'sent',
            reactions: [],
          },
        ],
      },
    ]),
  )
  await act(async () => {
    await result.current.deliver('assistant', '回复', 'request')
  })
  expect(backend.deliverMessage).toHaveBeenCalledWith(
    'assistant',
    '回复',
    'request',
    expect.any(Function),
    'confirmed',
  )
})

it('重开后不发送消息也加载历史和实际工作摘要，事件更新后恢复空闲', async () => {
  const page: AgentConversation = {
    messages: [bubble('first', '已保存的历史', 1)],
    hasMore: false,
    runs: [
      {
        id: 'run',
        bot_id: 'assistant',
        status: 'running',
        progress_summary: '正在整理用户提供的材料',
      },
    ],
  }
  const { result, backend } = setup(page)
  await waitFor(() =>
    expect(result.current.workspace.agents[0]?.messages[0]?.content).toBe('已保存的历史'),
  )
  expect(result.current.workStates.assistant?.summary).toBe('正在整理用户提供的材料')
  act(() => {
    result.current.commit((w) => updateDraft(w, 'assistant', '尚未发送的补充'))
  })
  page.messages.push({ ...bubble('answer', '已整理完成', 2), role: 'assistant' })
  page.runs = []
  act(() =>
    memoryBackend.publishEvent({
      sequence: 2,
      kind: 'run.finished',
      resource_id: 'run',
      payload: { bot_id: 'assistant', status: 'succeeded' },
      created_at: '',
    }),
  )
  await waitFor(() => expect(result.current.workspace.agents[0]?.messages).toHaveLength(2))
  expect(result.current.workStates.assistant).toBeUndefined()
  expect(result.current.workspace.agents[0]?.draft).toBe('尚未发送的补充')
  expect(backend.loadAgentConversation).toHaveBeenCalledWith('assistant')
})

it('工作中连续投递按稳定幂等键受理，合并时不覆盖第二条本地消息', async () => {
  const page: AgentConversation = {
    messages: [],
    hasMore: false,
    runs: [{ id: 'run', bot_id: 'assistant', status: 'running', progress_summary: '正在核对资料' }],
  }
  const { result, backend } = setup(page)
  await waitFor(() => expect(result.current.workStates.assistant).toBeDefined())
  const requests: string[] = []
  backend.deliverMessage = vi.fn(async (_id, content, requestId) => {
    requests.push(requestId)
    const saved = bubble(`server-${requestId}`, content, requests.length)
    page.messages.push(saved)
    return { acceptedMessageId: saved.id, messages: [...page.messages] }
  })
  const sent: Promise<unknown>[] = []
  act(() => {
    for (const id of ['first', 'second']) {
      result.current.commit((w) =>
        saveMessage(updateDraft(w, 'assistant', id), 'assistant', id, new Date().toISOString()),
      )
      sent.push(result.current.deliver('assistant', id, id))
    }
  })
  await act(async () => {
    await Promise.all(sent)
  })
  expect(requests).toEqual(['first', 'second'])
  expect(result.current.workspace.agents[0]?.messages.map((m) => m.id)).toEqual([
    'server-first',
    'server-second',
  ])
  expect(result.current.workStates.assistant?.summary).toBe('正在核对资料')
})

it('历史读取失败会提示，之后的事件可以恢复并去重', async () => {
  const page: AgentConversation = { messages: [], hasMore: false, runs: [] }
  const { result, backend } = setup(page)
  await waitFor(() => expect(backend.loadAgentConversation).toHaveBeenCalled())
  vi.mocked(backend.loadAgentConversation!).mockRejectedValueOnce(new Error('历史读取失败'))
  act(() =>
    memoryBackend.publishEvent({
      sequence: 1,
      kind: 'message.created',
      resource_id: 'm',
      payload: {},
      created_at: '',
    }),
  )
  await waitFor(() => expect(result.current.error).toBe('历史读取失败'))
  expect(result.current.hasUnsavedChanges).toBe(false)
  act(() => result.current.dismissError())
  expect(result.current.error).toBe('')
  page.messages = [bubble('saved', '新历史', 1)]
  act(() => {
    for (const sequence of [2, 3])
      memoryBackend.publishEvent({
        sequence,
        kind: 'message.created',
        resource_id: 'saved',
        payload: {},
        created_at: '',
      })
  })
  await waitFor(() => expect(result.current.workspace.agents[0]?.messages).toHaveLength(1))
})

it('保存失败不能通过关闭提示掩盖，草稿仍保留并要求重试', async () => {
  const { result, backend } = setup({ messages: [], hasMore: false, runs: [] })
  await waitFor(() => expect(backend.loadAgentConversation).toHaveBeenCalled())
  backend.saveWorkspace = vi.fn(() => {
    throw new Error('草稿保存失败')
  })
  act(() => {
    result.current.commit((workspace) => updateDraft(workspace, 'assistant', '不能丢失的草稿'))
  })
  expect(result.current.hasUnsavedChanges).toBe(true)
  act(() => result.current.dismissError())
  expect(result.current.error).toBe('草稿保存失败')
  expect(result.current.workspace.agents[0]?.draft).toBe('不能丢失的草稿')
})

it('历史分页按服务端序号合并，不因时间相同而乱序或重复', async () => {
  const page: AgentConversation = {
    messages: [bubble('latest', '第二条', 2)],
    hasMore: true,
    runs: [],
  }
  const { result, backend } = setup(page)
  await waitFor(() => expect(result.current.historyPages.assistant?.before).toBe(2))
  vi.mocked(backend.loadAgentConversation!).mockResolvedValueOnce({
    messages: [bubble('older', '第一条', 1)],
    hasMore: false,
    runs: [],
  })
  await act(async () => result.current.loadEarlier('assistant'))
  expect(backend.loadAgentConversation).toHaveBeenLastCalledWith('assistant', 2)
  expect(result.current.workspace.agents[0]?.messages.map((message) => message.content)).toEqual([
    '第一条',
    '第二条',
  ])
  expect(result.current.historyPages.assistant?.hasMore).toBe(false)
})
