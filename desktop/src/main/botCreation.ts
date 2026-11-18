import { addAgent, emptyWorkspace, type NewAgent } from '../renderer/src/features/workspace/model'
import type { ServerBot } from './workspaceSnapshot'

type Call = (method: string, path: string, body?: unknown) => Promise<unknown>
const serverId = /^[0-7][0-9A-HJKMNP-TV-Z]{25}$/

/** 创建资料与单聊身份均以服务端回执为准；头像失败后用原幂等键继续，不能再创建一只。 */
export async function createBot(call: Call, profile: NewAgent, requestId: string): Promise<string> {
  if (!serverId.test(requestId)) throw new Error('创建请求标识无效，请重新打开招聘页面。')
  const agent = addAgent(emptyWorkspace(), profile, requestId).agents[0]!
  const response = (await call('POST', '/bots', {
    client_request_id: requestId,
    name: agent.name,
    description: agent.description,
    avatar_ref: agent.avatar,
    category: agent.group,
    system_prompt:
      'You are a helpful assistant. Collaborate with the user and use the available tools when needed.',
    // 用户已配备电脑，默认开放完整操作能力；执行器按工具要求选择视觉模型。
    tool_permissions: [
      'computer_shell',
      'computer_browser',
      'computer_screenshot',
      'computer_input',
    ],
    approval_mode: 'standard',
    messaging_policy: { accept_dm: true, can_initiate: true, accept_group_invite: true },
    accent_hue: 0,
  })) as { bot?: ServerBot }
  const bot = response.bot
  if (
    !bot ||
    !serverId.test(bot.id) ||
    !serverId.test(bot.conversation_id) ||
    !Number.isSafeInteger(bot.revision) ||
    bot.revision < 1 ||
    bot.is_archived
  ) {
    throw new Error('服务没有确认猫咪及其会话，请重试。')
  }
  // 创建回执重放会返回最新修订；已有头像表示上次上传已完成，不重复覆盖。
  if (agent.customAvatar && !bot.has_custom_avatar) {
    const updated = (await call('PUT', `/bots/${bot.id}/avatar`, {
      expected_revision: bot.revision,
      content_base64: agent.customAvatar.slice('data:image/png;base64,'.length),
    })) as { bot?: ServerBot }
    if (updated.bot?.id !== bot.id || !updated.bot.has_custom_avatar) {
      throw new Error('服务没有确认猫咪头像，请重试。')
    }
  }
  return bot.id
}
