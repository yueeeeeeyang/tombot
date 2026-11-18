import { useCallback, useState } from 'react'
import { X } from 'lucide-react'
import { attachmentMediaType } from '../../../../shared/attachmentMedia'
import { fileSizeLabel } from '../../../../shared/chatFiles'
import { AttachmentIcon } from './AttachmentIcon'
import type { DraftFile } from './useAttachments'

function DraftCard({
  item,
  onRemove,
  isSending,
}: {
  item: DraftFile
  onRemove: () => void
  isSending: boolean
}) {
  const { file } = item
  const isImage = attachmentMediaType(file.name, file.type)?.startsWith('image/') === true
  const [unreadable, setUnreadable] = useState(false)
  const showThumbnail = useCallback(
    (node: HTMLImageElement | null) => {
      if (!node) return
      const url = URL.createObjectURL(file)
      node.src = url
      return () => URL.revokeObjectURL(url)
    },
    [file],
  )
  const showImage = isImage && !unreadable
  return (
    <li
      className={showImage ? 'draft-file draft-photo' : 'draft-file'}
      title={file.name}
      aria-busy={isSending && !item.attachment}
    >
      {showImage ? (
        <img ref={showThumbnail} alt={file.name} onError={() => setUnreadable(true)} />
      ) : (
        <>
          <span className="draft-file-icon">
            <AttachmentIcon name={file.name} mediaType={file.type} size={25} />
          </span>
          <span className="draft-file-details">
            <strong>{file.name}</strong>
            <small>{fileSizeLabel(file.size)}</small>
          </span>
        </>
      )}
      {isSending && !item.attachment && (
        <span className="draft-upload" role="status" aria-label="正在上传" />
      )}
      <button
        type="button"
        disabled={isSending}
        className="draft-remove"
        aria-label={`移除${file.name}`}
        onClick={onRemove}
      >
        <X size={12} />
      </button>
    </li>
  )
}

/** 将粘贴、拖入和选择的附件显示在同一条可横向滚动的草稿栏里。 */
export function ComposerFiles({
  files,
  onRemove,
  isSending,
}: {
  isSending: boolean
  files: DraftFile[]
  onRemove: (id: number) => void
}) {
  if (!files.length) return null
  return (
    <ul className="composer-files" aria-label="待发送附件">
      {files.map((item) => (
        <DraftCard
          isSending={isSending}
          key={item.id}
          item={item}
          onRemove={() => onRemove(item.id)}
        />
      ))}
    </ul>
  )
}
