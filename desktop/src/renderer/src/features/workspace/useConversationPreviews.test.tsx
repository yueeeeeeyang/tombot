import { act, renderHook, waitFor } from '@testing-library/react'
import { expect, it, vi } from 'vitest'
import { memoryBackend } from '../desktop/memoryBackend'
import type { ConversationPreview, UserEvent } from '../desktop/backend'
import { useConversationPreviews } from './useConversationPreviews'

it('启动读取未打开会话，事件刷新且旧序号不能覆盖摘要', async () => {
  let publish!: (event: UserEvent) => void
  const stop = vi.fn()
  vi.spyOn(memoryBackend, 'watchEvents').mockImplementation((callback) => {
    publish = callback
    return stop
  })
  const load = vi
    .spyOn(memoryBackend, 'loadConversationPreviews')
    .mockResolvedValueOnce([{ conversation_id: 'a', sequence: 2, content: '启动时最后一条' }])
    .mockResolvedValueOnce([{ conversation_id: 'a', sequence: 3, content: '新消息' }])
    .mockResolvedValue([{ conversation_id: 'a', sequence: 1, content: '迟到消息' }])
  const error = vi.fn()
  const { result, unmount } = renderHook(() => useConversationPreviews(['a'], error))
  await waitFor(() => expect(result.current.a?.content).toBe('启动时最后一条'))
  act(() =>
    publish({
      kind: 'message.created',
      payload: { conversation_id: 'a' },
      sequence: 1,
      resource_id: 'message',
      created_at: '',
    }),
  )
  await waitFor(() => expect(result.current.a?.content).toBe('新消息'))
  act(() => window.dispatchEvent(new Event('focus')))
  await waitFor(() => expect(load).toHaveBeenCalledTimes(3))
  expect(result.current.a?.content).toBe('新消息')
  expect(error).not.toHaveBeenCalled()
  unmount()
  expect(stop).toHaveBeenCalledOnce()
})
it('目录切换忽略旧回包，失败显示错误并保留已加载摘要', async () => {
  let finish!: (value: ConversationPreview[]) => void
  const load = vi
    .spyOn(memoryBackend, 'loadConversationPreviews')
    .mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          finish = resolve
        }),
    )
    .mockResolvedValueOnce([{ conversation_id: 'b', sequence: 1, content: '新目录' }])
    .mockRejectedValue(new Error('离线'))
  const error = vi.fn()
  const { result, rerender } = renderHook(({ ids }) => useConversationPreviews(ids, error), {
    initialProps: { ids: ['a'] },
  })
  rerender({ ids: ['b'] })
  await waitFor(() => expect(result.current.b?.content).toBe('新目录'))
  await act(async () => finish([{ conversation_id: 'a', sequence: 1, content: '旧目录' }]))
  expect(result.current.a).toBeUndefined()
  act(() => window.dispatchEvent(new Event('focus')))
  await waitFor(() => expect(error).toHaveBeenCalledWith('读取聊天摘要失败：离线'))
  expect(result.current.b?.content).toBe('新目录')
  expect(load).toHaveBeenCalledTimes(3)
})
it('超过单批上限时分批读取并去重', async () => {
  const load = vi.spyOn(memoryBackend, 'loadConversationPreviews').mockResolvedValue([])
  const ids = Array.from({ length: 101 }, (_, index) => `chat-${index}`)
  const error = vi.fn()
  renderHook(() => useConversationPreviews([...ids, ids[0]!], error))
  await waitFor(() => expect(load).toHaveBeenCalledTimes(2))
  expect(load.mock.calls.map(([batch]) => batch.length)).toEqual([100, 1])
})
