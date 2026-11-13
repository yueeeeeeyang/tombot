import { useRef, useState, type ReactNode } from 'react'
import { z } from 'zod'
import { Plus, RefreshCw, ArrowLeft } from 'lucide-react'
import { AdminClient } from '../api/client'
import {
  credentialsSchema,
  modelsSchema,
  remoteModelsSchema,
  policiesSchema,
  providersSchema,
  versionsSchema,
  type Policy,
  type Provider,
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
            setError(cause)
          } finally {
            setBusy(false)
          }
        }}
      >
        <fieldset className="form-grid" disabled={busy || attempted}>
          {action.fields?.map((field) => (
            <Field key={field.name} label={field.label} hint={field.hint}>
              {field.options ? (
                <select name={field.name} required defaultValue="">
                  <option value="" disabled>
                    请选择
                  </option>
                  {field.options.map((option) => (
                    <option key={option.value} value={option.value}>
                      {option.label}
                    </option>
                  ))}
                </select>
              ) : (
                <input
                  name={field.name}
                  type={field.type ?? 'text'}
                  required
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
  const [selected, setSelected] = useState<Provider>()
  const [action, setAction] = useState<Action>()
  if (selected)
    return (
      <ProviderDetail
        key={selected.id}
        client={client}
        provider={selected}
        onBack={() => {
          setSelected(undefined)
          providers.reload()
        }}
      />
    )
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
                      { value: 'openai_compat', label: 'OpenAI 兼容协议' },
                      { value: 'anthropic', label: 'Anthropic 协议' },
                    ],
                  },
                  {
                    name: 'base_url',
                    label: '服务地址',
                    type: 'url',
                    hint: '填写 HTTPS 地址；服务端会验证部署允许的访问范围。',
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
                    <td>{provider.protocol === 'anthropic' ? 'Anthropic' : 'OpenAI 兼容'}</td>
                    <td>
                      <Status value={provider.is_enabled ? 'active' : 'disabled'} />
                    </td>
                    <td>
                      <div className="actions">
                        <button className="text-button" onClick={() => setSelected(provider)}>
                          模型管理
                        </button>
                        <button
                          className="text-button"
                          onClick={() =>
                            setAction({
                              title: provider.is_enabled ? '停用供应商' : '启用供应商',
                              description:
                                '停用后使用该供应商的调用将被拒绝。已发布版本保持原有配置。',
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
          {providers.value.providers.length === 0 && (
            <p className="empty-state">尚未登记模型供应商。</p>
          )}
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

function ProviderDetail({
  client,
  provider,
  onBack,
}: {
  client: AdminClient
  provider: Provider
  onBack: () => void
}) {
  const [modelBefore, setModelBefore] = useState('')
  const models = useQuery(
    client,
    pagePath(`/model-providers/${provider.id}/models`, modelBefore),
    modelsSchema,
  )
  const [action, setAction] = useState<Action>()
  const [pulling, setPulling] = useState(false)
  function close() {
    setAction(undefined)
    models.reload()
  }
  return (
    <>
      <button className="text-button back-button" onClick={onBack}>
        <ArrowLeft size={16} />
        全部供应商
      </button>
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
            <button className="secondary-button" onClick={() => setPulling(true)}>
              从供应商拉取
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
        {!models.value ? (
          <LoadState error={models.error} onRetry={models.reload} />
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
                      </td>
                      <td>
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
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            {!models.value.models.length && <p className="empty-state">尚未登记模型。</p>}
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
      {pulling && (
        <PullModels
          client={client}
          providerID={provider.id}
          registered={models.value?.models.map((model) => model.model_key) ?? []}
          onClose={() => setPulling(false)}
          onImported={() => {
            setPulling(false)
            models.reload()
          }}
        />
      )}
    </>
  )
}

// PullModels 读取供应商模型列表，勾选后按统一规格登记到目录。
// 上游列表没有上下文与输出上限，因此这些规格必须由管理员显式填写。
function PullModels({
  client,
  providerID,
  registered,
  onClose,
  onImported,
}: {
  client: AdminClient
  providerID: string
  registered: string[]
  onClose: () => void
  onImported: () => void
}) {
  const [models, setModels] = useState<{ model_key: string; display_name: string }[] | null>(null)
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [error, setError] = useState<unknown>()
  const [busy, setBusy] = useState(false)
  const [progress, setProgress] = useState('')
  const form = useRef<HTMLFormElement>(null)
  const known = new Set(registered)

  async function load() {
    setError(undefined)
    setBusy(true)
    try {
      const result = await client.request(
        `/model-providers/${providerID}/remote-models`,
        remoteModelsSchema,
        { timeout: 30_000 },
      )
      setModels(result.models)
      setSelected(
        new Set(
          result.models.filter((item) => !known.has(item.model_key)).map((item) => item.model_key),
        ),
      )
    } catch (cause) {
      setError(cause)
    } finally {
      setBusy(false)
    }
  }

  async function submit() {
    if (busy || !form.current?.reportValidity() || selected.size === 0) return
    const fields = new FormData(form.current)
    const base = {
      context_limit: Number(fields.get('context_limit')),
      output_limit: Number(fields.get('output_limit')),
      supports_tools: fields.get('supports_tools') === 'true',
      supports_vision: fields.get('supports_vision') === 'true',
    }
    setBusy(true)
    setError(undefined)
    let imported = 0
    try {
      for (const item of models ?? []) {
        if (!selected.has(item.model_key)) continue
        setProgress(`正在登记 ${item.model_key}（${imported + 1}/${selected.size}）`)
        await client.request(`/model-providers/${providerID}/models`, modelsSchema, {
          method: 'POST',
          body: {
            ...base,
            provider_id: providerID,
            model_key: item.model_key,
            display_name: item.display_name,
          },
          timeout: 20_000,
        })
        imported++
      }
      onImported()
    } catch (cause) {
      // 已登记的模型保留；失败的模型标识不自动重试，由管理员确认后重新提交。
      setError(cause)
      setProgress(`已登记 ${imported} 个，其余未完成`)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog title="从供应商拉取模型" onClose={onClose} busy={busy}>
      <p className="description">
        读取供应商当前提供的模型标识。上游不提供上下文与输出上限，请填写适用于所选模型的规格。
      </p>
      <Alert error={error} />
      {!models ? (
        <p role="status">
          {busy ? '正在读取供应商模型…' : '点击下方按钮读取供应商当前提供的模型列表。'}
        </p>
      ) : (
        <form
          ref={form}
          onSubmit={(event) => {
            event.preventDefault()
            void submit()
          }}
        >
          <section className="remote-models" aria-label="可选模型">
            {models.length === 0 && <p className="empty-state">供应商没有返回任何模型。</p>}
            {models.map((item) => (
              <label key={item.model_key} className="remote-model">
                <input
                  type="checkbox"
                  checked={selected.has(item.model_key)}
                  disabled={busy || known.has(item.model_key)}
                  onChange={(event) => {
                    const next = new Set(selected)
                    if (event.target.checked) next.add(item.model_key)
                    else next.delete(item.model_key)
                    setSelected(next)
                  }}
                />
                <span>
                  <strong>{item.display_name}</strong>
                  <small>{item.model_key}</small>
                </span>
                {known.has(item.model_key) && <span className="status">已登记</span>}
              </label>
            ))}
          </section>
          <fieldset disabled={busy} className="form-grid">
            <Field label="上下文上限">
              <input
                name="context_limit"
                type="number"
                required
                min={1}
                max={100000000}
                defaultValue={128000}
              />
            </Field>
            <Field label="输出上限">
              <input
                name="output_limit"
                type="number"
                required
                min={1}
                max={100000000}
                defaultValue={8192}
              />
            </Field>
            <Field label="工具调用">
              <select name="supports_tools" defaultValue="true">
                <option value="true">支持</option>
                <option value="false">不支持</option>
              </select>
            </Field>
            <Field label="图像输入">
              <select name="supports_vision" defaultValue="false">
                <option value="true">支持</option>
                <option value="false">不支持</option>
              </select>
            </Field>
          </fieldset>
          {progress && <p role="status">{progress}</p>}
          <div className="dialog-actions">
            <button type="button" className="secondary-button" disabled={busy} onClick={onClose}>
              取消
            </button>
            <button type="submit" className="primary-button" disabled={busy || selected.size === 0}>
              {busy ? '正在登记…' : `登记所选（${selected.size}）`}
            </button>
          </div>
        </form>
      )}
      {!models && !busy && (
        <div className="dialog-actions">
          <button type="button" className="secondary-button" onClick={onClose}>
            取消
          </button>
          <button type="button" className="primary-button" onClick={() => void load()}>
            重新读取
          </button>
        </div>
      )}
    </Dialog>
  )
}

export function PoliciesPage({ client }: { client: AdminClient }) {
  const [before, setBefore] = useState('')
  const policies = useQuery(client, pagePath('/model-policies', before), policiesSchema)
  const [selectedID, setSelectedID] = useState<string>()
  const selected = policies.value?.policies.find((policy) => policy.id === selectedID)
  const [action, setAction] = useState<Action>()
  if (selected)
    return (
      <PolicyDetail
        key={selected.id}
        client={client}
        policy={selected}
        onChanged={policies.reload}
        onBack={() => {
          setSelectedID(undefined)
          policies.reload()
        }}
      />
    )
  return (
    <>
      <header className="page-heading">
        <div>
          <h1>模型策略</h1>
          <p>草稿通过能力测试后发布；用户和机器人无需配置模型。</p>
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
                    <td>{policy.current_version ? `v${policy.current_version}` : '尚未发布'}</td>
                    <td>
                      <Status value={policy.is_enabled ? 'active' : 'disabled'} />
                    </td>
                    <td>
                      <div className="actions">
                        <button className="text-button" onClick={() => setSelectedID(policy.id)}>
                          版本与发布
                        </button>
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
          {!policies.value.policies.length && <p className="empty-state">尚未创建模型策略。</p>}
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

function PolicyDetail({
  client,
  policy,
  onBack,
  onChanged,
}: {
  client: AdminClient
  policy: Policy
  onBack: () => void
  onChanged: () => void
}) {
  const [before, setBefore] = useState('')
  const versions = useQuery(
    client,
    pagePath(`/model-policies/${policy.id}/versions`, before),
    versionsSchema,
  )
  const [draft, setDraft] = useState(false)
  const [action, setAction] = useState<Action>()
  // 策略修订可能被其他管理者修改；操作完成后重新读取策略及版本，不在前端推算修订。
  return (
    <>
      <button className="text-button back-button" onClick={onBack}>
        <ArrowLeft size={16} />
        全部策略
      </button>
      <header className="page-heading">
        <div>
          <h1>{policy.name}</h1>
          <p>每次修改创建新版本，已发布的配置保持不变。</p>
        </div>
        <button className="primary-button" onClick={() => setDraft(true)}>
          创建版本草稿
        </button>
      </header>
      {!versions.value ? (
        <LoadState error={versions.error} onRetry={versions.reload} />
      ) : (
        <div className="table-card">
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>版本</th>
                  <th>模型</th>
                  <th>输出上限</th>
                  <th>状态</th>
                  <th>操作</th>
                </tr>
              </thead>
              <tbody>
                {versions.value.versions.map((version) => (
                  <tr key={version.version}>
                    <td>v{version.version}</td>
                    <td>{version.configuration.model_key}</td>
                    <td>{version.configuration.output_limit.toLocaleString()}</td>
                    <td>
                      <Status value={version.status} />
                    </td>
                    <td>
                      {version.status !== 'revoked' && (
                        <button
                          className="text-button"
                          onClick={() =>
                            setAction(
                              version.status === 'draft'
                                ? {
                                    title: '测试并发布版本',
                                    description:
                                      '服务端将实际测试文本、工具和图像能力。全部通过后才发布，并影响使用该策略的新运行。测试会产生供应商调用。',
                                    path: `/model-policies/${policy.id}/versions/${version.version}/publish`,
                                    body: { expected_revision: policy.revision },
                                  }
                                : {
                                    title: '撤销版本',
                                    description:
                                      '撤销后依赖该版本的调用将被拒绝。已记录的配置和用量仍保留。',
                                    path: `/model-policies/${policy.id}/versions/${version.version}/revoke`,
                                    body: {},
                                  },
                            )
                          }
                        >
                          {version.status === 'draft' ? '测试并发布' : '撤销'}
                        </button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {!versions.value.versions.length && <p className="empty-state">尚未创建策略版本。</p>}
          <Pagination
            before={before}
            onBefore={setBefore}
            count={versions.value.versions.length}
            last={versions.value.versions.at(-1)?.version.toString()}
          />
        </div>
      )}
      {draft && (
        <DraftDialog client={client} policy={policy} onClose={onChanged} onSaved={onChanged} />
      )}
      {action && (
        <ActionDialog client={client} action={action} onClose={onChanged} onSaved={onChanged} />
      )}
    </>
  )
}

function DraftDialog({
  client,
  policy,
  onClose,
  onSaved,
}: {
  client: AdminClient
  policy: Policy
  onClose: () => void
  onSaved: () => void
}) {
  const [before, setBefore] = useState('')
  const providers = useQuery(client, pagePath('/model-providers', before), providersSchema)
  const [provider, setProvider] = useState('')
  if (provider)
    return (
      <DraftForm
        client={client}
        policy={policy}
        provider={provider}
        onClose={onClose}
        onSaved={onSaved}
      />
    )
  return (
    <Dialog title="选择模型供应商" onClose={onClose}>
      {!providers.value ? (
        <LoadState error={providers.error} onRetry={providers.reload} />
      ) : (
        <>
          <div className="choice-list">
            {providers.value.providers.map((value) => (
              <button
                className="secondary-button"
                key={value.id}
                disabled={!value.is_enabled}
                onClick={() => setProvider(value.id)}
              >
                {value.name}
                {!value.is_enabled && '（已停用）'}
              </button>
            ))}
          </div>
          {!providers.value.providers.length && (
            <p className="empty-state">请先登记供应商和模型。</p>
          )}
          <Pagination
            before={before}
            onBefore={setBefore}
            count={providers.value.providers.length}
            last={providers.value.providers.at(-1)?.id}
          />
        </>
      )}
    </Dialog>
  )
}

function DraftForm({
  client,
  policy,
  provider,
  onClose,
  onSaved,
}: {
  client: AdminClient
  policy: Policy
  provider: string
  onClose: () => void
  onSaved: () => void
}) {
  const [modelBefore, setModelBefore] = useState('')
  const [credentialBefore, setCredentialBefore] = useState('')
  const models = useQuery(
    client,
    pagePath(`/model-providers/${provider}/models`, modelBefore),
    modelsSchema,
  )
  const credentials = useQuery(
    client,
    pagePath(`/model-providers/${provider}/credentials`, credentialBefore),
    credentialsSchema,
  )
  if (!models.value || !credentials.value)
    return (
      <Dialog title="读取模型与凭据" onClose={onClose}>
        <LoadState
          error={models.error || credentials.error}
          onRetry={() => {
            models.reload()
            credentials.reload()
          }}
        />
      </Dialog>
    )
  return (
    <div>
      <ActionDialog
        client={client}
        onClose={onClose}
        onSaved={onSaved}
        action={{
          title: '创建版本草稿',
          path: `/model-policies/${policy.id}/versions`,
          description: '选择固定模型和凭据版本。价格尚未录入时，费用保持未知。',
          fields: [
            {
              name: 'model_id',
              label: '模型',
              options: models.value.models
                .filter((model) => model.is_enabled)
                .map((model) => ({
                  value: model.id,
                  label: `${model.display_name} · 输出上限 ${model.output_limit}`,
                })),
            },
            {
              name: 'credential_id',
              label: '凭据版本',
              options: [
                { value: 'none', label: '无需凭据' },
                ...credentials.value.credentials
                  .filter((value) => !value.is_revoked && !value.is_destroyed)
                  .map((value) => ({ value: value.id, label: `v${value.version}` })),
              ],
            },
            {
              name: 'output_limit',
              label: '最大输出 token',
              type: 'number',
              min: 1,
              max: 100_000_000,
            },
          ],
          transform: (fields) => ({
            model_id: fields.get('model_id'),
            credential_id:
              fields.get('credential_id') === 'none' ? null : fields.get('credential_id'),
            output_limit: Number(fields.get('output_limit')),
            prices: {},
          }),
        }}
      >
        <div className="catalog-pages">
          <Pagination
            before={modelBefore}
            onBefore={setModelBefore}
            count={models.value.models.length}
            last={models.value.models.at(-1)?.id}
          />
          <Pagination
            before={credentialBefore}
            onBefore={setCredentialBefore}
            count={credentials.value.credentials.length}
            last={credentials.value.credentials.at(-1)?.version.toString()}
          />
        </div>
      </ActionDialog>
    </div>
  )
}
