// EventEditor is the create/edit modal for a calendar event. It surfaces the Google/Outlook
// attribute set the backend models, converting local wall-clock input to the UTC instants the
// store persists. All controls are @holistic/ui primitives (raw HTML is forbidden in service
// UIs); there is no SDK date picker, so we drive native pickers through <Input type="…">.
import { useMemo, useState } from 'react';
import {
  Autocomplete,
  Box,
  Button,
  ContactPicker,
  DropdownMenu,
  Field,
  Input,
  Modal,
  SegmentedControl,
  Stack,
  Switch,
  Text,
  Textarea,
  cn,
  type AutocompleteOption,
  type ContactOption,
  type ServiceApiClient,
  type ServiceContextProps,
} from '@holistic/ui';
import type { CalEvent, GeocodeResp, GeoPlace, GeoPoint, GeoSuggestion, Participant } from './types';
import {
  COLORS,
  RRULE_PRESETS,
  addDays,
  colorCss,
  dateInputToUTCISO,
  isoToDateInput,
  isoToLocalInput,
  localInputToISO,
  mapsURL,
  newGeoSession,
} from './helpers';

interface EventEditorProps {
  api: ServiceApiClient;
  ui: ServiceContextProps['ui'];
  calendarId: string;
  event: CalEvent | null; // null => create
  defaultStart?: Date;
  canEdit: boolean;
  selfEmail: string; // the caller's address, to surface RSVP controls when they are a guest
  searchContacts: (query: string) => Promise<ContactOption[]>; // contax directory for attendee pickers
  onClose: () => void;
  onSaved: () => void;
}

// EditScope mirrors Google Calendar's repeating-event choice when editing/deleting one occurrence.
type EditScope = 'this' | 'following' | 'all';

const norm = (s: string) => s.trim().toLowerCase().replace(/^mailto:/, '');
// A stored attendee → a picker chip: prefer its CN (name), fall back to the address.
const attendeeToOption = (a: Participant): ContactOption => ({ email: a.email, displayName: a.name || a.email });
const PARTSTATS: { value: string; label: string }[] = [
  { value: 'ACCEPTED', label: 'Yes' },
  { value: 'TENTATIVE', label: 'Maybe' },
  { value: 'DECLINED', label: 'No' },
];

const ALARM_PRESETS: { label: string; value: string }[] = [
  { label: 'No reminder', value: '' },
  { label: '10 minutes before', value: '-PT10M' },
  { label: '30 minutes before', value: '-PT30M' },
  { label: '1 hour before', value: '-PT1H' },
  { label: '1 day before', value: '-P1D' },
];

function nextHour(d: Date): Date {
  const x = new Date(d);
  x.setMinutes(0, 0, 0);
  x.setHours(x.getHours() + 1);
  return x;
}

