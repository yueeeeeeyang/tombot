import { expect, it } from 'vitest'
import { runFailure } from './runFailure'
it('电脑停止、结果未知与用户叫停有不同的处理方式', () => {
  expect(runFailure('failed', 'COMPUTER_STOPPED')).toContain('共用的电脑已停止')
  expect(runFailure('unknown', 'WORKER_LOST')).toContain('避免重复提交')
  expect(runFailure('canceled', 'CANCELED')).toBe('')
  expect(runFailure('failed', 'NO_PROGRESS')).toContain('补充')
})
