import { z } from 'zod'
import { parseGroupFilePath } from '../shared/groupFiles'

type Call = (method: string, path: string, body?: unknown) => Promise<unknown>
const groupSchema = z.object({
  id: z.string().min(1),
  name: z.string(),
  purpose: z.string(),
  member_ids: z.array(z.string()).min(1).max(32),
  max_cost_micro: z.number().int().nonnegative().nullable(),
  revision: z.number().int().positive(),
  conversation_id: z.string().min(1),
  is_archived: z.boolean(),
})
const groupReply = z.object({ group: groupSchema })
/** 校验失败只返回可操作的提示，不把协议解析堆栈暴露到聊天界面。 */
function parseResponse<T>(schema: z.ZodType<T>, value: unknown): T {
  const result = schema.safeParse(value)
  if (!result.success) throw new Error('群聊服务返回的数据不完整，请检查服务端版本后重试。')
  return result.data
}
export type GroupRecord = z.infer<typeof groupSchema>

/** 群目录按游标读取，重复页或异常结构不能伪装成空目录。 */
export async function readGroups(call: Call): Promise<GroupRecord[]> {
  const groups: GroupRecord[] = []
  let before = ''
  for (let index = 0; index < 10; index++) {
    const page = parseResponse(
      z.object({ groups: z.array(groupSchema).max(100) }),
      await call(
        'GET',
        `/groups?limit=100${before ? `&before=${encodeURIComponent(before)}` : ''}`,
      ),
    ).groups
    if (page.some((item) => groups.some((previous) => previous.id === item.id)))
      throw new Error('群目录发生变化，请重新读取。')
    groups.push(...page)
    if (page.length < 100) return groups
    before = page.at(-1)!.id
  }
  throw new Error('群目录超过读取上限。')
}

export async function createGroup(
  call: Call,
  name: string,
  memberIds: string[],
  requestId: string,
): Promise<GroupRecord> {
  return parseResponse(
    groupReply,
    await call('POST', '/groups', {
      client_request_id: requestId,
      name,
      purpose: '',
      member_ids: memberIds,
    }),
  ).group
}

/** 修改单项时携带原有成员、说明及预算，版本冲突直接返回，不自动重放修改。 */
export async function changeGroup(
  call: Call,
  id: string,
  revision: number,
  change: { name?: string; add?: string[]; remove?: string },
): Promise<void> {
  const group = parseResponse(
    groupReply,
    await call('GET', `/groups/${encodeURIComponent(id)}`),
  ).group
  const members = [...new Set([...group.member_ids, ...(change.add ?? [])])].filter(
    (member) => member !== change.remove,
  )
  await call('PUT', `/groups/${encodeURIComponent(id)}`, {
    expected_revision: revision,
    name: change.name ?? group.name,
    purpose: group.purpose,
    max_cost_micro: group.max_cost_micro,
    member_ids: members,
  })
}

export async function sendGroupMessage(
  call: Call,
  input: {
    requestId: string
    conversationId: string
    content: string
    attachments: Array<{ path: string; sha256: string }>
    replyToId?: string
    mentions: string[]
  },
): Promise<{ acceptedMessageId: string }> {
  const fileIds = input.attachments.map((attachment) => parseGroupFilePath(attachment.path).fileId)
  const accepted = parseResponse(
    z.object({ submission: z.object({ message_id: z.string().min(1) }) }),
    await call('POST', `/conversations/${encodeURIComponent(input.conversationId)}/messages`, {
      client_request_id: input.requestId,
      content: input.content,
      mentions: input.mentions,
      ...(fileIds.length ? { file_ids: fileIds } : {}),
      ...(input.replyToId ? { reply_to_id: input.replyToId } : {}),
    }),
  )
  return { acceptedMessageId: accepted.submission.message_id }
}
