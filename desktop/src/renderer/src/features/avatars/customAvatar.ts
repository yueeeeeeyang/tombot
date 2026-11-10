export const MAX_CUSTOM_AVATAR_LENGTH = 96 * 1024
export const CUSTOM_AVATAR_PATTERN = /^data:image\/png;base64,[A-Za-z0-9+/=]+$/u

/** 上传图片统一裁成小尺寸 PNG，避免把原图和元数据带入工作空间。 */
export async function createCustomAvatar(file: File): Promise<string> {
  if (!['image/png', 'image/jpeg', 'image/webp'].includes(file.type))
    throw new Error('请选择 PNG、JPEG 或 WebP 图片。')
  if (file.size > 4 * 1024 * 1024) throw new Error('头像图片不能超过 4 MiB。')
  let picture: ImageBitmap
  try {
    picture = await createImageBitmap(file)
  } catch {
    throw new Error('无法读取这张图片，请换一张。')
  }
  try {
    if (picture.width > 4096 || picture.height > 4096)
      throw new Error('头像图片长宽不能超过 4096 像素。')
    const canvas = document.createElement('canvas')
    canvas.width = canvas.height = 128
    const context = canvas.getContext('2d')
    if (!context) throw new Error('无法处理头像图片。')
    const side = Math.min(picture.width, picture.height)
    context.drawImage(
      picture,
      (picture.width - side) / 2,
      (picture.height - side) / 2,
      side,
      side,
      0,
      0,
      128,
      128,
    )
    const avatar = canvas.toDataURL('image/png')
    if (avatar.length > MAX_CUSTOM_AVATAR_LENGTH)
      throw new Error('头像处理后仍然过大，请换一张更简单的图片。')
    return avatar
  } finally {
    picture.close()
  }
}
