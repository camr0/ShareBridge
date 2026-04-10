import { describe, it, expect, vi } from 'vitest'
import { useShareBridgeExtension } from './useShareBridgeExtension'

// ShareBridgePanel imported inside the composable; mock it as a placeholder
vi.mock('../components/ShareBridgePanel.vue', () => ({ default: {} }))

describe('useShareBridgeExtension', () => {
  it('returns a SidebarPanelExtension with correct id and type', () => {
    const { extension } = useShareBridgeExtension()
    expect(extension.value.id).toBe('com.sharebridge.sidebar-panel')
    expect(extension.value.type).toBe('sidebarPanel')
  })

  it('extensionPointIds targets global.files.sidebar', () => {
    const { extension } = useShareBridgeExtension()
    expect(extension.value.extensionPointIds).toContain('global.files.sidebar')
  })

  it('panel.name is sharebridge', () => {
    const { extension } = useShareBridgeExtension()
    expect(extension.value.panel.name).toBe('sharebridge')
  })

  it('isVisible returns true for single file selection', () => {
    const { extension } = useShareBridgeExtension()
    const { isVisible } = extension.value.panel
    expect(isVisible({ items: [{ id: '1' }] })).toBe(true)
  })

  it('isVisible returns false for multiple file selection', () => {
    const { extension } = useShareBridgeExtension()
    const { isVisible } = extension.value.panel
    expect(isVisible({ items: [{ id: '1' }, { id: '2' }] })).toBe(false)
  })

  it('isVisible returns false for empty selection', () => {
    const { extension } = useShareBridgeExtension()
    const { isVisible } = extension.value.panel
    expect(isVisible({ items: [] })).toBe(false)
  })

  it('isVisible returns false when items is undefined', () => {
    const { extension } = useShareBridgeExtension()
    const { isVisible } = extension.value.panel
    expect(isVisible({ items: undefined })).toBe(false)
  })

  it('isRoot returns true', () => {
    const { extension } = useShareBridgeExtension()
    expect(extension.value.panel.isRoot?.({})).toBe(true)
  })
})