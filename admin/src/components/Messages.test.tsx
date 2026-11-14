import { act, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { Messages } from './Messages'
import { ApiError, type OperationMessage } from '../api/client'
import { Alert } from '../components'
import { makeClient, empty } from '../../tests/fixtures'

describe('全局操作消息', () => {
  it('顶层展示消息，限制数量、自动消失，卸载后注销监听与计时器', () => {
    vi.useFakeTimers()
    try {
      const { client } = makeClient(() => empty())
      let notify: ((message: OperationMessage) => void) | null = null
      vi.spyOn(client, 'setMessenger').mockImplementation((listener) => {
        notify = listener
      })
      const view = render(<Messages client={client} />)
      act(() => {
        for (let i = 0; i < 4; i++) notify!({ tone: 'info', text: `消息 ${i}` })
      })
      expect(screen.getAllByRole('status')).toHaveLength(3)
      expect(screen.queryByText('消息 0')).not.toBeInTheDocument()
      expect(screen.getByText('消息 3').closest('[popover]')).toBeVisible()
      act(() => vi.advanceTimersByTime(5_000))
      expect(screen.queryAllByRole('status')).toHaveLength(0)
      act(() => notify!({ tone: 'success', text: '操作成功' }))
      view.unmount()
      expect(notify).toBeNull()
      expect(vi.getTimerCount()).toBe(0)
    } finally {
      vi.useRealTimers()
    }
  })
  it('取消验证不在原区域显示报错，业务失败仍正常显示', () => {
    const view = render(
      <Alert error={new ApiError('VERIFICATION_CANCELED', '已取消授权，操作已终止')} />,
    )
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    view.rerender(<Alert error={new ApiError('NODE_CATALOG_UNAVAILABLE', '接入服务暂不可用')} />)
    expect(screen.getByRole('alert')).toHaveTextContent('接入服务暂不可用')
  })
})
