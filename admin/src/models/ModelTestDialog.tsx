import { useState } from 'react'
import { z } from 'zod'
import { AdminClient } from '../api/client'
import type { Model, Provider } from '../api/schemas'
import { Alert, Dialog } from '../components'

const probeSchema = z.object({
  status: z.enum(['succeeded', 'failed', 'skipped']),
  message: z.string(),
  duration_ms: z.number().int().nonnegative(),
})
const resultSchema = z.object({
  test: z.object({
    model_id: z.string(),
    provider_revision: z.number().int().positive(),
    model_revision: z.number().int().positive(),
    tested_at: z.string().datetime(),
    connectivity: probeSchema,
    tools: probeSchema,
  }),
})

/** 测试由管理员显式发起一次；超时或失败后不自动重放供应商请求。 */
export function ModelTestDialog({
  client,
  provider,
  model,
  onClose,
}: {
  client: AdminClient
  provider: Provider
  model: Model
  onClose: () => void
}) {
  const [busy, setBusy] = useState(false)
  const [attempted, setAttempted] = useState(false)
  const [error, setError] = useState<unknown>()
  const [result, setResult] = useState<z.infer<typeof resultSchema>['test']>()
  return (
    <Dialog title="测试模型" busy={busy} onClose={onClose}>
      <p className="description">
        {model.display_name}（{model.model_key}）
      </p>
      <p className="description">
        将发送临时消息，分别测试连通性和工具调用，可能产生供应商费用。不执行工具，也不修改模型能力或启用状态。
      </p>
      {busy && <p role="status">正在测试连通性和工具调用，请稍候…</p>}
      <Alert error={error} />
      {result && (
        <div role="status" aria-live="polite">
          {[
            { label: '连通性', probe: result.connectivity },
            { label: '工具调用', probe: result.tools },
          ].map(({ label, probe: check }) => {
            return (
              <p key={label}>
                <strong>
                  {label}：{{ succeeded: '通过', failed: '失败', skipped: '未执行' }[check.status]}
                </strong>
                <br />
                {check.message}（{check.duration_ms} 毫秒）
              </p>
            )
          })}
        </div>
      )}
      <div className="dialog-actions">
        <button type="button" className="secondary-button" disabled={busy} onClick={onClose}>
          {attempted ? '关闭' : '取消'}
        </button>
        <button
          className="primary-button"
          disabled={busy || attempted}
          onClick={async () => {
            if (busy || attempted) return
            setAttempted(true)
            setBusy(true)
            try {
              const response = await client.request(`/models/${model.id}/test`, resultSchema, {
                method: 'POST',
                body: {
                  expected_provider_revision: provider.revision,
                  expected_model_revision: model.revision,
                },
                timeout: 65_000,
              })
              setResult(response.test)
            } catch (cause) {
              setError(cause)
            } finally {
              setBusy(false)
            }
          }}
        >
          {busy ? '正在测试…' : '开始测试'}
        </button>
      </div>
    </Dialog>
  )
}
