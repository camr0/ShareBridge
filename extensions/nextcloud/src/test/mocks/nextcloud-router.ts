import { vi } from 'vitest'

export const generateUrl = vi.fn((path: string): string => path)