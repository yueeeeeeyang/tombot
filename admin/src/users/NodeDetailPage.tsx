import { useEffect, useRef, useState, type ReactNode } from 'react'
import { Link, useParams } from 'react-router'
import { ArrowLeft, Pencil, RefreshCw } from 'lucide-react'
import { upgradeStep } from './upgradeStep'
import { AdminClient, isVerificationCanceled } from '../api/client'
import { nodeDetailSchema, type NodeDetail } from '../api/schemas'
import { Alert, Dialog, LoadState, Status, useQuery } from '../components'
import { NodeEditDialog } from './NodeEditDialog'

const verificationNames: Record<string, string> = {
  checking: '能力检查中',
  cleaning: '清理验收资源',
  succeeded: '验收通过',
  failed: '验收未通过',
  quarantined: '验收已隔离',
}
const failureNames: Record<string, string> = {
  inspection_unconfirmed: '无法确认验收资源状态',
  creation_unconfirmed: '创建结果尚未确认',
  cleanup_unconfirmed: '无法确认验收资源已清理',
  resource_identity_unconfirmed: '资源身份与记录不一致',
  capability_check_failed: '必需的电脑能力未全部通过',
  node_control_changed: '节点运行实例已变化',
  verification_invalidated: '验收证据已失效',
  container_identity_mismatch: '容器身份或镜像布局标签与预期不符',
  container_privileges_mismatch: '容器权限或安全策略与预期不符',
  container_mount_mismatch: '容器挂载与预期不符',
  container_network_mismatch: '容器网络与预期不符',
  container_runtime_mismatch: '容器运行参数与预期不符',
  container_limits_mismatch: '容器资源限制与预期不符',
  computer_probe_failed: '电脑能力探针未全部通过',
  browser_start_unconfirmed: '未收到浏览器启动成功回执',
  browser_start_identity_failed: '浏览器启动进程的身份或隔离状态不符合要求',
  browser_start_request_failed: '浏览器启动授权无效或已过期',
  browser_start_directory_failed: '浏览器目录、权限或独占锁校验失败',
  browser_start_pipe_failed: '浏览器私有通信管道校验失败',
  browser_start_membership_failed: '浏览器进程加入受管资源组失败',
  browser_start_privileges_failed: '浏览器进程降权或降权后的校验失败',
  browser_start_launch_failed: '浏览器引导程序或 Chrome 无法执行',
  browser_start_group_failed: '浏览器资源组控制入口校验失败',
  browser_start_upload_failed: '浏览器文件传输目录初始化失败',
  browser_start_protocol_failed: '浏览器控制协议初始化失败',
  browser_start_sandbox_failed: 'Chrome 无法建立沙箱，请检查宿主命名空间及安全策略',
  browser_start_profile_failed: 'Chrome 配置目录不可用或已被其他实例占用',
  browser_start_process_exited: 'Chrome 在启动确认前已退出',
  browser_start_timeout: '浏览器启动确认超时',
  browser_start_deadline_failed: '浏览器启动期限或宿主与容器的共享时钟校验失败',
  browser_start_budget_failed: '浏览器运行预算超过电脑代理的部署限制',
  browser_start_state_failed: '浏览器启动授权已消费，或代理已停止接单',
  node_operation_unknown: '节点尚未提供具体错误分类',
  node_call_timeout: '节点调用超时',
}
const capabilities = {
  terminal: '终端',
  files: '文件读写',
  process_reaping: '进程回收',
  disk_quota: '磁盘配额',
  network_isolation: '网络隔离',
} as const
function capabilityResult(value?: boolean) {
  if (value === undefined) return '未确认'
  return value ? '已通过' : '未通过'
}
const date = (value?: string | null) => (value ? new Date(value).toLocaleString() : '尚无记录')
const bytes = (value: number) =>
  `${(value / 1024 ** 3).toLocaleString(undefined, { maximumFractionDigits: 2 })} GiB`

export function NodeStatus({ status, verification }: { status: string; verification?: string }) {
  if (
    status !== 'offline' &&
    status !== 'draining' &&
    (verification === 'quarantined' || verification === 'failed')
  )
    return <span className="status status--error">{verificationNames[verification]}</span>
  return <Status value={status} />
}

function DetailItem({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div>
      <dt>{label}</dt>
      <dd>{children}</dd>
    </div>
  )
}

