import { useCallback, useEffect, useState, type FormEvent } from 'react'
import { Avatar } from '../../components/Avatar'
import { Dialog } from '../../components/Dialog'
import { ArrowLeftRight, Plus, X } from 'lucide-react'
import type { CatAvatarId } from '../avatars/presets'
import type { ServerBubble } from '../desktop/backend'
import { getBackend } from '../desktop/backend'
import type { Agent, DmNotice, MessageAttachment } from '../workspace/model'
import { replyExcerpt } from '../workspace/model'
import { ChatMessage } from './ChatMessage'
import { MessageMenu, type MessageMenuAnchor } from './MessageMenu'
import { messageTimeSeparator } from './messageTime'

type GroupDetail = {
  id: string
  name: string
  revision: number
  conversation_id: string
  members: Array<{ bot_id: string; name: string }>
}

/** 群聊复用原来的气泡，可以发言和管理成员。猫咪之间的私信不在这里打开。 */
export function ConversationPanel({
  id,
  agents,
  onNotice,
  onError,
}: {
  id: string
  agents: Agent[]
  onNotice: (message: string) => void
  onError: (message: string) => void
}) {
  const [title, setTitle] = useState('群聊')
  const [conversationId, setConversationId] = useState('')
  const [group, setGroup] = useState<GroupDetail | null>(null)
  const [messages, setMessages] = useState<ServerBubble[]>([])
  const [draft, setDraft] = useState('')
  const [pending, setPending] = useState<MessageAttachment[]>([])
  const [replyToId, setReplyToId] = useState<string>()
  const [live, setLive] = useState('')
  const [menu, setMenu] = useState<MessageMenuAnchor>()
  const [membersOpen, setMembersOpen] = useState(false)
  const [busy, setBusy] = useState(false)
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
    if (!content || !conversationId || busy) return
    setBusy(true)
    setLive('')
    try {
      await getBackend().sendConversation?.(
        conversationId,
        content,
        pending.map((item) => ({ path: item.path, sha256: item.sha256 })),
        replyToId,
        (event) => {
          if (event.type === 'delta') setLive((current) => current + event.text)
        },
      )
      setDraft('')
      setPending([])
      setReplyToId(undefined)
      setLive('')
      await reload()
    } catch (failure) {
      onError((failure as Error).message)
    } finally {
      setBusy(false)
    }
  }

  async function addFiles(files: File[]): Promise<void> {
    const backend = getBackend()
    for (const file of files) {
      if (pending.length >= 4) {
        onError('一条消息最多 4 个附件。')
        return
      }
      const uploaded = await backend.uploadAttachment?.(
        { name: file.name, type: file.type, bytes: new Uint8Array(await file.arrayBuffer()) },
        { shared: true },
      )
      if (!uploaded) return
      const kind = file.type === 'image/png' || file.type === 'image/jpeg' ? 'image' : 'file'
      setPending((current) => [
        ...current,
        {
          name: file.name,
          path: uploaded.path,
          sha256: uploaded.sha256,
          mediaType: file.type,
          kind,
        },
      ])
    }
  }

  const reply = messages.find((message) => message.id === replyToId)
  const shown = live
    ? [
        ...messages,
        {
          id: 'live-reply',
          role: 'assistant' as const,
          speaker: '',
          content: live,
          createdAt: new Date().toISOString(),
          reactions: [],
          attachments: [],
        },
      ]
    : messages
  const menuMessage = menu ? messages.find((message) => message.id === menu.messageId) : undefined

  return (
    <main className="chat-panel">
      <header className="chat-header">
        <div className="chat-agent-pill" aria-current="page">
          <span>{title}</span>
        </div>
        <button
          type="button"
          className="text-button"
          onClick={() => setMembersOpen((open) => !open)}
        >
          成员
        </button>
      </header>
      <section className="chat-log">
        <div className="saved-messages" role="log" aria-label={title}>
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
                    item.role === 'assistant' ? names.get(item.speaker) || item.speaker : undefined
                  }
                  isHighlighted={false}
                  onOpenMenu={(messageId, x, y, trigger) => setMenu({ messageId, x, y, trigger })}
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
        onDragOver={(event) => event.preventDefault()}
        onDrop={(event) => {
          event.preventDefault()
          void addFiles([...event.dataTransfer.files]).catch((failure: Error) =>
            onError(failure.message),
          )
        }}
      >
        {membersOpen && group && (
          <div className="member-panel">
            {group.members.map((member) => (
              <p key={member.bot_id}>
                <span>{member.name}</span>
                <button
                  type="button"
                  aria-label={`移出${member.name}`}
                  onClick={() => {
                    void getBackend()
                      .removeGroupMember?.(group.id, group.revision, member.bot_id)
                      .then(reload)
                      .catch((failure: Error) => onError(failure.message))
                  }}
                >
                  移出
                </button>
              </p>
            ))}
            <label>
              加入猫咪
              <select
                aria-label="加入猫咪"
                defaultValue=""
                onChange={(event) => {
                  const botId = event.target.value
                  event.target.value = ''
                  if (!botId) return
                  void getBackend()
                    .addGroupMembers?.(group.id, group.revision, [botId])
                    .then(reload)
                    .catch((failure: Error) => onError(failure.message))
                }}
              >
                <option value="">选择</option>
                {agents
                  .filter((agent) => !group.members.some((member) => member.bot_id === agent.id))
                  .map((agent) => (
                    <option key={agent.id} value={agent.id}>
                      {agent.name}
                    </option>
                  ))}
              </select>
            </label>
          </div>
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
          <textarea
            value={draft}
            rows={1}
            aria-label={`给${title}发消息`}
            placeholder={`给 ${title} 发消息`}
            onChange={(event) => setDraft(event.target.value)}
          />
          <button
            className="composer-send"
            type="submit"
            disabled={busy || (!draft.trim() && pending.length === 0)}
            aria-label="发送"
          >
            发送
          </button>
        </form>
      </div>
    </main>
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
    void getBackend()
      .listConversationMessages?.(notice.conversationId)
      .then((rows) => {
        if (!cancelled) setLines(rows ?? [])
      })
      .catch((failure: Error) => {
        if (!cancelled) onError(failure.message)
      })
    return () => {
      cancelled = true
    }
  }, [notice.conversationId, onError])
  const peer = agents.find((agent) => agent.id === notice.peerId)
  const peerName = peer?.name || notice.peerName
  const title = `${self.name} ↔ ${peerName}`
  // 消息接口按序号倒序返回，阅读时从早到晚。
  const ordered = [...(lines ?? [])].reverse()
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
                    agentId={mine ? self.id : line.speaker || notice.peerId}
                    size="small"
                  />
                  <span className={`dm-name ${mine ? 'dm-name--self' : 'dm-name--peer'}`}>
                    {name}
                  </span>
                  <div className="dm-bubble">
                    <div className="message-text">{line.content}</div>
                  </div>
                </div>
              )
            })}
          </div>
        </div>
        <div className="dm-top">
          <div className="dm-capsule">
            <Avatar preset={self.avatar} agentId={self.id} size="small" />
            <span>{self.name}</span>
            <ArrowLeftRight size={15} aria-hidden="true" />
            <Avatar preset={peer?.avatar ?? 'bombay'} agentId={notice.peerId} size="small" />
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

