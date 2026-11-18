import { z } from 'zod'

export const consoleID = z.string().regex(/^[0-9A-HJKMNP-TV-Z]{26}$/)
export const consoleSchema = z.object({
  id: consoleID,
  bot_id: consoleID,
  state: z.enum([
    'opening',
    'viewing',
    'waiting',
    'pausing',
    'active',
    'resuming',
    'closed',
    'failed',
  ]),
  error_code: z.string(),
  expires_at: z.iso.datetime({ offset: true }),
})
export type DesktopConsole = z.infer<typeof consoleSchema>
export type ConsoleStreamEvent =
  { kind: 'open' | 'close' | 'error' } | { kind: 'bytes'; bytes: Uint8Array }
export interface ConsoleConnection {
  send(bytes: Uint8Array): void
  close(): void
}
export interface ConsoleBackend {
  openConsole(botId: string, requestId: string): Promise<DesktopConsole>
  consoleStatus(id: string): Promise<DesktopConsole>
  closeConsole(id: string): Promise<DesktopConsole>
  controlConsole(id: string, takeover: boolean): Promise<DesktopConsole>
  connectConsole(
    id: string,
    streamId: string,
    receive: (event: ConsoleStreamEvent) => void,
  ): ConsoleConnection
}
