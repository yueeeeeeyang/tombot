import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { MarkdownMessage } from './MarkdownMessage'

describe('模型消息 Markdown', () => {
  it('渲染标题、强调、列表、代码、引用和表格', () => {
    const { container } = render(
      <MarkdownMessage
        content={
          '## 计划\n\n**重点**与`代码`\n\n- 第一项\n- 第二项\n\n> 引用\n\n```js\nconst value = 1\n```\n\n| 名称 | 状态 |\n| --- | --- |\n| 任务 | 完成 |'
        }
      />,
    )
    expect(screen.getByRole('heading', { name: '计划' })).toBeInTheDocument()
    expect(container.querySelector('strong')).toHaveTextContent('重点')
    expect(screen.getAllByRole('listitem')).toHaveLength(2)
    expect(container.querySelector('blockquote')).toHaveTextContent('引用')
    expect(container.querySelector('pre code')).toHaveTextContent('const value = 1')
    expect(screen.getByRole('table')).toHaveTextContent('任务完成')
  })
  it('不执行 HTML、不加载外部图片、不允许危险协议', () => {
    const { container } = render(
      <MarkdownMessage
        content={
          '<script>alert(1)</script>\n\n[危险](javascript:alert%281%29)\n\n[文档](https://example.test/docs)\n\n![预览](https://example.test/image.png)'
        }
      />,
    )
    expect(container.querySelector('script')).toBeNull()
    expect(container.querySelector('img')).toBeNull()
    expect(screen.queryByRole('link', { name: '危险' })).toBeNull()
    expect(screen.getByRole('link', { name: '文档' })).toHaveAttribute(
      'href',
      'https://example.test/docs',
    )
    expect(screen.getByRole('link')).toHaveAttribute('rel', 'noreferrer')
  })
  it('流式未闭合代码块与空内容可安全渲染', () => {
    const { container, rerender } = render(<MarkdownMessage content={'```ts\nconst value'} />)
    expect(container.querySelector('code')).toHaveTextContent('const value')
    rerender(<MarkdownMessage content="" />)
    expect(container.textContent).toBe('')
  })
})
