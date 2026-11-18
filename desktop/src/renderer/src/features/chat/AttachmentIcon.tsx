import {
  File,
  FileArchive,
  FileCode,
  FileImage,
  FileMusic,
  FileSpreadsheet,
  FileText,
  FileVideo,
} from 'lucide-react'
import type { ChatFile } from '../../../../shared/chatFiles'

const fileIcons = {
  image: FileImage,
  audio: FileMusic,
  video: FileVideo,
  archive: FileArchive,
  spreadsheet: FileSpreadsheet,
  code: FileCode,
  text: FileText,
  file: File,
}

/** 文件按类型共用 Lucide 线性图标，颜色随主题变化，不依赖远程资源。 */
export function fileIconKind(file: Pick<ChatFile, 'name' | 'media_type'>) {
  const extension = file.name.split('.').at(-1)?.toLowerCase() ?? ''
  if (file.media_type.startsWith('image/')) return 'image' as const
  if (file.media_type.startsWith('audio/')) return 'audio' as const
  if (file.media_type.startsWith('video/')) return 'video' as const
  if (['zip', 'jar', 'tar', 'gz', '7z', 'rar'].includes(extension)) return 'archive' as const
  if (['csv', 'tsv', 'xls', 'xlsx', 'ods'].includes(extension)) return 'spreadsheet' as const
  if (
    ['js', 'ts', 'tsx', 'jsx', 'py', 'go', 'java', 'json', 'html', 'css', 'sql', 'sh'].includes(
      extension,
    )
  )
    return 'code' as const
  if (file.media_type.startsWith('text/') || ['pdf', 'doc', 'docx', 'md'].includes(extension))
    return 'text' as const
  return 'file' as const
}

export function AttachmentIcon({
  name,
  mediaType,
  size = 20,
}: {
  name: string
  mediaType: string
  size?: number
}) {
  const Icon = fileIcons[fileIconKind({ name, media_type: mediaType })]
  return <Icon size={size} strokeWidth={1.5} aria-hidden="true" />
}
