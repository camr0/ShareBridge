export interface Share {
  code: string
  public_url: string
  share_url: string
  file_id: string
  downloads: number
  max_downloads: number
  relay_only: boolean
  expires_at: string
  created_at: string
}

export interface CreateShareParams {
  share_url: string
  password?: string
  expiry_hours: number
  max_downloads: number
  relay_only: boolean
}

export interface CreateShareResult {
  code: string
  public_url: string
  expires_at: string
}

export interface AgentSettings {
  default_expiry_hours: number
  default_max_downloads: number
  default_relay_only: boolean
  turn_available: boolean
}