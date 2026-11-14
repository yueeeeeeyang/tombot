import { useState } from 'react'
import { z } from 'zod'
import { AdminClient, ApiError, isVerificationCanceled } from '../api/client'
import { modelSchema, type Model, type ModelMetadata } from '../api/schemas'
import { Alert, Dialog, Field } from '../components'
import { Select } from '../components/Select'

function identifiers(value: string): string[] {
  return value.trim() ? value.trim().split(/[,，\s]+/) : []
}

/** 编辑只作用于停用目录，保留未提交表单；提交结果不明确时先重新读取，不自动补发。 */
export function ModelEditor({
  client,
  model,
  onClose,
}: {
  client: AdminClient
  model: Model
  onClose: () => void
}) {
  const [busy, setBusy] = useState(false)
  const [attempted, setAttempted] = useState(false)
  const [error, setError] = useState<unknown>()
  const [levelsText, setLevelsText] = useState(
    model.metadata.effort?.supported_levels.join(', ') ?? '',
  )
  const [effort, setEffort] = useState(model.reasoning_effort ?? '')
  const [defaultEffort, setDefaultEffort] = useState(model.metadata.effort?.default_level ?? '')
  const levels = identifiers(levelsText)
  const effortOptions = [...new Set(levels)].map((value) => ({ value, label: value }))
  return (
    <Dialog title="编辑模型" onClose={onClose} busy={busy}>
      <p className="description">
        保存后保持停用。参数修改不会覆盖历史策略，请重新测试模型并启用新的策略版本。
      </p>
      <form
        onSubmit={async (event) => {
          event.preventDefault()
          if (busy || attempted) return
          const fields = new FormData(event.currentTarget)
          setError(undefined)
          try {
            const input = identifiers(String(fields.get('input_modalities') ?? ''))
            const output = identifiers(String(fields.get('output_modalities') ?? ''))
            if (
              [input, output, levels].some(
                (values) =>
                  values.length > 16 ||
                  new Set(values).size !== values.length ||
                  values.some((value) => !/^[a-z][a-z0-9_-]{0,31}$/.test(value)),
              )
            )
              throw new ApiError(
                'INPUT_INVALID',
                '媒体类型和思考级别须为不重复的小写标识，每组最多 16 项。',
              )
            const contextLimit = Number(fields.get('context_limit'))
            const outputLimit = Number(fields.get('output_limit'))
            const vision = fields.get('supports_vision') === 'true'
            if (outputLimit > contextLimit)
              throw new ApiError('INPUT_INVALID', '输出上限不能超过上下文窗口。')
            if (
              (input.length && (!input.includes('text') || (vision && !input.includes('image')))) ||
              (output.length && !output.includes('text'))
            )
              throw new ApiError(
                'INPUT_INVALID',
                '媒体类型须支持文本；启用图像输入时，输入媒体类型须包含 image。',
              )
            const metadata: ModelMetadata = {
              ...(input.length ? { input_modalities: input } : {}),
              ...(output.length ? { output_modalities: output } : {}),
              ...(levels.length
                ? {
                    effort: {
                      supported_levels: levels,
                      ...(defaultEffort ? { default_level: defaultEffort } : {}),
                    },
                  }
                : {}),
            }
            setBusy(true)
            setAttempted(true)
            await client.request(`/models/${model.id}/edit`, z.object({ model: modelSchema }), {
              method: 'POST',
              body: {
                expected_revision: model.revision,
                model_key: String(fields.get('model_key')).trim(),
                display_name: String(fields.get('display_name')).trim(),
                context_limit: contextLimit,
                output_limit: outputLimit,
                supports_tools: fields.get('supports_tools') === 'true',
                supports_vision: vision,
                metadata,
                reasoning_effort: effort,
              },
            })
            onClose()
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
          <div className="field-pair">
            <Field label="显示名称">
              <input
                name="display_name"
                defaultValue={model.display_name}
                required
                maxLength={120}
              />
            </Field>
            <Field label="模型标识">
              <input name="model_key" defaultValue={model.model_key} required maxLength={256} />
            </Field>
          </div>
          <div className="field-pair">
            <Field label="上下文窗口">
              <input
                name="context_limit"
                type="number"
                defaultValue={model.context_limit}
                required
                min={1}
                max={100_000_000}
              />
            </Field>
            <Field label="输出上限">
              <input
                name="output_limit"
                type="number"
                defaultValue={model.output_limit}
                required
                min={1}
                max={100_000_000}
              />
            </Field>
          </div>
          <div className="field-pair">
            <Field label="工具调用">
              <Select
                label="工具调用"
                name="supports_tools"
                defaultValue={String(model.supports_tools)}
                options={[
                  { value: 'true', label: '支持' },
                  { value: 'false', label: '不支持' },
                ]}
                disabled={busy || attempted}
              />
            </Field>
            <Field label="图像输入">
              <Select
                label="图像输入"
                name="supports_vision"
                defaultValue={String(model.supports_vision)}
                options={[
                  { value: 'true', label: '支持' },
                  { value: 'false', label: '不支持' },
                ]}
                disabled={busy || attempted}
              />
            </Field>
          </div>
          <div className="field-pair">
            <Field label="输入媒体类型" hint="逗号分隔，如 text, image；留空表示未知。">
              <input
                name="input_modalities"
                defaultValue={model.metadata.input_modalities?.join(', ') ?? ''}
                maxLength={528}
              />
            </Field>
            <Field label="输出媒体类型" hint="逗号分隔，如 text；留空表示未知。">
              <input
                name="output_modalities"
                defaultValue={model.metadata.output_modalities?.join(', ') ?? ''}
                maxLength={528}
              />
            </Field>
          </div>
          <Field label="支持的思考级别" hint="逗号分隔，如 low, high, max；留空表示未知。">
            <input
              value={levelsText}
              maxLength={528}
              onChange={(event) => {
                const value = event.target.value
                setLevelsText(value)
                const next = identifiers(value)
                if (!next.includes(effort)) setEffort('')
                if (!next.includes(defaultEffort)) setDefaultEffort('')
              }}
            />
          </Field>
          <div className="field-pair">
            <Field label="默认思考级别">
              <Select
                label="默认思考级别"
                value={defaultEffort}
                onChange={setDefaultEffort}
                options={[{ value: '', label: '未提供' }, ...effortOptions]}
                disabled={busy || attempted || !levels.length}
              />
            </Field>
            <Field label="使用的思考级别">
              <Select
                label="使用的思考级别"
                value={effort}
                onChange={setEffort}
                options={[{ value: '', label: '供应商默认' }, ...effortOptions]}
                disabled={busy || attempted || !levels.length}
              />
            </Field>
          </div>
        </fieldset>
        <Alert error={error} />
        {attempted && Boolean(error) && (
          <p className="description">关闭后重新读取当前状态，再决定是否提交新操作。</p>
        )}
        <div className="dialog-actions">
          <button type="button" className="secondary-button" disabled={busy} onClick={onClose}>
            {attempted ? '关闭并重新读取' : '取消'}
          </button>
          <button className="primary-button" disabled={busy || attempted}>
            {busy ? '正在保存…' : '保存修改'}
          </button>
        </div>
      </form>
    </Dialog>
  )
}
