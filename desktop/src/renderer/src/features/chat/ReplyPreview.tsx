import { Reply, X } from 'lucide-react'

/** 单聊和群聊共享引用条，保留键盘取消入口与可辨认的回复对象。 */
export function ReplyPreview({
  author,
  excerpt,
  onCancel,
}: {
  author: string
  excerpt: string
  onCancel: () => void
}) {
  return (
    <div className="composer-reply" role="region" aria-label={`正在回复${author}`}>
      <span className="reply-symbol" aria-hidden="true">
        <Reply size={16} />
      </span>
      <div className="composer-reply-content">
        <div className="composer-reply-heading">
          <strong>回复</strong>
          <span>{author}</span>
        </div>
        <p>{excerpt}</p>
      </div>
      <button type="button" aria-label="取消回复" onClick={onCancel}>
        <X size={14} />
      </button>
    </div>
  )
}
