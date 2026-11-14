import type { Page } from '@playwright/test'

export async function chooseOption(page: Page, label: string, option: string | RegExp) {
  await page.getByRole('combobox', { name: label, exact: true }).click()
  await page.getByRole('option', { name: option, exact: true }).click()
}
