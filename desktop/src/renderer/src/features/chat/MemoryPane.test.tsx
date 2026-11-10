import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { MemoryPane } from './MemoryPane'

describe('记忆面板', () => {
  it('只展示 agent 记下的记忆，没有记下和删除', async () => {
    render(
      <MemoryPane
        agentId="assistant"
        onLoad={async () => [{ path: 'note.md', title: '晨跑', summary: '每周三', sha256: 'abc' }]}
      />,
    )
    expect(await screen.findByText('晨跑')).toBeInTheDocument()
    expect(screen.getByText('每周三')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '记下' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '删除' })).not.toBeInTheDocument()
  })

  it('没有记忆时展示空状态', async () => {
    render(<MemoryPane agentId="assistant" onLoad={async () => []} />)
    expect(await screen.findByRole('status')).toHaveTextContent('还没有记忆')
  })
})
