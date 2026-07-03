// Dashboard is icaly's plugin root. It renders a live calendar (month / week / agenda) over the
// caller's calendars, gated by the hp_icaly_* rights, and hosts the event editor, the subscribe/
// share panel and calendar creation. Live updates come from useLiveEvents (SSE + polling).
//
// Rights (keep in sync with permissions/icaly.json and backend internal/rights):
//   hp_icaly_view  — see calendars/events, subscribe, export
//   hp_icaly_edit  — create/update/delete events, import
//   hp_icaly_share — create calendars
import { useCallback, useEffect, useMemo, useState } from 'react';
import {
  Badge,
  Box,
  Button,
  ChevronLeftIcon,
  ChevronRightIcon,
  DropdownMenu,
  EmptyState,
  Field,
  IconButton,
  Input,
  Modal,
  Panel,
  PlusIcon,
  SegmentedControl,
  Spinner,
  Stack,
  Text,
  cn,
  useLiveQuery,
  userHasRight,
  type ContactOption,
  type ServiceContextProps,
} from '@holistic/ui';
import type { Calendar, CalendarsResp, CalEvent, ViewMode } from './types';
import { COLORS, addDays, monthLabel, monthMatrix, startOfDay, startOfWeek, weekLabel } from './helpers';
import { useLiveEvents } from './useLiveEvents';
import { MonthView } from './MonthView';
import { WeekView } from './WeekView';
import { AgendaView } from './AgendaView';
import { EventEditor } from './EventEditor';
import { SubscribePanel } from './SubscribePanel';

const VIEW = 'hp_icaly_view';
const EDIT = 'hp_icaly_edit';
const SHARE = 'hp_icaly_share';

const VIEW_OPTIONS: { value: ViewMode; label: string }[] = [
  { value: 'month', label: 'Month' },
  { value: 'week', label: 'Week' },
  { value: 'agenda', label: 'Agenda' },
];

interface EditorState {
  event: CalEvent | null;
  defaultStart?: Date;
}

export function Dashboard({ user, api, apiFor, ui, nav }: ServiceContextProps) {
  // Contact directory for the attendee fields: contax resolves who this user may address, so an
  // event can invite people by name/nickname (with an avatar) instead of a bare email.
  const searchContacts = useCallback(
    async (q: string): Promise<ContactOption[]> => {
      try {
        const res = await apiFor('contax').get<{ contacts: ContactOption[] }>(`lookup?q=${encodeURIComponent(q)}`);
        return res.contacts ?? [];
      } catch {
        return [];
      }
    },
    [apiFor],
  );
  const canView = userHasRight(user, VIEW);
  const canEdit = userHasRight(user, EDIT);
  const canShare = userHasRight(user, SHARE);

  const calsQ = useLiveQuery<CalendarsResp>(() => api.get<CalendarsResp>('calendars'), 30000, [canView]);
  const calendars: Calendar[] = calsQ.data?.calendars ?? [];

  const [selectedId, setSelectedId] = useState('');
  const [view, setView] = useState<ViewMode>('month');
  const [anchor, setAnchor] = useState<Date>(() => new Date());
  const [editor, setEditor] = useState<EditorState | null>(null);
  const [subscribeOpen, setSubscribeOpen] = useState(false);
  const [createOpen, setCreateOpen] = useState(false);

  // Default to the first calendar once they load (or keep a still-present selection).
  useEffect(() => {
    if (calendars.length === 0) return;
    if (!calendars.some((c) => c.id === selectedId)) setSelectedId(calendars[0].id);
  }, [calendars, selectedId]);

  const selected = calendars.find((c) => c.id === selectedId);

  useEffect(() => {
    nav.setTitle(selected ? `Calendar — ${selected.name}` : 'Calendar');
    return () => nav.setTitle(null);
  }, [nav, selected]);

  const { rangeStart, rangeEnd } = useMemo(() => {
    if (view === 'month') {
      const days = monthMatrix(anchor);
      return { rangeStart: days[0], rangeEnd: addDays(days[41], 1) };
    }
    if (view === 'week') {
      const s = startOfWeek(anchor);
      return { rangeStart: s, rangeEnd: addDays(s, 7) };
    }
    const s = startOfDay(anchor);
    return { rangeStart: s, rangeEnd: addDays(s, 30) };
  }, [view, anchor]);

  const live = useLiveEvents(api, selectedId || 'personal', rangeStart, rangeEnd);

  if (!canView) {
    return (
      <Panel title="Calendar" className="p-4">
        <Text color="secondary">
          You need the “Use calendar” right. An admin can grant it per user in the Rights (privleg) service.
        </Text>
      </Panel>
    );
  }

  function shift(dir: number) {
    setAnchor((a) => {
      if (view === 'month') return new Date(a.getFullYear(), a.getMonth() + dir, 1);
      return addDays(a, 7 * dir);
    });
  }

  function openNew(day?: Date) {
    if (!canEdit) return;
    setEditor({ event: null, defaultStart: day });
  }

  const label = view === 'month' ? monthLabel(anchor) : view === 'week' ? weekLabel(anchor) : `From ${anchor.toLocaleDateString(undefined, { day: 'numeric', month: 'short', year: 'numeric' })}`;

  return (
    <Stack gap={4}>
      {/* Toolbar */}
      <Stack direction="row" align="center" justify="between" wrap gap={3}>
        <Stack direction="row" align="center" gap={2}>
          <IconButton label="Previous" variant="ghost" onClick={() => shift(-1)}>
            <ChevronLeftIcon />
          </IconButton>
          <Button variant="secondary" size="sm" onClick={() => setAnchor(new Date())}>
            Today
          </Button>
          <IconButton label="Next" variant="ghost" onClick={() => shift(1)}>
            <ChevronRightIcon />
          </IconButton>
          <Text variant="title3" weight="semibold">
            {label}
          </Text>
          <LiveBadge live={live.live} />
        </Stack>

        <Stack direction="row" align="center" gap={2} wrap>
          <SegmentedControl value={view} onChange={(v) => setView(v as ViewMode)} options={VIEW_OPTIONS} />
          {calendars.length > 0 && (
            <DropdownMenu
              trigger={<Button variant="secondary">{selected?.name ?? 'Calendar'}</Button>}
              items={[
                ...calendars.map((c) => ({ id: c.id, label: c.name, checked: c.id === selectedId, onSelect: () => setSelectedId(c.id) })),
                ...(canShare ? [{ id: '__new', label: 'New calendar…', separatorBefore: true, icon: <PlusIcon />, onSelect: () => setCreateOpen(true) }] : []),
              ]}
            />
          )}
          <Button variant="secondary" onClick={() => setSubscribeOpen(true)} disabled={!selected}>
            Subscribe
          </Button>
          {canEdit && (
            <Button variant="primary" iconLeft={<PlusIcon />} onClick={() => openNew()}>
              New event
            </Button>
          )}
        </Stack>
      </Stack>

      {/* Body */}
      {calsQ.loading && calendars.length === 0 ? (
        <Stack direction="row" align="center" gap={2}>
          <Spinner />
          <Text color="secondary">Loading calendars…</Text>
        </Stack>
      ) : calendars.length === 0 ? (
        <EmptyState title="No calendars yet" description="Your personal calendar is created on first use." />
      ) : view === 'agenda' ? (
        <AgendaView events={live.events} onEventClick={(ev) => setEditor({ event: ev })} />
      ) : (
        <Box className="overflow-hidden rounded-lg border-l border-t border-separator">
          {view === 'month' ? (
            <MonthView anchor={anchor} events={live.events} onDayClick={openNew} onEventClick={(ev) => setEditor({ event: ev })} />
          ) : (
            <WeekView anchor={anchor} events={live.events} onDayClick={openNew} onEventClick={(ev) => setEditor({ event: ev })} />
          )}
        </Box>
      )}

      {live.error && !live.loading && (
        <Text variant="caption" color="danger">
          Could not load events: {live.error.message}
        </Text>
      )}

      {/* Overlays */}
      {editor && selected && (
        <EventEditor
          api={api}
          ui={ui}
          calendarId={selectedId}
          event={editor.event}
          defaultStart={editor.defaultStart}
          canEdit={canEdit}
          selfEmail={calsQ.data?.address ?? ''}
          searchContacts={searchContacts}
          onClose={() => setEditor(null)}
          onSaved={live.refresh}
        />
      )}
      {subscribeOpen && selected && (
        <SubscribePanel
          api={api}
          ui={ui}
          calendar={selected}
          canImport={canEdit}
          canShare={canShare}
          username={user.username}
          onChanged={live.refresh}
          onClose={() => setSubscribeOpen(false)}
        />
      )}
      {createOpen && (
        <CreateCalendarModal
          api={api}
          ui={ui}
          onClose={() => setCreateOpen(false)}
          onCreated={(id) => {
            calsQ.refresh();
            setSelectedId(id);
          }}
        />
      )}
    </Stack>
  );
}

