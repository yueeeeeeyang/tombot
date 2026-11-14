import { screen } from '@testing-library/react'
import type userEvent from '@testing-library/user-event'

export async function chooseOption(
  user: ReturnType<typeof userEvent.setup>,
  label: string,
  option: string | RegExp,
) {
  await user.click(screen.getByRole('combobox', { name: label }))
  await user.click(screen.getByRole('option', { name: option }))
}