export function EventEditor({ api, ui, calendarId, event, defaultStart, canEdit, selfEmail, searchContacts, onClose, onSaved }: EventEditorProps) {
  const seed = useMemo(() => {
    if (event) {
      const allDay = !!event.allDay;
      const endInclusive = allDay ? addDays(new Date(event.end || event.start), -1) : new Date(event.end || event.start);
      return {
        allDay,
        start: allDay ? isoToDateInput(event.start) : isoToLocalInput(event.start),
        end: allDay ? isoToDateInput(endInclusive.toISOString()) : isoToLocalInput(event.end || event.start),
      };
    }
    const s = defaultStart ? nextHour(defaultStart) : nextHour(new Date());
    const e = new Date(s.getTime() + 60 * 60 * 1000);
    return { allDay: false, start: isoToLocalInput(s.toISOString()), end: isoToLocalInput(e.toISOString()) };
  }, [event, defaultStart]);

  const [summary, setSummary] = useState(event?.summary ?? '');
  const [location, setLocation] = useState(event?.location ?? '');
  const [geo, setGeo] = useState<GeoPoint | undefined>(event?.geo);
  const [geoSession, setGeoSession] = useState(newGeoSession);
  const [description, setDescription] = useState(event?.description ?? '');
  const [conference, setConference] = useState(event?.conference ?? '');
  const [url, setUrl] = useState(event?.url ?? '');
  const [allDay, setAllDay] = useState(seed.allDay);
  const [start, setStart] = useState(seed.start);
  const [end, setEnd] = useState(seed.end);
  const [color, setColor] = useState(event?.color ?? '');
  const [status, setStatus] = useState(event?.status || 'CONFIRMED');
  const [busy, setBusy] = useState((event?.transparency || 'OPAQUE') !== 'TRANSPARENT');
  const [rrule, setRrule] = useState(event?.rrule ?? '');
  const [categories, setCategories] = useState((event?.categories ?? []).join(', '));
  // Outlook-style split: required (REQ-PARTICIPANT, the default) vs optional (OPT-PARTICIPANT).
  const [required, setRequired] = useState<ContactOption[]>(
    (event?.attendees ?? []).filter((a) => (a.role || 'REQ-PARTICIPANT') !== 'OPT-PARTICIPANT').map(attendeeToOption),
  );
  const [optional, setOptional] = useState<ContactOption[]>(
    (event?.attendees ?? []).filter((a) => a.role === 'OPT-PARTICIPANT').map(attendeeToOption),
  );
  // Attendees with a role the editor doesn't manage (e.g. CHAIR, NON-PARTICIPANT) are carried
  // through unchanged so a re-save doesn't flatten them to required.
  const otherAttendees = (event?.attendees ?? []).filter(
    (a) => a.role && a.role !== 'REQ-PARTICIPANT' && a.role !== 'OPT-PARTICIPANT',
  );
  const [alarm, setAlarm] = useState(event?.alarms?.[0]?.trigger ?? '');
  const [saving, setSaving] = useState(false);
  // A clicked occurrence of a repeating series carries both the master's rrule and its own
  // recurrenceId; editing/deleting it asks the user for the scope (this / following / all).
  const isSeries = !!event?.rrule && !!event?.recurrenceId;
  const [scopePrompt, setScopePrompt] = useState<'save' | 'delete' | null>(null);

  const myAttendee = event?.attendees?.find((a) => norm(a.email) === norm(selfEmail));
  const [myStat, setMyStat] = useState(myAttendee?.partStat ?? '');

  // When toggling all-day, convert between date and datetime-local representations.
  function toggleAllDay(on: boolean) {
    if (on) {
      setStart((s) => s.slice(0, 10));
      setEnd((e) => e.slice(0, 10));
    } else {
      setStart((s) => (s.length === 10 ? `${s}T09:00` : s));
      setEnd((e) => (e.length === 10 ? `${e}T10:00` : e));
    }
    setAllDay(on);
  }

  // Location autocomplete: query icaly's geocode proxy, map results to display options, and keep
  // the full suggestions so a selection can resolve coordinates.
  async function searchPlaces(q: string): Promise<AutocompleteOption[]> {
    const resp = await api.get<GeocodeResp>(`geocode?q=${encodeURIComponent(q)}&session=${encodeURIComponent(geoSession)}`);
    // Carry the full suggestion on the option (data), so a pick reads its own payload — no shared
    // lookup map that a racing search could overwrite.
    return (resp.suggestions ?? []).map((s, i) => ({
      id: s.id || `i${i}`,
      label: s.primary || s.label,
      sublabel: s.secondary || undefined,
      data: s,
    }));
  }

  async function pickPlace(opt: AutocompleteOption) {
    const s = opt.data as GeoSuggestion | undefined;
    if (!s) {
      setLocation(opt.label);
      return;
    }
    if (s.resolved && s.lat != null && s.lon != null) {
      setLocation(s.label || s.address || s.primary);
      setGeo({ lat: s.lat, lon: s.lon });
    } else {
      // Google: resolve the place id into address + coordinates (ends the billed session).
      try {
        const place = await api.get<GeoPlace>(`geocode/resolve?id=${encodeURIComponent(s.id)}&session=${encodeURIComponent(geoSession)}`);
        setLocation(s.label || place.address || s.primary);
        setGeo({ lat: place.lat, lon: place.lon });
      } catch {
        setLocation(s.label || s.primary); // keep the text even if coordinate resolution failed
      }
    }
    setGeoSession(newGeoSession()); // a session token is single-use; mint a fresh one
  }

  function toParticipants(list: ContactOption[], role: 'REQ-PARTICIPANT' | 'OPT-PARTICIPANT'): Participant[] {
    return list.map((o) => ({
      email: o.email,
      name: o.displayName && o.displayName !== o.email ? o.displayName : undefined,
      role,
      partStat: 'NEEDS-ACTION',
      rsvp: role === 'REQ-PARTICIPANT',
    }));
  }

  function allAttendees(): Participant[] {
    return [...toParticipants(required, 'REQ-PARTICIPANT'), ...toParticipants(optional, 'OPT-PARTICIPANT'), ...otherAttendees];
  }

  async function save(scope?: EditScope) {
    if (!summary.trim()) {
      ui.toast({ title: 'A title is required', variant: 'error' });
      return;
    }
    const payload: Partial<CalEvent> & { editScope?: EditScope } = {
      uid: event?.uid,
      calendarId,
      summary: summary.trim(),
      description: description.trim() || undefined,
      location: location.trim() || undefined,
      geo: location.trim() && geo ? geo : undefined,
      conference: conference.trim() || undefined,
      url: url.trim() || undefined,
      allDay,
      color: color || undefined,
      status,
      transparency: busy ? 'OPAQUE' : 'TRANSPARENT',
      rrule: rrule || undefined,
      categories: categories.split(',').map((c) => c.trim()).filter(Boolean),
      attendees: allAttendees(),
      alarms: alarm ? [{ action: 'DISPLAY', trigger: alarm }] : undefined,
    };
    if (allDay) {
      payload.start = dateInputToUTCISO(start);
      payload.end = dateInputToUTCISO(isoToDateInput(addDays(new Date(`${end}T00:00:00Z`), 1).toISOString()));
    } else {
      payload.start = localInputToISO(start);
      payload.end = localInputToISO(end);
    }
    if (!payload.start) {
      ui.toast({ title: 'A valid start time is required', variant: 'error' });
      return;
    }
    if (scope) {
      payload.editScope = scope; // recurring-series scope; recurrenceId pins the clicked occurrence
      payload.recurrenceId = event?.recurrenceId;
    }
    setScopePrompt(null);
    setSaving(true);
    try {
      await api.post('events', payload);
      ui.toast({ title: event ? 'Event updated' : 'Event created', variant: 'success' });
      onSaved();
      onClose();
    } catch (e) {
      ui.toast({ title: 'Could not save', description: (e as Error).message, variant: 'error' });
    } finally {
      setSaving(false);
    }
  }

  async function remove(scope?: EditScope) {
    if (!event) return;
    if (!scope) {
      // Non-recurring (or whole-series) delete still asks for plain confirmation; the scope
      // chooser itself is the confirmation when a scope is given.
      const ok = await ui.confirm({ title: 'Delete this event?', danger: true, confirmLabel: 'Delete' });
      if (!ok) return;
    }
    setScopePrompt(null);
    setSaving(true);
    try {
      await api.post('events/delete', { calendar: calendarId, uid: event.uid, editScope: scope, recurrenceId: event.recurrenceId });
      ui.toast({ title: 'Event deleted', variant: 'success' });
      onSaved();
      onClose();
    } catch (e) {
      ui.toast({ title: 'Could not delete', description: (e as Error).message, variant: 'error' });
    } finally {
      setSaving(false);
    }
  }

  async function rsvp(partStat: string) {
    if (!event) return;
    try {
      await api.post('events/rsvp', { calendar: calendarId, uid: event.uid, partStat });
      setMyStat(partStat);
      ui.toast({ title: 'Response sent', variant: 'success' });
      onSaved();
    } catch (e) {
      ui.toast({ title: 'Could not respond', description: (e as Error).message, variant: 'error' });
    }
  }

  const rruleLabel = RRULE_PRESETS.find((p) => p.value === rrule)?.label ?? 'Custom rule';
  const alarmLabel = ALARM_PRESETS.find((p) => p.value === alarm)?.label ?? `Custom (${alarm})`;

  const footer = (
    <Stack direction="row" justify="between" align="center" grow>
      <Box>
        {event && canEdit && (
          <Button variant="destructive" onClick={() => (isSeries ? setScopePrompt('delete') : remove())} disabled={saving}>
            Delete
          </Button>
        )}
      </Box>
      <Stack direction="row" gap={2}>
        <Button variant="ghost" onClick={onClose} disabled={saving}>
          {canEdit ? 'Cancel' : 'Close'}
        </Button>
        {canEdit && (
          <Button variant="primary" onClick={() => (isSeries ? setScopePrompt('save') : save())} loading={saving}>
            {event ? 'Save changes' : 'Create event'}
          </Button>
        )}
      </Stack>
    </Stack>
  );

  // Repeating-event scope chooser (Google-style), shown before a scoped save/delete.
  const scopeChooser = scopePrompt && (
    <Modal
      open
      onOpenChange={(o) => !o && setScopePrompt(null)}
      title={scopePrompt === 'delete' ? 'Delete repeating event' : 'Edit repeating event'}
      size="sm"
      footer={
        <Button variant="ghost" onClick={() => setScopePrompt(null)}>
          Cancel
        </Button>
      }
    >
      <Stack gap={2}>
        {(
          [
            { scope: 'this', label: 'This event' },
            { scope: 'following', label: 'This and following events' },
            { scope: 'all', label: 'All events' },
          ] as { scope: EditScope; label: string }[]
        ).map((o) => (
          <Button
            key={o.scope}
            variant={o.scope === 'this' ? 'primary' : 'secondary'}
            onClick={() => (scopePrompt === 'delete' ? remove(o.scope) : save(o.scope))}
          >
            {o.label}
          </Button>
        ))}
      </Stack>
    </Modal>
  );

  return (
    <>
      {scopeChooser}
    <Modal open onOpenChange={(o) => !o && onClose()} title={event ? 'Edit event' : 'New event'} size="lg" footer={footer}>
      <Stack gap={4}>
        <Field label="Title">
          <Input value={summary} onChange={(e) => setSummary(e.target.value)} placeholder="Add a title" disabled={!canEdit} autoFocus />
        </Field>

        {event && myAttendee && (
          <Field label="Your response">
            <Stack direction="row" gap={2}>
              {PARTSTATS.map((p) => (
                <Button key={p.value} variant={myStat === p.value ? 'primary' : 'secondary'} onClick={() => rsvp(p.value)}>
                  {p.label}
                </Button>
              ))}
            </Stack>
          </Field>
        )}

        <Stack direction="row" align="center" justify="between">
          <Switch checked={allDay} onChange={toggleAllDay} label="All day" disabled={!canEdit} />
          <Switch checked={busy} onChange={setBusy} label="Show as busy" disabled={!canEdit} />
        </Stack>

        <Stack direction="row" gap={3} wrap>
          <Field label="Starts">
            <Input type={allDay ? 'date' : 'datetime-local'} value={start} onChange={(e) => setStart(e.target.value)} disabled={!canEdit} />
          </Field>
          <Field label="Ends">
            <Input type={allDay ? 'date' : 'datetime-local'} value={end} onChange={(e) => setEnd(e.target.value)} disabled={!canEdit} />
          </Field>
        </Stack>

        <Field label="Location">
          <Autocomplete
            value={location}
            onChange={(v) => {
              setLocation(v);
              setGeo(undefined); // a manual edit no longer matches the picked coordinates
            }}
            onSearch={searchPlaces}
            onSelect={pickPlace}
            placeholder="Search a place or address"
            disabled={!canEdit}
          />
          {location.trim() && (
            <Box className="mt-1">
              <Button variant="ghost" size="sm" onClick={() => window.open(mapsURL(location), '_blank')}>
                Open in maps
              </Button>
            </Box>
          )}
        </Field>

        <Field label="Repeat">
          <DropdownMenu
            trigger={<Button variant="secondary" disabled={!canEdit}>{rruleLabel}</Button>}
            items={RRULE_PRESETS.map((p) => ({ id: p.value || 'none', label: p.label, onSelect: () => setRrule(p.value) }))}
          />
        </Field>

        <Field label="Status">
          <SegmentedControl
            value={status}
            onChange={setStatus}
            options={[
              { value: 'CONFIRMED', label: 'Confirmed' },
              { value: 'TENTATIVE', label: 'Tentative' },
              { value: 'CANCELLED', label: 'Cancelled' },
            ]}
          />
        </Field>

        <Field label="Colour">
          <Stack direction="row" gap={2} wrap>
            {COLORS.map((c) => (
              <Box
                key={c.name}
                onClick={() => canEdit && setColor(color === c.name ? '' : c.name)}
                className={cn('h-7 w-7 cursor-pointer rounded-full ring-offset-2 ring-offset-surface', color === c.name && 'ring-2 ring-accent')}
                style={{ backgroundColor: c.css }}
              />
            ))}
          </Stack>
        </Field>

        <Field label="Reminder">
          <DropdownMenu
            trigger={<Button variant="secondary" disabled={!canEdit}>{alarmLabel}</Button>}
            items={ALARM_PRESETS.map((p) => ({ id: p.value || 'none', label: p.label, onSelect: () => setAlarm(p.value) }))}
          />
        </Field>

        <Field label="Required attendees">
          <ContactPicker value={required} onChange={setRequired} onSearch={searchContacts} placeholder="Name or address …" disabled={!canEdit} />
        </Field>
        <Field label="Optional attendees">
          <ContactPicker value={optional} onChange={setOptional} onSearch={searchContacts} placeholder="Name or address …" disabled={!canEdit} />
        </Field>

        <Field label="Categories" hint="Comma separated">
          <Input value={categories} onChange={(e) => setCategories(e.target.value)} placeholder="work, personal" disabled={!canEdit} />
        </Field>

        <Stack direction="row" gap={3} wrap>
          <Field label="Video / conference link">
            <Input value={conference} onChange={(e) => setConference(e.target.value)} placeholder="https://…" disabled={!canEdit} />
          </Field>
          <Field label="URL">
            <Input value={url} onChange={(e) => setUrl(e.target.value)} placeholder="https://…" disabled={!canEdit} />
          </Field>
        </Stack>

        <Field label="Notes">
          <Textarea value={description} onChange={(e) => setDescription(e.target.value)} placeholder="Add notes or an agenda" disabled={!canEdit} />
        </Field>
      </Stack>
    </Modal>
    </>
  );
}
