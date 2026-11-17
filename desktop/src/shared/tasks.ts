import { z } from 'zod'

export const taskSchema = z.object({
  id: z.string().min(1),
  bot_id: z.string().min(1),
  conversation_id: z.string().min(1),
  name: z.string().min(1),
  prompt: z.string().min(1),
  cron: z.string(),
  timezone: z.string().refine((value) => {
    try {
      new Intl.DateTimeFormat('zh-CN', { timeZone: value })
      return value.length > 0
    } catch {
      return false
    }
  }),
  run_at: z.iso.datetime({ offset: true }).nullable(),
  is_enabled: z.boolean(),
  revision: z.number().int().positive(),
  next_run_at: z.iso.datetime({ offset: true }).nullable(),
  last_run_at: z.iso.datetime({ offset: true }).nullable(),
  last_run_id: z.string().nullable(),
  last_delivery_status: z.string(),
  last_status: z.string(),
  last_error: z.string(),
})
export type CatTask = z.infer<typeof taskSchema>
export const taskPageSchema = z.object({ tasks: z.array(taskSchema), next_before: z.string() })
