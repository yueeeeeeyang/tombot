import type { CatTask } from './tasks'

export const taskFixture: CatTask = {
  id: 'task',
  bot_id: 'assistant',
  conversation_id: 'conversation',
  name: '晨跑提醒',
  prompt: '提醒晨跑',
  cron: '0 9 * * *',
  timezone: 'Asia/Shanghai',
  run_at: null,
  is_enabled: true,
  revision: 1,
  next_run_at: '2026-11-17T01:00:00Z',
  last_run_at: null,
  last_run_id: null,
  last_delivery_status: '',
  last_status: '',
  last_error: '',
}
