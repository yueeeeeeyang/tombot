import { describe, it, expect, vi } from 'vitest'
import { readGroupHistory } from './groupHistory'
import type { ServerBubble } from '../desktop/backend'
const message = (sequence: number): ServerBubble => ({
  id: String(sequence),
  sequence,
  role: 'user',
  speaker: 'user',
  content: '消息',
  createdAt: '2026-11-17T00:00:00Z',
  reactions: [],
  attachments: [],
})
describe('群历史补齐', () => {
  it('事件断线超过一页时补齐，不把已读历史和最新页直接拼成缺口', async () => {
    const read = vi
      .fn()
      .mockResolvedValueOnce({ messages: [message(5), message(6)], hasMore: true })
      .mockResolvedValueOnce({ messages: [message(3), message(4)], hasMore: true })
      .mockResolvedValueOnce({ messages: [message(1), message(2)], hasMore: false })
    const result = await readGroupHistory(read, 2)
    expect(result.messages.map((item) => item.sequence)).toEqual([1, 2, 3, 4, 5, 6])
    expect(result.replace).toBe(false)
    expect(read.mock.calls).toEqual([[], [5], [3]])
  })
  it('分页错误不覆盖现有记录，最大补载窗口保留翻页入口', async () => {
    const bad = vi.fn().mockResolvedValue({ messages: [message(20)], hasMore: true })
    await expect(readGroupHistory(bad, 1)).rejects.toThrow('消息分页发生变化')
    let sequence = 40
    const read = vi.fn(async () => ({ messages: [message(sequence--)], hasMore: true }))
    const result = await readGroupHistory(read, 1)
    expect(read).toHaveBeenCalledTimes(20)
    expect(result.replace).toBe(true)
    expect(result.hasMore).toBe(true)
  })
})
