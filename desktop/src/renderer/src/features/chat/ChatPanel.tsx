import { ReplyPreview } from './ReplyPreview'
import { useCallback, useEffect, useRef, useState, type FormEvent, type ReactNode } from 'react'
import { ArrowUp, ChevronRight, Mic, Plus } from 'lucide-react'
import { Avatar } from '../../components/Avatar'
import {
  MAX_MESSAGE_LENGTH,
  replyExcerpt,
  type Agent,
  type DmNotice,
  type MessageAttachment,
} from '../workspace/model'
import { WorkStatus } from './WorkStatus'
import { ComposerFiles } from './ComposerFiles'
import { useAttachments } from './useAttachments'
import { pasteFiles } from './pasteFiles'
import { ChatMessage } from './ChatMessage'
import { MessageMenu, type MessageMenuAnchor } from './MessageMenu'
import { messageTimeSeparator } from './messageTime'
import { ScrollToBottom } from './ScrollToBottom'
import { useMessageScroll } from './useMessageScroll'

type ChatPanelProps = {
  agent: Agent
  composerNotice?: ReactNode
  isBlocked: boolean
  workSummary?: string
  pendingMessageIds?: string[]
  hasEarlierMessages?: boolean
  isLoadingEarlier?: boolean
  onLoadEarlier?: () => void
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
  onDownload?: (attachment: MessageAttachment) => Promise<void>
  onPreview?: (attachment: MessageAttachment) => Promise<string>
  peers?: readonly Agent[]
  onOpenDm?: (notice: DmNotice) => void
}

/** 渲染当前助手的消息、工作状态和输入区。 */
export function ChatPanel({
  agent,
  composerNotice,
  isBlocked,
  workSummary,
  pendingMessageIds,
  hasEarlierMessages,
  isLoadingEarlier,
  onLoadEarlier,
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
  onDownload,
  peers = [],
  onOpenDm,
}: ChatPanelProps) {
  const { viewportRef, contentRef, onScroll, showScrollToBottom, scrollToBottom } =
    useMessageScroll(agent.messages)
  const composer = useRef<HTMLTextAreaElement>(null)
  const highlightTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const [isAttachmentOpen, setAttachmentOpen] = useState(false)
  const {
    draftFiles,
    addFiles: stageFiles,
    removeFile,
    sendFiles,
    isSending,
  } = useAttachments(async (file) => {
    if (!onUpload) throw new Error('还不能添加附件。')
    return onUpload(file)
  })
  const fileInput = useRef<HTMLInputElement>(null)
  const [menu, setMenu] = useState<MessageMenuAnchor>()
  const [replyToId, setReplyToId] = useState<string>()
  const [highlightedId, setHighlightedId] = useState('')
  const closeMenu = useCallback(() => setMenu(undefined), [])
  const replyMessage = replyToId
    ? agent.messages.find((message) => message.id === replyToId || message.requestId === replyToId)
    : undefined
  const menuMessage = menu
    ? agent.messages.find((message) => message.id === menu.messageId)
    : undefined
  const now = new Date()

  useEffect(() => () => clearTimeout(highlightTimer.current), [])
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
  async function submitDraft(): Promise<void> {
    if ((!agent.draft.trim() && !draftFiles.length) || isBlocked) return
    try {
      if (await sendFiles((files) => onSave(replyMessage?.id, files))) {
        setReplyToId(undefined)
        composer.current?.focus()
      }
    } catch (failure) {
      onError((failure as Error).message)
    }
  }
  async function addFiles(files: File[]): Promise<void> {
    try {
      await stageFiles(files)
    } catch (failure) {
      onError((failure as Error).message)
    }
  }
  function handleSubmit(event: FormEvent): void {
    event.preventDefault()
    void submitDraft()
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
      <section
        ref={viewportRef}
        onScroll={onScroll}
        className="messages-scroll"
        aria-label={`与${agent.name}的会话`}
      >
        <div ref={contentRef} className="conversation">
          {hasEarlierMessages && (
            <button
              type="button"
              className="text-button"
              disabled={isLoadingEarlier}
              onClick={onLoadEarlier}
            >
              {isLoadingEarlier ? '正在加载…' : '加载更早消息'}
            </button>
          )}
          <div className="saved-messages" role="log" aria-label="消息" aria-live="polite">
            {agent.messages.map((message, index) => {
              const label = messageTimeSeparator(
                message.createdAt,
                agent.messages[index - 1]?.createdAt,
                now,
              )
              return (
                <div className="message-entry" key={message.requestId ?? message.id}>
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
                      isPending={
                        message.status !== 'sent' &&
                        pendingMessageIds?.includes(message.requestId ?? message.id)
                      }
                      isHighlighted={highlightedId === message.id}
                      onOpenMenu={openMenu}
                      onReply={startReply}
                      onRetry={onRetry}
                      onReact={onReact}
                      onReveal={revealMessage}
                      onPreview={onPreview}
                      onDownload={
                        onDownload
                          ? (attachment) => {
                              void onDownload(attachment).catch((failure: Error) =>
                                onError(failure.message),
                              )
                            }
                          : undefined
                      }
                    />
                  )}
                </div>
              )
            })}
          </div>
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
        {showScrollToBottom && <ScrollToBottom onClick={scrollToBottom} />}
        {workSummary !== undefined && <WorkStatus summary={workSummary} agents={[agent]} />}
        {composerNotice}
        {replyMessage && (
          <ReplyPreview
            author={replyMessage.role === 'user' ? '自己' : agent.name}
            excerpt={replyExcerpt(replyMessage.content)}
            onCancel={() => setReplyToId(undefined)}
          />
        )}
        <form
          className={draftFiles.length ? 'composer has-files' : 'composer'}
          onSubmit={handleSubmit}
        >
          <ComposerFiles files={draftFiles} onRemove={removeFile} isSending={isSending} />
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
            disabled={isSending || isBlocked}
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
            readOnly={isSending}
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
                void submitDraft()
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
            disabled={isSending || isBlocked || (!agent.draft.trim() && !draftFiles.length)}
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
