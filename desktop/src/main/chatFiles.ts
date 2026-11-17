import { createHash } from 'node:crypto'
import { z } from 'zod'
import {
  CHAT_FILE_MAX_BYTES,
  type ChatFile,
  type ChatFilePage,
  type ChatFilePreview,
} from '../shared/chatFiles'

type Call = (method: string, path: string, body?: unknown, timeout?: number) => Promise<unknown>
const idSchema = z.string().regex(/^[0-9A-HJKMNP-TV-Z]{26}$/)
const fileSchema = z.object({
  id: idSchema,
  conversation_id: idSchema,
  name: z
    .string()
    .min(1)
    .max(240)
    .refine(
      (name) =>
        Buffer.byteLength(name) <= 240 &&
        name === name.trim() &&
        name !== '.' &&
        name !== '..' &&
        !/[\p{Cc}\p{Cf}/\\]/u.test(name),
    ),
  media_type: z.string(),
  size: z.number().int().min(0).max(CHAT_FILE_MAX_BYTES),
  sha256: z.string().regex(/^[a-f0-9]{64}$/),
  revision: z.number().int().positive(),
  created_at: z.iso.datetime({ offset: true }),
})
function parse<T>(schema: z.ZodType<T>, value: unknown): T {
  const result = schema.safeParse(value)
  if (!result.success) throw new Error('资料库返回的数据不完整，请刷新后重试。')
  return result.data
}
function route(conversationId: string, fileId?: string): string {
  if (
    !idSchema.safeParse(conversationId).success ||
    (fileId !== undefined && !idSchema.safeParse(fileId).success)
  )
    throw new Error('聊天文件地址无效。')
  return `/conversations/${conversationId}/files${fileId ? `/${fileId}` : ''}`
}
export async function listChatFiles(
  call: Call,
  conversationId: string,
  before = '',
  query = '',
): Promise<ChatFilePage> {
  const page = parse(
    z.object({
      files: z.array(fileSchema).max(20),
      next: z.union([idSchema, z.literal('')]),
      used_bytes: z.number().nonnegative(),
      max_bytes: z.number().positive(),
      max_file_bytes: z.number().positive(),
    }),
    await call('GET', `${route(conversationId)}?${new URLSearchParams({ before, q: query })}`),
  )
  if (page.files.some((file) => file.conversation_id !== conversationId))
    throw new Error('资料库返回的会话不匹配。')
  return page
}
export async function uploadChatFile(
  call: Call,
  conversationId: string,
  file: { name: string; bytes: Uint8Array },
  requestId: string,
): Promise<ChatFile> {
  if (!idSchema.safeParse(requestId).success || file.bytes.byteLength > CHAT_FILE_MAX_BYTES)
    throw new Error('单个文件最多 16 MB。')
  const bytes = Buffer.from(file.bytes)
  const uploaded = parse(
    z.object({ file: fileSchema }),
    await call(
      'POST',
      route(conversationId),
      { client_request_id: requestId, name: file.name, content_base64: bytes.toString('base64') },
      60_000,
    ),
  ).file
  if (
    uploaded.conversation_id !== conversationId ||
    uploaded.id !== requestId ||
    uploaded.sha256 !== createHash('sha256').update(bytes).digest('hex')
  )
    throw new Error('文件上传校验失败，请重试。')
  return uploaded
}
export async function readChatFile(
  call: Call,
  conversationId: string,
  fileId: string,
): Promise<{ file: ChatFile; bytes: Buffer }> {
  const result = parse(
    z.object({
      file: fileSchema,
      content_base64: z.string().max(Math.ceil(CHAT_FILE_MAX_BYTES / 3) * 4),
    }),
    await call('GET', `${route(conversationId, fileId)}/content`, undefined, 60_000),
  )
  const bytes = Buffer.from(result.content_base64, 'base64')
  if (
    result.file.conversation_id !== conversationId ||
    result.file.id !== fileId ||
    bytes.length !== result.file.size ||
    createHash('sha256').update(bytes).digest('hex') !== result.file.sha256
  )
    throw new Error('文件内容校验失败，请重新下载。')
  return { file: result.file, bytes }
}
/** 只展示静态图片和转义后的纯文本；HTML、SVG 及其他二进制不会作为页面执行。 */
export async function previewChatFile(
  call: Call,
  conversationId: string,
  fileId: string,
): Promise<ChatFilePreview> {
  const { file, bytes } = await readChatFile(call, conversationId, fileId)
  if (['image/png', 'image/jpeg', 'image/gif'].includes(file.media_type))
    return { file, image: `data:${file.media_type};base64,${bytes.toString('base64')}` }
  if (file.media_type.startsWith('text/') || bytes.length === 0) {
    const maximum = 64 * 1024
    return {
      file,
      text: new TextDecoder('utf-8').decode(bytes.subarray(0, maximum)),
      truncated: bytes.length > maximum,
    }
  }
  return { file }
}
export async function renameChatFile(
  call: Call,
  conversationId: string,
  fileId: string,
  revision: number,
  name: string,
): Promise<void> {
  parse(
    z.object({ file: fileSchema }),
    await call('PUT', route(conversationId, fileId), { expected_revision: revision, name }),
  )
}
export async function deleteChatFile(
  call: Call,
  conversationId: string,
  fileId: string,
  revision: number,
): Promise<void> {
  await call('DELETE', route(conversationId, fileId), { expected_revision: revision })
}
