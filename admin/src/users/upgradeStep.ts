const steps: Record<string, string> = {
  maintenance: '申请节点维护',
  stop: '停止旧节点服务',
  activate: '切换程序和配置',
  start: '启动并检查节点服务',
  finish: '交还维护窗口',
  rollback: '恢复原程序',
  rolled_back: '已恢复原程序',
  download: '下载并核验发布包',
  preflight: '检查安装条件',
  heartbeat: '等待节点心跳',
  verification: '等待节点能力验收',
  verification_quarantined: '节点验收已隔离',
  computer_quarantined: '电脑升级已隔离',
  draining: '排空电脑会话',
  switching: '切换电脑实例',
  node_draining: '节点正在排空',
  awaiting_worker: '等待工作进程领取',
  active_session: '等待活动会话结束',
  computer_upgrade: '等待原电脑升级',
  computer_transition: '等待电脑完成在途操作',
  verification_lease: '等待验收工作交还租约',
}

export function upgradeStep(value: string) {
  const [stage = value, reason] = value.split('/')
  return [steps[stage] ?? stage, reason].filter(Boolean).join(' · ')
}
