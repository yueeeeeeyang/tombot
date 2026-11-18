import { act, renderHook } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { DesktopBackend, WorkspaceSnapshot } from '../desktop/backend'
import { memoryBackend } from '../desktop/memoryBackend'
import { useConnectionStatus } from '../session/useConnectionStatus'
import { createWorkspace, type ChatMessage } from './model'
import { mergeIncoming, useWorkspace } from './useWorkspace'

it('实时通知同时更新双方，重复事件不增加消息，并保留当前猫咪和草稿', () => {
  const workspace = createWorkspace()
  workspace.agents[0]!.draft = '尚未发送的草稿'
  const [first, second] = workspace.agents
  const sent: ChatMessage = {
    id: 'sent',
    role: 'assistant',
    content: '已发消息给对方',
    createdAt: '2026-11-10T01:00:00Z',
    status: 'sent',
    reactions: [],
    dmNotice: {
      direction: 'sent',
      peerId: second!.id,
      peerName: second!.name,
      conversationId: 'dm',
    },
  }
  const received: ChatMessage = {
    ...sent,
    id: 'received',
    content: '收到消息',
    dmNotice: {
      direction: 'received',
      peerId: first!.id,
      peerName: first!.name,
      conversationId: 'dm',
    },
  }
  const incoming = [
    { id: first!.id, messages: [sent] },
    { id: second!.id, messages: [received] },
    { id: 'missing', messages: [sent] },
  ]
  const merged = mergeIncoming(mergeIncoming(workspace, incoming), incoming)
  expect(merged.agents[0]!.messages).toEqual([sent])
  expect(merged.agents[1]!.messages).toEqual([received])
  expect(merged.agents[0]!.draft).toBe('尚未发送的草稿')
  expect(merged.activeAgentId).toBe(workspace.activeAgentId)
  expect(merged.agents).toHaveLength(workspace.agents.length)
  const result = { ...received, id: 'result', content: '接收猫咪自己的结果', dmNotice: undefined }
  expect(
    mergeIncoming(merged, [{ id: second!.id, messages: [result] }]).agents[1]!.messages,
  ).toEqual([received, result])
})

describe('工作空间首次加载失败', () => {
  const previous = window.desktop

  afterEach(() => {
    window.desktop = previous
  })

  function openWorkspace(loadWorkspace: DesktopBackend['loadWorkspace']) {
    const saveWorkspace = vi.fn()
    const session = vi
      .fn<DesktopBackend['session']>()
      .mockResolvedValueOnce({ phase: 'offline' })
      .mockResolvedValue({ phase: 'ready' })
    const backend = Object.create(memoryBackend) as DesktopBackend
    backend.tryWorkspaceSync = undefined
    backend.loadWorkspace = loadWorkspace
    backend.saveWorkspace = saveWorkspace
    backend.session = session
    window.desktop = { platform: 'linux', backend }
    return {
      saveWorkspace,
      ...renderHook(() => {
        const workspace = useWorkspace()
        useConnectionStatus(workspace.retryLoadingWorkspace)
        return workspace
      }),
    }
  }

  it('会话恢复后串行补读工作空间，加载成功才解除阻塞并恢复草稿', async () => {
    const workspace = createWorkspace()
    workspace.agents[0]!.draft = '之前保存的草稿'
    let complete!: (snapshot: WorkspaceSnapshot) => void
    const loadWorkspace = vi
      .fn<DesktopBackend['loadWorkspace']>()
      .mockResolvedValueOnce({ errorCode: 'CONNECTION_TIMEOUT', message: '连接超时' })
      .mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            complete = resolve
          }),
      )
    const { result, saveWorkspace } = openWorkspace(loadWorkspace)
    await act(async () => {})
    expect(result.current.isBlocked).toBe(true)
    expect(result.current.error).toBe('连接超时')

    await act(async () => window.dispatchEvent(new Event('online')))
    await act(async () => window.dispatchEvent(new Event('focus')))
    expect(loadWorkspace).toHaveBeenCalledTimes(2)
    expect(result.current.isBlocked).toBe(true)

    await act(async () => complete({ workspace, pristine: false }))
    expect(result.current.isBlocked).toBe(false)
    expect(result.current.error).toBe('')
    expect(result.current.workspace.agents[0]?.draft).toBe('之前保存的草稿')
    await act(async () => window.dispatchEvent(new Event('focus')))
    expect(loadWorkspace).toHaveBeenCalledTimes(2)
    expect(saveWorkspace).not.toHaveBeenCalled()
  })

  it('补读返回需人工处理的错误时保留阻塞，后续在线探测不会再次补读', async () => {
    const loadWorkspace = vi
      .fn<DesktopBackend['loadWorkspace']>()
      .mockResolvedValueOnce({ errorCode: 'OFFLINE', message: '无法连接服务' })
      .mockResolvedValue({ errorCode: 'RESPONSE_INVALID', message: '工作空间格式错误' })
    const { result } = openWorkspace(loadWorkspace)
    await act(async () => {})
    await act(async () => window.dispatchEvent(new Event('online')))
    await act(async () => window.dispatchEvent(new Event('focus')))

    expect(result.current.isBlocked).toBe(true)
    expect(result.current.error).toBe('工作空间格式错误')
    expect(loadWorkspace).toHaveBeenCalledTimes(2)
  })
})
