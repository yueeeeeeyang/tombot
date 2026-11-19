import { createHash } from 'node:crypto'
import { mkdtemp, readdir, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { expect, it, vi } from 'vitest'
import { checkClientUpdate, saveClientUpdate } from './clientUpdates'

const origin = 'https://tom.example.test'
const first = Buffer.alloc(4 * 1024 * 1024, 5)
const last = Buffer.from('结束')
const release = {
  id: '00000000000000000000000001',
  version: '1.10.0',
  platform: 'darwin-arm64',
  extension: '.dmg' as const,
  notes: '修复消息显示',
  size: first.length + last.length,
  chunks: [first, last].map((b) => createHash('sha256').update(b).digest('hex')),
}

it('以本机版本和架构检查更新，拒绝不匹配的清单并区分检查失败和无更新', async () => {
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(Response.json({ release }))
    .mockResolvedValueOnce(Response.json({ release: null }))
    .mockResolvedValueOnce(Response.json({ release: { ...release, platform: 'darwin-x64' } }))
    .mockResolvedValueOnce(Response.json({ release: { ...release, chunks: [] } }))
    .mockResolvedValueOnce(new Response(null, { status: 503 }))
  const check = () => checkClientUpdate({ fetch }, origin, 'darwin-arm64', '1.9.0')
  await expect(check()).resolves.toEqual(release)
  expect(fetch).toHaveBeenCalledWith(
    origin + '/api/v2/client-updates/latest?platform=darwin-arm64&version=1.9.0',
    expect.objectContaining({
      credentials: 'omit',
      redirect: 'error',
      signal: expect.any(AbortSignal),
    }),
  )
  await expect(check()).resolves.toBeNull()
  await expect(check()).rejects.toThrow('不匹配')
  await expect(check()).rejects.toThrow('无效')
  await expect(check()).rejects.toThrow('检查客户端更新失败')
})
it('按顺序保存多个分片，截断、超长、篡改或请求失败均保留原文件并清理暂存', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'tombot-update-test-'))
  const target = join(dir, 'TomBot.dmg')
  try {
    const fetch = vi
      .fn()
      .mockResolvedValueOnce(new Response(first))
      .mockResolvedValueOnce(new Response(last))
    await saveClientUpdate({ fetch }, origin, release, target)
    expect((await readFile(target)).equals(Buffer.concat([first, last]))).toBe(true)
    expect(fetch.mock.calls[1]?.[0]).toBe(
      origin + '/api/v2/client-updates/' + release.id + '/parts/1',
    )
    await writeFile(target, '原文件')
    for (const invalid of [
      new Response(last.subarray(1)),
      new Response(Buffer.alloc(last.length + 1)),
      new Response(Buffer.alloc(last.length)),
      new Response(null, { status: 404 }),
    ]) {
      fetch.mockResolvedValueOnce(new Response(first)).mockResolvedValueOnce(invalid)
      await expect(saveClientUpdate({ fetch }, origin, release, target)).rejects.toThrow()
      expect(await readFile(target, 'utf8')).toBe('原文件')
      expect(await readdir(dir)).toEqual(['TomBot.dmg'])
    }
  } finally {
    await rm(dir, { recursive: true, force: true })
  }
})

it.each(['1.10.0', '2.0.0'])('拒绝与本机 %s 相同或更低的更新清单', async (installed) => {
  const fetch = vi.fn().mockResolvedValue(Response.json({ release }))
  await expect(checkClientUpdate({ fetch }, origin, release.platform, installed)).rejects.toThrow(
    '没有高于',
  )
})

it('连接切换会中断旧服务的分片请求，保留已有文件并清理暂存', async () => {
  const folder = await mkdtemp(join(tmpdir(), 'tombot-update-switch-'))
  const destination = join(folder, 'TomBot.zip')
  const connection = new AbortController()
  try {
    await writeFile(destination, '原包')
    const fetch = vi.fn(async (_url: string, options: RequestInit) => {
      const signal = options.signal!
      return new Promise<Response>((_resolve, reject) => {
        signal.addEventListener('abort', () => reject(signal.reason), { once: true })
        connection.abort(new Error('服务地址已变更'))
      })
    })
    await expect(
      saveClientUpdate({ fetch }, origin, release, destination, undefined, connection.signal),
    ).rejects.toThrow('服务地址已变更')
    expect(fetch).toHaveBeenCalledOnce()
    expect(await readFile(destination, 'utf8')).toBe('原包')
    expect(await readdir(folder)).toEqual(['TomBot.zip'])
  } finally {
    await rm(folder, { recursive: true, force: true })
  }
})
