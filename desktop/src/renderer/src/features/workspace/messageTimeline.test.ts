import { expect, it } from 'vitest'
import type { ChatMessage } from './model'
import { mergeMessages } from './messageTimeline'

function message(id: string, overrides: Partial<ChatMessage> = {}): ChatMessage {
  return {
    id,
    role: 'user',
    content: '相同正文',
    createdAt: '2026-11-16T01:00:00Z',
    status: 'unsent',
    reactions: [],
    ...overrides,
  }
}

it('回执前的服务端回声与本地消息合并，重复快照保留历史和列表身份', () => {
  const history = message('history', { sequence: 1, status: 'sent' })
  const local = message('request', { requestId: 'request' })
  const confirmed = message('server', { requestId: 'request', sequence: 2, status: 'sent' })
  const merged = mergeMessages([history, local], [confirmed])
  expect(merged).toHaveLength(2)
  expect(merged[0]).toBe(history)
  expect(merged[1]).toMatchObject({ id: 'server', requestId: 'request', status: 'sent' })
  expect(mergeMessages(merged, [structuredClone(history), structuredClone(confirmed)])).toBe(merged)
})

it('相同正文的连续发送不互相覆盖，已确认消息按序号排列，不受客户端时钟影响', () => {
  const pending = message('second', { requestId: 'second', createdAt: '2020-01-01T00:00:00Z' })
  const first = message('first', { requestId: 'first' })
  const history = message('history', { sequence: 1, status: 'sent' })
  const confirmed = message('server-first', { requestId: 'first', sequence: 2, status: 'sent' })
  const merged = mergeMessages([history, first, pending], [confirmed])
  expect(merged.map((item) => item.id)).toEqual(['history', 'server-first', 'second'])
  expect(merged[2]).toBe(pending)
})

it('只替换有变化的消息，向前翻页不丢失已加载历史或未发送内容', () => {
  const first = message('first', { sequence: 2, status: 'sent' })
  const last = message('last', { status: 'failed' })
  const changed = { ...first, reactions: [{ emoji: '👍' as const, count: 1 }] }
  const merged = mergeMessages(
    [first, last],
    [message('older', { sequence: 1, status: 'sent' }), changed],
  )
  expect(merged.map((item) => item.id)).toEqual(['older', 'first', 'last'])
  expect(merged[1]?.reactions).toEqual(changed.reactions)
  expect(merged[2]).toBe(last)
})
