import { z } from 'zod'

export const computerStatus = z.object({
  state: z.enum(['running', 'stopped', 'restarting', 'failed']),
  version: z.string().regex(/^[a-f0-9]{64}$/),
})
export type ComputerStatus = z.infer<typeof computerStatus>
