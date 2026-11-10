import { FileText, Film, Image as ImageIcon } from 'lucide-react'
import { useState } from 'react'
import { ARTIFACT_PREVIEW_COUNT, groupArtifacts } from './artifactGroups'
import type { Artifact } from '../workspace/model'

/** 只把已经校验过的 data 图片当作缩略图，避免把任意地址嵌进页面。 */
function imagePreview(artifact: Artifact): string | undefined {
  if (artifact.kind !== 'image' || !artifact.preview?.startsWith('data:image/')) return undefined
  return artifact.preview
}

/** 按时间列出这只猫咪留下的产物；没有产物时不编造条目。 */
export function ArtifactLibrary({
  artifacts,
  now = new Date(),
}: {
  artifacts: Artifact[]
  now?: Date
}) {
  const groups = groupArtifacts(artifacts, now)
  const [expanded, setExpanded] = useState<Record<string, boolean>>({})
  if (groups.length === 0) {
    return (
      <p className="cat-panel-empty" role="status">
        资料库还是空的。这只猫咪还没有留下文件。
      </p>
    )
  }
  return (
    <div className="artifact-library">
      {groups.map((group) => {
        const isExpanded = expanded[group.label] === true
        const visible = isExpanded
          ? group.artifacts
          : group.artifacts.slice(0, ARTIFACT_PREVIEW_COUNT)
        const hiddenCount = group.artifacts.length - ARTIFACT_PREVIEW_COUNT
        return (
          <section className="artifact-group" key={group.label} aria-label={group.label}>
            <h3>{group.label}</h3>
            <div className="artifact-card">
              <ul>
                {visible.map((artifact) => {
                  const preview = imagePreview(artifact)
                  return (
                    <li key={artifact.id}>
                      {preview ? (
                        <img src={preview} alt="" />
                      ) : artifact.kind === 'video' ? (
                        <Film size={16} aria-hidden="true" />
                      ) : artifact.kind === 'image' ? (
                        <ImageIcon size={16} aria-hidden="true" />
                      ) : (
                        <FileText size={16} aria-hidden="true" />
                      )}
                      <span>{artifact.title}</span>
                    </li>
                  )
                })}
              </ul>
              {hiddenCount > 0 && (
                <button
                  type="button"
                  onClick={() =>
                    setExpanded((current) => ({ ...current, [group.label]: !isExpanded }))
                  }
                >
                  {isExpanded ? '收起' : '展开'}
                </button>
              )}
            </div>
          </section>
        )
      })}
    </div>
  )
}
