import { useId, useRef, useState, type RefObject } from 'react'
import { flushSync } from 'react-dom'
import { ChevronDown } from 'lucide-react'
import { z } from 'zod'
import { AdminClient, isVerificationCanceled } from '../api/client'
import { modelSchema, type RemoteModel } from '../api/schemas'
import { Alert, Dialog, Field } from '../components'
import { Select } from '../components/Select'

export function formatModalities(values: string[] | undefined) {
  const labels: Record<string, string> = {
    text: '文本',
    image: '图像',
    audio: '音频',
    video: '视频',
    file: '文件',
  }
  return values?.map((value) => labels[value] ?? value).join('、') || '供应商未提供'
}

type ImportModel = {
  offered: RemoteModel
  selected: boolean
  expanded: boolean
  contextLimit: string
  outputLimit: string
  tools: string
  vision: string
  reasoningEffort: string
}

function supportsText(model: RemoteModel) {
  return (
    (!model.metadata.input_modalities || model.metadata.input_modalities.includes('text')) &&
    (!model.metadata.output_modalities || model.metadata.output_modalities.includes('text'))
  )
}

/** 展开状态与选择状态独立；校验失败先展开对应模型，避免隐藏字段阻止提交却无处修改。 */
export function PullModels({
  client,
  providerID,
  offered,
  registered,
  returnFocusRef,
  onClose,
  onImported,
}: {
  client: AdminClient
  providerID: string
  offered: RemoteModel[]
  registered: string[]
  returnFocusRef: RefObject<HTMLElement | null>
  onClose: () => void
  onImported: () => void
}) {
  const [models, setModels] = useState<ImportModel[]>(() =>
    offered.map((model) => ({
      offered: model,
      selected: !registered.includes(model.model_key) && supportsText(model),
      expanded: false,
      contextLimit: model.context_limit?.toString() ?? '',
      outputLimit: model.output_limit?.toString() ?? '',
      tools: model.supports_tools?.toString() ?? '',
      vision: model.supports_vision?.toString() ?? '',
      reasoningEffort: model.metadata.effort?.default_level ?? '',
    })),
  )
  const [error, setError] = useState<unknown>()
  const [busy, setBusy] = useState(false)
  const [attempted, setAttempted] = useState(false)
  const [progress, setProgress] = useState('')
  const form = useRef<HTMLFormElement>(null)
  const detailsID = useId()
  const known = new Set(registered)
  const selected = models.filter((model) => model.selected)
  function update(key: string, values: Partial<ImportModel>) {
    setModels((current) =>
      current.map((model) => (model.offered.model_key === key ? { ...model, ...values } : model)),
    )
  }
  async function submit() {
    if (busy || attempted || !selected.length || !form.current) return
    const invalid = form.current.querySelector<HTMLInputElement>('input:invalid')
    if (invalid) {
      const key = invalid.closest<HTMLElement>('[data-model-key]')?.dataset.modelKey
      if (key) flushSync(() => update(key, { expanded: true }))
      invalid.reportValidity()
      return
    }
    setBusy(true)
    setAttempted(true)
    setError(undefined)
    let imported = 0
    try {
      for (const model of selected) {
        setProgress(`正在登记 ${model.offered.model_key}（${imported + 1}/${selected.length}）`)
        await client.request(
          `/model-providers/${providerID}/models`,
          z.object({ model: modelSchema }),
          {
            method: 'POST',
            body: {
              model_key: model.offered.model_key,
              display_name: model.offered.display_name,
              context_limit: Number(model.contextLimit),
              output_limit: Number(model.outputLimit),
              supports_tools: model.tools === 'true',
              supports_vision: model.vision === 'true',
              metadata: model.offered.metadata,
              reasoning_effort: model.reasoningEffort,
            },
            timeout: 20_000,
          },
        )
        imported++
      }
      onImported()
    } catch (cause) {
      if (isVerificationCanceled(cause)) {
        setError(undefined)
        if (imported === 0) setAttempted(false)
        setProgress(imported ? `已登记 ${imported} 个，其余未完成` : '')
      } else {
        setError(cause)
        setProgress(`已登记 ${imported} 个，其余未完成`)
      }
    } finally {
      setBusy(false)
    }
  }
  return (
    <Dialog
      title="从供应商拉取模型"
      className="model-import-dialog"
      returnFocusRef={returnFocusRef}
      busy={busy}
      onClose={attempted ? onImported : onClose}
    >
      <p className="description">选择要登记的模型，展开查看或调整参数。</p>
      <Alert error={error} />
      <form
        ref={form}
        noValidate
        onSubmit={(event) => {
          event.preventDefault()
          void submit()
        }}
      >
        <section className="model-import-list" aria-label="可选模型">
          {models.length === 0 && <p className="empty-state">供应商没有返回任何模型。</p>}
          {models.map((model, index) => {
            const { offered } = model
            const label = (name: string) => `${offered.display_name} · ${name}`
            const disabled = busy || attempted || !model.selected
            return (
              <article
                key={offered.model_key}
                data-model-key={offered.model_key}
                className="model-import-card"
              >
                <div className="model-import-header">
                  <input
                    type="checkbox"
                    aria-label={`选择 ${offered.display_name}`}
                    checked={model.selected}
                    disabled={
                      busy || attempted || known.has(offered.model_key) || !supportsText(offered)
                    }
                    onChange={(event) =>
                      update(offered.model_key, { selected: event.target.checked })
                    }
                  />
                  <button
                    type="button"
                    className="model-import-toggle"
                    aria-expanded={model.expanded}
                    aria-controls={`${detailsID}-${index}`}
                    aria-label={`${model.expanded ? '收起' : '展开'} ${offered.display_name} 详细信息`}
                    onClick={() => update(offered.model_key, { expanded: !model.expanded })}
                  >
                    <span>
                      <strong>{offered.display_name}</strong>
                      <small>{offered.model_key}</small>
                    </span>
                    {known.has(offered.model_key) && <span className="status">已登记</span>}
                    <ChevronDown size={16} aria-hidden="true" />
                  </button>
                </div>
                <div
                  id={`${detailsID}-${index}`}
                  className="model-import-details"
                  hidden={!model.expanded}
                >
                  <dl className="model-import-media">
                    <div>
                      <dt>输入类型</dt>
                      <dd>{formatModalities(offered.metadata.input_modalities)}</dd>
                    </div>
                    <div>
                      <dt>输出类型</dt>
                      <dd>{formatModalities(offered.metadata.output_modalities)}</dd>
                    </div>
                  </dl>
                  {!supportsText(offered) && (
                    <p className="description">该模型不提供文本对话，暂不能登记。</p>
                  )}
                  <fieldset className="form-grid" disabled={disabled}>
                    <div className="field-pair">
                      <Field
                        label="上下文窗口"
                        hint={
                          offered.context_limit === undefined
                            ? '供应商未提供，请填写'
                            : 'Token，可调低'
                        }
                      >
                        <input
                          aria-label={label('上下文窗口')}
                          type="number"
                          required
                          min={1}
                          max={offered.context_limit ?? 100000000}
                          value={model.contextLimit}
                          onChange={(event) =>
                            update(offered.model_key, { contextLimit: event.target.value })
                          }
                        />
                      </Field>
                      <Field
                        label="输出上限"
                        hint={
                          offered.output_limit === undefined
                            ? '供应商未提供，请填写'
                            : 'Token，可调低'
                        }
                      >
                        <input
                          aria-label={label('输出上限')}
                          type="number"
                          required
                          min={1}
                          max={Math.min(
                            offered.output_limit ?? 100000000,
                            Number(model.contextLimit) || 100000000,
                          )}
                          value={model.outputLimit}
                          onChange={(event) =>
                            update(offered.model_key, { outputLimit: event.target.value })
                          }
                        />
                      </Field>
                    </div>
                    <div className="field-pair">
                      <Field
                        label="工具调用"
                        hint={
                          offered.supports_tools === undefined ? '供应商未提供，请确认' : undefined
                        }
                      >
                        <Select
                          label={label('工具调用')}
                          required
                          value={model.tools}
                          disabled={disabled}
                          onChange={(value) => update(offered.model_key, { tools: value })}
                          options={[
                            { value: 'true', label: '支持' },
                            { value: 'false', label: '不支持' },
                          ]}
                        />
                      </Field>
                      <Field
                        label="图像输入"
                        hint={
                          offered.supports_vision === undefined ? '供应商未提供，请确认' : undefined
                        }
                      >
                        <Select
                          label={label('图像输入')}
                          required
                          value={model.vision}
                          disabled={disabled}
                          onChange={(value) => update(offered.model_key, { vision: value })}
                          options={[
                            {
                              value: 'true',
                              label: '支持',
                              disabled: offered.supports_vision === false,
                            },
                            { value: 'false', label: '不支持' },
                          ]}
                        />
                      </Field>
                    </div>
                    <Field
                      label="思考级别"
                      hint={
                        offered.metadata.effort ? '选项来自供应商' : '供应商未提供，沿用默认行为'
                      }
                    >
                      <Select
                        label={label('思考级别')}
                        value={model.reasoningEffort}
                        disabled={disabled || !offered.metadata.effort}
                        onChange={(value) => update(offered.model_key, { reasoningEffort: value })}
                        options={[
                          { value: '', label: '供应商默认' },
                          ...(offered.metadata.effort?.supported_levels.map((level) => ({
                            value: level,
                            label:
                              level +
                              (level === offered.metadata.effort?.default_level ? '（默认）' : ''),
                          })) ?? []),
                        ]}
                      />
                    </Field>
                  </fieldset>
                  <p className="model-import-note">
                    媒体类型由供应商提供；当前支持文本、图像输入与文本输出。
                  </p>
                </div>
              </article>
            )
          })}
        </section>
        {progress && <p role="status">{progress}</p>}
        <div className="dialog-actions">
          <button
            type="button"
            className="secondary-button"
            disabled={busy}
            onClick={attempted ? onImported : onClose}
          >
            {attempted ? '关闭并重新读取' : '取消'}
          </button>
          <button
            type="submit"
            className="primary-button"
            disabled={busy || attempted || !selected.length}
          >
            {busy ? '正在登记…' : `登记所选（${selected.length}）`}
          </button>
        </div>
      </form>
    </Dialog>
  )
}
