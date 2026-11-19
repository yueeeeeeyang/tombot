const messages: Record<string, string> = {
  COMPUTER_STOPPED:
    '所有 Bot 共用的电脑已停止。请在「设置 → 电脑」重启，恢复后再继续任务。切换 Bot 也会使用这台电脑。',
  COMPUTER_RESTARTING:
    '所有 Bot 共用的电脑正在恢复。请在「设置 → 电脑」查看状态，等电脑运行后再继续任务。',
  COMPUTER_UNAVAILABLE:
    '暂时无法连接共用电脑。请在「设置 → 电脑」查看状态；若节点持续离线，请联系管理员。',
  COMPUTER_LEASE_LOST: '任务与共用电脑的连接已中断。请先检查文件或操作结果，再决定是否继续。',
  COMPUTER_QUEUE_FULL: '电脑正在处理其他任务，请等正在执行的任务结束后再继续。',
  MODEL_QUOTA_EXCEEDED: '模型用量已达额度。请等额度恢复，或联系管理员调整额度。',
  MODEL_NO_CANDIDATE: '当前没有可用模型。请联系管理员检查模型连接和配置。',
  MODEL_CONCURRENCY_LIMIT: '模型正在处理其他请求，请稍后继续。',
  NO_PROGRESS:
    '多次尝试没有取得新的结果，任务已停止。请查看上一阶段的结果，补充缺少的信息或调整目标。',
  CONTEXT_LIMIT: '任务内容超过模型可处理的范围。请指定要先处理的文件或步骤。',
  RUN_TIMEOUT: '本阶段等待执行结果超时。请先检查已生成的文件或操作结果，再决定从哪一步继续。',
  EXTERNAL_RESULT_UNKNOWN:
    '上一次操作的结果尚未确认。请先检查文件或目标服务，避免重复提交同一操作。',
}
export function runFailure(status: unknown, code: unknown) {
  if (status === 'succeeded' || status === 'canceled') return ''
  if (typeof code === 'string' && messages[code]) return messages[code]
  if (status === 'unknown') return messages.EXTERNAL_RESULT_UNKNOWN ?? '请先确认上一项操作的结果。'
  return '任务中断。请查看最后一条进度或已生成的文件；若原因仍不清楚，请联系管理员并提供本次运行编号。'
}
