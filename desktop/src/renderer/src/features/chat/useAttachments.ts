import { useRef, useState } from 'react'
import { CHAT_FILE_MAX_BYTES } from '../../../../shared/chatFiles'
import type { MessageAttachment } from '../workspace/model'

export type DraftFile = { id: number; file: File; attachment?: MessageAttachment }

/** 选择文件只更新本地草稿。发送失败保留文件和已确认的上传结果，重试可继续同一批次。 */
export function useAttachments(upload: (file: File) => Promise<MessageAttachment>) {
  const [draftFiles, setDraftFiles] = useState<DraftFile[]>([])
  const current = useRef<DraftFile[]>([])
  const nextId = useRef(0)
  const sending = useRef(false)
  const [isSending, setSending] = useState(false)

  function update(files: DraftFile[]): void {
    current.current = files
    setDraftFiles(files)
  }
  function removeFile(id: number): void {
    if (!sending.current) update(current.current.filter((item) => item.id !== id))
  }
  async function addFiles(files: File[]): Promise<void> {
    if (!files.length) return
    if (sending.current) throw new Error('消息正在发送，请稍后再添加附件。')
    if (files.length + current.current.length > 4) throw new Error('一条消息最多 4 个附件。')
    if (files.some((file) => file.size > CHAT_FILE_MAX_BYTES))
      throw new Error('单个文件最多 16 MB。')
    if (files.reduce((size, file) => size + file.size, 0) > 32 * 1024 * 1024)
      throw new Error('一次添加的附件总大小不能超过 32 MiB。')
    update([...current.current, ...files.map((file) => ({ id: nextId.current++, file }))])
  }
  async function sendFiles(
    submit: (files: MessageAttachment[]) => boolean | Promise<boolean>,
  ): Promise<boolean> {
    if (sending.current) return false
    sending.current = true
    setSending(true)
    try {
      const attachments: MessageAttachment[] = []
      for (const entry of current.current) {
        const attachment = entry.attachment ?? (await upload(entry.file))
        if (!entry.attachment)
          update(
            current.current.map((item) => (item.id === entry.id ? { ...item, attachment } : item)),
          )
        if (!attachments.some((item) => item.path === attachment.path)) attachments.push(attachment)
      }
      const sent = await submit(attachments)
      if (sent) update([])
      return sent
    } finally {
      sending.current = false
      setSending(false)
    }
  }
  return { draftFiles, removeFile, addFiles, sendFiles, isSending }
}
