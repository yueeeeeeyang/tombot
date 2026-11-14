import { useEffect, useRef, useState, type ReactNode } from 'react'
import { z } from 'zod'
import { Link, useParams } from 'react-router'
import { Plus, RefreshCw, ArrowLeft, Server, Boxes, SlidersHorizontal, History } from 'lucide-react'
import { AdminClient, isVerificationCanceled } from '../api/client'
import { Select } from '../components/Select'
import { ModelTestDialog } from './ModelTestDialog'
import { ModelEditor } from './ModelEditor'
import { PolicyEditor, routingLabels } from './PolicyEditor'
import { PullModels, formatModalities } from './PullModels'
import {
  modelsSchema,
  remoteModelsSchema,
  type RemoteModel,
  policiesSchema,
  providersSchema,
  providerSchema,
  policySchema,
  versionsSchema,
  type Policy,
  type Provider,
  type Model,
} from '../api/schemas'
import {
  Alert,
  Dialog,
  Field,
  LoadState,
  Pagination,
  Status,
  pagePath,
  useQuery,
} from '../components'

type FormField = {
  name: string
  label: string
  type?: string
  min?: number
  max?: number
  hint?: string
  defaultValue?: string
  optional?: boolean
  options?: { value: string; label: string }[]
}
type Action = {
  title: string
  description?: string
  path: string
  fields?: FormField[]
  body?: Record<string, unknown>
  transform?: (fields: FormData) => unknown
}

