import { useCallback, useEffect, useRef, useState, type FormEvent } from 'react'
import { ArrowUp, ChevronRight, Mic, Plus, X } from 'lucide-react'
import { Avatar } from '../../components/Avatar'
import {
  MAX_MESSAGE_LENGTH,
  replyExcerpt,
  type Agent,
  type DmNotice,
  type MessageAttachment,
} from '../workspace/model'
import { WorkStatus } from './WorkStatus'
import { useAttachments } from './useAttachments'
import { pasteFiles } from './pasteFiles'
import { ChatMessage } from './ChatMessage'
import { MessageMenu, type MessageMenuAnchor } from './MessageMenu'
import { messageTimeSeparator } from './messageTime'

type ChatPanelProps = {
  agent: Agent
  isBlocked: boolean
  workSummary?: string
  pendingMessageId?: string
  onDraft: (draft: string) => void
  onSave: (replyToId?: string, attachments?: MessageAttachment[]) => boolean
  onDelete: (messageId: string) => void
  onReact: (messageId: string, emoji: string) => void
  onRetry: (messageId: string) => void
  onDetails: () => void
  isDetailsOpen: boolean
  onNotice: (message: string) => void
  onError: (message: string) => void
  onUpload?: (file: File) => Promise<MessageAttachment>
  onPreview?: (attachment: MessageAttachment) => Promise<string>
  peers?: readonly Agent[]
  onOpenDm?: (notice: DmNotice) => void
}

