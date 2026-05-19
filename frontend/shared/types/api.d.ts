// Request/response shapes for the Mini Fleet Tracker API.
// Mirrors backend/internal/handler/*. Keep in sync by hand when the Go
// handlers change.

import type { Driver, Role } from './domain'

// --- Auth ---

export interface LoginRequest {
  email: string
  password: string
}

export interface LoginResponse {
  user: Driver
}

export interface RegisterRequest {
  email: string
  password: string
  name: string
  role: Role
}

export interface RegisterResponse {
  user: Driver
}

export interface MeResponse {
  user: Driver
}

// --- Vehicles ---

export interface VehicleCreateRequest {
  plate_number: string
  model?: string
  driver_id?: string
}

export type VehicleUpdateRequest = Partial<VehicleCreateRequest>

// --- Positions ---

export interface PositionWriteRequest {
  vehicle_id: string
  lat: number
  lng: number
  speed_kmh?: number
  recorded_at: number // unix-ms
}

export interface PositionHistoryQuery {
  from?: number
  to?: number
  limit?: number
}

// --- Errors ---

export interface ApiError {
  error: string
  message: string
  request_id?: string
}

// Emitted by both backend (Go) and gateway (Worker) after DEMO_EXPIRES_AT
// (2026-05-31T23:59:59+07:00). The frontend interceptor in utils/api.ts
// short-circuits to /expired.
export interface DemoExpiredError {
  error: 'demo_expired'
  repo_url: string
  expired_at: string
}
