import { StrictMode, Component, type ReactNode } from 'react'
import { createRoot } from 'react-dom/client'
import App from './App'
import './styles.css'

/** 捕获界面渲染错误，保留本地存储并给出可操作的恢复入口。 */
class ApplicationBoundary extends Component<{ children: ReactNode }, { hasError: boolean }> {
  state = { hasError: false }
  static getDerivedStateFromError() {
    return { hasError: true }
  }
  componentDidCatch(): void {
    console.error('客户端界面渲染失败，应用=TomBot')
  }
  render() {
    if (this.state.hasError)
      return (
        <main className="fatal-error">
          <h1>界面暂时遇到了一点问题</h1>
          <p>本地草稿仍保存在当前设备。请重新加载后继续。</p>
          <button className="primary-button" onClick={() => location.reload()}>
            重新加载
          </button>
        </main>
      )
    return this.props.children
  }
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <ApplicationBoundary>
      <App />
    </ApplicationBoundary>
  </StrictMode>,
)
