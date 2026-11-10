import { act, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { memoryBackend } from '../desktop/memoryBackend'
import { CatIntroduction } from './CatIntroduction'

afterEach(() => vi.useRealTimers())

describe('猫咪展示介绍', () => {
  it('只读取介绍，消息变化后刷新，不写回工作空间', async () => {
    const load = vi
      .spyOn(memoryBackend, 'loadIntroduction')
      .mockResolvedValue({ text: '喜欢一起整理灵感。' })
    const save = vi.spyOn(memoryBackend, 'saveWorkspace')
    const view = render(<CatIntroduction agentId="cat" revision={0} />)
    expect(await screen.findByText('喜欢一起整理灵感。')).toBeInTheDocument()
    load.mockResolvedValue({ text: '熟悉你的写作习惯。' })
    view.rerender(<CatIntroduction agentId="cat" revision={1} />)
    expect(await screen.findByText('熟悉你的写作习惯。')).toBeInTheDocument()
    expect(load).toHaveBeenCalledTimes(2)
    expect(save).not.toHaveBeenCalled()
  })

  it('读取失败提示并允许显式重试，没有记忆时显示空介绍', async () => {
    const load = vi
      .spyOn(memoryBackend, 'loadIntroduction')
      .mockRejectedValueOnce(new Error('连接中断'))
      .mockResolvedValue({ text: '' })
    render(<CatIntroduction agentId="cat" revision={0} />)
    expect(await screen.findByRole('alert')).toHaveTextContent('连接中断')
    fireEvent.click(screen.getByRole('button', { name: '重试' }))
    await act(async () => undefined)
    expect(screen.queryByRole('alert')).toBeNull()
    expect(screen.getByText('相处久一点，会更了解彼此。')).toBeInTheDocument()
    expect(load).toHaveBeenCalledTimes(2)
  })

  it('顺序刷新，卸载后停止并忽略在途结果', async () => {
    vi.useFakeTimers()
    const load = vi
      .spyOn(memoryBackend, 'loadIntroduction')
      .mockResolvedValue({ text: '一起工作。' })
    const view = render(<CatIntroduction agentId="cat" revision={0} />)
    await act(async () => undefined)
    let finish!: (value: { text: string }) => void
    load.mockImplementation(
      () =>
        new Promise((resolve) => {
          finish = resolve
        }),
    )
    await act(() => vi.advanceTimersByTimeAsync(30_000))
    await act(() => vi.advanceTimersByTimeAsync(90_000))
    expect(load).toHaveBeenCalledTimes(2)
    view.unmount()
    await act(async () => finish({ text: '更新' }))
    await act(() => vi.advanceTimersByTimeAsync(90_000))
    expect(load).toHaveBeenCalledTimes(2)
  })
})
