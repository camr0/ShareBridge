import { vi } from 'vitest'

const axios = {
	get:    vi.fn(),
	post:   vi.fn(),
	put:    vi.fn(),
	delete: vi.fn(),
}

export default axios