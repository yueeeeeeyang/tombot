import { act, renderHook, waitFor } from '@testing-library/react'
import { afterEach, expect, it, vi } from 'vitest'
import type { AgentConversation, DesktopBackend, ServerBubble } from '../desktop/backend'
import { memoryBackend } from '../desktop/memoryBackend'
import { useWorkActivity } from '../chat/useWorkActivity'
import { createWorkspace, saveMessage, updateDraft } from './model'
import { useWorkspace } from './useWorkspace'

const desktop = window.desktop

afterEach(() => {
  window.desktop = desktop
  vi.restoreAllMocks()
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
