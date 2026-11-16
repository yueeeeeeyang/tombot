// @ts-check

export const maxAvatarSourceBytes = 8 * 1024 * 1024
const maxAvatarDimension = 4096

/** 解码前读取 PNG/JPEG 尺寸，拒绝伪装文件和会引起过量内存分配的图片。 */
export function readAvatarDimensions(/** @type {ArrayBuffer} */ buffer) {
  const bytes = new Uint8Array(buffer)
  const view = new DataView(buffer)
  let width = 0
  let height = 0
  if (
    bytes.length >= 33 &&
    [137, 80, 78, 71, 13, 10, 26, 10].every((value, index) => bytes[index] === value) &&
    view.getUint32(8) === 13 &&
    view.getUint32(12) === 0x49484452
  ) {
    width = view.getUint32(16)
    height = view.getUint32(20)
  } else if (bytes[0] === 0xff && bytes[1] === 0xd8) {
    let offset = 2
    while (offset + 4 <= bytes.length) {
      if (bytes[offset++] !== 0xff) break
      while (bytes[offset] === 0xff) offset++
      const marker = bytes[offset++]
      if (marker === 0xd9 || marker === 0xda || offset + 2 > bytes.length) break
      const length = view.getUint16(offset)
      if (length < 2 || offset + length > bytes.length) break
      if ([0xc0, 0xc1, 0xc2].includes(marker ?? 0) && length >= 8) {
        height = view.getUint16(offset + 3)
        width = view.getUint16(offset + 5)
        break
      }
      offset += length
    }
  }
  if (!width || !height) throw new Error('图片格式无效，请选择 PNG 或 JPEG 图片。')
  if (width > maxAvatarDimension || height > maxAvatarDimension) {
    throw new Error('图片宽高不能超过 4096 像素，请缩小后重试。')
  }
  return { width, height }
}

/** 只在浏览器内读取有界原图，调用方在取消、替换或离开页面时释放位图。 */
export async function loadAvatar(/** @type {File} */ file) {
  if (!['image/png', 'image/jpeg'].includes(file.type) || !file.size) {
    throw new Error('请选择 PNG 或 JPEG 图片。')
  }
  if (file.size > maxAvatarSourceBytes) throw new Error('图片不能超过 8 MiB，请缩小后重试。')
  const buffer = await file.arrayBuffer().catch(() => {
    throw new Error('图片读取失败，请重新选择。')
  })
  readAvatarDimensions(buffer)
  /** @type {ImageBitmap} */
  let picture
  try {
    picture = await createImageBitmap(file)
  } catch {
    throw new Error('图片无法读取，请选择完整的 PNG 或 JPEG 图片。')
  }
  if (
    !picture.width ||
    !picture.height ||
    picture.width > maxAvatarDimension ||
    picture.height > maxAvatarDimension
  ) {
    picture.close()
    throw new Error('图片尺寸无效，请重新选择。')
  }
  return picture
}

/** 保持裁剪框在原图内，缩放或拖至边缘时仍填满头像，不出现空白边。 */
export function cropRectangle(
  /** @type {number} */ width,
  /** @type {number} */ height,
  /** @type {number} */ zoom,
  /** @type {number} */ centerX,
  /** @type {number} */ centerY,
) {
  const size = Math.min(width, height) / zoom
  const x = Math.max(0, Math.min(width - size, centerX - size / 2))
  const y = Math.max(0, Math.min(height - size, centerY - size / 2))
  return { x, y, size, centerX: x + size / 2, centerY: y + size / 2 }
}