/** 渲染当前助手的消息、工作状态和输入区。 */
export function ChatPanel({
  agent,
  isBlocked,
  workSummary,
  pendingMessageId,
  onDraft,
  onSave,
  onDelete,
  onReact,
  onRetry,
  onDetails,
  isDetailsOpen,
  onNotice,
  onError,
  onUpload,
  onPreview,
  peers = [],
  onOpenDm,
}: ChatPanelProps) {
  const messagesEnd = useRef<HTMLDivElement>(null)
  const composer = useRef<HTMLTextAreaElement>(null)
  const highlightTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const [isAttachmentOpen, setAttachmentOpen] = useState(false)
  const {
    pending: pendingAttachments,
    setPending: setPendingAttachments,
    addFiles: uploadFiles,
    isUploading,
    uploading,
  } = useAttachments(async (file) => {
    if (!onUpload) throw new Error('还不能添加附件。')
    return onUpload(file)
  })
  const fileInput = useRef<HTMLInputElement>(null)
  const [menu, setMenu] = useState<MessageMenuAnchor>()
  const [replyToId, setReplyToId] = useState<string>()
  const [highlightedId, setHighlightedId] = useState('')
  const closeMenu = useCallback(() => setMenu(undefined), [])
  const replyMessage = agent.messages.find((message) => message.id === replyToId)
  const menuMessage = menu
    ? agent.messages.find((message) => message.id === menu.messageId)
    : undefined
  const now = new Date()

  useEffect(() => () => clearTimeout(highlightTimer.current), [])
  useEffect(() => {
    messagesEnd.current?.scrollIntoView({ behavior: 'instant', block: 'end' })
  }, [agent.id, agent.messages.length])
  useEffect(() => {
    if (composer.current) {
      composer.current.style.height = 'auto'
      composer.current.style.height = `${composer.current.scrollHeight}px`
    }
  }, [agent.draft])

  function openMenu(messageId: string, x: number, y: number, trigger: HTMLElement): void {
    setMenu({ messageId, x, y, trigger })
  }
  function startReply(messageId: string): void {
    setReplyToId(messageId)
    closeMenu()
    composer.current?.focus()
  }
  function revealMessage(messageId: string): void {
    const element = document.getElementById(`message-${messageId}`)
    if (!element) {
      onError('原消息已不在当前会话中。')
      return
    }
    const reduceMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches
    element.scrollIntoView({ behavior: reduceMotion ? 'auto' : 'smooth', block: 'center' })
    setHighlightedId(messageId)
    clearTimeout(highlightTimer.current)
    highlightTimer.current = setTimeout(() => setHighlightedId(''), 1600)
  }
  async function copyText(value: string, success: string): Promise<void> {
    try {
      await navigator.clipboard.writeText(value)
      onNotice(success)
    } catch {
      onError('复制失败，请检查剪贴板权限后重试。')
    }
  }
  function submitDraft(): void {
    if (
      (!agent.draft.trim() && pendingAttachments.length === 0) ||
      uploading.current ||
      isBlocked ||
      workSummary !== undefined ||
      pendingMessageId !== undefined
    )
      return
    if (onSave(replyMessage?.id, pendingAttachments)) {
      setPendingAttachments([])
      setReplyToId(undefined)
      composer.current?.focus()
    }
  }
  async function addFiles(files: File[]): Promise<void> {
    try {
      await uploadFiles(files)
    } catch (failure) {
      onError((failure as Error).message)
    }
  }
  function handleSubmit(event: FormEvent): void {
    event.preventDefault()
    submitDraft()
  }

  return (
    <main className="chat-panel">
      <header className="chat-header">
        <button
          className="chat-agent-pill"
          onClick={onDetails}
          aria-expanded={isDetailsOpen}
          aria-label={`查看${agent.name}资料`}
        >
          <Avatar
            preset={agent.avatar}
            customAvatar={agent.customAvatar}
            agentId={agent.id}
            size="small"
          />
          <span>{agent.name}</span>
          <ChevronRight className="pill-chevron" size={14} aria-hidden="true" />
        </button>
      </header>
      <section className="messages-scroll" aria-label={`与${agent.name}的会话`}>
        <div className="conversation">
          <div className="saved-messages" role="log" aria-label="消息" aria-live="polite">
            {agent.messages.map((message, index) => {
              const label = messageTimeSeparator(
                message.createdAt,
                agent.messages[index - 1]?.createdAt,
                now,
              )
              return (
                <div className="message-entry" key={message.id}>
                  {label && (
                    <div className="time-separator">
                      <time dateTime={message.createdAt}>{label}</time>
                    </div>
                  )}
                  {message.dmNotice ? (
                    <button
                      type="button"
                      className="dm-notice"
                      onClick={() => onOpenDm?.(message.dmNotice!)}
                    >
                      <span>
                        {message.dmNotice.direction === 'received' ? '收到' : '已发消息给'}
                      </span>
                      <Avatar
                        customAvatar={
                          peers.find((peer) => peer.id === message.dmNotice?.peerId)?.customAvatar
                        }
                        preset={
                          peers.find((peer) => peer.id === message.dmNotice?.peerId)?.avatar ??
                          'bombay'
                        }
                        agentId={message.dmNotice.peerId}
                        size="small"
                      />
                      <span>
                        {message.dmNotice.peerName}
                        {message.dmNotice.direction === 'received' ? ' 的消息' : ''}
                      </span>
                    </button>
                  ) : (
                    <ChatMessage
                      message={message}
                      isPending={message.id === pendingMessageId}
                      isHighlighted={highlightedId === message.id}
                      onOpenMenu={openMenu}
                      onReply={startReply}
                      onRetry={onRetry}
                      onReact={onReact}
                      onReveal={revealMessage}
                      onPreview={onPreview}
                    />
                  )}
                </div>
              )
            })}
          </div>
          <div ref={messagesEnd} />
        </div>
      </section>
      {menu && menuMessage && (
        <MessageMenu
          anchor={menu}
          message={menuMessage}
          onReact={(emoji) => onReact(menuMessage.id, emoji)}
          onReply={() => startReply(menuMessage.id)}
          onCopy={() => void copyText(menuMessage.content, '已复制')}
          onCopyRequestId={() => void copyText(menuMessage.requestId ?? '', '已复制请求 ID')}
          onRetry={() => onRetry(menuMessage.id)}
          onDelete={() => onDelete(menuMessage.id)}
          onClose={closeMenu}
        />
      )}
      <div
        className={`composer-area ${isAttachmentOpen ? 'is-dropping' : ''}`}
        onPaste={(event) => pasteFiles(event, addFiles, onError)}
        onDragOver={(event) => {
          event.preventDefault()
          setAttachmentOpen(true)
        }}
        onDragLeave={() => setAttachmentOpen(false)}
        onDrop={(event) => {
          event.preventDefault()
          setAttachmentOpen(false)
          void addFiles([...event.dataTransfer.files])
        }}
      >
        {workSummary !== undefined && <WorkStatus summary={workSummary} agents={[agent]} />}
        {pendingAttachments.length > 0 && (
          <div className="composer-files">
            {pendingAttachments.map((item) => (
              <span key={item.path}>
                {item.name}
                <button
                  type="button"
                  aria-label={`移除${item.name}`}
                  onClick={() =>
                    setPendingAttachments((current) =>
                      current.filter((candidate) => candidate.path !== item.path),
                    )
                  }
                >
                  <X size={12} />
                </button>
              </span>
            ))}
          </div>
        )}
        {replyMessage && (
          <div className="composer-reply">
            <div>
              <strong>回复</strong>
              <p>{replyExcerpt(replyMessage.content)}</p>
            </div>
            <button type="button" aria-label="取消回复" onClick={() => setReplyToId(undefined)}>
              <X size={14} />
            </button>
          </div>
        )}
        <form className="composer" onSubmit={handleSubmit}>
          <input
            ref={fileInput}
            type="file"
            hidden
            multiple
            onChange={(event) => {
              void addFiles([...(event.target.files ?? [])])
              event.target.value = ''
            }}
          />
          <button
            className={`composer-add ${isAttachmentOpen ? 'is-open' : ''}`}
            type="button"
            aria-label="添加附件"
            onClick={() => fileInput.current?.click()}
          >
            <Plus size={20} strokeWidth={1.7} />
          </button>
          <textarea
            ref={composer}
            value={agent.draft}
            rows={1}
            maxLength={MAX_MESSAGE_LENGTH}
            disabled={isBlocked}
            aria-label={`给${agent.name}发消息`}
            placeholder={`给 ${agent.name} 发消息`}
            onChange={(event) => onDraft(event.target.value)}
            onKeyDown={(event) => {
              if (
                event.key === 'Enter' &&
                !event.shiftKey &&
                !event.nativeEvent.isComposing &&
                event.keyCode !== 229
              ) {
                event.preventDefault()
                submitDraft()
              }
            }}
          />
          <button
            className="composer-mic"
            type="button"
            disabled
            aria-label="语音输入（等待服务端接入）"
            title="语音输入将在服务端接入后可用"
          >
            <Mic size={18} strokeWidth={1.8} />
          </button>
          <button
            className="composer-send"
            type="submit"
            disabled={
              isUploading ||
              isBlocked ||
              workSummary !== undefined ||
              pendingMessageId !== undefined ||
              (!agent.draft.trim() && pendingAttachments.length === 0)
            }
            aria-label="发送"
            title="发送"
          >
            <ArrowUp size={18} />
          </button>
        </form>
      </div>
    </main>
  )
}
