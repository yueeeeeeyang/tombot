import { taskPageSchema, type CatTask } from '../shared/tasks'

type Call = (method: string, path: string, body?: unknown) => Promise<unknown>

/** 任务页按服务端游标有界合并；无效响应明确失败，不能显示成空列表。 */
export async function listTasks(call: Call, agentId: string): Promise<CatTask[]> {
  const items: CatTask[] = []
  const cursors = new Set<string>()
  let before = ''
  for (let page = 0; page < 5; page++) {
    const query = before ? `?before=${encodeURIComponent(before)}` : ''
    const result = taskPageSchema.parse(
      await call('GET', `/bots/${encodeURIComponent(agentId)}/tasks${query}`),
    )
    if (result.tasks.some((task) => task.bot_id !== agentId) || result.tasks.length > 20) {
      throw new Error('任务列表响应无效，请重新读取。')
    }
    items.push(...result.tasks)
    if (new Set(items.map((task) => task.id)).size !== items.length) {
      throw new Error('任务列表在读取时发生变化，请重新读取。')
    }
    if (!result.next_before) return items
    if (cursors.has(result.next_before)) throw new Error('任务列表分页无效，请重新读取。')
    cursors.add(result.next_before)
    before = result.next_before
  }
  throw new Error('任务列表超过允许数量，请重新读取。')
}