/** 用现有对话框风格创建群聊，并选上要一起说话的猫咪。 */
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
  async function submit(event: FormEvent): Promise<void> {
    event.preventDefault()
    if (memberIds.length < 1) {
      setError('至少选择一只猫咪。')
      return
    }
    try {
      await onCreate(name.trim(), memberIds)
    } catch (failure) {
      setError((failure as Error).message)
    }
  }
  return (
    <Dialog title="新建群聊" onClose={onClose}>
      <form onSubmit={(event) => void submit(event)}>
        <label>
          群名称
          <input
            aria-label="群名称"
            value={name}
            maxLength={40}
            required
            onChange={(event) => setName(event.target.value)}
          />
        </label>
        <fieldset>
          <legend>成员</legend>
          {agents.map((agent) => (
            <label key={agent.id}>
              <input
                type="checkbox"
                checked={memberIds.includes(agent.id)}
                onChange={(event) =>
                  setMemberIds((current) =>
                    event.target.checked
                      ? [...current, agent.id]
                      : current.filter((item) => item !== agent.id),
                  )
                }
              />
              {agent.name}
            </label>
          ))}
        </fieldset>
        {error ? <p role="alert">{error}</p> : null}
        <button type="submit">创建群聊</button>
      </form>
    </Dialog>
  )
}
