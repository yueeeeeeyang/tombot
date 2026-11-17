import { z } from 'zod'
import type { ConversationPreview } from '../renderer/src/features/desktop/backend'

const idsSchema = z
  .array(z.string().regex(/^[0-9A-HJKMNP-TV-Z]{26}$/))
  .min(1)
  .max(100)
  .refine((ids) => new Set(ids).size === ids.length)
const responseSchema = z.object({
  previews: z
    .array(
      z.object({
        conversation_id: z.string(),
        sequence: z.number().int().nonnegative(),
        content: z.string().max(160),
      }),
    )
    .max(100),
})
/** 受限批量查询只读取短摘要，响应缺项、重复或身份不匹配都作为失败处理。 */
export async function readConversationPreviews(
  call: (method: string, path: string, body?: unknown) => Promise<unknown>,
  ids: string[],
): Promise<ConversationPreview[]> {
  if (!idsSchema.safeParse(ids).success) throw new Error('会话摘要查询参数无效。')
  const response = responseSchema.safeParse(
    await call('POST', '/conversations/previews', { conversation_ids: ids }),
  )
  if (
    !response.success ||
    response.data.previews.length !== ids.length ||
    new Set(response.data.previews.map((preview) => preview.conversation_id)).size !== ids.length ||
    response.data.previews.some((preview) => !ids.includes(preview.conversation_id))
  )
    throw new Error('服务端会话摘要不完整，请重试。')
  return response.data.previews
}
