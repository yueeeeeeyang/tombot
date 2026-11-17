import { fireEvent, render, screen, within } from '@testing-library/react'
import { expect, it, vi } from 'vitest'
import { fileIconKind, LibraryFileCard } from './LibraryFileCard'
const file = {
  id: 'file',
  conversation_id: 'chat',
  name: 'jedis-3.10.0.jar',
  media_type: 'application/java-archive',
  size: 128,
  sha256: '',
  revision: 1,
  created_at: '2026-11-09T05:47:00Z',
}
it('悬浮显示文件信息，操作集中在更多菜单并支持键盘与外部关闭', () => {
  const action = vi.fn()
  render(
    <ul>
      <LibraryFileCard file={file} size="128 B" onAction={action} />
    </ul>,
  )
  expect(screen.queryByRole('tooltip')).toBeNull()
  expect(screen.queryByRole('menuitem')).toBeNull()
  fireEvent.mouseEnter(screen.getByRole('listitem'))
  expect(screen.getByRole('tooltip')).toHaveTextContent(file.name)
  expect(screen.getByRole('tooltip')).toHaveTextContent('128 B')
  const more = screen.getByRole('button', { name: `更多操作 ${file.name}` })
  fireEvent.click(more)
  expect(screen.queryByRole('tooltip')).toBeNull()
  const menu = screen.getByRole('menu')
  expect(within(menu).getAllByRole('menuitem')).toHaveLength(4)
  fireEvent.keyDown(menu, { key: 'End' })
  expect(screen.getByRole('menuitem', { name: `删除 ${file.name}` })).toHaveFocus()
  fireEvent.keyDown(menu, { key: 'Escape' })
  expect(screen.queryByRole('menu')).toBeNull()
  expect(more).toHaveFocus()
  fireEvent.click(more)
  fireEvent.click(screen.getByRole('menuitem', { name: `下载 ${file.name}` }))
  expect(action).toHaveBeenCalledWith('download')
  fireEvent.click(more)
  fireEvent.pointerDown(document.body)
  expect(screen.queryByRole('menu')).toBeNull()
})
it('按常用文件类型选择一致的线性图标，未知类型使用通用文件', () => {
  expect(fileIconKind(file)).toBe('archive')
  expect(fileIconKind({ name: 'a.ts', media_type: 'text/plain' })).toBe('code')
  expect(fileIconKind({ name: 'a.csv', media_type: 'text/csv' })).toBe('spreadsheet')
  expect(fileIconKind({ name: 'a.png', media_type: 'image/png' })).toBe('image')
  expect(fileIconKind({ name: 'a.unknown', media_type: 'application/octet-stream' })).toBe('file')
})
