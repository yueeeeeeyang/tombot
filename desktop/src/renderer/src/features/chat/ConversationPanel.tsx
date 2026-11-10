import { GroupAvatar } from '../../components/GroupAvatar'
import { GroupComposer } from './GroupComposer'
import { useCallback, useEffect, useRef, useState, type FormEvent } from 'react'
import { Avatar } from '../../components/Avatar'
import { Dialog } from '../../components/Dialog'
import { ArrowLeftRight, ArrowUp, Check, ChevronRight, Plus, Users, X } from 'lucide-react'
import type { CatAvatarId } from '../avatars/presets'
import type { ServerBubble, IncomingMessages, StreamEvent } from '../desktop/backend'
import { getBackend } from '../desktop/backend'
import type { Agent, DmNotice } from '../workspace/model'
import { replyExcerpt } from '../workspace/model'
import { GroupSidePanel } from './GroupSidePanel'
import type { GroupDetail } from '../desktop/backend'
import { WorkStatus } from './WorkStatus'
import type { WorkStates } from './useWorkActivity'
import { useAttachments } from './useAttachments'
import { pasteFiles } from './pasteFiles'
import { MarkdownMessage } from './MarkdownMessage'
import { ChatMessage } from './ChatMessage'
import { MessageMenu, type MessageMenuAnchor } from './MessageMenu'
import { messageTimeSeparator } from './messageTime'

