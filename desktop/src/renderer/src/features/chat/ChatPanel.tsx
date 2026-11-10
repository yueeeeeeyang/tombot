import { useCallback, useEffect, useRef, useState, type FormEvent } from 'react'
import { ArrowUp, AudioLines, ChevronRight, Mic, Plus, X } from 'lucide-react'
import { Avatar } from '../../components/Avatar'
import {
  MAX_MESSAGE_LENGTH,
  replyExcerpt,
  type Agent,
  type DmNotice,
  type MessageAttachment,
} from '../workspace/model'
import { ChatMessage } from './ChatMessage'
import { MessageMenu, type MessageMenuAnchor } from './MessageMenu'
import { messageTimeSeparator } from './messageTime'

type ChatPanelProps = {
  agent: Agent
  isBlocked: boolean
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

/** 渲染当前助手的使用指南、消息和输入区；不生成或伪造模型回复。 */
export function ChatPanel({
  agent,
  isBlocked,
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
  const [pendingAttachments, setPendingAttachments] = useState<MessageAttachment[]>([])
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
    if ((!agent.draft.trim() && pendingAttachments.length === 0) || isBlocked) return
    if (onSave(replyMessage?.id, pendingAttachments)) {
      setPendingAttachments([])
      setReplyToId(undefined)
      composer.current?.focus()
    }
  }
  async function addFiles(files: File[]): Promise<void> {
    if (!onUpload) {
      onError('还不能添加附件。')
      return
    }
    for (const file of files) {
      if (pendingAttachments.length >= 4) {
        onError('一条消息最多 4 个附件。')
        return
      }
      try {
        const uploaded = await onUpload(file)
        setPendingAttachments((current) =>
          current.length >= 4 || current.some((item) => item.path === uploaded.path)
            ? current
            : [...current, uploaded],
        )
      } catch (failure) {
        onError((failure as Error).message)
      }
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
          <Avatar preset={agent.avatar} agentId={agent.id} size="small" />
          <span>{agent.name}</span>
          <ChevronRight className="pill-chevron" size={14} aria-hidden="true" />
        </button>
      </header>
      <section className="messages-scroll" aria-label={`与${agent.name}的会话`}>
        <div className="conversation">
          <div className="message-row assistant-row">
            <div className="message-bubble assistant-bubble greeting">
              <p>
                嗨，我是{agent.name}
                <span className="greeting-wave"> ✦</span>
              </p>
              <p>
                {agent.id === 'assistant'
                  ? '很高兴见到你。想做点什么？一个灵感、一项计划，或者一个还没想明白的问题，都可以从这里开始。'
                  : agent.description}
              </p>
            </div>
          </div>
          <div className="message-row assistant-row">
            <div className="message-bubble assistant-bubble guide-bubble">
              <p className="guide-heading">你的想法，有了一个新家。</p>
              <p>把不同的事情，交给不同的搭档。每个助手都有独立的会话，让思路自然延续。</p>
              <ul className="guide-list">
                <li>
                  <span className="guide-dot guide-dot--orange" />
                  <div>
                    <strong>一起创造</strong>
                    <p>从策划、程序到美术和音效，让好点子逐渐成形。</p>
                  </div>
                </li>
                <li>
                  <span className="guide-dot guide-dot--blue" />
                  <div>
                    <strong>专注当下</strong>
                    <p>写代码、整理资讯，或是理清手边的工作。</p>
                  </div>
                </li>
                <li>
                  <span className="guide-dot guide-dot--green" />
                  <div>
                    <strong>按你的方式来</strong>
                    <p>点击侧栏的 ＋，创建属于你的专属助手。</p>
                  </div>
                </li>
              </ul>
              <div className="local-note">
                <span className="local-note-dot" />
                消息会保存到服务。配置模型后，助手才会回复。
              </div>
            </div>
          </div>
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
          {agent.draft.trim() || pendingAttachments.length > 0 ? (
            <button
              className="composer-send"
              type="submit"
              disabled={isBlocked}
              aria-label="保存为本地草稿"
              title="保存为本地草稿"
            >
              <ArrowUp size={18} />
            </button>
          ) : (
            <button
              className="composer-voice"
              type="button"
              disabled
              aria-label="语音对话（等待服务端接入）"
              title="语音对话将在服务端接入后可用"
            >
              <AudioLines size={18} />
            </button>
          )}
        </form>
      </div>
    </main>
  )
}
