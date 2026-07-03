// Shapes returned by / sent to the icaly backend under /api/services/icaly/.
// These mirror backend/internal/event (JSON tags) and the api responses.

export interface Info {
  service: string;
  version: string;
  user: string;
  isAdmin: boolean;
  address: string;
  mailDomain: string;
}

export interface Participant {
  email: string;
  name?: string;
  role?: string; // CHAIR | REQ-PARTICIPANT | OPT-PARTICIPANT | NON-PARTICIPANT
  partStat?: string; // NEEDS-ACTION | ACCEPTED | DECLINED | TENTATIVE
  rsvp?: boolean;
  cuType?: string; // INDIVIDUAL | GROUP | ROOM | RESOURCE
  isInternal?: boolean;
  username?: string;
}

export interface Alarm {
  action: string; // DISPLAY | EMAIL
  trigger: string; // e.g. -PT15M
  description?: string;
}

export interface GeoPoint {
  lat: number;
  lon: number;
}

export interface CalEvent {
  uid: string;
  calendarId?: string;
  summary: string;
  description?: string;
  location?: string;
  geo?: GeoPoint;
  start: string; // RFC3339
  end: string; // RFC3339
  allDay?: boolean;
  timeZone?: string;
  status?: string; // CONFIRMED | TENTATIVE | CANCELLED
  class?: string; // PUBLIC | PRIVATE | CONFIDENTIAL
  transparency?: string; // OPAQUE | TRANSPARENT
  priority?: number;
  color?: string;
  categories?: string[];
  rrule?: string;
  exDates?: string[];
  recurrenceId?: string;
  organizer?: Participant;
  attendees?: Participant[];
  alarms?: Alarm[];
  conference?: string;
  url?: string;
  sequence?: number;
  created?: string;
  updated?: string;
}

export interface Calendar {
  id: string;
  owner: string;
  kind: string; // personal | subscription | scheduling-inbox | scheduling-outbox
  name: string;
  description?: string;
  color?: string;
  timeZone?: string;
  order: number;
  readOnly: boolean;
  public: boolean;
  ctag: string;
  feedToken?: string;
}

export interface CalendarsResp {
  address: string;
  mailDomain: string;
  calendars: Calendar[];
}

export interface EventsResp {
  calendar: string;
  events: CalEvent[];
}

export interface BusySlot {
  start: string;
  end: string;
}

export interface FreeBusyResp {
  calendar: string;
  busy: BusySlot[];
}

export type ViewMode = 'month' | 'week' | 'agenda';

// App passwords let native CalDAV clients authenticate over HTTP Basic (the holistic session
// cookie only exists in the browser). The clear-text token is returned exactly once on create.
export interface AppPassword {
  id: string;
  label: string;
  created: string;
}

export interface AppPasswordsResp {
  passwords: AppPassword[];
}

export interface CreatedAppPassword {
  token: string;
  username: string;
  password: AppPassword;
}

// Location picker: one autocomplete suggestion from icaly's /geocode proxy. Photon results arrive
// resolved (lat/lon/address set); Google results are unresolved and need /geocode/resolve.
export interface GeoSuggestion {
  id: string;
  label: string;
  primary: string;
  secondary: string;
  resolved: boolean;
  address?: string;
  lat?: number;
  lon?: number;
}

export interface GeocodeResp {
  provider: string;
  suggestions: GeoSuggestion[];
}

export interface GeoPlace {
  name: string;
  address: string;
  lat: number;
  lon: number;
}