export function NodeDetailPage({ client }: { client: AdminClient }) {
  const { nodeID = '' } = useParams()
  const query = useQuery(client, `/computer-nodes/${nodeID}`, nodeDetailSchema)
  const [retrying, setRetrying] = useState(false)
  const [editing, setEditing] = useState(false)
  const node = query.value
  return (
    <>
      <Link className="back-link" to="/computer-nodes">
        <ArrowLeft size={16} />
        全部节点
      </Link>
      <header className="page-heading">
        <div>
          <h1>{node?.name ?? '节点详情'}</h1>
          <p>查看连接、心跳和电脑能力验收，定位节点未就绪的原因。</p>
        </div>
        <div className="actions">
          <button className="icon-button" aria-label="刷新节点详情" onClick={query.reload}>
            <RefreshCw size={17} />
          </button>
          {node && (
            <button className="primary-button" onClick={() => setEditing(true)}>
              <Pencil size={16} />
              编辑节点
            </button>
          )}
        </div>
      </header>
      {editing && (
        <NodeEditDialog
          client={client}
          nodeID={nodeID}
          onClose={() => setEditing(false)}
          onSaved={() => {
            setEditing(false)
            query.reload()
          }}
        />
      )}
      {!node ? (
        <LoadState error={query.error} onRetry={query.reload} />
      ) : (
        <>
          <section
            className={`node-diagnosis ${['quarantined', 'verification_failed', 'offline'].includes(node.diagnosis.code) ? 'node-diagnosis--error' : ''}`}
            aria-label="节点状态"
          >
            <div>
              <NodeStatus status={node.status} verification={node.verification?.state} />
              <p>{node.diagnosis.message}</p>
              {node.upgrade_step && <p>升级阶段 · {upgradeStep(node.upgrade_step)}</p>}
            </div>
            {node.can_retry_verification && (
              <button className="secondary-button" onClick={() => setRetrying(true)}>
                重新核对验收
              </button>
            )}
          </section>
          <div className="node-detail-grid">
            <section className="node-panel">
              <h2>基本信息</h2>
              <dl className="node-properties">
                <DetailItem label="节点标识">
                  <code>{node.id}</code>
                </DetailItem>
                <DetailItem label="接入时间">{date(node.created_at)}</DetailItem>
                <DetailItem label="准入容量">{node.capacity} 台</DetailItem>
                <DetailItem label="已预留">{node.reserved} 台</DetailItem>
                <DetailItem label="接收新电脑">
                  {node.is_draining ? '已暂停' : '允许（需通过验收）'}
                </DetailItem>
              </dl>
            </section>
            <section className="node-panel">
              <h2>连接与心跳</h2>
              <dl className="node-properties">
                <DetailItem label="节点 HTTPS 地址">
                  <code>{node.url ?? '尚未绑定连接'}</code>
                </DetailItem>
                <DetailItem label="最近成功心跳">{date(node.last_seen_at)}</DetailItem>
                <DetailItem label="心跳有效期">{date(node.control_expires_at)}</DetailItem>
                <DetailItem label="运行代次">{node.generation}</DetailItem>
                <DetailItem label="心跳序号">{node.control_sequence}</DetailItem>
                <DetailItem label="运行实例">
                  <code>{node.runtime_id ?? '尚未确认'}</code>
                </DetailItem>
                <DetailItem label="证书 SHA-256 指纹">
                  <code>{node.certificate_fingerprint ?? '尚未绑定'}</code>
                </DetailItem>
              </dl>
            </section>
            <section className="node-panel node-panel--wide">
              <h2>电脑能力验收</h2>
              {!node.verification ? (
                <p className="description">尚无验收记录，首次心跳确认后自动开始。</p>
              ) : (
                <>
                  <dl className="node-properties node-properties--columns">
                    <DetailItem label="验收阶段">
                      {verificationNames[node.verification.state] ?? node.verification.state}
                    </DetailItem>
                    <DetailItem label="最近更新">{date(node.verification.updated_at)}</DetailItem>
                    <DetailItem label="当前阶段尝试次数">{node.verification.attempts}</DetailItem>
                    <DetailItem label="能力有效期">{date(node.capability_expires_at)}</DetailItem>
                    <DetailItem label="最近通过时间">
                      {date(node.verification.verified_at)}
                    </DetailItem>
                    <DetailItem label="下次核对">
                      {node.verification.state === 'quarantined'
                        ? '已停止自动重试'
                        : date(node.verification.next_attempt_at)}
                    </DetailItem>
                  </dl>
                  {node.verification.error_code && (
                    <div className="node-failure">
                      <strong>
                        {failureNames[node.verification.error_code] ?? '节点验收未完成'}
                      </strong>
                      <code>{node.verification.error_code}</code>
                      {node.verification.node_error_code && (
                        <p>
                          {failureNames[node.verification.node_error_code] ?? '节点返回的错误分类'}
                          ：<code>{node.verification.node_error_code}</code>
                        </p>
                      )}
                      {!node.verification.node_error_code && (
                        <p>此记录未包含节点错误码，可结合节点服务日志定位。</p>
                      )}
                    </div>
                  )}
                  <ul className="node-capabilities">
                    {Object.entries(capabilities).map(([key, label]) => (
                      <li key={key}>
                        <span>{label}</span>
                        <strong>
                          {capabilityResult(
                            node.verification?.capabilities?.[key as keyof typeof capabilities],
                          )}
                        </strong>
                      </li>
                    ))}
                    {(node.verification.capabilities?.browser !== undefined ||
                      node.verification.node_error_code?.startsWith('browser_')) && (
                      <li>
                        <span>历史浏览器探针</span>
                        <strong>{capabilityResult(node.verification.capabilities?.browser)}</strong>
                      </li>
                    )}
                  </ul>
                  <p className="description">
                    基础能力以当前心跳和验收有效期为准。Chrome 由各 Bot 在自己的桌面按需启动。
                  </p>
                  <p className="description">
                    验收电脑资源：{node.verification.resources.cpu_millis / 1000} 核 · 内存{' '}
                    {bytes(node.verification.resources.memory_bytes)} · 磁盘{' '}
                    {bytes(node.verification.resources.disk_bytes)} · 最多{' '}
                    {node.verification.resources.process_limit} 个进程
                  </p>
                </>
              )}
            </section>
            <section className="node-panel node-panel--wide">
              <h2>电脑镜像</h2>
              {node.image ? (
                <dl className="node-properties">
                  <DetailItem label="镜像标识">
                    <code>{node.image.id}</code>
                  </DetailItem>
                  <DetailItem label="固定镜像引用">
                    <code>{node.image.reference}</code>
                  </DetailItem>
                  <DetailItem label="镜像状态">
                    {node.image.is_enabled ? '已启用' : '已停用'}
                  </DetailItem>
                </dl>
              ) : (
                <p className="description">尚未绑定镜像。</p>
              )}
            </section>
          </div>
          {retrying && (
            <RetryVerification
              client={client}
              node={node}
              onClose={() => setRetrying(false)}
              onSaved={() => {
                setRetrying(false)
                query.reload()
              }}
            />
          )}
        </>
      )}
    </>
  )
}

