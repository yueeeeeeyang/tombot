import { z } from 'zod'
import { AdminClient, ApiError } from '../api/client'

export const nodeRelease = z.object({ id: z.string(), label: z.string(), platform: z.string() })
const chunkSize = 4 * 1024 * 1024
const maxSize = 17 * 1024 ** 3

async function digest(bytes: ArrayBuffer) {
  const hash = new Uint8Array(await crypto.subtle.digest('SHA-256', bytes))
  return Array.from(hash, (byte) => byte.toString(16).padStart(2, '0')).join('')
}

/** 浏览器按内容续传原包，服务端核对包内文件后才返回可下发版本。 */
export async function uploadNodeRelease(
  client: AdminClient,
  archive: File,
  signal: AbortSignal,
  onProgress: (percent: number, phase: 'hash' | 'upload' | 'verify') => void,
) {
  if (archive.size === 0 || archive.size > maxSize)
    throw new ApiError('INVALID_PACKAGE', '请选择大小不超过 17 GiB 的非空节点离线包。')
  const chunks: string[] = []
  for (let offset = 0; offset < archive.size; offset += chunkSize) {
    signal.throwIfAborted()
    const end = Math.min(offset + chunkSize, archive.size)
    chunks.push(await digest(await archive.slice(offset, end).arrayBuffer()))
    onProgress(Math.floor((100 * end) / archive.size), 'hash')
  }
  const artifact = await digest(new TextEncoder().encode(chunks.join('\n')).buffer)
  const { next } = await client.request(
    '/node-updates/uploads',
    z.object({ next: z.number().int().min(0).max(chunks.length) }),
    {
      method: 'POST',
      body: { artifact, size: archive.size },
      signal,
    },
  )
  onProgress(Math.floor((100 * Math.min(next * chunkSize, archive.size)) / archive.size), 'upload')
  for (let i = next; i < chunks.length; i++) {
    const end = Math.min((i + 1) * chunkSize, archive.size)
    await client.request(
      '/node-updates/parts/' + artifact + '/' + i + '/' + chunks[i],
      z.object({}),
      {
        method: 'PUT',
        body: archive.slice(i * chunkSize, end),
        signal,
        timeout: 30000,
      },
    )
    onProgress(Math.floor((100 * end) / archive.size), 'upload')
  }
  onProgress(100, 'verify')
  return client.request('/node-updates/releases', nodeRelease, {
    method: 'POST',
    body: { artifact },
    signal,
    timeout: 16 * 60_000,
  })
}
