import { useState } from 'react'
import { z } from 'zod'
import { Plus, Trash2 } from 'lucide-react'
import { AdminClient, ApiError, isVerificationCanceled } from '../api/client'
import {
  credentialsSchema,
  modelsSchema,
  providersSchema,
  versionSchema,
  type Model,
  type Policy,
} from '../api/schemas'
import { Alert, Dialog, Field, LoadState, pagePath, useQuery } from '../components'
import { Select } from '../components/Select'

export const routingOptions = [
  { value: 'priority', label: '优先级主备' },
  { value: 'weighted', label: '加权分配' },
  { value: 'round_robin', label: '轮询' },
]
export const routingLabels: Record<string, string> = Object.fromEntries(
  routingOptions.map((option) => [option.value, option.label]),
)
const routingHints: Record<string, string> = {
  priority: '优先级数值越小越优先，相同优先级按添加顺序选择。',
  weighted: '新任务按符合要求的候选权重随机分配；权重越大，被选中的机会越高。',
  round_robin: '新任务按候选顺序依次分配，进度在服务重启后保留。',
}
type Credential = z.infer<typeof credentialsSchema>['credentials'][number]
type Choice = {
  model: Model
  providerName: string
  credentials: Credential[]
  priority: number
  weight: number
  outputLimit: string
  effort: string
  credential: string
}

function effortOptions(model: Model) {
  return [
    {
      value: 'inherit',
      label: `继承模型配置${model.reasoning_effort ? `（${model.reasoning_effort}）` : ''}`,
    },
    { value: '', label: '不发送思考级别' },
    ...(model.metadata.effort?.supported_levels ?? []).map((value) => ({ value, label: value })),
  ]
}