/** 群聊复用原来的气泡，可以发言和管理成员。猫咪之间的私信不在这里打开。 */
export function ConversationPanel({
  id,
  agents,
  onNotice,
  onError,
  onGroupChange,
  onIncoming,
  workStates,
  onExecutionEvent,
  onExecutionEnd,
}: {
  id: string
  workStates: WorkStates
  onExecutionEvent: (source: string, event: StreamEvent) => void
  onExecutionEnd: (source: string) => void
  agents: Agent[]
  onNotice: (message: string) => void
  onError: (message: string) => void
  onIncoming?: (incoming: IncomingMessages) => void
  onGroupChange: (group: GroupDetail) => void
}) {
  const [title, setTitle] = useState('群聊')
  const [conversationId, setConversationId] = useState('')
  const [group, setGroup] = useState<GroupDetail | null>(null)
  const [messages, setMessages] = useState<ServerBubble[]>([])
  const [draft, setDraft] = useState('')
  const [mentions, setMentions] = useState<string[]>([])
  const { pending, setPending, addFiles, isUploading, uploading } = useAttachments(async (file) => {
    const upload = getBackend().uploadAttachment
    if (!upload) throw new Error('还不能添加附件。')
    const uploaded = await upload(
      { name: file.name, type: file.type, bytes: new Uint8Array(await file.arrayBuffer()) },
      { shared: true },
    )
    return {
      name: file.name,
      path: uploaded.path,
      sha256: uploaded.sha256,
      mediaType: file.type,
      kind: file.type === 'image/png' || file.type === 'image/jpeg' ? 'image' : 'file',
    }
  })
  const [replyToId, setReplyToId] = useState<string>()
  const [live, setLive] = useState<ServerBubble[]>([])
  const [menu, setMenu] = useState<MessageMenuAnchor>()
  const [isDetailsOpen, setDetailsOpen] = useState(false)
  const log = useRef<HTMLElement>(null)
  const [busy, setBusy] = useState(false)
  const workingAgents = agents.filter(
    (agent) => workStates[agent.id] && group?.members.some((member) => member.bot_id === agent.id),
  )
  const names = new Map<string, string>()
  for (const agent of agents) names.set(agent.id, agent.name)
  for (const member of group?.members ?? []) names.set(member.bot_id, member.name)

  const reload = useCallback(async () => {
    const backend = getBackend()
    const detail = await backend.groupDetail?.(id)
    if (!detail) return
    setGroup(detail)
    setTitle(detail.name)
    setConversationId(detail.conversation_id)
    setMessages((await backend.listConversationMessages?.(detail.conversation_id)) ?? [])
  }, [id])

  useEffect(() => {
    let cancelled = false
    const backend = window.desktop?.backend
    const failed = (failure: Error) => {
      if (!cancelled) onError(failure.message)
    }
    void backend
      ?.groupDetail?.(id)
      .then((detail) => {
        if (cancelled || !detail) return []
        setGroup(detail)
        setTitle(detail.name)
        setConversationId(detail.conversation_id)
        return backend.listConversationMessages?.(detail.conversation_id) ?? []
      })
      .then((rows) => {
        if (!cancelled && rows) setMessages(rows)
      })
      .catch(failed)
    return () => {
      cancelled = true
    }
  }, [id, onError])

  useEffect(() => {
    const element = log.current
    if (element) element.scrollTop = element.scrollHeight
  }, [messages, live])

  function bubbleMessage(item: ServerBubble) {
    const reply = item.replyToId
      ? messages.find((message) => message.id === item.replyToId)
      : undefined
    return {
      id: item.id,
      role: item.role,
      content: item.content,
      createdAt: item.createdAt,
      status: 'sent' as const,
      reactions: item.reactions.flatMap((reaction) =>
        reaction.emoji ? [{ emoji: reaction.emoji as never, count: reaction.count }] : [],
      ),
      attachments: item.attachments,
      ...(reply ? { replyTo: { messageId: reply.id, excerpt: replyExcerpt(reply.content) } } : {}),
    }
  }

  async function send(): Promise<void> {
    const content = draft.trim() || pending.map((item) => item.name).join('、')
    if (!content || !conversationId || busy || uploading.current) return
    setBusy(true)
    const source = crypto.randomUUID()
    setLive([])
    let streamFailure: Error | undefined
    try {
      await getBackend().sendConversation?.(
        conversationId,
        content,
        pending.map((item) => ({ path: item.path, sha256: item.sha256 })),
        replyToId,
        mentions,
        (event) => {
          if (event.type === 'incoming' && event.incoming) onIncoming?.(event.incoming)
          onExecutionEvent(source, event)
          if (event.type === 'delta') {
            if (!event.messageId || !event.botId) {
              streamFailure = new Error('群消息缺少发言成员信息。')
              return
            }
            setLive((current) => {
              const existing = current.find((item) => item.id === event.messageId)
              if (existing)
                return current.map((item) =>
                  item.id === event.messageId
                    ? { ...item, content: item.content + event.text }
                    : item,
                )
              return [
                ...current,
                {
                  id: event.messageId!,
                  role: 'assistant',
                  speaker: event.botId!,
                  content: event.text,
                  createdAt: new Date().toISOString(),
                  reactions: [],
                  attachments: [],
                },
              ]
            })
          }
        },
      )
      if (streamFailure) throw streamFailure
      setDraft('')
      setMentions([])
      setPending([])
      setReplyToId(undefined)
      setLive([])
      await reload()
    } catch (failure) {
      onError((failure as Error).message)
    } finally {
      onExecutionEnd(source)
      setBusy(false)
    }
  }

  const reply = messages.find((message) => message.id === replyToId)
  const shown = [
    ...messages,
    ...live.filter((item) => !messages.some((saved) => saved.id === item.id)),
  ]
  const menuMessage = menu ? messages.find((message) => message.id === menu.messageId) : undefined

  return (
    <>
      <main className="chat-panel">
        <header className="chat-header">
          <button
            className="chat-agent-pill"
            aria-label={`查看${title}群资料`}
            aria-expanded={isDetailsOpen}
            onClick={() => setDetailsOpen((open) => !open)}
            disabled={!group}
          >
            {group && <GroupAvatar presets={group.avatar_presets} size="small" />}
            <span>{title}</span>
            <ChevronRight className="pill-chevron" size={14} aria-hidden="true" />
          </button>
        </header>
        <section ref={log} className="messages-scroll" aria-label={`与${title}的会话`}>
          <div className="conversation">
            <div className="saved-messages" role="log" aria-label={title} aria-live="polite">
              {shown.length === 0 ? (
                <p className="local-note">在下面说一句，群里的猫咪会一起回应。</p>
              ) : null}
              {shown.map((item, index) => {
                const label = messageTimeSeparator(
                  item.createdAt,
                  shown[index - 1]?.createdAt,
                  new Date(),
                )
                return (
                  <div className="message-entry" key={item.id}>
                    {label ? (
                      <div className="time-separator">
                        <time dateTime={item.createdAt}>{label}</time>
                      </div>
                    ) : null}
                    <ChatMessage
                      message={bubbleMessage(item)}
                      speaker={
                        item.role === 'assistant'
                          ? names.get(item.speaker) || item.speaker
                          : undefined
                      }
                      isHighlighted={false}
                      onOpenMenu={(messageId, x, y, trigger) =>
                        setMenu({ messageId, x, y, trigger })
                      }
                      onReply={setReplyToId}
                      onRetry={() => undefined}
                      onReact={(messageId, emoji) => {
                        void getBackend()
                          .toggleReaction?.(messageId, emoji)
                          .then(reload)
                          .catch((failure: Error) => onError(failure.message))
                      }}
                      onReveal={() => undefined}
                      onPreview={(attachment) =>
                        getBackend().previewAttachment?.(attachment.path) ?? Promise.resolve('')
                      }
                    />
                  </div>
                )
              })}
            </div>
          </div>
        </section>
        {menu && menuMessage && (
          <MessageMenu
            anchor={menu}
            message={bubbleMessage(menuMessage)}
            onReact={(emoji) => {
              void getBackend().toggleReaction?.(menuMessage.id, emoji).then(reload)
            }}
            onReply={() => setReplyToId(menuMessage.id)}
            onCopy={() =>
              void navigator.clipboard.writeText(menuMessage.content).then(() => onNotice('已复制'))
            }
            onCopyRequestId={() => undefined}
            onRetry={() => undefined}
            onDelete={() => onNotice('群里的消息由服务端保留。')}
            onClose={() => setMenu(undefined)}
          />
        )}
        <div
          className="composer-area"
          onPaste={(event) => pasteFiles(event, addFiles, onError)}
          onDragOver={(event) => event.preventDefault()}
          onDrop={(event) => {
            event.preventDefault()
            void addFiles([...event.dataTransfer.files]).catch((failure: Error) =>
              onError(failure.message),
            )
          }}
        >
          {workingAgents.length > 0 && (
            <WorkStatus
              summary={workStates[workingAgents[0]!.id]!.summary}
              agents={workingAgents}
            />
          )}
          {reply && (
            <div className="composer-reply">
              <div>
                <strong>回复</strong>
                <p>{replyExcerpt(reply.content)}</p>
              </div>
              <button type="button" aria-label="取消回复" onClick={() => setReplyToId(undefined)}>
                <X size={14} />
              </button>
            </div>
          )}
          {pending.length > 0 && (
            <div className="composer-files">
              {pending.map((item) => (
                <span key={item.path}>{item.name}</span>
              ))}
            </div>
          )}
          <form
            className="composer"
            onSubmit={(event) => {
              event.preventDefault()
              void send()
            }}
          >
            <input
              type="file"
              hidden
              id="group-file"
              onChange={(event) => {
                void addFiles([...(event.target.files ?? [])]).catch((failure: Error) =>
                  onError(failure.message),
                )
                event.target.value = ''
              }}
            />
            <button
              className="composer-add"
              type="button"
              aria-label="添加附件"
              onClick={() => document.getElementById('group-file')?.click()}
            >
              <Plus size={20} />
            </button>
            <GroupComposer
              title={title}
              draft={draft}
              members={agents.filter((agent) =>
                group?.members.some((member) => member.bot_id === agent.id),
              )}
              onChange={(value, ids) => {
                setDraft(value)
                setMentions(ids)
              }}
              onSend={() => void send()}
            />
            <button
              className="composer-send"
              type="submit"
              disabled={isUploading || busy || (!draft.trim() && pending.length === 0)}
              aria-label="发送"
            >
              <ArrowUp size={20} aria-hidden="true" />
            </button>
          </form>
        </div>
      </main>
      {isDetailsOpen && group && (
        <GroupSidePanel
          group={group}
          agents={agents}
          onClose={() => setDetailsOpen(false)}
          onChange={(next) => {
            setGroup(next)
            setTitle(next.name)
            onGroupChange(next)
          }}
        />
      )}
    </>
  )
}