/** 写入按管理员显式确认执行一次；结果未知时保留错误并要求重新读取，不自动补发。 */
function ActionDialog({
  client,
  action,
  onClose,
  onSaved,
  children,
}: {
  client: AdminClient
  action: Action
  onClose: () => void
  onSaved: () => void
  children?: ReactNode
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>()
  const [attempted, setAttempted] = useState(false)
  return (
    <Dialog title={action.title} onClose={onClose} busy={busy}>
      {action.description && <p className="description">{action.description}</p>}
      <form
        onSubmit={async (event) => {
          event.preventDefault()
          if (busy || attempted) return
          const form = event.currentTarget
          const fields = new FormData(form)
          const body = action.transform
            ? action.transform(fields)
            : (action.body ?? Object.fromEntries(fields))
          setBusy(true)
          setError(undefined)
          setAttempted(true)
          // 凭据输入在提交后立即清空，避免错误响应后仍留在 DOM 中。
          for (const element of Array.from(form.elements))
            if (element instanceof HTMLInputElement && element.type === 'password')
              element.value = ''
          try {
            await client.request(action.path, z.unknown(), {
              method: 'POST',
              body,
              timeout: 65_000,
            })
            onSaved()
          } catch (cause) {
            if (isVerificationCanceled(cause)) {
              setAttempted(false)
              setError(undefined)
            } else setError(cause)
          } finally {
            setBusy(false)
          }
        }}
      >
        <fieldset className="form-grid" disabled={busy || attempted}>
          {action.fields?.map((field) => (
            <Field key={field.name} label={field.label} hint={field.hint}>
              {field.options ? (
                <Select
                  name={field.name}
                  label={field.label}
                  required={!field.optional}
                  defaultValue={field.defaultValue ?? ''}
                  options={field.options}
                  disabled={busy || attempted}
                />
              ) : (
                <input
                  name={field.name}
                  type={field.type ?? 'text'}
                  defaultValue={field.defaultValue}
                  required={!field.optional}
                  min={field.min}
                  max={field.max}
                  maxLength={field.type === 'password' ? 8192 : 256}
                  autoComplete={field.type === 'password' ? 'new-password' : 'off'}
                />
              )}
            </Field>
          ))}
        </fieldset>
        {!attempted && children}
        <Alert error={error} />
        {Boolean(error) && (
          <p className="description">关闭后重新读取当前状态，再决定是否提交新操作。</p>
        )}
        <div className="dialog-actions">
          <button type="button" className="secondary-button" disabled={busy} onClick={onClose}>
            {attempted ? '关闭并重新读取' : '取消'}
          </button>
          <button className="primary-button" disabled={busy || attempted}>
            {busy ? '正在处理…' : '确认' + action.title}
          </button>
        </div>
      </form>
    </Dialog>
  )
}

export function ModelsPage({ client }: { client: AdminClient }) {
  const [before, setBefore] = useState('')
  const providers = useQuery(client, pagePath('/model-providers', before), providersSchema)
  const [action, setAction] = useState<Action>()
  return (
    <>
      <header className="page-heading">
        <div>
          <h1>模型供应商</h1>
          <p>统一配置模型与凭据，用户登录后直接使用分配的策略。</p>
        </div>
        <div className="actions">
          <button className="icon-button" aria-label="刷新供应商" onClick={providers.reload}>
            <RefreshCw size={17} />
          </button>
          <button
            className="primary-button"
            onClick={() =>
              setAction({
                title: '登记供应商',
                path: '/model-providers',
                fields: [
                  { name: 'name', label: '名称' },
                  {
                    name: 'protocol',
                    label: '协议',
                    options: [
                      { value: 'responses', label: 'Responses 协议' },
                      { value: 'openai_compat', label: 'Chat Completions 兼容协议' },
                      { value: 'anthropic', label: 'Anthropic 协议' },
                    ],
                  },
                  {
                    name: 'base_url',
                    label: '服务地址',
                    type: 'url',
                    hint: '填写 HTTPS 地址，支持公网、内网和代理地址。',
                  },
                  {
                    name: 'credential',
                    label: '供应商凭据',
                    type: 'password',
                    hint: '服务端加密保存，不会回显。供应商与凭据在同一事务内登记。',
                  },
                ],
              })
            }
          >
            <Plus size={16} />
            登记供应商
          </button>
        </div>
      </header>
      {!providers.value ? (
        <LoadState error={providers.error} onRetry={providers.reload} />
      ) : providers.value.providers.length === 0 ? (
        <div className="empty-state">
          <Server size={28} />
          <h2>暂无模型供应商</h2>
          <p>登记供应商后，可拉取并配置模型。</p>
          <Pagination before={before} onBefore={setBefore} count={0} last={undefined} />
        </div>
      ) : (
        <div className="table-card">
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>供应商</th>
                  <th>协议</th>
                  <th>状态</th>
                  <th>操作</th>
                </tr>
              </thead>
              <tbody>
                {providers.value.providers.map((provider) => (
                  <tr key={provider.id}>
                    <td>
                      <strong>{provider.name}</strong>
                      <small>{provider.base_url}</small>
                    </td>
                    <td>
                      {
                        {
                          anthropic: 'Anthropic',
                          openai_compat: 'Chat Completions',
                          responses: 'Responses',
                        }[provider.protocol]
                      }
                    </td>
                    <td>
                      <Status value={provider.is_enabled ? 'active' : 'disabled'} />
                    </td>
                    <td>
                      <div className="actions">
                        <Link className="text-button" to={`/model-providers/${provider.id}`}>
                          模型管理
                        </Link>
                        {!provider.is_enabled && (
                          <button
                            className="text-button"
                            onClick={() =>
                              setAction({
                                title: '编辑供应商',
                                description:
                                  '保存后仍保持停用。修改协议或地址后，原策略需要创建并启用新版本。',
                                path: `/model-providers/${provider.id}/edit`,
                                fields: [
                                  { name: 'name', label: '名称', defaultValue: provider.name },
                                  {
                                    name: 'protocol',
                                    label: '协议',
                                    defaultValue: provider.protocol,
                                    options: [
                                      { value: 'responses', label: 'Responses 协议' },
                                      {
                                        value: 'openai_compat',
                                        label: 'Chat Completions 兼容协议',
                                      },
                                      { value: 'anthropic', label: 'Anthropic 协议' },
                                    ],
                                  },
                                  {
                                    name: 'base_url',
                                    label: '服务地址',
                                    type: 'url',
                                    defaultValue: provider.base_url,
                                    hint: '填写 HTTPS API 基础地址，不包含 /responses 或 /models 端点。',
                                  },
                                  {
                                    name: 'credential',
                                    label: '新供应商凭据',
                                    type: 'password',
                                    optional: true,
                                    hint: '留空保留现有凭据。更换主机或端口必须填写新凭据，旧凭据将被撤销。',
                                  },
                                ],
                                transform: (fields) => ({
                                  expected_revision: provider.revision,
                                  name: fields.get('name'),
                                  protocol: fields.get('protocol'),
                                  base_url: fields.get('base_url'),
                                  credential: fields.get('credential'),
                                }),
                              })
                            }
                          >
                            编辑
                          </button>
                        )}
                        <button
                          className="text-button"
                          onClick={() =>
                            setAction({
                              title: provider.is_enabled ? '停用供应商' : '启用供应商',
                              description:
                                '停用后使用该供应商的调用将被拒绝。已有版本保持原有配置。',
                              path: `/model-providers/${provider.id}/state`,
                              body: {
                                expected_revision: provider.revision,
                                is_enabled: !provider.is_enabled,
                              },
                            })
                          }
                        >
                          {provider.is_enabled ? '停用' : '启用'}
                        </button>
                        {!provider.is_enabled && (
                          <button
                            className="text-button"
                            onClick={() =>
                              setAction({
                                title: '删除供应商',
                                description:
                                  '删除会一并移除该供应商的模型与凭据，且无法恢复。被策略版本引用时服务端会拒绝删除。',
                                path: `/model-providers/${provider.id}/delete`,
                                body: { expected_revision: provider.revision },
                              })
                            }
                          >
                            删除
                          </button>
                        )}
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <Pagination
            before={before}
            onBefore={setBefore}
            count={providers.value.providers.length}
            last={providers.value.providers.at(-1)?.id}
          />
        </div>
      )}
      {action && (
        <ActionDialog
          client={client}
          action={action}
          onClose={() => {
            setAction(undefined)
            providers.reload()
          }}
          onSaved={() => {
            setAction(undefined)
            providers.reload()
          }}
        />
      )}
    </>
  )
}

const providerResult = z.object({ provider: providerSchema })
const policyResult = z.object({ policy: policySchema })

export function ProviderPage({ client }: { client: AdminClient }) {
  const { providerID } = useParams()
  const result = useQuery(client, `/model-providers/${providerID}`, providerResult)
  return result.value ? (
    <ProviderDetail
      key={result.value.provider.id}
      client={client}
      provider={result.value.provider}
    />
  ) : (
    <LoadState error={result.error} onRetry={result.reload} />
  )
}

function ProviderDetail({ client, provider }: { client: AdminClient; provider: Provider }) {
  const [modelBefore, setModelBefore] = useState('')
  const models = useQuery(
    client,
    pagePath(`/model-providers/${provider.id}/models`, modelBefore),
    modelsSchema,
  )
  const [action, setAction] = useState<Action>()
  const [offered, setOffered] = useState<RemoteModel[]>()
  const [pulling, setPulling] = useState(false)
  const [pullError, setPullError] = useState<unknown>()
  const pullRequest = useRef<AbortController | null>(null)
  const pullTrigger = useRef<HTMLButtonElement>(null)
  useEffect(() => () => pullRequest.current?.abort(), [])
  async function pullModels() {
    if (pullRequest.current || !models.value) return
    const controller = new AbortController()
    pullRequest.current = controller
    setPulling(true)
    setPullError(undefined)
    try {
      const result = await client.request(
        `/model-providers/${provider.id}/remote-models`,
        remoteModelsSchema,
        { signal: controller.signal, timeout: 30_000 },
      )
      if (!controller.signal.aborted) setOffered(result.models)
    } catch (cause) {
      if (!controller.signal.aborted) setPullError(cause)
    } finally {
      if (!controller.signal.aborted) setPulling(false)
      if (pullRequest.current === controller) pullRequest.current = null
    }
  }
  const [testing, setTesting] = useState<Model>()
  const [editing, setEditing] = useState<Model>()
  function close() {
    setAction(undefined)
    models.reload()
  }
  return (
    <>
      <Link className="text-button back-button" to="/model-providers">
        <ArrowLeft size={16} />
        全部供应商
      </Link>
      <header className="page-heading">
        <div>
          <h1>{provider.name}</h1>
          <p>{provider.base_url}</p>
        </div>
      </header>
      <section>
        <div className="section-heading">
          <h2>模型目录</h2>
          <div className="actions">
            <button
              className="secondary-button"
              ref={pullTrigger}
              onClick={() => void pullModels()}
              disabled={pulling}
              aria-disabled={pulling || !models.value}
              aria-busy={pulling}
            >
              {pulling ? '正在拉取…' : '从供应商拉取'}
            </button>
            <button
              className="secondary-button"
              onClick={() =>
                setAction({
                  title: '登记模型',
                  path: `/model-providers/${provider.id}/models`,
                  fields: [
                    { name: 'model_key', label: '模型标识' },
                    { name: 'display_name', label: '显示名称' },
                    {
                      name: 'context_limit',
                      label: '上下文上限',
                      type: 'number',
                      min: 1,
                      max: 100_000_000,
                    },
                    {
                      name: 'output_limit',
                      label: '输出上限',
                      type: 'number',
                      min: 1,
                      max: 100_000_000,
                    },
                    {
                      name: 'supports_tools',
                      label: '工具调用',
                      options: [
                        { value: 'true', label: '支持' },
                        { value: 'false', label: '不支持' },
                      ],
                    },
                    {
                      name: 'supports_vision',
                      label: '图像输入',
                      options: [
                        { value: 'true', label: '支持' },
                        { value: 'false', label: '不支持' },
                      ],
                    },
                  ],
                  transform: (fields) => ({
                    model_key: fields.get('model_key'),
                    display_name: fields.get('display_name'),
                    context_limit: Number(fields.get('context_limit')),
                    output_limit: Number(fields.get('output_limit')),
                    supports_tools: fields.get('supports_tools') === 'true',
                    supports_vision: fields.get('supports_vision') === 'true',
                  }),
                })
              }
            >
              登记模型
            </button>
          </div>
        </div>
        <Alert error={pullError} />
        {!models.value ? (
          <LoadState error={models.error} onRetry={models.reload} />
        ) : models.value.models.length === 0 ? (
          <div className="empty-state">
            <Boxes size={28} />
            <h2>暂无模型</h2>
            <p>从供应商拉取或手动登记模型后，可配置并测试模型能力。</p>
            <Pagination before={modelBefore} onBefore={setModelBefore} count={0} last={undefined} />
          </div>
        ) : (
          <div className="table-card">
            <div className="table-scroll">
              <table>
                <thead>
                  <tr>
                    <th>模型</th>
                    <th>上下文 / 输出</th>
                    <th>能力</th>
                    <th>操作</th>
                  </tr>
                </thead>
                <tbody>
                  {models.value.models.map((model) => (
                    <tr key={model.id}>
                      <td>
                        <strong>{model.display_name}</strong>
                        <small>{model.model_key}</small>
                        <Status value={model.is_enabled ? 'active' : 'disabled'} />
                      </td>
                      <td>
                        {model.context_limit.toLocaleString()} /{' '}
                        {model.output_limit.toLocaleString()}
                      </td>
                      <td>
                        {[model.supports_tools && '工具', model.supports_vision && '图像']
                          .filter(Boolean)
                          .join('、') || '文本'}
                        <small>
                          供应商输入：{formatModalities(model.metadata.input_modalities)}
                        </small>
                        <small>
                          供应商输出：{formatModalities(model.metadata.output_modalities)}
                        </small>
                        <small>思考级别：{model.reasoning_effort || '供应商默认'}</small>
                      </td>
                      <td>
                        <button className="text-button" onClick={() => setTesting(model)}>
                          测试模型
                        </button>
                        <button
                          className="text-button"
                          onClick={() =>
                            setAction({
                              title: model.is_enabled ? '停用模型' : '启用模型',
                              path: `/models/${model.id}/state`,
                              body: {
                                expected_revision: model.revision,
                                is_enabled: !model.is_enabled,
                              },
                            })
                          }
                        >
                          {model.is_enabled ? '停用' : '启用'}
                        </button>
                        {!model.is_enabled && (
                          <>
                            <button className="text-button" onClick={() => setEditing(model)}>
                              编辑
                            </button>
                            <button
                              className="text-button"
                              onClick={() =>
                                setAction({
                                  title: '删除模型',
                                  path: `/models/${model.id}/delete`,
                                  description: `确定删除「${model.display_name}」？删除后不可恢复；已被策略版本引用的模型不能删除。`,
                                  body: { expected_revision: model.revision },
                                })
                              }
                            >
                              删除
                            </button>
                          </>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <Pagination
              before={modelBefore}
              onBefore={setModelBefore}
              count={models.value.models.length}
              last={models.value.models.at(-1)?.id}
            />
          </div>
        )}
      </section>
      {action && <ActionDialog client={client} action={action} onClose={close} onSaved={close} />}
      {testing && (
        <ModelTestDialog
          client={client}
          provider={provider}
          model={testing}
          onClose={() => setTesting(undefined)}
        />
      )}
      {editing && (
        <ModelEditor
          client={client}
          model={editing}
          onClose={() => {
            setEditing(undefined)
            models.reload()
          }}
        />
      )}
      {offered && (
        <PullModels
          client={client}
          providerID={provider.id}
          offered={offered}
          returnFocusRef={pullTrigger}
          registered={models.value?.models.map((model) => model.model_key) ?? []}
          onClose={() => setOffered(undefined)}
          onImported={() => {
            setOffered(undefined)
            models.reload()
          }}
        />
      )}
    </>
  )
}

export function PoliciesPage({ client }: { client: AdminClient }) {
  const [before, setBefore] = useState('')
  const policies = useQuery(client, pagePath('/model-policies', before), policiesSchema)
  const [action, setAction] = useState<Action>()
  return (
    <>
      <header className="page-heading">
        <div>
          <h1>模型策略</h1>
          <p>配置多个候选模型和路由方式，按任务能力选择；每个策略只能启用一个版本。</p>
        </div>
        <div className="actions">
          <button className="icon-button" aria-label="刷新策略" onClick={policies.reload}>
            <RefreshCw size={17} />
          </button>
          <button
            className="primary-button"
            onClick={() =>
              setAction({
                title: '创建策略',
                path: '/model-policies',
                fields: [{ name: 'name', label: '策略名称' }],
              })
            }
          >
            <Plus size={16} />
            创建策略
          </button>
        </div>
      </header>
      {!policies.value ? (
        <LoadState error={policies.error} onRetry={policies.reload} />
      ) : policies.value.policies.length === 0 ? (
        <div className="empty-state">
          <SlidersHorizontal size={28} />
          <h2>暂无模型策略</h2>
          <p>创建策略后，可配置候选模型和路由方式。</p>
          <Pagination before={before} onBefore={setBefore} count={0} last={undefined} />
        </div>
      ) : (
        <div className="table-card">
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>策略</th>
                  <th>当前版本</th>
                  <th>状态</th>
                  <th>操作</th>
                </tr>
              </thead>
              <tbody>
                {policies.value.policies.map((policy) => (
                  <tr key={policy.id}>
                    <td>
                      <strong>{policy.name}</strong>
                      {policy.is_default && <small>平台默认策略</small>}
                    </td>
                    <td>{policy.current_version ? `v${policy.current_version}` : '未启用版本'}</td>
                    <td>
                      <Status value={policy.is_enabled ? 'enabled' : 'disabled'} />
                    </td>
                    <td>
                      <div className="actions">
                        <Link className="text-button" to={`/model-policies/${policy.id}/versions`}>
                          版本管理
                        </Link>
                        {!policy.is_default && policy.current_version && policy.is_enabled && (
                          <button
                            className="text-button"
                            onClick={() =>
                              setAction({
                                title: '设为默认策略',
                                description:
                                  '所有继承平台配置的用户和机器人将在新运行中使用此策略。',
                                path: `/model-policies/${policy.id}/default`,
                                body: { expected_revision: policy.revision },
                              })
                            }
                          >
                            设为默认
                          </button>
                        )}
                        <button
                          className="text-button"
                          onClick={() =>
                            setAction({
                              title: policy.is_enabled ? '停用策略' : '启用策略',
                              path: `/model-policies/${policy.id}/state`,
                              body: {
                                expected_revision: policy.revision,
                                is_enabled: !policy.is_enabled,
                              },
                            })
                          }
                        >
                          {policy.is_enabled ? '停用' : '启用'}
                        </button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <Pagination
            before={before}
            onBefore={setBefore}
            count={policies.value.policies.length}
            last={policies.value.policies.at(-1)?.id}
          />
        </div>
      )}
      {action && (
        <ActionDialog
          client={client}
          action={action}
          onClose={() => {
            setAction(undefined)
            policies.reload()
          }}
          onSaved={() => {
            setAction(undefined)
            policies.reload()
          }}
        />
      )}
    </>
  )
}

export function PolicyPage({ client }: { client: AdminClient }) {
  const { policyID } = useParams()
  const result = useQuery(client, `/model-policies/${policyID}`, policyResult)
  return result.value ? (
    <PolicyDetail
      key={result.value.policy.id}
      client={client}
      policy={result.value.policy}
      onChanged={result.reload}
    />
  ) : (
    <LoadState error={result.error} onRetry={result.reload} />
  )
}

function PolicyDetail({
  client,
  policy,
  onChanged,
}: {
  client: AdminClient
  policy: Policy
  onChanged: () => void
}) {
  const [before, setBefore] = useState('')
  const versions = useQuery(
    client,
    pagePath(`/model-policies/${policy.id}/versions`, before),
    versionsSchema,
  )
  const [creating, setCreating] = useState(false)
  const [action, setAction] = useState<Action>()
  // 策略修订可能被其他管理者修改；操作完成后重新读取策略及版本，不在前端推算修订。
  return (
    <>
      <Link className="text-button back-button" to="/model-policies">
        <ArrowLeft size={16} />
        全部策略
      </Link>
      <header className="page-heading">
        <div>
          <h1>{policy.name}</h1>
          <p>版本可随时启用或停用，同一策略只能启用一个版本；切换仅影响新任务。</p>
        </div>
        <button className="primary-button" onClick={() => setCreating(true)}>
          创建策略版本
        </button>
      </header>
      {!policy.is_enabled && (
        <div className="notice policy-disabled-notice">
          <div>
            <strong>策略已停用</strong>
            <p id="policy-disabled-reason">请先启用策略，再启用所需版本。</p>
          </div>
          <button
            className="secondary-button"
            onClick={() =>
              setAction({
                title: '启用策略',
                description: '启用后，该策略下的有效版本可供任务使用。',
                path: `/model-policies/${policy.id}/state`,
                body: { expected_revision: policy.revision, is_enabled: true },
              })
            }
          >
            启用策略
          </button>
        </div>
      )}
      {!versions.value ? (
        <LoadState error={versions.error} onRetry={versions.reload} />
      ) : versions.value.versions.length === 0 ? (
        <div className="empty-state">
          <History size={28} />
          <h2>暂无策略版本</h2>
          <p>创建版本并添加候选模型后，可启用该版本供新任务使用。</p>
          <Pagination before={before} onBefore={setBefore} count={0} last={undefined} />
        </div>
      ) : (
        <div className="table-card">
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>版本</th>
                  <th>路由方式</th>
                  <th>候选模型</th>
                  <th>状态</th>
                  <th>操作</th>
                </tr>
              </thead>
              <tbody>
                {versions.value.versions.map((version) => (
                  <tr key={version.version}>
                    <td>
                      <strong>v{version.version}</strong>
                    </td>
                    <td>{routingLabels[version.routing_mode]}</td>
                    <td>
                      <details className="policy-version-candidates">
                        <summary>
                          {version.candidates.length} 个模型 ·{' '}
                          {version.candidates[0]?.configuration.model_key}
                          {version.candidates.length > 1 ? ' 等' : ''}
                        </summary>
                        {version.candidates.map((candidate) => (
                          <div key={candidate.model_id}>
                            <strong>{candidate.configuration.model_key}</strong>
                            <small>
                              优先级 {candidate.priority}
                              {version.routing_mode === 'weighted'
                                ? ` · 权重 ${candidate.weight}`
                                : ''}{' '}
                              · 输出 {candidate.configuration.output_limit.toLocaleString()}
                              {candidate.configuration.supports_tools ? ' · 工具' : ''}
                              {candidate.configuration.supports_vision ? ' · 图像' : ''}
                            </small>
                          </div>
                        ))}
                      </details>
                    </td>
                    <td>
                      <Status value={version.is_enabled ? 'enabled' : 'disabled'} />
                    </td>
                    <td>
                      <button
                        className="text-button"
                        disabled={!policy.is_enabled && !version.is_enabled}
                        aria-describedby={
                          !policy.is_enabled && !version.is_enabled
                            ? 'policy-disabled-reason'
                            : undefined
                        }
                        onClick={() =>
                          setAction({
                            title: `${version.is_enabled ? '停用' : '启用'}版本 v${version.version}`,
                            description: version.is_enabled
                              ? '停用后新任务无法使用此策略，正在执行的任务保持原版本。可以随时重新启用。'
                              : version.verified_at
                                ? '启用此版本时会自动停用同策略的其他版本，正在执行的任务保持原版本。'
                                : '首次启用将实际测试全部候选的文本、工具和图像能力；全部通过后启用并替换当前版本。测试会产生供应商调用。',
                            path: `/model-policies/${policy.id}/versions/${version.version}/${version.is_enabled ? 'disable' : 'enable'}`,
                            body: { expected_revision: policy.revision },
                          })
                        }
                      >
                        {version.is_enabled ? '停用' : '启用'}
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <Pagination
            before={before}
            onBefore={setBefore}
            count={versions.value.versions.length}
            last={versions.value.versions.at(-1)?.version.toString()}
          />
        </div>
      )}
      {creating && (
        <PolicyEditor
          client={client}
          policy={policy}
          onClose={() => setCreating(false)}
          onSaved={() => {
            setCreating(false)
            versions.reload()
            onChanged()
          }}
        />
      )}
      {action && (
        <ActionDialog
          client={client}
          action={action}
          onClose={() => {
            setAction(undefined)
            versions.reload()
            onChanged()
          }}
          onSaved={() => {
            setAction(undefined)
            versions.reload()
            onChanged()
          }}
        />
      )}
    </>
  )
}
