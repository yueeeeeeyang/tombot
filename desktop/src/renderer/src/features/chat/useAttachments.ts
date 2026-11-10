import { useRef, useState } from 'react'
import type { MessageAttachment } from '../workspace/model'

/** 串行上传一个附件批次，避免重复粘贴越过数量限制或发送尚未上传完的消息。 */
export function useAttachments(upload: (file: File) => Promise<MessageAttachment>) {
  const [pending, setState] = useState<MessageAttachment[]>([])
  const current = useRef(pending)
  const uploading = useRef(false)
  const [isUploading, setUploading] = useState(false)
  function setPending(
    next: MessageAttachment[] | ((items: MessageAttachment[]) => MessageAttachment[]),
  ): void {
    current.current = typeof next === 'function' ? next(current.current) : next
    setState(current.current)
  }
  async function addFiles(files: File[]): Promise<void> {
    if (!files.length) return
    if (uploading.current) throw new Error('附件正在上传，请稍后再添加。')
    if (files.length + current.current.length > 4) throw new Error('一条消息最多 4 个附件。')
    if (files.reduce((size, file) => size + file.size, 0) > 32 * 1024 * 1024)
      throw new Error('一次添加的附件总大小不能超过 32 MiB。')
    uploading.current = true
    setUploading(true)
    try {
      for (const file of files) {
        const uploaded = await upload(file)
        setPending((items) =>
          items.some((item) => item.path === uploaded.path) ? items : [...items, uploaded],
        )
      }
    } finally {
      uploading.current = false
      setUploading(false)
    }
  }
  return { pending, setPending, addFiles, isUploading, uploading }
}
