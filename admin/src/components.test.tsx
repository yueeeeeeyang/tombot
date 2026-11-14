import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { Pagination } from './components'

describe('分页空状态', () => {
  it('首页没有数据时不渲染条数、分隔线或分页操作', () => {
    const { container } = render(
      <Pagination before="" onBefore={vi.fn()} count={0} last={undefined} />,
    )
    expect(container).toBeEmptyDOMElement()
  })

  it('后续空页隐藏零条计数并保留返回首页', async () => {
    const onBefore = vi.fn()
    render(<Pagination before="previous-page" onBefore={onBefore} count={0} last={undefined} />)
    expect(screen.queryByText(/本页/)).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '下一页' })).not.toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: '返回首页' }))
    expect(onBefore).toHaveBeenCalledExactlyOnceWith('')
  })

  it('有数据时显示真实条数，未满一页不显示下一页', () => {
    render(<Pagination before="" onBefore={vi.fn()} count={2} last="last-record" />)
    expect(screen.getByText('本页 2 条')).toBeInTheDocument()
    expect(screen.queryByRole('button')).not.toBeInTheDocument()
  })

  it('满页且有游标时可读取下一页', async () => {
    const onBefore = vi.fn()
    render(<Pagination before="" onBefore={onBefore} count={50} last="last-record" />)
    expect(screen.getByText('本页 50 条')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: '下一页' }))
    expect(onBefore).toHaveBeenCalledExactlyOnceWith('last-record')
  })
})
