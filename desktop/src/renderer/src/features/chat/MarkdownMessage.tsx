import { memo } from 'react'
import Markdown from 'react-markdown'
import remarkGfm from 'remark-gfm'

/** 解析模型正文；禁用原始 HTML 和自动加载远程图片，链接只接受安全协议。 */
export const MarkdownMessage = memo(function MarkdownMessage({ content }: { content: string }) {
  return (
    <div className="message-text message-markdown">
      <Markdown
        remarkPlugins={[remarkGfm]}
        skipHtml
        urlTransform={(url) => (/^https?:\/\//i.test(url) ? url : '')}
        components={{
          a: ({ href, children }) =>
            href ? (
              <a href={href} target="_blank" rel="noreferrer">
                {children}
              </a>
            ) : (
              <span>{children}</span>
            ),
          img: ({ alt }) => <span>{alt ? `图片：${alt}` : '图片'}</span>,
          table: ({ children }) => (
            <div className="markdown-table">
              <table>{children}</table>
            </div>
          ),
        }}
      >
        {content}
      </Markdown>
    </div>
  )
})
