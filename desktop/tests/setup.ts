import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/react'
import { afterEach, vi } from 'vitest'

afterEach(() => {
  cleanup()
  localStorage.clear()
})

// jsdom 没有布局引擎和模态框实现；真实窗口行为由 Electron 端到端测试验证。
Element.prototype.scrollIntoView = vi.fn()
HTMLDialogElement.prototype.showModal = function () {
  this.setAttribute('open', '')
}
HTMLDialogElement.prototype.close = function () {
  this.removeAttribute('open')
}
// jsdom 的弹出层不会进入可见顶层；此处仅模拟可见性，层级和裁剪由 Electron 测试覆盖。
HTMLElement.prototype.showPopover = function () {
  this.style.display = 'block'
}
HTMLElement.prototype.hidePopover = function () {
  this.style.display = 'none'
}

window.matchMedia = vi.fn((query: string) => ({
  matches: false,
  media: query,
  onchange: null,
  addEventListener: vi.fn(),
  removeEventListener: vi.fn(),
  addListener: vi.fn(),
  removeListener: vi.fn(),
  dispatchEvent: vi.fn(),
}))
