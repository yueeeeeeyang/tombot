/** 只把已知的图片、音频和视频交给原生媒体元素。服务端对部分容器只返回通用类型。 */
const mediaExtensions: Record<string, string> = {
  png: 'image/png',
  jpg: 'image/jpeg',
  jpeg: 'image/jpeg',
  gif: 'image/gif',
  webp: 'image/webp',
  avif: 'image/avif',
  bmp: 'image/bmp',
  mp3: 'audio/mpeg',
  wav: 'audio/wav',
  m4a: 'audio/mp4',
  aac: 'audio/aac',
  oga: 'audio/ogg',
  ogg: 'audio/ogg',
  opus: 'audio/ogg',
  flac: 'audio/flac',
  weba: 'audio/webm',
  mp4: 'video/mp4',
  m4v: 'video/mp4',
  webm: 'video/webm',
  mov: 'video/quicktime',
  ogv: 'video/ogg',
}
const mediaTypes = new Set(Object.values(mediaExtensions))
const aliases: Record<string, string> = {
  'audio/x-wav': 'audio/wav',
  'audio/wave': 'audio/wav',
  'audio/mp3': 'audio/mpeg',
  'audio/x-m4a': 'audio/mp4',
  'audio/x-flac': 'audio/flac',
}

export function attachmentMediaType(name: string, type: string): string | undefined {
  const extension = name.includes('.') ? name.split('.').at(-1)!.toLowerCase() : ''
  const inferred = Object.hasOwn(mediaExtensions, extension)
    ? mediaExtensions[extension]
    : undefined
  const declared = type.split(';')[0]!.trim().toLowerCase()
  if (declared === 'application/ogg') return inferred === 'video/ogg' ? inferred : 'audio/ogg'
  if (declared === 'video/mp4' && inferred === 'audio/mp4') return inferred
  if (declared === 'video/webm' && inferred === 'audio/webm') return inferred
  if (declared === '' || declared === 'application/octet-stream') return inferred
  if (Object.hasOwn(aliases, declared)) return aliases[declared]
  return mediaTypes.has(declared) ? declared : undefined
}

/** 工作区的附件类型只区分图片与普通文件，具体播放器由媒体类型决定。 */
export function attachmentKind(name: string, type: string): 'image' | 'file' {
  return attachmentMediaType(name, type)?.startsWith('image/') ? 'image' : 'file'
}
