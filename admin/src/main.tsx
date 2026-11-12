import { createRoot } from 'react-dom/client'
import { App } from './App'
import { AdminClient } from './api/client'
import './styles.css'

const root = document.getElementById('root')
if (!root) throw new Error('管理平台缺少挂载节点')
createRoot(root).render(<App client={new AdminClient()} />)