function RetryVerification({
  client,
  node,
  onClose,
  onSaved,
}: {
  client: AdminClient
  node: NodeDetail
  onClose: () => void
  onSaved: () => void
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>()
  const [attempted, setAttempted] = useState(false)
  const request = useRef<AbortController | null>(null)
  useEffect(() => () => request.current?.abort(), [])
  return (
    <Dialog title="重新核对验收" onClose={onClose} busy={busy}>
      <p className="description">
        请先修复节点故障。系统会先核对并清理原验收资源，再重新执行能力验收。原资源未确认清理前不会新建替代电脑。
      </p>
      <Alert error={error} />
      <div className="dialog-actions">
        <button className="secondary-button" onClick={onClose} disabled={busy}>
          取消
        </button>
        <button
          className="primary-button"
          disabled={busy || attempted}
          onClick={async () => {
            const controller = new AbortController()
            request.current = controller
            setBusy(true)
            setAttempted(true)
            try {
              await client.request(
                `/computer-nodes/${node.id}/verification-retry`,
                nodeDetailSchema,
                {
                  method: 'POST',
                  body: { fencing_token: node.verification?.fencing_token },
                  signal: controller.signal,
                },
              )
              if (!controller.signal.aborted) onSaved()
            } catch (cause) {
              if (!controller.signal.aborted) {
                setError(cause)
                if (isVerificationCanceled(cause)) setAttempted(false)
              }
            } finally {
              if (!controller.signal.aborted) setBusy(false)
            }
          }}
        >
          {busy ? '正在提交…' : '开始核对'}
        </button>
      </div>
    </Dialog>
  )
}