/** 用靠左的浅色气泡只读查看两只猫咪的私信。这里不能代发，也不能回复。 */
export function DmThreadDialog({
  notice,
  self,
  agents,
  onClose,
  onError,
}: {
  notice: DmNotice
  self: Agent
  agents: Agent[]
  onClose: () => void
  onError: (message: string) => void
}) {
  const [lines, setLines] = useState<ServerBubble[] | null>(null)
  useEffect(() => {
    let cancelled = false
    let timer: ReturnType<typeof setTimeout> | undefined
    async function refresh(): Promise<void> {
      try {
        const rows = await getBackend().listConversationMessages?.(notice.conversationId)
        if (cancelled) return
        setLines(rows ?? [])
        timer = setTimeout(() => void refresh(), 1500)
      } catch (failure) {
        if (!cancelled) onError((failure as Error).message)
      }
    }
    void refresh()
    return () => {
      cancelled = true
      clearTimeout(timer)
    }
  }, [notice.conversationId, onError])
  const peer = agents.find((agent) => agent.id === notice.peerId)
  const peerName = peer?.name || notice.peerName
  const title = `${self.name} ↔ ${peerName}`
  const ordered = lines ?? []
  return (
    <Dialog title={title} onClose={onClose} variant="dm">
      <div className="dm-modal">
        <div className="dm-thread" role="log" aria-label={title}>
          <div className="dm-thread-inner">
            {lines !== null && ordered.length === 0 ? (
              <p className="dm-thread-empty">这段私信还没有文字。</p>
            ) : null}
            {ordered.map((line) => {
              const speaker = agents.find((agent) => agent.id === line.speaker)
              const mine = line.speaker === self.id
              const name = mine ? self.name : (speaker?.name ?? peerName)
              const avatar: CatAvatarId = mine
                ? self.avatar
                : (speaker?.avatar ?? peer?.avatar ?? 'bombay')
              return (
                <div className="dm-line" key={line.id}>
                  <Avatar
                    preset={avatar}
                    customAvatar={
                      mine ? self.customAvatar : (speaker?.customAvatar ?? peer?.customAvatar)
                    }
                    agentId={mine ? self.id : line.speaker || notice.peerId}
                    size="small"
                  />
                  <span className={`dm-name ${mine ? 'dm-name--self' : 'dm-name--peer'}`}>
                    {name}
                  </span>
                  <div className="dm-bubble">
                    <MarkdownMessage content={line.content} />
                  </div>
                </div>
              )
            })}
          </div>
        </div>
        <div className="dm-top">
          <div className="dm-capsule">
            <Avatar
              preset={self.avatar}
              customAvatar={self.customAvatar}
              agentId={self.id}
              size="small"
            />
            <span>{self.name}</span>
            <ArrowLeftRight size={15} aria-hidden="true" />
            <Avatar
              preset={peer?.avatar ?? 'bombay'}
              customAvatar={peer?.customAvatar}
              agentId={notice.peerId}
              size="small"
            />
            <span>{peerName}</span>
          </div>
        </div>
        <button
          type="button"
          className="dm-close icon-button"
          aria-label="关闭弹窗"
          onClick={onClose}
        >
          <X size={16} />
        </button>
      </div>
    </Dialog>
  )
}

