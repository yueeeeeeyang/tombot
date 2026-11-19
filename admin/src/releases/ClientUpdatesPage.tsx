import { useEffect, useRef, useState } from 'react'
import { Plus, RefreshCw } from 'lucide-react'
import { z } from 'zod'
import { AdminClient } from '../api/client'
import { Alert, Dialog, Field, LoadState, useQuery } from '../components'
import { Select } from '../components/Select'

const catalogSchema = z.object({
  releases: z.array(
    z.object({ platform: z.string(), version: z.string(), notes: z.string(), size: z.number() }),
  ),
  platforms: z.record(z.string(), z.array(z.string())),
  chunk_size: z.number().int().positive(),
  max_size: z.number().int().positive(),
})
const path = '/client-updates'
const systems: Record<string, string> = { darwin: 'macOS', win32: 'Windows', linux: 'Linux' }
function platformName(platform: string) {
  const [system, architecture] = platform.split('-')
  return `${systems[system!] ?? system} · ${architecture}`
}

export function ClientUpdatesPage({ client }: { client: AdminClient }) {
  const catalog = useQuery(client, path, catalogSchema)
  const [publishing, setPublishing] = useState(false)
  const [notice, setNotice] = useState('')
  return (
    <>
      <header className="page-heading">
        <div>
          <h1>客户端升级</h1>
          <p>各平台的客户端会收到与其系统和架构匹配的最新版本。</p>
        </div>
        <div className="actions">
          <button className="icon-button" aria-label="刷新客户端版本" onClick={catalog.reload}>
            <RefreshCw size={17} />
          </button>
          <button
            className="primary-button"
            disabled={!catalog.value}
            onClick={() => {
              setNotice('')
              setPublishing(true)
            }}
          >
            <Plus size={16} />
            发布版本
          </button>
        </div>
      </header>
      {notice && (
        <p role="status" className="notice">
          {notice}
        </p>
      )}
      {!catalog.value ? (
        <LoadState error={catalog.error} onRetry={catalog.reload} />
      ) : catalog.value.releases.length === 0 ? (
        <div className="empty-state">
          <h2>尚未发布客户端版本</h2>
        </div>
      ) : (
        <div className="table-card">
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>目标平台</th>
                  <th>最新版本</th>
                  <th>安装包大小</th>
                  <th>升级内容</th>
                </tr>
              </thead>
              <tbody>
                {catalog.value.releases.map((r) => (
                  <tr key={r.platform}>
                    <td>{platformName(r.platform)}</td>
                    <td>{r.version}</td>
                    <td>{(r.size / (1 << 20)).toFixed(1)} MiB</td>
                    <td className="release-notes">{r.notes}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
      {publishing && catalog.value && (
        <PublishDialog
          client={client}
          catalog={catalog.value}
          onClose={() => setPublishing(false)}
          onPublished={(version, platform) => {
            setPublishing(false)
            setNotice(`已发布 ${platformName(platform)} 客户端 ${version}`)
            catalog.reload()
          }}
        />
      )}
    </>
  )
}

/** 浏览器逐片计算摘要并上传，只有全部分片确认后才提交可见的发布记录。 */
function PublishDialog({
  client,
  catalog,
  onClose,
  onPublished,
}: {
  client: AdminClient
  catalog: z.infer<typeof catalogSchema>
  onClose: () => void
  onPublished: (version: string, platform: string) => void
}) {
  const [platform, setPlatform] = useState('')
  const [file, setFile] = useState<File>()
  const [progress, setProgress] = useState('')
  const [error, setError] = useState<unknown>()
  const pending = useRef<AbortController | null>(null)
  useEffect(() => () => pending.current?.abort(), [])
  const formats = catalog.platforms[platform] ?? []
  const automaticFormat = platform.startsWith('darwin')
    ? '签名应用 ZIP'
    : platform.startsWith('win32')
      ? '签名 NSIS EXE'
      : 'AppImage'
  return (
    <Dialog title="发布客户端版本" onClose={onClose} busy={!!progress}>
      <form
        onSubmit={async (event) => {
          event.preventDefault()
          if (pending.current) return
          const fields = new FormData(event.currentTarget)
          const version = String(fields.get('version')).trim()
          const notes = String(fields.get('notes')).trim()
          const controller = new AbortController()
          pending.current = controller
          const signal = controller.signal
          setError(undefined)
          try {
            const extension = formats.find((ext) =>
              file?.name.toLowerCase().endsWith(ext.toLowerCase()),
            )
            if (!file || !extension || !file.size || file.size > catalog.max_size)
              throw new Error('安装包格式或大小不符合要求')
            if (!notes) throw new Error('请填写升级内容')
            const chunks: string[] = []
            for (let offset = 0; offset < file.size; offset += catalog.chunk_size) {
              setProgress(
                `正在上传 ${chunks.length + 1} / ${Math.ceil(file.size / catalog.chunk_size)} 个分片…`,
              )
              const part = file.slice(offset, offset + catalog.chunk_size)
              const hash = new Uint8Array(
                await crypto.subtle.digest('SHA-256', await part.arrayBuffer()),
              )
              const digest = Array.from(hash, (b) => b.toString(16).padStart(2, '0')).join('')
              await client.request(path + '/parts/' + digest, z.object({}), {
                method: 'PUT',
                body: part,
                signal,
              })
              chunks.push(digest)
            }
            setProgress('正在发布…')
            await client.request(path, z.object({ id: z.string().min(1) }), {
              method: 'POST',
              body: { platform, version, notes, extension, size: file.size, chunks },
              signal,
            })
            if (!signal.aborted) onPublished(version, platform)
          } catch (cause) {
            if (!signal.aborted) setError(cause)
          } finally {
            pending.current = null
            if (!signal.aborted) setProgress('')
          }
        }}
      >
        <fieldset className="form-grid" disabled={!!progress}>
          <Field label="目标平台">
            <Select
              label="目标平台"
              required
              value={platform}
              disabled={!!progress}
              onChange={(value) => {
                setPlatform(value)
                setFile(undefined)
              }}
              options={Object.keys(catalog.platforms)
                .sort()
                .map((value) => ({ value, label: platformName(value) }))}
            />
          </Field>
          <Field label="版本号" hint="填写安装包的三段式版本号，例如 1.10.0。">
            <input
              name="version"
              required
              placeholder="1.10.0"
              pattern="(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})"
            />
          </Field>
          <Field label="升级内容">
            <textarea name="notes" required maxLength={4000} rows={4} />
          </Field>
          <Field
            label="安装包"
            hint={
              platform
                ? `${formats.join('、')}，最大 ${catalog.max_size / (1 << 20)} MiB。要支持「立即升级」，请上传${automaticFormat}。`
                : '请先选择目标平台。'
            }
          >
            <input
              key={platform}
              type="file"
              required
              disabled={!platform}
              accept={formats.join(',')}
              onChange={(e) => setFile(e.target.files?.[0])}
            />
          </Field>
        </fieldset>
        <Alert error={error} />
        <button className="primary-button" disabled={!file || !!progress}>
          {progress || '上传并发布'}
        </button>
      </form>
    </Dialog>
  )
}