/** 版本一次保存候选和覆盖参数；空覆盖由服务端继承，提交结果未知时不补发。 */
export function PolicyEditor({
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
  const [mode, setMode] = useState('priority')
  const [choices, setChoices] = useState<Choice[]>([])
  const [busy, setBusy] = useState(false)
  const [attempted, setAttempted] = useState(false)
  const [error, setError] = useState<unknown>()
  const disabled = busy || attempted
  const totalWeight = choices.reduce((total, choice) => total + choice.weight, 0)
  const update = (id: string, fields: Partial<Choice>) =>
    setChoices((current) =>
      current.map((choice) => (choice.model.id === id ? { ...choice, ...fields } : choice)),
    )
  return (
    <Dialog title="创建策略版本" onClose={onClose} busy={busy} className="policy-dialog">
      <form
        className="policy-editor"
        onSubmit={async (event) => {
          event.preventDefault()
          if (disabled) return
          setError(undefined)
          try {
            if (!choices.length) throw new ApiError('INPUT_INVALID', '请至少添加一个候选模型。')
            if (
              choices.some(
                (choice) =>
                  choice.outputLimit &&
                  (!Number.isInteger(Number(choice.outputLimit)) ||
                    Number(choice.outputLimit) < 1 ||
                    Number(choice.outputLimit) > choice.model.output_limit),
              )
            )
              throw new ApiError('INPUT_INVALID', '输出上限不能超过对应模型的配置。')
            setBusy(true)
            setAttempted(true)
            await client.request(
              `/model-policies/${policy.id}/versions`,
              z.object({ version: versionSchema }),
              {
                method: 'POST',
                body: {
                  routing_mode: mode,
                  candidates: choices.map((choice) => ({
                    model_id: choice.model.id,
                    priority: choice.priority,
                    weight: choice.weight,
                    ...(choice.outputLimit ? { output_limit: Number(choice.outputLimit) } : {}),
                    ...(choice.effort !== 'inherit' ? { reasoning_effort: choice.effort } : {}),
                    ...(choice.credential !== 'inherit'
                      ? { credential_id: choice.credential }
                      : {}),
                  })),
                },
              },
            )
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
        <div className="policy-editor-body">
          <p className="description">
            添加候选模型并选择分配方式。参数默认继承模型配置，保存后处于停用状态，首次启用时测试全部候选。
          </p>
          <Field label="路由方式">
            <Select
              label="路由方式"
              value={mode}
              onChange={setMode}
              options={routingOptions}
              disabled={disabled}
            />
          </Field>
          <p className="field-hint">
            {routingHints[mode]} 自动排除不支持所需工具、图像或上下文容量不足的候选。
          </p>
          <div className="policy-candidates-heading">
            <strong>候选模型</strong>
            <span>{choices.length} / 16</span>
          </div>
          {choices.map((choice, index) => (
            <section
              className="policy-candidate"
              key={choice.model.id}
              aria-label={`候选 ${choice.model.display_name}`}
            >
              <div className="policy-candidate-heading">
                <span className="policy-candidate-order">{index + 1}</span>
                <div>
                  <strong>{choice.model.display_name}</strong>
                  <small>
                    {choice.providerName} · {choice.model.model_key}
                  </small>
                </div>
                <button
                  type="button"
                  className="icon-button"
                  disabled={disabled}
                  aria-label={`移除 ${choice.model.display_name}`}
                  onClick={() =>
                    setChoices((current) => current.filter((value) => value !== choice))
                  }
                >
                  <Trash2 size={16} />
                </button>
              </div>
              <div className="policy-candidate-facts">
                <span>{choice.model.supports_tools ? '支持工具' : '文本对话'}</span>
                {choice.model.supports_vision && <span>支持图像</span>}
                <span>上下文 {choice.model.context_limit.toLocaleString()}</span>
                <span>
                  思考级别：
                  {
                    effortOptions(choice.model).find((option) => option.value === choice.effort)
                      ?.label
                  }
                </span>
              </div>
              <div className="form-grid">
                <Field label="优先级" hint="数值越小越优先，也用于备用切换。">
                  <input
                    aria-label={`${choice.model.display_name} 优先级`}
                    type="number"
                    min={1}
                    max={1000}
                    required
                    value={choice.priority}
                    disabled={disabled}
                    onChange={(event) =>
                      update(choice.model.id, { priority: Number(event.target.value) })
                    }
                  />
                </Field>
                {mode === 'weighted' && (
                  <Field
                    label="分配权重"
                    hint={`全部候选可用时约 ${totalWeight ? Math.round((choice.weight / totalWeight) * 100) : 0}%`}
                  >
                    <input
                      aria-label={`${choice.model.display_name} 分配权重`}
                      type="number"
                      min={1}
                      max={1000}
                      required
                      value={choice.weight}
                      disabled={disabled}
                      onChange={(event) =>
                        update(choice.model.id, { weight: Number(event.target.value) })
                      }
                    />
                  </Field>
                )}
              </div>
              <details className="policy-overrides">
                <summary>
                  参数设置{' '}
                  <span>
                    {choice.outputLimit ||
                    choice.effort !== 'inherit' ||
                    choice.credential !== 'inherit'
                      ? '已单独设置'
                      : '继承模型配置'}
                  </span>
                </summary>
                <div className="form-grid">
                  <Field
                    label="输出上限"
                    hint={`默认 ${choice.model.output_limit.toLocaleString()} Token，留空继承。`}
                  >
                    <input
                      aria-label={`${choice.model.display_name} 输出上限`}
                      type="number"
                      min={1}
                      max={choice.model.output_limit}
                      placeholder={String(choice.model.output_limit)}
                      value={choice.outputLimit}
                      disabled={disabled}
                      onChange={(event) =>
                        update(choice.model.id, { outputLimit: event.target.value })
                      }
                    />
                  </Field>
                  <Field label="思考级别">
                    <Select
                      label={`${choice.model.display_name} 思考级别`}
                      value={choice.effort}
                      disabled={disabled}
                      onChange={(effort) => update(choice.model.id, { effort })}
                      options={effortOptions(choice.model)}
                    />
                  </Field>
                  <Field label="供应商凭据">
                    <Select
                      label={`${choice.model.display_name} 供应商凭据`}
                      value={choice.credential}
                      disabled={disabled}
                      onChange={(credential) => update(choice.model.id, { credential })}
                      options={[
                        { value: 'inherit', label: '继承供应商最新有效凭据' },
                        ...choice.credentials.map((value) => ({
                          value: value.id,
                          label: `凭据版本 v${value.version}`,
                        })),
                      ]}
                    />
                  </Field>
                </div>
              </details>
            </section>
          ))}
          {choices.length < 16 && (
            <CandidatePicker
              client={client}
              selected={choices.map((choice) => choice.model.id)}
              disabled={disabled}
              onAdd={(model, providerName, credentials, effort) =>
                setChoices((current) => [
                  ...current,
                  {
                    model,
                    providerName,
                    credentials,
                    priority: current.length + 1,
                    weight: 1,
                    outputLimit: '',
                    effort,
                    credential: 'inherit',
                  },
                ])
              }
            />
          )}
          <p className="field-hint">
            任务默认保持选中的模型。无有效输出的暂时性故障可切换备用候选，失败候选不再重试，工具操作不重放。供应商未返回的用量保持未知。
          </p>
        </div>
        <Alert error={error} />
        <div className="dialog-actions">
          <button type="button" className="secondary-button" disabled={busy} onClick={onClose}>
            {attempted ? '关闭并重新读取' : '取消'}
          </button>
          <button className="primary-button" disabled={disabled || !choices.length}>
            {busy ? '保存中…' : '保存版本'}
          </button>
        </div>
      </form>
    </Dialog>
  )
}

function CandidatePicker({
  client,
  selected,
  disabled,
  onAdd,
}: {
  client: AdminClient
  selected: string[]
  disabled: boolean
  onAdd: (model: Model, providerName: string, credentials: Credential[], effort: string) => void
}) {
  const [before, setBefore] = useState('')
  const providers = useQuery(client, pagePath('/model-providers', before), providersSchema)
  const [providerID, setProviderID] = useState('')
  const provider = providers.value?.providers.find((value) => value.id === providerID)
  return (
    <div className="policy-picker">
      <strong>添加候选</strong>
      {!providers.value ? (
        <LoadState error={providers.error} onRetry={providers.reload} />
      ) : (
        <>
          <Field label="供应商">
            <Select
              label="候选供应商"
              value={providerID}
              disabled={disabled}
              onChange={setProviderID}
              options={[
                { value: '', label: '选择供应商' },
                ...providers.value.providers
                  .filter((value) => value.is_enabled)
                  .map((value) => ({ value: value.id, label: value.name })),
              ]}
            />
          </Field>
          <PickerPages
            label="供应商"
            disabled={disabled}
            before={before}
            onBefore={(value) => {
              setBefore(value)
              setProviderID('')
            }}
            count={providers.value.providers.length}
            last={providers.value.providers.at(-1)?.id}
          />
          {provider && (
            <ProviderModels
              key={provider.id}
              client={client}
              providerID={provider.id}
              selected={selected}
              disabled={disabled}
              onAdd={(model, credentials, effort) =>
                onAdd(model, provider.name, credentials, effort)
              }
            />
          )}
        </>
      )}
    </div>
  )
}
function ProviderModels({
  client,
  providerID,
  selected,
  disabled,
  onAdd,
}: {
  client: AdminClient
  providerID: string
  selected: string[]
  disabled: boolean
  onAdd: (model: Model, credentials: Credential[], effort: string) => void
}) {
  const [before, setBefore] = useState('')
  const models = useQuery(
    client,
    pagePath(`/model-providers/${providerID}/models`, before),
    modelsSchema,
  )
  const credentials = useQuery(
    client,
    pagePath(`/model-providers/${providerID}/credentials`, ''),
    credentialsSchema,
  )
  const [modelID, setModelID] = useState('')
  const [effort, setEffort] = useState('inherit')
  const model = models.value?.models.find(
    (value) => value.id === modelID && value.is_enabled && !selected.includes(value.id),
  )
  if (!models.value || !credentials.value)
    return (
      <LoadState
        error={models.error || credentials.error}
        onRetry={() => {
          models.reload()
          credentials.reload()
        }}
      />
    )
  return (
    <>
      <div className="policy-add-model">
        <Field label="模型">
          <Select
            label="候选模型"
            value={model ? modelID : ''}
            disabled={disabled}
            onChange={(value) => {
              setModelID(value)
              setEffort('inherit')
            }}
            options={[
              { value: '', label: '选择模型' },
              ...models.value.models
                .filter((value) => value.is_enabled && !selected.includes(value.id))
                .map((value) => ({ value: value.id, label: value.display_name })),
            ]}
          />
        </Field>
        <Field label="思考级别">
          <Select
            label="候选思考级别"
            value={effort}
            disabled={disabled || !model}
            placeholder="先选择模型"
            onChange={setEffort}
            options={model ? effortOptions(model) : []}
          />
        </Field>
        <button
          type="button"
          className="secondary-button"
          disabled={disabled || !model}
          onClick={() => {
            if (model) {
              onAdd(
                model,
                credentials.value!.credentials.filter(
                  (value) => !value.is_revoked && !value.is_destroyed,
                ),
                effort,
              )
              setModelID('')
              setEffort('inherit')
            }
          }}
        >
          <Plus size={16} />
          添加
        </button>
      </div>
      <PickerPages
        label="模型"
        disabled={disabled}
        before={before}
        onBefore={(value) => {
          setBefore(value)
          setModelID('')
          setEffort('inherit')
        }}
        count={models.value.models.length}
        last={models.value.models.at(-1)?.id}
      />
    </>
  )
}

/** 选择器只在需要翻页时显示导航，不带入表格计数和页脚。 */
function PickerPages({
  label,
  before,
  onBefore,
  last,
  count,
  disabled,
}: {
  label: string
  before: string
  onBefore: (value: string) => void
  last: string | undefined
  count: number
  disabled: boolean
}) {
  const hasNext = count === 50 && !!last
  if (!before && !hasNext) return null
  return (
    <div className="policy-picker-pages" role="group" aria-label={`${label}翻页`}>
      {before && (
        <button
          type="button"
          className="text-button"
          disabled={disabled}
          onClick={() => onBefore('')}
        >
          返回{label}首页
        </button>
      )}
      {hasNext && (
        <button
          type="button"
          className="text-button"
          disabled={disabled}
          onClick={() => onBefore(last!)}
        >
          更多{label}
        </button>
      )}
    </div>
  )
}
