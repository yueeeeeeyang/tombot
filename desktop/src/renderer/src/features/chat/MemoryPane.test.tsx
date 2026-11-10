import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { MemoryPane } from './MemoryPane'

describe('记忆面板', () => {
  it('没有记忆时展示空状态，记下后刷新列表', async () => {
    const user = userEvent.setup()
    const created = vi.fn(async () => undefined)
    const onLoad = vi
      .fn<
        (
          _agentId: string,
        ) => Promise<Array<{ path: string; title: string; summary: string; sha256: string }>>
      >()
      .mockResolvedValueOnce([])
      .mockResolvedValueOnce([
        { path: 'note.md', title: '偏好', summary: '喜欢短句', sha256: 'abc' },
      ])
    render(
      <MemoryPane
        agentId="assistant"
        onLoad={onLoad}
        onCreate={created}
        onDelete={vi.fn(async () => undefined)}
      />,
    )
    expect(await screen.findByRole('status')).toHaveTextContent('还没有记忆')
    await user.type(screen.getByLabelText('标题'), '偏好')
    await user.type(screen.getByLabelText('内容'), '喜欢短句')
    await user.click(screen.getByRole('button', { name: '记下' }))
    expect(created).toHaveBeenCalledWith('assistant', '偏好', '喜欢短句')
    expect(await screen.findByText('偏好')).toBeInTheDocument()
  })
})
