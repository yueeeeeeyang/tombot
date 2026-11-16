import { afterEach, expect, it, vi } from 'vitest'
import {
  cropRectangle,
  loadAvatar,
  maxAvatarSourceBytes,
  readAvatarDimensions,
} from '../../../ui/avatar-crop.js'

function pngHeader(width: number, height: number) {
  const buffer = new ArrayBuffer(33)
  new Uint8Array(buffer).set([137, 80, 78, 71, 13, 10, 26, 10])
  const view = new DataView(buffer)
  view.setUint32(8, 13)
  view.setUint32(12, 0x49484452)
  view.setUint32(16, width)
  view.setUint32(20, height)
  return buffer
}

function jpegHeader(width: number, height: number) {
  return Uint8Array.from([
    0xff,
    0xd8,
    0xff,
    0xe0,
    0,
    4,
    0,
    0,
    0xff,
    0xc2,
    0,
    8,
    8,
    height >> 8,
    height & 255,
    width >> 8,
    width & 255,
    1,
  ]).buffer
}

afterEach(() => vi.unstubAllGlobals())

it('裁剪横图、竖图与缩放时保持方形并限制边缘，不产生空白区域', () => {
  expect(cropRectangle(600, 400, 1, 300, 200)).toEqual({
    x: 100,
    y: 0,
    size: 400,
    centerX: 300,
    centerY: 200,
  })
  expect(cropRectangle(400, 600, 2, 200, 300)).toEqual({
    x: 100,
    y: 200,
    size: 200,
    centerX: 200,
    centerY: 300,
  })
  expect(cropRectangle(600, 400, 2, -100, 1000)).toEqual({
    x: 0,
    y: 200,
    size: 200,
    centerX: 100,
    centerY: 300,
  })
  expect(cropRectangle(1, 1, 1, 0, 0).size).toBe(1)
})

it('解码前检查 PNG 和带元数据的渐进式 JPEG 尺寸', () => {
  expect(readAvatarDimensions(pngHeader(4096, 4096))).toEqual({ width: 4096, height: 4096 })
  expect(readAvatarDimensions(jpegHeader(1200, 800))).toEqual({ width: 1200, height: 800 })
  for (const invalid of [
    new ArrayBuffer(0),
    pngHeader(0, 2),
    pngHeader(4097, 2),
    pngHeader(2, 4097),
    jpegHeader(10, 10).slice(0, 12),
    Uint8Array.from([255, 216, 255, 224, 0, 0]).buffer,
  ]) {
    expect(() => readAvatarDimensions(invalid)).toThrow()
  }
})

it('无效和超大原图在位图解码前被拒绝，解码失败返回中文提示', async () => {
  const decode = vi.fn().mockRejectedValue(new Error('decode'))
  vi.stubGlobal('createImageBitmap', decode)
  const file = (size: number, type: string, buffer: ArrayBuffer) =>
    ({ size, type, arrayBuffer: async () => buffer }) as File
  for (const invalid of [
    file(10, 'image/svg+xml', pngHeader(2, 2)),
    file(0, 'image/png', pngHeader(2, 2)),
    file(maxAvatarSourceBytes + 1, 'image/png', pngHeader(2, 2)),
    file(33, 'image/png', pngHeader(5000, 20)),
  ]) {
    await expect(loadAvatar(invalid)).rejects.toThrow()
  }
  expect(decode).not.toHaveBeenCalled()
  await expect(loadAvatar(file(33, 'image/png', pngHeader(2, 2)))).rejects.toThrow('图片无法读取')
})

it('解码结果异常时释放位图，正常图片交给裁剪器管理生命周期', async () => {
  const close = vi.fn()
  const decode = vi.fn().mockResolvedValue({ width: 5000, height: 1, close })
  vi.stubGlobal('createImageBitmap', decode)
  const file = { size: 33, type: 'image/png', arrayBuffer: async () => pngHeader(20, 20) } as File
  await expect(loadAvatar(file)).rejects.toThrow('图片尺寸无效')
  expect(close).toHaveBeenCalledOnce()
  const picture = { width: 20, height: 20, close: vi.fn() }
  decode.mockResolvedValue(picture)
  await expect(loadAvatar(file)).resolves.toBe(picture)
  expect(picture.close).not.toHaveBeenCalled()
})
