import { createHash, randomUUID } from 'node:crypto'
import { open, rename, rm } from 'node:fs/promises'
import { dirname, join } from 'node:path'
import { z } from 'zod'

const chunkSize = 4 * 1024 * 1024
const versionPattern = /^(?:0|[1-9]\d{0,5})\.(?:0|[1-9]\d{0,5})\.(?:0|[1-9]\d{0,5})$/
const manifestSchema = z.object({
  id: z.string().regex(/^[0-9A-HJKMNP-TV-Z]{26}$/),
  platform: z.string(),
  version: z.string().regex(versionPattern),
  extension: z.enum([
    '.dmg',
    '.pkg',
    '.zip',
    '.msi',
    '.exe',
    '.deb',
    '.rpm',
    '.AppImage',
    '.tar.gz',
  ]),
  notes: z.string().trim().min(1).max(8000),
  size: z
    .number()
    .int()
    .positive()
    .max(1024 ** 3),
  chunks: z
    .array(z.string().regex(/^[a-f0-9]{64}$/))
    .min(1)
    .max(256),
})
export type ClientRelease = z.infer<typeof manifestSchema>
type Transport = { fetch: (url: string, options: RequestInit) => Promise<Response> }

/** 对不可信服务响应逐段限长；超限时取消读取，避免一次分配整个安装包。 */
async function readBytes(response: Response, limit: number): Promise<Buffer> {
  const reader = response.body?.getReader()
  if (!reader) throw new Error('升级服务未返回内容。')
  const buffers: Uint8Array[] = []
  let size = 0
  try {
    for (;;) {
      const part = await reader.read().catch(() => {
        throw new Error('升级数据读取中断，请检查网络后重试。')
      })
      if (part.done) break
      size += part.value.length
      if (size > limit) {
        await reader.cancel()
        throw new Error('升级服务响应超出大小限制。')
      }
      buffers.push(part.value)
    }
  } finally {
    reader.releaseLock()
  }
  return Buffer.concat(buffers, size)
}

/** 换服务与单次请求超时都应结束旧请求，避免旧分片继续占用下载通道。 */
function requestSignal(timeout: number, connection?: AbortSignal) {
  const deadline = AbortSignal.timeout(timeout)
  return connection ? AbortSignal.any([connection, deadline]) : deadline
}

/** 只查询已连接服务，主进程提供本机平台和版本，渲染页面不能指定下载地址。 */
export async function checkClientUpdate(
  transport: Transport,
  origin: string,
  platform: string,
  version: string,
  signal?: AbortSignal,
) {
  const query = new URLSearchParams({ platform, version })
  const response = await transport.fetch(`${origin}/api/v2/client-updates/latest?${query}`, {
    credentials: 'omit',
    redirect: 'error',
    signal: requestSignal(10000, signal),
  })
  if (!response.ok) {
    await response.body?.cancel()
    throw new Error('检查客户端更新失败，请确认服务端已升级后重试。')
  }
  const bytes = await readBytes(response, 64 * 1024)
  let payload: unknown
  try {
    payload = JSON.parse(bytes.toString('utf8'))
  } catch {
    throw new Error('服务端返回的升级信息无效。')
  }
  const parsed = z.object({ release: manifestSchema.nullable() }).safeParse(payload)
  if (!parsed.success) throw new Error('服务端返回的升级信息无效。')
  const release = parsed.data.release
  if (
    release &&
    (release.platform !== platform || release.chunks.length !== Math.ceil(release.size / chunkSize))
  )
    throw new Error('安装包与本机平台或分片清单不匹配。')
  if (release) {
    const installed = version.split('.').map(Number)
    const offered = release.version.split('.').map(Number)
    const difference = offered.findIndex((part, index) => part !== installed[index])
    if (
      !versionPattern.test(version) ||
      difference < 0 ||
      offered[difference]! < installed[difference]!
    )
      throw new Error('服务端返回的升级版本没有高于当前版本。')
  }
  return release
}

/** 在目标目录暂存逐片校验的内容，完成后原子替换，失败时保留用户已有文件。 */
export async function saveClientUpdate(
  transport: Transport,
  origin: string,
  release: ClientRelease,
  destination: string,
  onVerified?: (bytes: number) => void,
  signal?: AbortSignal,
) {
  const staging = join(dirname(destination), '.tombot-update-' + randomUUID() + '.part')
  const file = await open(staging, 'wx', 0o600)
  try {
    try {
      for (const [index, digest] of release.chunks.entries()) {
        const response = await transport.fetch(
          `${origin}/api/v2/client-updates/${release.id}/parts/${index}`,
          { credentials: 'omit', redirect: 'error', signal: requestSignal(30_000, signal) },
        )
        if (!response.ok) {
          await response.body?.cancel()
          throw new Error('安装包下载中断，请重试。')
        }
        const expected = Math.min(chunkSize, release.size - index * chunkSize)
        const bytes = await readBytes(response, expected)
        if (
          bytes.length !== expected ||
          createHash('sha256').update(bytes).digest('hex') !== digest
        )
          throw new Error('安装包完整性校验失败，请重新下载。')
        await file.writeFile(bytes)
        onVerified?.(index * chunkSize + bytes.length)
      }
      // AppImage 本身是可执行应用，只在完整性校验成功后授予当前用户执行权限。
      if (release.extension === '.AppImage' && release.platform.startsWith('linux-'))
        await file.chmod(0o700)
      await file.sync()
    } finally {
      await file.close()
    }
    signal?.throwIfAborted()
    await rename(staging, destination)
  } finally {
    await rm(staging, { force: true })
  }
}
