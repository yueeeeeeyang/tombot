import { Settings, SlidersHorizontal, X } from 'lucide-react'
import { Dialog } from '../../components/Dialog'
import { Select } from '../../components/Select'
import {
  MODEL_PARAMETER_LIMITS,
  validateSettingsDraft,
  type SettingsDraft,
  type SettingsSection,
  type ThemePreference,
} from './settings'

interface SettingsDialogProps {
  theme: ThemePreference
  themeError: string
  onThemeChange: (theme: ThemePreference) => void
  section: SettingsSection
  draft: SettingsDraft
  onSectionChange: (section: SettingsSection) => void
  onChange: (draft: SettingsDraft) => void
  apiKey: string
  hasApiKey: boolean
  onApiKeyChange: (apiKey: string) => void
  onClose: () => void
}

/** 主题即时生效并留在本机。语言和模型设置保存到服务，关闭或切换栏目不丢失当前输入。 */
export function SettingsDialog({
  theme,
  themeError,
  onThemeChange,
  section,
  draft,
  onSectionChange,
  onChange,
  apiKey,
  hasApiKey,
  onApiKeyChange,
  onClose,
}: SettingsDialogProps) {
  return (
    <Dialog title="设置" variant="settings" onClose={onClose}>
      <div className="settings-layout">
        <nav className="settings-navigation" aria-label="设置分类">
          <button
            className={section === 'general' ? 'is-active' : ''}
            aria-current={section === 'general' ? 'page' : undefined}
            onClick={() => onSectionChange('general')}
          >
            <Settings size={17} strokeWidth={1.7} />
            通用
          </button>
          <button
            className={section === 'models' ? 'is-active' : ''}
            aria-current={section === 'models' ? 'page' : undefined}
            onClick={() => onSectionChange('models')}
          >
            <SlidersHorizontal size={17} strokeWidth={1.7} />
            模型设置
          </button>
        </nav>
        <section className="settings-pane" aria-labelledby="settings-section-title">
          <h2 id="settings-section-title">{section === 'general' ? '通用' : '模型设置'}</h2>
          {section === 'general' ? (
            <GeneralSettings
              draft={draft}
              onChange={onChange}
              theme={theme}
              onThemeChange={onThemeChange}
            />
          ) : (
            <ModelSettings
              draft={draft}
              onChange={onChange}
              apiKey={apiKey}
              hasApiKey={hasApiKey}
              onApiKeyChange={onApiKeyChange}
            />
          )}
          <p className="settings-preview-note">
            {section === 'general'
              ? '主题即时生效。语言选择会保存，界面仍使用简体中文'
              : '模型设置会保存到服务。填写 API 地址和模型名称后即可发送'}
          </p>
          {themeError && (
            <p className="form-error" role="alert">
              {themeError}
            </p>
          )}
        </section>
        <button className="icon-button settings-close" aria-label="关闭设置" onClick={onClose}>
          <X size={18} strokeWidth={1.5} />
        </button>
      </div>
    </Dialog>
  )
}

/** 主题通过独立回调应用和保存，语言选项保留为界面预览。 */
function GeneralSettings({
  draft,
  onChange,
  theme,
  onThemeChange,
}: Pick<SettingsDialogProps, 'draft' | 'onChange' | 'theme' | 'onThemeChange'>) {
  return (
    <div className="settings-section">
      <h3>外观</h3>
      <div className="settings-card">
        <div className="setting-row">
          <span>主题</span>
          <Select
            label="主题"
            value={theme}
            onChange={(value) => onThemeChange(value)}
            options={[
              { value: 'system', label: '跟随系统' },
              { value: 'light', label: '浅色' },
              { value: 'dark', label: '深色' },
            ]}
          />
        </div>
        <div className="setting-row">
          <span>语言</span>
          <Select
            label="语言"
            value={draft.language}
            onChange={(value) => onChange({ ...draft, language: value })}
            options={[
              { value: 'system', label: '跟随系统' },
              { value: 'zh-CN', label: '简体中文' },
              { value: 'en', label: '英语' },
            ]}
          />
        </div>
      </div>
    </div>
  )
}

