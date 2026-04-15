import { vi } from 'vitest'

export const getSidebar = vi.fn(() => ({
	registerTab: vi.fn(),
}))