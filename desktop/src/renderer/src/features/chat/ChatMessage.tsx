import { Ellipsis, FileText, Reply, Smile } from 'lucide-react'
import {
  useEffect,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
  type MouseEvent as ReactMouseEvent,
} from 'react'
import type { ChatMessage as ChatMessageModel, MessageAttachment } from '../workspace/model'

/** 用户气泡在右侧，助手气泡在左侧；快捷按钮放在朝向会话中央的一侧。 */
export function ChatMessage({
  message,
  speaker,
  isHighlighted,
  onOpenMenu,
  onReply,
  onRetry,
  onReact,
  onReveal,
  onPreview,
}: {
  message: ChatMessageModel
  speaker?: string
  isHighlighted: boolean
  onOpenMenu: (messageId: string, x: number, y: number, trigger: HTMLElement) => void
  onReply: (messageId: string) => void
  onRetry: (messageId: string) => void
  onReact: (messageId: string, emoji: string) => void
  onReveal: (messageId: string) => void
  onPreview?: (attachment: MessageAttachment) => Promise<string>
}) {
  const needsStatus = message.status === 'unsent' || message.status === 'failed'
  const statusLabel = message.status === 'failed' ? '发送失败，点击重试' : '未发送，点击重试'

  function openFromPointer(event: ReactMouseEvent<HTMLElement>): void {
    event.preventDefault()
    onOpenMenu(message.id, event.clientX, event.clientY, event.currentTarget)
  }
  function openFromButton(event: ReactMouseEvent<HTMLButtonElement>): void {
    const bounds = event.currentTarget.getBoundingClientRect()
    onOpenMenu(message.id, bounds.left, bounds.bottom + 6, event.currentTarget)
  }
  function openFromKeyboard(event: ReactKeyboardEvent<HTMLElement>): void {
    if (event.key !== 'ContextMenu' && !(event.shiftKey && event.key === 'F10')) return
    event.preventDefault()
    const bounds = event.currentTarget.getBoundingClientRect()
    onOpenMenu(message.id, bounds.left, bounds.bottom, event.currentTarget)
  }

  const actions = (
    <div className="message-actions">
      <button type="button" aria-label="添加表情" aria-haspopup="menu" onClick={openFromButton}>
        <Smile size={15} />
      </button>
      <button type="button" aria-label="回复" onClick={() => onReply(message.id)}>
        <Reply size={15} />
      </button>
      <button type="button" aria-label="更多操作" aria-haspopup="menu" onClick={openFromButton}>
        <Ellipsis size={15} />
      </button>
    </div>
  )
  const status = needsStatus ? (
    <button
      type="button"
      className="message-status"
      aria-label={statusLabel}
      onClick={() => onRetry(message.id)}
    >
      <span aria-hidden="true">!</span>
    </button>
  ) : null

  return (
    <div className={`message-row ${message.role}-row`}>
      <div
        className={`message-line ${isHighlighted ? 'is-highlighted' : ''}`}
        id={`message-${message.id}`}
        tabIndex={-1}
        onContextMenu={openFromPointer}
        onKeyDown={openFromKeyboard}
      >
        <div className="bubble-row">
          {message.role === 'user' && actions}
          {message.role === 'user' && status}
          <div
            className={`message-bubble ${message.role === 'user' ? 'user-bubble' : 'assistant-bubble'}`}
          >
            {message.replyTo && (
              <button
                type="button"
                className="message-quote"
                aria-label={`查看原消息：${message.replyTo.excerpt}`}
                onClick={() => onReveal(message.replyTo!.messageId)}
              >
                <span aria-hidden="true">{message.replyTo.excerpt}</span>
              </button>
            )}
            {speaker && message.role === 'assistant' ? (
              <span className="message-speaker">{speaker}</span>
            ) : null}
            <div className="message-text">{message.content}</div>
            {message.attachments?.map((attachment) => (
              <MessageFile key={attachment.path} attachment={attachment} onPreview={onPreview} />
            ))}
            {message.reactions.length > 0 && (
              <div className="reaction-pills">
                {message.reactions.map((reaction) => (
                  <button
                    key={reaction.emoji}
                    type="button"
                    className="reaction-pill"
                    aria-pressed="true"
                    aria-label={
                      reaction.count > 1
                        ? `${reaction.emoji} ${reaction.count}，点击取消`
                        : `${reaction.emoji}，点击取消`
                    }
                    onClick={() => onReact(message.id, reaction.emoji)}
                  >
                    <span aria-hidden="true">{reaction.emoji}</span>
                    {reaction.count > 1 && <span>{reaction.count}</span>}
                  </button>
                ))}
              </div>
            )}
          </div>
          {message.role === 'assistant' && status}
          {message.role === 'assistant' && actions}
        </div>
      </div>
    </div>
  )
}

function MessageFile({
  attachment,
  onPreview,
}: {
  attachment: MessageAttachment
  onPreview?: (attachment: MessageAttachment) => Promise<string>
}) {
  const [preview, setPreview] = useState('')
  useEffect(() => {
    if (attachment.kind !== 'image' || !onPreview) return
    let cancelled = false
    void onPreview(attachment).then(
      (url) => {
        if (!cancelled) setPreview(url)
      },
      () => undefined,
    )
    return () => {
      cancelled = true
    }
  }, [attachment, onPreview])
  if (attachment.kind === 'image' && preview) {
    return <img className="message-image" src={preview} alt={attachment.name} />
  }
  return (
    <p className="message-file">
      <FileText size={14} />
      <span>{attachment.name}</span>
    </p>
  )
}
