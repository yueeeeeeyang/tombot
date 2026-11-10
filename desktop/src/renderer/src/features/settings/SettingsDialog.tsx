import { Settings, SlidersHorizontal, X } from 'lucide-react'
import { ModelProviderFields } from './ModelProviderFields'
import { Dialog } from '../../components/Dialog'
import { Select } from '../../components/Select'
import { type SettingsDraft, type SettingsSection, type ThemePreference } from './settings'

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
              : '模型设置会保存到服务。密钥只留在服务端，不会回显。'}
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

/** 渲染供应商表单。密钥只提交给服务，不会留在本地存储。 */
function ModelSettings(
  props: Pick<
    SettingsDialogProps,
    'draft' | 'onChange' | 'apiKey' | 'hasApiKey' | 'onApiKeyChange'
  >,
) {
  return (
    <div className="settings-section">
      <h3>模型供应商</h3>
      <div className="settings-card">
        <ModelProviderFields {...props} />
      </div>
    </div>
  )
}
