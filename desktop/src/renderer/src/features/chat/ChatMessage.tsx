import { Avatar } from '../../components/Avatar'
import type { Agent } from '../workspace/model'
import { attachmentCaption, MessageFile } from './MessageFile'
import { MarkdownMessage } from './MarkdownMessage'
import { Clock3, Ellipsis, Reply, Smile } from 'lucide-react'
import { type KeyboardEvent as ReactKeyboardEvent, type MouseEvent as ReactMouseEvent } from 'react'
import type { ChatMessage as ChatMessageModel, MessageAttachment } from '../workspace/model'

/** 用户气泡在右侧，助手气泡在左侧；快捷按钮放在朝向会话中央的一侧。 */
export function ChatMessage({
  message,
  speaker,
  speakerAgent,
  isPending = false,
  isHighlighted,
  onOpenMenu,
  onReply,
  onRetry,
  onReact,
  onReveal,
  onPreview,
  onDownload,
}: {
  message: ChatMessageModel
  speaker?: string
  speakerAgent?: Agent
  isPending?: boolean
  isHighlighted: boolean
  onOpenMenu: (messageId: string, x: number, y: number, trigger: HTMLElement) => void
  onReply: (messageId: string) => void
  onRetry: (messageId: string) => void
  onReact: (messageId: string, emoji: string) => void
  onReveal: (messageId: string) => void
  onDownload?: (attachment: MessageAttachment) => void
  onPreview?: (attachment: MessageAttachment) => Promise<string>
}) {
  const caption = attachmentCaption(message.content, message.attachments)
  const attachmentOnly = !caption && Boolean(message.attachments?.length) && !message.replyTo
  const needsStatus = !isPending && (message.status === 'unsent' || message.status === 'failed')
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
  const status = isPending ? (
    <span className="message-status message-pending" role="status" aria-label="发送中">
      <Clock3 size={14} aria-hidden="true" />
    </span>
  ) : needsStatus ? (
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
            className={`message-bubble ${message.role === 'user' ? 'user-bubble' : 'assistant-bubble'}${message.replyTo ? ' has-quote' : ''}${attachmentOnly ? ' attachment-only' : ''}`}
          >
            {message.replyTo && (
              <button
                type="button"
                className="message-quote"
                aria-label={`查看原消息：${message.replyTo.excerpt}`}
                onClick={() => onReveal(message.replyTo!.messageId)}
              >
                <span className="message-quote-heading" aria-hidden="true">
                  <Reply size={12} />
                  引用消息
                </span>
                <span className="message-quote-excerpt" aria-hidden="true">
                  {message.replyTo.excerpt}
                </span>
              </button>
            )}
            {speaker && message.role === 'assistant' ? (
              <div className="message-speaker">
                <Avatar
                  preset={speakerAgent?.avatar ?? 'bombay'}
                  customAvatar={speakerAgent?.customAvatar}
                  agentId={speakerAgent?.id}
                  size="small"
                />
                <span>{speaker}</span>
              </div>
            ) : null}
            {caption &&
              (message.role === 'assistant' ? (
                <MarkdownMessage content={caption} />
              ) : (
                <div className="message-text">{caption}</div>
              ))}
            {message.attachments?.map((attachment) => (
              <MessageFile
                key={`${attachment.path}:${attachment.sha256}`}
                attachment={attachment}
                onPreview={onPreview}
                onDownload={onDownload}
              />
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
