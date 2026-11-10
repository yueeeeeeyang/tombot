import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { memoryBackend } from '../desktop/memoryBackend'
import { GroupLibrary } from './GroupLibrary'

describe('群资料库', () => {
  it('按游标加载本群文件并去重', async () => {
    const artifact = {
      id: 'file',
      title: '方案.pdf',
      kind: 'file' as const,
      createdAt: new Date().toISOString(),
    }
    const load = vi
      .spyOn(memoryBackend, 'listGroupArtifacts')
      .mockResolvedValueOnce({ artifacts: [artifact], next: 10 })
      .mockResolvedValueOnce({
        artifacts: [artifact, { ...artifact, id: 'image', title: '草图.png', kind: 'image' }],
        next: 0,
      })
    render(<GroupLibrary groupId="group" />)
    expect(await screen.findByText('方案.pdf')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '加载更多' }))
    expect(await screen.findByText('草图.png')).toBeInTheDocument()
    expect(screen.getAllByText('方案.pdf')).toHaveLength(1)
    expect(load).toHaveBeenLastCalledWith('group', 10)
  })
  it('失败不能假装为空，可重试读取', async () => {
    vi.spyOn(memoryBackend, 'listGroupArtifacts')
      .mockRejectedValueOnce(new Error('读取失败'))
      .mockResolvedValueOnce({ artifacts: [], next: 0 })
    render(<GroupLibrary groupId="group" />)
    expect(await screen.findByRole('alert')).toHaveTextContent('读取失败')
    expect(screen.queryByText(/资料库还是空的/)).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: '重试' }))
    await waitFor(() => expect(screen.getByRole('status')).toHaveClass('cat-panel-empty'))
  })
})
