import { createHash } from 'node:crypto'
import theme from '../../../ui/theme.css?raw'
import controls from '../../../ui/controls.css?raw'
import loginStyle from '../../../ui/browser-login.css?raw'
import brand from '../renderer/src/assets/cats/frames/bombay.png?inline'

const styles = `${theme}\n@media (prefers-color-scheme: dark) {${theme.slice(theme.indexOf(':root[data-theme="dark"]')).replace(':root[data-theme="dark"]', ':root')}}\n${controls}\n${loginStyle}`
// 回调处理后立即清除地址栏中的一次性材料；页面不加载任何远程资源。
const cleanAddress = 'history.replaceState(null,"","/callback")'
const hash = (value: string) => createHash('sha256').update(value).digest('base64')

/** 按真实登录结果生成自包含页面；页面不接收或展示授权材料和内部错误。 */
export function loginResultPage(isSuccess: boolean) {
  const title = isSuccess ? '授权成功' : '登录未完成'
  const description = isSuccess
    ? '客户端已完成登录。回到 TomBot，继续与你的猫咪搭档对话。'
    : '本次登录没有完成。请回到 TomBot 客户端查看提示，并重新发起登录。'
  const icon = isSuccess ? '<path d="m6 12 4 4 8-8"/>' : '<path d="m8 8 8 8m0-8-8 8"/>'
  return {
    headers: {
      'Content-Type': 'text/html; charset=utf-8',
      'Cache-Control': 'no-store',
      'Referrer-Policy': 'no-referrer',
      'X-Content-Type-Options': 'nosniff',
      'Content-Security-Policy': `default-src 'none'; style-src 'sha256-${hash(styles)}'; script-src 'sha256-${hash(cleanAddress)}'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'`,
    },
    html: `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>${title} · TomBot</title><style>${styles}</style><script>${cleanAddress}</script></head><body><main class="login-card result-card"><img class="login-brand" src="${brand}" alt="TomBot 猫咪头像" width="64" height="64"><span class="result-label">TOMBOT · 客户端授权</span><span class="result-icon ${isSuccess ? '' : 'result-error'}" aria-hidden="true"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">${icon}</svg></span><h1>${title}</h1><p class="login-description">${description}</p><section class="result-next"><span class="eyebrow">下一步</span><strong>回到 TomBot 桌面客户端</strong><span>${isSuccess ? '你的账号已经准备就绪' : '确认提示后再次尝试'}</span></section><p class="result-footer">此页面可以安全关闭</p></main></body></html>`,
  }
}
