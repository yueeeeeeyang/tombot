import { z } from 'zod'
import { consoleID, consoleSchema } from '../shared/console'

type Call = (method: string, path: string, body?: unknown, timeout?: number) => Promise<unknown>
const path = (id: string): string => `/consoles/${consoleID.parse(id)}`

export async function openConsole(call: Call, botId: string, requestId: string) {
  const result = z.object({ console: consoleSchema }).parse(
    await call('POST', `/bots/${consoleID.parse(botId)}/console`, {
      client_request_id: consoleID.parse(requestId),
    }),
  )
  if (result.console.bot_id !== botId) throw new Error('电脑接管返回的猫咪不匹配。')
  return result.console
}
export async function controlConsole(
  call: Call,
  id: string,
  action: 'heartbeat' | 'close' | 'takeover' | 'release',
) {
  const result = z
    .object({ console: consoleSchema })
    .parse(await call('POST', `${path(id)}/${action}`, {}))
  if (result.console.id !== id) throw new Error('电脑接管返回的会话不匹配。')
  return result.console
}
