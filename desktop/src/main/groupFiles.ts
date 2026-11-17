import { createHash } from 'node:crypto'
import { z } from 'zod'
import {
  GROUP_FILE_MAX_BYTES,
  type GroupFile,
  type GroupFilePage,
  type GroupFilePreview,
} from '../shared/groupFiles'

type Call = (method: string, path: string, body?: unknown, timeout?: number) => Promise<unknown>
const idSchema = z.string().regex(/^[0-9A-HJKMNP-TV-Z]{26}$/)
const fileSchema = z.object({
  id: idSchema,
  group_id: idSchema,
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
  size: z.number().int().min(0).max(GROUP_FILE_MAX_BYTES),
  sha256: z.string().regex(/^[a-f0-9]{64}$/),
  revision: z.number().int().positive(),
  created_at: z.iso.datetime({ offset: true }),
})
function parse<T>(schema: z.ZodType<T>, value: unknown): T {
  const result = schema.safeParse(value)
  if (!result.success) throw new Error('资料库返回的数据不完整，请刷新后重试。')
  return result.data
}
function route(groupId: string, fileId?: string): string {
  if (
    !idSchema.safeParse(groupId).success ||
    (fileId !== undefined && !idSchema.safeParse(fileId).success)
  )
    throw new Error('群文件地址无效。')
  return `/groups/${groupId}/files${fileId ? `/${fileId}` : ''}`
}
export async function listGroupFiles(
  call: Call,
  groupId: string,
  before = '',
  query = '',
): Promise<GroupFilePage> {
  const page = parse(
    z.object({
      files: z.array(fileSchema).max(20),
      next: z.union([idSchema, z.literal('')]),
      used_bytes: z.number().nonnegative(),
      max_bytes: z.number().positive(),
      max_file_bytes: z.number().positive(),
    }),
    await call('GET', `${route(groupId)}?${new URLSearchParams({ before, q: query })}`),
  )
  if (page.files.some((file) => file.group_id !== groupId))
    throw new Error('资料库返回的群组不匹配。')
  return page
}
export async function uploadGroupFile(
  call: Call,
  groupId: string,
  file: { name: string; bytes: Uint8Array },
  requestId: string,
): Promise<GroupFile> {
  if (!idSchema.safeParse(requestId).success || file.bytes.byteLength > GROUP_FILE_MAX_BYTES)
    throw new Error('单个文件最多 16 MB。')
  const bytes = Buffer.from(file.bytes)
  const uploaded = parse(
    z.object({ file: fileSchema }),
    await call(
      'POST',
      route(groupId),
      { client_request_id: requestId, name: file.name, content_base64: bytes.toString('base64') },
      60_000,
    ),
  ).file
  if (
    uploaded.group_id !== groupId ||
    uploaded.id !== requestId ||
    uploaded.sha256 !== createHash('sha256').update(bytes).digest('hex')
  )
    throw new Error('文件上传校验失败，请重试。')
  return uploaded
}
export async function readGroupFile(
  call: Call,
  groupId: string,
  fileId: string,
): Promise<{ file: GroupFile; bytes: Buffer }> {
  const result = parse(
    z.object({
      file: fileSchema,
      content_base64: z.string().max(Math.ceil(GROUP_FILE_MAX_BYTES / 3) * 4),
    }),
    await call('GET', `${route(groupId, fileId)}/content`, undefined, 60_000),
  )
  const bytes = Buffer.from(result.content_base64, 'base64')
  if (
    result.file.group_id !== groupId ||
    result.file.id !== fileId ||
    bytes.length !== result.file.size ||
    createHash('sha256').update(bytes).digest('hex') !== result.file.sha256
  )
    throw new Error('文件内容校验失败，请重新下载。')
  return { file: result.file, bytes }
}
/** 只展示静态图片和转义后的纯文本；HTML、SVG 及其他二进制不会作为页面执行。 */
export async function previewGroupFile(
  call: Call,
  groupId: string,
  fileId: string,
): Promise<GroupFilePreview> {
  const { file, bytes } = await readGroupFile(call, groupId, fileId)
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
export async function renameGroupFile(
  call: Call,
  groupId: string,
  fileId: string,
  revision: number,
  name: string,
): Promise<void> {
  parse(
    z.object({ file: fileSchema }),
    await call('PUT', route(groupId, fileId), { expected_revision: revision, name }),
  )
}
export async function deleteGroupFile(
  call: Call,
  groupId: string,
  fileId: string,
  revision: number,
): Promise<void> {
  await call('DELETE', route(groupId, fileId), { expected_revision: revision })
}
