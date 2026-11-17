import { expect, it, vi } from 'vitest'
import { readConversationPreviews } from './conversationPreviews'
const id = '01KAAA00000000000000000000'
it('批量读取摘要并保留空会话', async () => {
  const previews = [{ conversation_id: id, sequence: 0, content: '' }]
  const call = vi.fn().mockResolvedValue({ previews })
  await expect(readConversationPreviews(call, [id])).resolves.toEqual(previews)
  expect(call).toHaveBeenCalledWith('POST', '/conversations/previews', { conversation_ids: [id] })
})
it('拒绝非法或重复身份、超量请求、缺项和失败响应', async () => {
  const call = vi.fn().mockResolvedValue({ previews: [] })
  for (const ids of [[], [id, id], ['invalid'], Array.from({ length: 101 }, () => id)])
    await expect(readConversationPreviews(call, ids)).rejects.toThrow('参数无效')
  expect(call).not.toHaveBeenCalled()
  await expect(readConversationPreviews(call, [id])).rejects.toThrow('不完整')
  call.mockRejectedValue(new Error('离线'))
  await expect(readConversationPreviews(call, [id])).rejects.toThrow('离线')
})
