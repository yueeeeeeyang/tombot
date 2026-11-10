import { expect, it } from 'vitest'
import { createWorkspace, type ChatMessage } from './model'
import { mergeIncoming } from './useWorkspace'

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