/** 渲染供应商与参数表单。密钥只提交给服务，不会留在本地存储。 */
function ModelSettings({
  draft,
  onChange,
  apiKey,
  hasApiKey,
  onApiKeyChange,
}: Pick<SettingsDialogProps, 'draft' | 'onChange' | 'apiKey' | 'hasApiKey' | 'onApiKeyChange'>) {
  const errors = validateSettingsDraft(draft)
  return (
    <>
      <div className="settings-section">
        <h3>模型供应商</h3>
        <div className="settings-card">
          <div className="setting-row">
            <span>供应商</span>
            <Select
              label="模型供应商"
              value={draft.provider}
              onChange={(value) => onChange({ ...draft, provider: value })}
              options={[
                { value: 'openai', label: 'OpenAI' },
                { value: 'anthropic', label: 'Anthropic' },
                { value: 'deepseek', label: 'DeepSeek' },
                { value: 'google', label: 'Google' },
                { value: 'custom', label: '自定义（兼容 OpenAI）' },
              ]}
            />
          </div>
          <div className="setting-field">
            <label className="setting-row" htmlFor="model-base-url">
              <span>API 地址</span>
              <input
                id="model-base-url"
                className="setting-text-input"
                type="url"
                value={draft.baseUrl}
                maxLength={500}
                placeholder="输入供应商 API 地址"
                aria-invalid={Boolean(errors.baseUrl)}
                aria-describedby={errors.baseUrl ? 'model-base-url-error' : undefined}
                onChange={(event) => onChange({ ...draft, baseUrl: event.target.value })}
              />
            </label>
            {errors.baseUrl && (
              <p className="setting-error" id="model-base-url-error" role="alert">
                {errors.baseUrl}
              </p>
            )}
          </div>
          <label className="setting-row" htmlFor="model-api-key">
            <span>API 密钥</span>
            <input
              id="model-api-key"
              className="setting-text-input"
              type="password"
              value={apiKey}
              placeholder={hasApiKey ? '已保存，留空则不修改' : '输入 API 密钥'}
              autoComplete="off"
              onChange={(event) => onApiKeyChange(event.target.value)}
            />
          </label>
          <label className="setting-row">
            <span>模型名称</span>
            <input
              className="setting-text-input"
              value={draft.modelName}
              maxLength={120}
              placeholder="输入模型名称"
              onChange={(event) => onChange({ ...draft, modelName: event.target.value })}
            />
          </label>
        </div>
      </div>
      <div className="settings-section">
        <h3>模型参数</h3>
        <div className="settings-card">
          <label className="setting-row">
            <span className="setting-label">
              温度<small>控制回复的随机性</small>
            </span>
            <span className="setting-range">
              <input
                type="range"
                aria-label="温度"
                {...MODEL_PARAMETER_LIMITS.temperature}
                value={draft.temperature}
                onChange={(event) =>
                  onChange({ ...draft, temperature: Number(event.target.value) })
                }
              />
              <output>{draft.temperature.toFixed(1)}</output>
            </span>
          </label>
          <label className="setting-row">
            <span className="setting-label">
              Top P<small>控制候选词的采样范围</small>
            </span>
            <span className="setting-range">
              <input
                type="range"
                aria-label="Top P"
                {...MODEL_PARAMETER_LIMITS.topP}
                value={draft.topP}
                onChange={(event) => onChange({ ...draft, topP: Number(event.target.value) })}
              />
              <output>{draft.topP.toFixed(2)}</output>
            </span>
          </label>
          <div className="setting-field">
            <label className="setting-row" htmlFor="max-output-tokens">
              <span className="setting-label">
                最大输出长度<small>单次回复的长度上限</small>
              </span>
              <span className="setting-number">
                <input
                  id="max-output-tokens"
                  type="number"
                  step={1}
                  {...MODEL_PARAMETER_LIMITS.maxOutputTokens}
                  value={draft.maxOutputTokens}
                  aria-invalid={Boolean(errors.maxOutputTokens)}
                  aria-describedby={errors.maxOutputTokens ? 'max-output-error' : undefined}
                  onChange={(event) => onChange({ ...draft, maxOutputTokens: event.target.value })}
                />
                <span>Token</span>
              </span>
            </label>
            {errors.maxOutputTokens && (
              <p className="setting-error" id="max-output-error" role="alert">
                {errors.maxOutputTokens}
              </p>
            )}
          </div>
          <div className="setting-row">
            <span className="setting-label" id="streaming-label">
              流式响应<small>开启后逐段出现，关闭后在完成后一次出现</small>
            </span>
            <button
              type="button"
              className="setting-switch"
              role="switch"
              aria-labelledby="streaming-label"
              aria-checked={draft.isStreaming}
              onClick={() => onChange({ ...draft, isStreaming: !draft.isStreaming })}
            >
              <span />
            </button>
          </div>
        </div>
      </div>
    </>
  )
}
