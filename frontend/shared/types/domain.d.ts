// Domain types — mirrors backend/internal/domain/*. Hand-typed; sync by hand
// when the Go domain evolves. (Auto-gen via swag/oapi-codegen is overkill for
// a portfolio.)

export type Role = 'driver' | 'manager'

export interface Driver {
  id: string
  email: string
  name: string
  role: Role
  created_at: number
  updated_at: number
}

export interface Vehicle {
  id: string
  plate_number: string
  model?: string
  driver_id?: string
  created_at: number
  updated_at: number
}

export interface Position {
  id: number
  vehicle_id: string
  lat: number
  lng: number
  speed_kmh?: number
  recorded_at: number
  created_at: number
}

export interface Geofence {
  id: string
  vehicle_id: string
  center_lat: number
  center_lng: number
  radius_m: number
  created_at: number
}

// WebSocket message envelope broadcast by the FleetHub Durable Object.
// Tagged-union shape — switch on `type` to narrow.
export type FleetMsg =
  | {
      type: 'position.update'
      vehicle_id: string
      lat: number
      lng: number
      recorded_at: number
    }
  | {
      type: 'geofence.alert'
      vehicle_id: string
      alert_type: 'enter' | 'exit'
      at: number
    }
