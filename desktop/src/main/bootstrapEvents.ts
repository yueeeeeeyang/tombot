export interface BootstrapStep {
  id: string
  title: string
  status: string
  detail?: string
  duration_ms: number
}

export interface BootstrapStatus {
  phase: string
  steps: BootstrapStep[]
  error?: string
  cursor: number
}

export type ParsedEvent =
  { kind: 'ignore' } | { kind: 'resync' } | { kind: 'status'; status: BootstrapStatus }

/** 解析引导 SSE。注释和无关事件忽略，resync 表示要重新读取状态。 */
export function parseEventBlock(block: string, cursor: number): ParsedEvent {
  let event = ''
  let id = cursor
  const data: string[] = []
  for (const line of block.split('\n')) {
    if (line === '' || line.startsWith(':')) continue
    if (line.startsWith('id:')) {
      const parsed = Number(line.slice(3).trim())
      if (Number.isFinite(parsed) && parsed >= 0) id = parsed
      continue
    }
    if (line.startsWith('event:')) {
      event = line.slice(6).trim()
      continue
    }
    if (line.startsWith('data:')) data.push(line.slice(5).replace(/^ /, ''))
  }
  if (event === 'resync') return { kind: 'resync' }
  if (event !== 'bootstrap.step' || data.length === 0) return { kind: 'ignore' }
  let payload: unknown
  try {
    payload = JSON.parse(data.join('\n'))
  } catch {
    throw new Error('引导事件无法识别')
  }
  const status = normalizeStatus(payload)
  status.cursor = id
  return { kind: 'status', status }
}

/** 校验状态快照。缺阶段或步骤形状不对时失败，不用空进度假装已完成。 */
export function normalizeStatus(value: unknown): BootstrapStatus {
  if (!value || typeof value !== 'object') throw new Error('引导状态无法识别')
  const record = value as { phase?: unknown; steps?: unknown; error?: unknown; cursor?: unknown }
  if (typeof record.phase !== 'string' || record.phase === '') throw new Error('引导状态缺少阶段')
  const cursor =
    typeof record.cursor === 'number' && Number.isFinite(record.cursor) && record.cursor >= 0
      ? record.cursor
      : 0
  return {
    phase: record.phase,
    steps: normalizeSteps(record.steps),
    error: typeof record.error === 'string' && record.error !== '' ? record.error : undefined,
    cursor,
  }
}

function normalizeSteps(value: unknown): BootstrapStep[] {
  if (value == null) return []
  if (!Array.isArray(value)) throw new Error('引导步骤无法识别')
  return value.map((item) => {
    if (!item || typeof item !== 'object') throw new Error('引导步骤无法识别')
    const step = item as {
      id?: unknown
      title?: unknown
      status?: unknown
      detail?: unknown
      duration_ms?: unknown
    }
    if (
      typeof step.id !== 'string' ||
      typeof step.title !== 'string' ||
      typeof step.status !== 'string'
    ) {
      throw new Error('引导步骤缺少标识')
    }
    return {
      id: step.id,
      title: step.title,
      status: step.status,
      detail: typeof step.detail === 'string' && step.detail !== '' ? step.detail : undefined,
      duration_ms:
        typeof step.duration_ms === 'number' && Number.isFinite(step.duration_ms)
          ? step.duration_ms
          : 0,
    }
  })
}
