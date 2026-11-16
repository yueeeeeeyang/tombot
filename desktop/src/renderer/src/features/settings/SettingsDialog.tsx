import { Settings, SlidersHorizontal, X } from 'lucide-react'
import { Dialog } from '../../components/Dialog'
import { Select } from '../../../../../../ui/Select'
import { type LanguagePreference, type SettingsSection, type ThemePreference } from './settings'

interface SettingsDialogProps {
  theme: ThemePreference
  themeError: string
  onThemeChange: (theme: ThemePreference) => void
  section: SettingsSection
  language: LanguagePreference
  languageError: string
  onSectionChange: (section: SettingsSection) => void
  onLanguageChange: (language: LanguagePreference) => void
  onClose: () => void
}

/** 主题即时生效并留在本机。语言保存在本机，模型由管理员统一配置。 */
export function SettingsDialog({
  theme,
  themeError,
  languageError,
  onThemeChange,
  section,
  language,
  onSectionChange,
  onLanguageChange,
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
            模型策略
          </button>
        </nav>
        <section className="settings-pane" aria-labelledby="settings-section-title">
          <h2 id="settings-section-title">{section === 'general' ? '通用' : '模型策略'}</h2>
          {section === 'general' ? (
            <GeneralSettings
              language={language}
              onLanguageChange={onLanguageChange}
              theme={theme}
              onThemeChange={onThemeChange}
            />
          ) : (
            <div className="settings-section">
              <h3>由服务端管理</h3>
              <p className="settings-preview-note">
                自动使用管理员分配的模型策略，无需在客户端配置。
              </p>
            </div>
          )}
          <p className="settings-preview-note">
            {section === 'general'
              ? '主题即时生效。语言选择保存在本机，界面仍使用简体中文'
              : '如需更换模型或策略，请联系管理员。'}
          </p>
          {(themeError || languageError) && (
            <p className="form-error" role="alert">
              {themeError || languageError}
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
  language,
  onLanguageChange,
  theme,
  onThemeChange,
}: Pick<SettingsDialogProps, 'language' | 'onLanguageChange' | 'theme' | 'onThemeChange'>) {
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
            value={language}
            onChange={onLanguageChange}
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
