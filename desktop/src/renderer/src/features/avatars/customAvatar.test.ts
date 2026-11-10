import { afterEach, expect, it, vi } from 'vitest'
import { createCustomAvatar } from './customAvatar'
afterEach(() => vi.unstubAllGlobals())
it('拒绝不支持的文件、过大的原图和损坏的图片', async () => {
  await expect(
    createCustomAvatar(new File(['x'], 'a.svg', { type: 'image/svg+xml' })),
  ).rejects.toThrow('PNG')
  await expect(
    createCustomAvatar(
      new File([new Uint8Array(4 * 1024 * 1024 + 1)], 'a.png', { type: 'image/png' }),
    ),
  ).rejects.toThrow('4 MiB')
  vi.stubGlobal('createImageBitmap', vi.fn().mockRejectedValue(new Error('decode')))
  await expect(
    createCustomAvatar(new File(['broken'], 'a.png', { type: 'image/png' })),
  ).rejects.toThrow('无法读取')
})
it('超出像素尺寸时释放解码资源', async () => {
  const close = vi.fn()
  vi.stubGlobal('createImageBitmap', vi.fn().mockResolvedValue({ width: 4097, height: 128, close }))
  await expect(createCustomAvatar(new File(['x'], 'a.png', { type: 'image/png' }))).rejects.toThrow(
    '4096',
  )
  expect(close).toHaveBeenCalledOnce()
})
