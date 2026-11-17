import { describe, it, expect, vi } from 'vitest'
import { changeGroup, createGroup, readGroups, sendGroupMessage } from './groupChat'
const group = {
  id: 'group',
  name: '讨论组',
  purpose: '协作',
  member_ids: ['a', 'b'],
  max_cost_micro: 123,
  revision: 4,
  conversation_id: 'conversation',
  is_archived: false,
}
describe('群聊服务协议', () => {
  it('添加和移出保留现有成员和预算，重命名返回真实目录', async () => {
    const call = vi.fn().mockResolvedValue({ group })
    await changeGroup(call, 'group', 4, { add: ['b', 'c'] })
    expect(call).toHaveBeenLastCalledWith('PUT', '/groups/group', {
      expected_revision: 4,
      name: '讨论组',
      purpose: '协作',
      max_cost_micro: 123,
      member_ids: ['a', 'b', 'c'],
    })
    await changeGroup(call, 'group', 4, { remove: 'b' })
    expect(call.mock.calls.at(-1)?.[2].member_ids).toEqual(['a'])
    await changeGroup(call, 'group', 4, { name: '新名字' })
    expect(call.mock.calls.at(-1)?.[2].name).toBe('新名字')
    expect(await createGroup(call, '新群', ['a'], 'request')).toEqual(group)
  })
  it('持久发送携带幂等键、回复和成员标识，拒绝无效受理响应', async () => {
    const call = vi.fn().mockResolvedValue({ submission: { message_id: 'saved' } })
    const input = {
      requestId: 'request',
      conversationId: 'conversation',
      content: '@小黑 请看',
      attachments: [],
      replyToId: 'original',
      mentions: ['a'],
    }
    expect(await sendGroupMessage(call, input)).toEqual({ acceptedMessageId: 'saved' })
    expect(call).toHaveBeenCalledWith('POST', '/conversations/conversation/messages', {
      client_request_id: 'request',
      content: input.content,
      reply_to_id: 'original',
      mentions: ['a'],
    })
    const fileID = '01J00000000000000000000002'
    await sendGroupMessage(call, {
      ...input,
      attachments: [{ path: `chat-files/01J00000000000000000000001/${fileID}`, sha256: 'digest' }],
    })
    expect(call.mock.calls.at(-1)?.[2].file_ids).toEqual([fileID])
    call.mockResolvedValue({})
    await expect(sendGroupMessage(call, input)).rejects.toThrow()
    await expect(
      sendGroupMessage(call, { ...input, attachments: [{ path: 'file', sha256: 'digest' }] }),
    ).rejects.toThrow('聊天文件地址无效')
  })
  it('未设置预算时原样保留空值', async () => {
    const call = vi.fn().mockResolvedValue({ group: { ...group, max_cost_micro: null } })
    await changeGroup(call, 'group', 4, { name: '新名称' })
    expect(call.mock.calls.at(-1)?.[2].max_cost_micro).toBeNull()
  })
  it('分页完整、错误和修订冲突直接上报', async () => {
    const call = vi
      .fn()
      .mockResolvedValueOnce({
        groups: Array.from({ length: 100 }, (_, i) => ({ ...group, id: `group-${i}` })),
      })
      .mockResolvedValueOnce({ groups: [{ ...group, id: 'last' }] })
    expect(await readGroups(call)).toHaveLength(101)
    expect(call).toHaveBeenLastCalledWith('GET', '/groups?limit=100&before=group-99')
    const conflict = vi
      .fn()
      .mockResolvedValueOnce({ group })
      .mockRejectedValue(new Error('版本冲突'))
    await expect(changeGroup(conflict, 'group', 3, { add: ['c'] })).rejects.toThrow('版本冲突')
    expect(conflict).toHaveBeenCalledTimes(2)
    await expect(readGroups(vi.fn().mockResolvedValue({}))).rejects.toThrow(
      '群聊服务返回的数据不完整',
    )
  })
})
