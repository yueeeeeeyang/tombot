import type { ChatMessage } from './model'

/** 侧栏和搜索共用发送状态文案；收到的消息与私聊通知不属于待发送内容。 */
export function messagePreview(message: ChatMessage): string {
  if (message.role === 'user' && !message.dmNotice) {
    if (message.status === 'unsent') return `未发送：${message.content}`
    if (message.status === 'failed') return `发送失败：${message.content}`
  }
  return message.content
}
