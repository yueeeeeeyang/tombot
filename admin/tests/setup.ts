import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/react'
import { afterEach, beforeEach, vi } from 'vitest'

beforeEach(() => {
  window.history.replaceState(null, '', '/admin/')
  Object.defineProperty(navigator, 'locks', {
    configurable: true,
    value: {
      request: async (_name: string, _options: LockOptions, callback: () => Promise<unknown>) =>
        callback(),
    },
  })
})

afterEach(() => {
  cleanup()
  localStorage.clear()
  vi.unstubAllGlobals()
})
// jsdom 不实现模态顶层；焦点及真实布局由浏览器验收补充。
HTMLDialogElement.prototype.showModal = function () {
  this.setAttribute('open', '')
}
HTMLDialogElement.prototype.close = function () {
  this.removeAttribute('open')
}
window.matchMedia = vi.fn((media: string) => ({
  matches: false,
  media,
  onchange: null,
  addEventListener: vi.fn(),
  removeEventListener: vi.fn(),
  addListener: vi.fn(),
  removeListener: vi.fn(),
  dispatchEvent: vi.fn(),
}))

// jsdom 不实现弹出层布局，遮挡和滚动定位由真实浏览器验证。
Element.prototype.scrollIntoView = vi.fn()
HTMLElement.prototype.showPopover = function () {
  this.style.display = 'block'
}
HTMLElement.prototype.hidePopover = function () {
  this.style.display = 'none'
}
