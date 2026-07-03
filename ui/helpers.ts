// Pure helpers for the icaly plugin: date math, datetime-local <-> ISO conversion (events are
// stored UTC, shown in the browser's local zone), the month matrix, a small colour palette,
// RRULE presets, and file pick/download via browser globals (no forbidden imports).
import type { CalEvent } from './types';

const pad2 = (n: number) => String(n).padStart(2, '0');

// ── datetime-local <-> ISO ────────────────────────────────────────────────────────────
// <Input type="datetime-local"> speaks "YYYY-MM-DDTHH:MM" in local wall-clock time.

export function isoToLocalInput(iso: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return '';
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}T${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
}

export function localInputToISO(local: string): string {
  // new Date("YYYY-MM-DDTHH:MM") is parsed as local time -> toISOString() yields UTC.
  const d = new Date(local);
  return isNaN(d.getTime()) ? '' : d.toISOString();
}

export function isoToDateInput(iso: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return '';
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`;
}

// All-day date -> ISO at UTC midnight (the canonical all-day anchor the backend expects).
export function dateInputToUTCISO(date: string): string {
  if (!date) return '';
  return new Date(`${date}T00:00:00Z`).toISOString();
}

// ── date math ─────────────────────────────────────────────────────────────────────────

export function addDays(d: Date, n: number): Date {
  const x = new Date(d);
  x.setDate(x.getDate() + n);
  return x;
}

export function startOfDay(d: Date): Date {
  const x = new Date(d);
  x.setHours(0, 0, 0, 0);
  return x;
}

// Week starts Monday (ISO).
export function startOfWeek(d: Date): Date {
  const x = startOfDay(d);
  const dow = (x.getDay() + 6) % 7; // Mon=0 … Sun=6
  return addDays(x, -dow);
}

export function startOfMonth(d: Date): Date {
  return new Date(d.getFullYear(), d.getMonth(), 1);
}

export function sameDay(a: Date, b: Date): boolean {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
}

// The 6×7 grid of days covering the anchor's month, each row starting Monday.
export function monthMatrix(anchor: Date): Date[] {
  const first = startOfWeek(startOfMonth(anchor));
  return Array.from({ length: 42 }, (_, i) => addDays(first, i));
}

export const WEEKDAYS = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'];

export function monthLabel(d: Date): string {
  return d.toLocaleDateString(undefined, { month: 'long', year: 'numeric' });
}

export function weekLabel(d: Date): string {
  const s = startOfWeek(d);
  const e = addDays(s, 6);
  return `${s.toLocaleDateString(undefined, { day: 'numeric', month: 'short' })} – ${e.toLocaleDateString(undefined, { day: 'numeric', month: 'short', year: 'numeric' })}`;
}

export function fmtTime(iso: string): string {
  const d = new Date(iso);
  return isNaN(d.getTime()) ? '' : d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
}

export function fmtDayLong(d: Date): string {
  return d.toLocaleDateString(undefined, { weekday: 'long', day: 'numeric', month: 'long' });
}

// Does an event instance intersect the given calendar day?
export function eventOnDay(ev: CalEvent, day: Date): boolean {
  const s = new Date(ev.start);
  const e = new Date(ev.end || ev.start);
  const ds = startOfDay(day).getTime();
  const de = ds + 24 * 3600 * 1000;
  return s.getTime() < de && e.getTime() > ds;
}

// ── colours (RFC 7986 COLOR uses CSS3 names; these render directly as CSS) ───────────────
export const COLORS: { name: string; css: string }[] = [
  { name: 'dodgerblue', css: '#3b82f6' },
  { name: 'mediumseagreen', css: '#22c55e' },
  { name: 'tomato', css: '#ef4444' },
  { name: 'orange', css: '#f97316' },
  { name: 'gold', css: '#eab308' },
  { name: 'orchid', css: '#a855f7' },
  { name: 'turquoise', css: '#14b8a6' },
  { name: 'slategray', css: '#64748b' },
];

export function colorCss(name?: string): string {
  if (!name) return '#64748b';
  const hit = COLORS.find((c) => c.name === name);
  return hit ? hit.css : name; // a raw CSS colour name/hex also renders
}

// ── RRULE presets (the editor offers these; advanced rules pass through untouched) ───────
export const RRULE_PRESETS: { label: string; value: string }[] = [
  { label: 'Does not repeat', value: '' },
  { label: 'Daily', value: 'FREQ=DAILY' },
  { label: 'Weekly', value: 'FREQ=WEEKLY' },
  { label: 'Every weekday', value: 'FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR' },
  { label: 'Monthly', value: 'FREQ=MONTHLY' },
  { label: 'Yearly', value: 'FREQ=YEARLY' },
];

// ── browser file pick / download (document/window are not restricted globals) ────────────

export function pickTextFile(accept = '.ics,text/calendar'): Promise<string | null> {
  return new Promise((resolve) => {
    const input = document.createElement('input');
    input.type = 'file';
    input.accept = accept;
    input.onchange = () => {
      const file = input.files && input.files[0];
      if (!file) return resolve(null);
      const reader = new FileReader();
      reader.onload = () => resolve(typeof reader.result === 'string' ? reader.result : null);
      reader.onerror = () => resolve(null);
      reader.readAsText(file);
    };
    input.click();
  });
}

export function webcalURL(absoluteFeedURL: string): string {
  return absoluteFeedURL.replace(/^https?:/, 'webcal:');
}

// A maps link that opens the named place/address (not a bare coordinate pin — far more legible).
// Uses the free-text location as the search query, so it works with or without a geocoded pick.
export function mapsURL(query: string): string {
  return `https://www.google.com/maps/search/?api=1&query=${encodeURIComponent(query)}`;
}

// An opaque per-session token for the geocoding picker. Providers that bill per session (Google)
// group a search's keystrokes + the final details call under one token, so we mint a fresh one
// per search session. Uniqueness is all that matters; no crypto-grade randomness needed.
export function newGeoSession(): string {
  return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 12)}-${Math.random().toString(36).slice(2, 12)}`;
}
