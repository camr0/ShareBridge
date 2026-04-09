import { describe, it, expect, beforeEach } from 'vitest'
import { setActivePinia, createPinia } from 'pinia'
import { useSettingsStore } from './settings'

describe('useSettingsStore', () => {
  beforeEach(() => {
    localStorage.clear()
    setActivePinia(createPinia())
  })

  it('initializes with empty values when localStorage is empty', () => {
    const store = useSettingsStore()
    expect(store.agentUrl).toBe('')
    expect(store.apiKey).toBe('')
  })

  it('initializes agentUrl from localStorage', () => {
    localStorage.setItem('sharebridge_agent_url', 'http://localhost:7878')
    const store = useSettingsStore()
    expect(store.agentUrl).toBe('http://localhost:7878')
  })

  it('initializes apiKey from localStorage', () => {
    localStorage.setItem('sharebridge_api_key', 'sb_agent_test')
    const store = useSettingsStore()
    expect(store.apiKey).toBe('sb_agent_test')
  })

  it('setAgentUrl updates state and persists to localStorage', () => {
    const store = useSettingsStore()
    store.setAgentUrl('http://localhost:7878')
    expect(store.agentUrl).toBe('http://localhost:7878')
    expect(localStorage.getItem('sharebridge_agent_url')).toBe('http://localhost:7878')
  })

  it('setApiKey updates state and persists to localStorage', () => {
    const store = useSettingsStore()
    store.setApiKey('sb_agent_key123')
    expect(store.apiKey).toBe('sb_agent_key123')
    expect(localStorage.getItem('sharebridge_api_key')).toBe('sb_agent_key123')
  })

  it('isConfigured returns false when agentUrl is empty', () => {
    const store = useSettingsStore()
    expect(store.isConfigured).toBe(false)
  })

  it('isConfigured returns false when apiKey is empty', () => {
    const store = useSettingsStore()
    store.setAgentUrl('http://localhost:7878')
    expect(store.isConfigured).toBe(false)
  })

  it('isConfigured returns true when both agentUrl and apiKey are set', () => {
    const store = useSettingsStore()
    store.setAgentUrl('http://localhost:7878')
    store.setApiKey('sb_agent_key123')
    expect(store.isConfigured).toBe(true)
  })
})