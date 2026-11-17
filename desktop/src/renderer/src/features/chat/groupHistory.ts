import type { MessagePage, ServerBubble } from '../desktop/backend'

/** 重连期间超过一页时补齐缺口；达到有界窗口时整体切换最近记录，保留向前翻页入口。 */
export async function readGroupHistory(
  read: (before?: number) => Promise<MessagePage>,
  latest: number,
): Promise<MessagePage & { replace: boolean }> {
  let page = await read()
  const messages: ServerBubble[] = [...page.messages]
  const seen = new Set(messages.map((message) => message.id))
  let count = 1
  while (latest > 0 && page.hasMore && (page.messages[0]?.sequence ?? 0) > latest && count < 20) {
    const before = page.messages[0]!.sequence
    page = await read(before)
    if (page.messages.some((message) => message.sequence >= before || seen.has(message.id)))
      throw new Error('消息分页发生变化，请重新读取。')
    for (const message of page.messages) seen.add(message.id)
    messages.unshift(...page.messages)
    count++
  }
  return {
    messages,
    hasMore: page.hasMore,
    replace: latest > 0 && page.hasMore && (messages[0]?.sequence ?? 0) > latest,
  }
}
