import type { ChatMessage } from './model'

/** 已确认消息使用服务端序号；尚未确认的本地消息保持发送顺序，不比较两台机器的时钟。 */
export function mergeMessages(current: ChatMessage[], incoming: ChatMessage[]): ChatMessage[] {
  const messages = [...current]
  const confirmedIds = new Map<string, string>()
  for (const received of incoming) {
    const index = messages.findIndex(
      (message) =>
        message.id === received.id ||
        (received.role === 'user' &&
          received.requestId &&
          (message.requestId ?? message.id) === received.requestId),
    )
    if (index < 0) {
      messages.push(received)
      continue
    }
    const previous = messages[index]!
    if (previous.id !== received.id) confirmedIds.set(previous.id, received.id)
    const next = { ...previous, ...received, requestId: received.requestId ?? previous.requestId }
    // 回执和刷新可以乱序；只有更近的表情版本才能覆盖当前标记。
    if ((previous.reactionRevision ?? 0) > (received.reactionRevision ?? 0)) {
      next.reactions = previous.reactions
      next.reactionRevision = previous.reactionRevision
    }
    messages[index] = sameMessage(previous, next) ? previous : next
  }
  // 原消息先以本地标识显示；回执或推送确认后，尚未发送的引用也必须指向服务端标识。
  for (let index = 0; index < messages.length; index++) {
    const message = messages[index]!
    const messageId = message.replyTo && confirmedIds.get(message.replyTo.messageId)
    if (messageId) messages[index] = { ...message, replyTo: { ...message.replyTo!, messageId } }
  }
  messages.sort((left, right) => {
    if (left.sequence && right.sequence) return left.sequence - right.sequence
    if (left.sequence) return -1
    if (right.sequence) return 1
    return 0
  })
  return messages.length === current.length &&
    messages.every((message, i) => message === current[i])
    ? current
    : messages
}

/** 无变化的服务端快照保留对象身份，历史正文和附件不因工作进度更新而重新处理。 */
function sameMessage(left: ChatMessage, right: ChatMessage): boolean {
  return (
    left.id === right.id &&
    left.requestId === right.requestId &&
    left.sequence === right.sequence &&
    left.role === right.role &&
    left.content === right.content &&
    left.createdAt === right.createdAt &&
    left.status === right.status &&
    left.reactionRevision === right.reactionRevision &&
    JSON.stringify(left.reactions) === JSON.stringify(right.reactions) &&
    JSON.stringify(left.attachments) === JSON.stringify(right.attachments) &&
    JSON.stringify(left.replyTo) === JSON.stringify(right.replyTo) &&
    JSON.stringify(left.dmNotice) === JSON.stringify(right.dmNotice)
  )
}