/** 群聊与招聘表单共享外观，保存期间锁定提交并保留失败输入。 */
export function GroupCreateDialog({
  agents,
  onClose,
  onCreate,
}: {
  agents: Agent[]
  onClose: () => void
  onCreate: (name: string, memberIds: string[]) => Promise<void>
}) {
  const [name, setName] = useState('')
  const [memberIds, setMemberIds] = useState<string[]>([])
  const [error, setError] = useState('')
  const [isSubmitting, setSubmitting] = useState(false)
  async function submit(event: FormEvent): Promise<void> {
    event.preventDefault()
    if (isSubmitting) return
    if (!name.trim()) {
      setError('请输入群名称。')
      return
    }
    if (memberIds.length < 1) {
      setError('至少选择一只猫咪。')
      return
    }
    setError('')
    setSubmitting(true)
    try {
      await onCreate(name.trim(), memberIds)
    } catch (failure) {
      setError((failure as Error).message)
    } finally {
      setSubmitting(false)
    }
  }
  return (
    <Dialog title="新建群聊" variant="recruit" onClose={onClose}>
      <form className="agent-form group-form" onSubmit={(event) => void submit(event)}>
        <div className="group-intro">
          <span className="group-intro-icon">
            <Users size={24} strokeWidth={1.5} />
          </span>
          <div>
            <strong>让好搭档聚在一起</strong>
            <p>把不同专长的猫咪邀进群，一起讨论下一个想法。</p>
          </div>
        </div>
        <label>
          群名称
          <input
            autoFocus
            aria-label="群名称"
            value={name}
            maxLength={40}
            required
            placeholder="例如：新游戏创作小队"
            disabled={isSubmitting}
            onChange={(event) => setName(event.target.value)}
          />
        </label>
        <fieldset className="group-members" disabled={isSubmitting}>
          <legend>
            选择成员 <span>已选 {memberIds.length} 位</span>
          </legend>
          <div className="group-member-list">
            {agents.length === 0 && (
              <p className="dialog-description">还没有猫咪，先招聘一位搭档再来创建群聊。</p>
            )}
            {agents.map((agent) => (
              <label className="group-member" key={agent.id}>
                <input
                  type="checkbox"
                  aria-label={agent.name}
                  checked={memberIds.includes(agent.id)}
                  onChange={(event) =>
                    setMemberIds((current) =>
                      event.target.checked
                        ? [...current, agent.id]
                        : current.filter((item) => item !== agent.id),
                    )
                  }
                />
                <Avatar
                  preset={agent.avatar}
                  customAvatar={agent.customAvatar}
                  agentId={agent.id}
                />
                <span className="group-member-copy">
                  <strong>{agent.name}</strong>
                  <small>{agent.description || agent.group}</small>
                </span>
                <span className="group-member-check" aria-hidden="true">
                  {memberIds.includes(agent.id) && <Check size={13} />}
                </span>
              </label>
            ))}
          </div>
        </fieldset>
        {error && (
          <p role="alert" className="form-error">
            {error}
          </p>
        )}
        <button
          className="primary-button"
          type="submit"
          disabled={isSubmitting || agents.length === 0}
        >
          <Plus size={16} />
          {isSubmitting ? '正在创建…' : '创建群聊'}
        </button>
        <p className="form-footnote">创建后即可开始聊天，成员也可以随时调整。</p>
      </form>
    </Dialog>
  )
}