function LiveBadge({ live }: { live: boolean }) {
  return (
    <Stack direction="row" align="center" gap={1}>
      <Box className={cn('h-2 w-2 rounded-full', live ? 'bg-success' : 'bg-warning')} />
      <Text variant="caption" color="tertiary">
        {live ? 'Live' : 'Polling'}
      </Text>
    </Stack>
  );
}

function CreateCalendarModal({
  api,
  ui,
  onClose,
  onCreated,
}: {
  api: ServiceContextProps['api'];
  ui: ServiceContextProps['ui'];
  onClose: () => void;
  onCreated: (id: string) => void;
}) {
  const [name, setName] = useState('');
  const [color, setColor] = useState(COLORS[0].name);
  const [busy, setBusy] = useState(false);

  async function create() {
    if (!name.trim()) {
      ui.toast({ title: 'A name is required', variant: 'error' });
      return;
    }
    setBusy(true);
    try {
      const cal = await api.post<Calendar>('calendars', { name: name.trim(), color });
      ui.toast({ title: 'Calendar created', variant: 'success' });
      onCreated(cal.id);
      onClose();
    } catch (e) {
      ui.toast({ title: 'Could not create calendar', description: (e as Error).message, variant: 'error' });
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal
      open
      onOpenChange={(o) => !o && onClose()}
      title="New calendar"
      size="sm"
      footer={
        <Stack direction="row" justify="end" gap={2}>
          <Button variant="ghost" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button variant="primary" onClick={create} loading={busy}>
            Create
          </Button>
        </Stack>
      }
    >
      <Stack gap={4}>
        <Field label="Name">
          <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="Work, Family, …" autoFocus />
        </Field>
        <Field label="Colour">
          <Stack direction="row" gap={2} wrap>
            {COLORS.map((c) => (
              <Box
                key={c.name}
                onClick={() => setColor(c.name)}
                className={cn('h-7 w-7 cursor-pointer rounded-full ring-offset-2 ring-offset-surface', color === c.name && 'ring-2 ring-accent')}
                style={{ backgroundColor: c.css }}
              />
            ))}
          </Stack>
        </Field>
      </Stack>
    </Modal>
  );
}
