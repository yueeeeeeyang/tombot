import { expect, it, vi } from 'vitest'
import { ChatImageCache } from './chatImageCache'

const photo = { path: '会话/照片', sha256: 'a'.repeat(64) }
const revisedPhoto = { ...photo, sha256: 'b'.repeat(64) }
const otherPhoto = { ...photo, path: '其他会话/照片' }

function deferPreview() {
  let resolve!: (source: string) => void
  const promise = new Promise<string>((finish) => {
    resolve = finish
  })
  return { promise, resolve }
}

it('用附件路径和摘要区分图片，相同附件同时请求时只读一次', async () => {
  const cache = new ChatImageCache()
  const pending = deferPreview()
  const read = vi.fn(() => pending.promise)
  const first = cache.load(photo, read)
  const second = cache.load({ ...photo }, read)
  expect(cache.get(photo)).toBeUndefined()

  pending.resolve('照片')
  await expect(Promise.all([first, second])).resolves.toEqual(['照片', '照片'])
  await expect(cache.load({ ...photo }, read)).resolves.toBe('照片')
  expect(read).toHaveBeenCalledOnce()
  expect(cache.get(revisedPhoto)).toBeUndefined()
  expect(cache.get(otherPhoto)).toBeUndefined()

  await cache.load(revisedPhoto, async () => '更新后的照片')
  await cache.load(otherPhoto, async () => '另一张照片')
  expect(cache.get(photo)).toBe('照片')
  expect(cache.get(revisedPhoto)).toBe('更新后的照片')
  expect(cache.get(otherPhoto)).toBe('另一张照片')
})

it('读取失败或预览为空时不留缓存，下次读取可以成功', async () => {
  const cache = new ChatImageCache()
  const read = vi
    .fn<() => Promise<string>>()
    .mockRejectedValueOnce(new Error('读取失败'))
    .mockResolvedValueOnce('')
    .mockResolvedValueOnce('照片')

  await expect(cache.load(photo, read)).rejects.toThrow('读取失败')
  expect(cache.get(photo)).toBeUndefined()
  await expect(cache.load(photo, read)).resolves.toBe('')
  expect(cache.get(photo)).toBeUndefined()
  await expect(cache.load(photo, read)).resolves.toBe('照片')
  expect(cache.get(photo)).toBe('照片')
  expect(read).toHaveBeenCalledTimes(3)
})

it('字节预算耗尽时按插入顺序淘汰，超预算的单张图片仍可展示', async () => {
  const cache = new ChatImageCache(8, 10)
  await cache.load(photo, async () => '照片')
  await cache.load(revisedPhoto, async () => '插图')
  expect(cache.get(photo)).toBe('照片')
  expect(cache.get(revisedPhoto)).toBe('插图')

  await cache.load(otherPhoto, async () => '图')
  expect(cache.get(photo)).toBeUndefined()
  expect(cache.get(revisedPhoto)).toBe('插图')
  expect(cache.get(otherPhoto)).toBe('图')

  await expect(cache.load(photo, async () => '超出预算的照片')).resolves.toBe('超出预算的照片')
  expect(cache.get(photo)).toBeUndefined()
  expect(cache.get(revisedPhoto)).toBe('插图')
  expect(cache.get(otherPhoto)).toBe('图')
})

it('条数预算耗尽时淘汰最早存入的图片', async () => {
  const cache = new ChatImageCache(1_024, 2)
  await cache.load(photo, async () => '照片')
  await cache.load(revisedPhoto, async () => '更新后的照片')
  expect(cache.get(photo)).toBe('照片')

  await cache.load(otherPhoto, async () => '另一张照片')
  expect(cache.get(photo)).toBeUndefined()
  expect(cache.get(revisedPhoto)).toBe('更新后的照片')
  expect(cache.get(otherPhoto)).toBe('另一张照片')
})

it('文件变更使同路径的全部摘要失效，迟到结果不会干扰新的读取', async () => {
  const cache = new ChatImageCache()
  const prior = deferPreview()
  const replacement = deferPreview()
  await cache.load(revisedPhoto, async () => '同一文件的其他版本')
  await cache.load(otherPhoto, async () => '另一张照片')
  const stale = cache.load(photo, () => prior.promise)

  cache.invalidate(photo.path)
  expect(cache.get(revisedPhoto)).toBeUndefined()
  expect(cache.get(otherPhoto)).toBe('另一张照片')
  const read = vi.fn(() => replacement.promise)
  const current = cache.load(photo, read)

  prior.resolve('旧照片')
  await expect(stale).resolves.toBe('旧照片')
  expect(cache.get(photo)).toBeUndefined()
  const next = cache.load(photo, read)
  replacement.resolve('新照片')
  await expect(Promise.all([current, next])).resolves.toEqual(['新照片', '新照片'])
  expect(read).toHaveBeenCalledOnce()
  expect(cache.get(photo)).toBe('新照片')
})

it('只丢弃与图片解码错误对应的缓存地址', async () => {
  const cache = new ChatImageCache()
  await cache.load(photo, async () => '旧地址')
  cache.discard(photo, '已过期的地址')
  expect(cache.get(photo)).toBe('旧地址')

  cache.discard(photo, '旧地址')
  expect(cache.get(photo)).toBeUndefined()
  await cache.load(photo, async () => '新地址')
  cache.discard(photo, '旧地址')
  expect(cache.get(photo)).toBe('新地址')
})

it('清理同时释放全部图片和等待中的读取，迟到结果不会重填缓存', async () => {
  const cache = new ChatImageCache()
  const prior = deferPreview()
  await cache.load(otherPhoto, async () => '另一张照片')
  const stale = cache.load(photo, () => prior.promise)

  cache.clear()
  expect(cache.get(otherPhoto)).toBeUndefined()
  await expect(cache.load(photo, async () => '新照片')).resolves.toBe('新照片')

  prior.resolve('旧照片')
  await expect(stale).resolves.toBe('旧照片')
  expect(cache.get(photo)).toBe('新照片')
})
