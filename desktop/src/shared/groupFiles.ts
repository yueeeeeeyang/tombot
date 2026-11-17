/** 群文件是服务端快照，路径只标识资源，不能作为本机文件路径使用。 */
export type GroupFile = {
  id: string
  group_id: string
  name: string
  media_type: string
  size: number
  sha256: string
  revision: number
  created_at: string
}
export type GroupFilePage = {
  files: GroupFile[]
  next: string
  used_bytes: number
  max_bytes: number
  max_file_bytes: number
}
export type GroupFilePreview = {
  file: GroupFile
  image?: string
  text?: string
  truncated?: boolean
}
export const GROUP_FILE_MAX_BYTES = 16 * 1024 * 1024
export function groupFilePath(file: Pick<GroupFile, 'group_id' | 'id'>): string {
  return `group-files/${file.group_id}/${file.id}`
}
export function parseGroupFilePath(path: string): { groupId: string; fileId: string } {
  const match = /^group-files\/([0-9A-HJKMNP-TV-Z]{26})\/([0-9A-HJKMNP-TV-Z]{26})$/.exec(path)
  if (!match) throw new Error('群文件地址无效。')
  return { groupId: match[1]!, fileId: match[2]! }
}
