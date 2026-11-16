import { createRoot } from 'react-dom/client'
import { App } from './App'
import { AdminClient } from './api/client'
import { UserActivationPage, userActivationPath } from './auth/UserActivationPage'
import './styles.css'

const root = document.getElementById('root')
if (!root) throw new Error('管理平台缺少挂载节点')
const client = new AdminClient()
// 激活是独立公开页面，不启动管理平台的会话恢复或登录跳转。
createRoot(root).render(
  window.location.pathname === userActivationPath ? (
    <UserActivationPage client={client} />
  ) : (
    <App client={client} />
  ),
)
