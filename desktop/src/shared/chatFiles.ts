/** 聊天文件是服务端快照，路径只标识资源，不能作为本机文件路径使用。 */
export type ChatFile = {
  id: string
  conversation_id: string
  name: string
  media_type: string
  size: number
  sha256: string
  revision: number
  created_at: string
}
export type ChatFilePage = {
  files: ChatFile[]
  next: string
  used_bytes: number
  max_bytes: number
  max_file_bytes: number
}
export type ChatFilePreview = {
  file: ChatFile
  image?: string
  text?: string
  truncated?: boolean
}
export const CHAT_FILE_MAX_BYTES = 16 * 1024 * 1024
export function chatFilePath(file: Pick<ChatFile, 'conversation_id' | 'id'>): string {
  return `chat-files/${file.conversation_id}/${file.id}`
}
export function parseChatFilePath(path: string): { conversationId: string; fileId: string } {
  const match = /^chat-files\/([0-9A-HJKMNP-TV-Z]{26})\/([0-9A-HJKMNP-TV-Z]{26})$/.exec(path)
  if (!match) throw new Error('聊天文件地址无效。')
  return { conversationId: match[1]!, fileId: match[2]! }
}

export function fileSizeLabel(bytes: number): string {
  return bytes < 1024
    ? `${bytes} B`
    : bytes < 1024 * 1024
      ? `${(bytes / 1024).toFixed(1)} KB`
      : `${(bytes / 1024 / 1024).toFixed(1)} MB`
}
