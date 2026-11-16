import { createHash } from 'node:crypto'
import { describe, expect, it } from 'vitest'
import { loginResultPage } from './loginResultPage'

describe('浏览器授权结果页', () => {
  it.each([true, false])('按真实结果展示并使用独立资源与严格内容策略：%s', (isSuccess) => {
    const { html, headers } = loginResultPage(isSuccess)
    const document = new DOMParser().parseFromString(html, 'text/html')
    expect(document.querySelector('h1')?.textContent).toBe(isSuccess ? '授权成功' : '登录未完成')
    expect(document.querySelector('.login-card')).not.toBeNull()
    expect(document.querySelector('img')?.src).toMatch(/^data:image\/png;base64,/)
    expect(document.querySelectorAll('link, iframe, form')).toHaveLength(0)
    for (const [tag, directive] of [
      ['style', 'style-src'],
      ['script', 'script-src'],
    ] as const) {
      const content = document.querySelector(tag)?.textContent ?? ''
      expect(content).not.toBe('')
      const hash = createHash('sha256').update(content).digest('base64')
      expect(headers['Content-Security-Policy']).toContain(`${directive} 'sha256-${hash}'`)
    }
    expect(document.querySelector('script')?.textContent).toBe(
      'history.replaceState(null,"","/callback")',
    )
    expect(headers['Content-Security-Policy']).not.toContain('unsafe-inline')
    expect(headers['Cache-Control']).toBe('no-store')
    expect(headers['Referrer-Policy']).toBe('no-referrer')
  })
})
