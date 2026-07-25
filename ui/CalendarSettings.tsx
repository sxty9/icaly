// CalendarSettings is the single configuration surface for the currently selected calendar. It
// gathers everything scoped to that one calendar: its details (name, colour), the ways to connect
// it (read-only webcal feed, CalDAV two-way sync + per-user app passwords, .ics import/export) and
// — last, behind a confirm — deleting it. Editing details and deleting need the share right
// (calendar lifecycle); importing needs the edit right; the connect surfaces are always shown.
// The default "personal" calendar cannot be deleted (the backend refuses it, so the action hides).
import { useEffect, useState } from 'react';
import {
  Autocomplete,
  Badge,
  Box,
  Button,
  ContactPicker,
  DownloadIcon,
  Field,
  IconButton,
  Input,
  Modal,
  SegmentedControl,
  Spinner,
  Stack,
  Text,
  TrashIcon,
  UploadIcon,
  cn,
  type AutocompleteOption,
  type ContactOption,
  type ServiceApiClient,
  type ServiceContextProps,
} from '@holistic/ui';
import type { AppPassword, AppPasswordsResp, Calendar, CreatedAppPassword, Grant, GrantsResp } from './types';
import { COLORS, pickTextFile, webcalURL } from './helpers';

interface CalendarSettingsProps {
  api: ServiceApiClient;
  apiFor: ServiceContextProps['apiFor']; // sibling-service client (contax directory for the share picker)
  ui: ServiceContextProps['ui'];
  calendar: Calendar;
  canImport: boolean; // edit right — import .ics
  canShare: boolean; // share right — rename/recolour, sharing, app passwords, delete
  username: string;
  onEventsChanged: () => void; // import touched events → refresh the grid
  onCalendarsChanged: () => void; // name/colour changed → refresh the calendar list
  onDeleted: () => void; // calendar removed → parent resets selection + closes
  onClose: () => void;
}

export function CalendarSettings({
  api,
  apiFor,
  ui,
  calendar,
  canImport,
  canShare,
  username,
  onEventsChanged,
  onCalendarsChanged,
  onDeleted,
  onClose,
}: CalendarSettingsProps) {
  const feedUrl = calendar.feedToken ? api.url(`feeds/${calendar.feedToken}.ics`) : '';
  const webcal = feedUrl ? webcalURL(feedUrl) : '';
  const davUrl = api.url('dav/');

  // Details — editable with the share right. Save only lights up when something actually changed.
  const initialColor = calendar.color || COLORS[0].name;
  const [name, setName] = useState(calendar.name);
  const [color, setColor] = useState(initialColor);
  const [saving, setSaving] = useState(false);
  const dirty = name.trim() !== calendar.name || color !== initialColor;

  async function saveDetails() {
    if (!name.trim()) {
      ui.toast({ title: 'A name is required', variant: 'error' });
      return;
    }
    setSaving(true);
    try {
      await api.post('calendars/update', { id: calendar.id, name: name.trim(), color });
      ui.toast({ title: 'Calendar updated', variant: 'success' });
      onCalendarsChanged();
    } catch (e) {
      ui.toast({ title: 'Could not update calendar', description: (e as Error).message, variant: 'error' });
    } finally {
      setSaving(false);
    }
  }

  async function copyText(value: string, label: string) {
    try {
      await navigator.clipboard.writeText(value);
      ui.toast({ title: `${label} copied`, variant: 'success' });
    } catch {
      ui.toast({ title: 'Copy failed', description: value, variant: 'error' });
    }
  }

  async function importIcs() {
    const text = await pickTextFile();
    if (!text) return;
    try {
      const res = await api.post<{ imported: number; skipped: number }>(
        `events/import?calendar=${encodeURIComponent(calendar.id)}`,
        { ics: text },
      );
      ui.toast({
        title: `Imported ${res.imported} event${res.imported === 1 ? '' : 's'}`,
        description: res.skipped ? `${res.skipped} skipped` : undefined,
        variant: 'success',
      });
      onEventsChanged();
    } catch (e) {
      ui.toast({ title: 'Import failed', description: (e as Error).message, variant: 'error' });
    }
  }

  function exportIcs() {
    window.open(api.url(`events/export?calendar=${encodeURIComponent(calendar.id)}`), '_blank');
  }

  // Deleting a calendar is irreversible (calendar + every event), so it is confirmed. The default
  // "personal" calendar is not deletable (the backend refuses it), so the action is hidden for it.
  async function deleteCalendar() {
    const ok = await ui.confirm({
      title: `Delete “${calendar.name}”?`,
      description: 'This permanently removes the calendar and all of its events.',
      danger: true,
      confirmLabel: 'Delete',
    });
    if (!ok) return;
    try {
      await api.post('calendars/delete', { id: calendar.id });
      ui.toast({ title: 'Calendar deleted', variant: 'success' });
      onDeleted();
    } catch (e) {
      ui.toast({ title: 'Could not delete calendar', description: (e as Error).message, variant: 'error' });
    }
  }

  const deletable = canShare && calendar.id !== 'personal';

  return (
    <Modal open onOpenChange={(o) => !o && onClose()} title={`Calendar settings — ${calendar.name}`} size="md">
      <Stack gap={5}>
        {canShare && (
          <>
            <Field label="Name">
              <Stack direction="row" gap={2}>
                <Box className="grow">
                  <Input className="w-full" value={name} onChange={(e) => setName(e.target.value)} />
                </Box>
                <Button variant="primary" onClick={saveDetails} loading={saving} disabled={!dirty}>
                  Save
                </Button>
              </Stack>
            </Field>
            <Field label="Colour">
              <Stack direction="row" gap={2} wrap>
                {COLORS.map((c) => (
                  <Box
                    key={c.name}
                    onClick={() => setColor(c.name)}
                    className={cn(
                      'h-7 w-7 cursor-pointer rounded-full ring-offset-2 ring-offset-surface',
                      color === c.name && 'ring-2 ring-accent',
                    )}
                    style={{ backgroundColor: c.css }}
                  />
                ))}
              </Stack>
            </Field>
          </>
        )}

        {canShare && (
          <Sharing api={api} apiFor={apiFor} ui={ui} calendar={calendar} webcal={webcal} onCopy={copyText} />
        )}

        <Field label="Subscription URL (webcal)" hint="Read-only. Add in Apple Calendar, Google, Thunderbird … Subscribers refresh periodically, not instantly.">
          <Stack direction="row" gap={2}>
            <Box className="grow">
              <Input className="w-full" value={webcal} readOnly onFocus={(e) => e.target.select()} />
            </Box>
            <Button variant="secondary" onClick={() => copyText(webcal, 'Subscription URL')} disabled={!webcal}>
              Copy
            </Button>
          </Stack>
        </Field>

        <Field label="CalDAV (two-way sync)" hint="Add a CalDAV account in your client with this server URL and your username, using an app password below.">
          <Stack gap={2}>
            <Stack direction="row" gap={2}>
              <Box className="grow">
                <Input className="w-full" value={davUrl} readOnly onFocus={(e) => e.target.select()} />
              </Box>
              <Button variant="secondary" onClick={() => copyText(davUrl, 'Server URL')}>
                Copy
              </Button>
            </Stack>
            <Input value={username} readOnly onFocus={(e) => e.target.select()} />
          </Stack>
        </Field>

        {canShare && <AppPasswords api={api} ui={ui} />}

        <Stack direction="row" gap={2} wrap>
          <Button variant="secondary" iconLeft={<DownloadIcon />} onClick={exportIcs}>
            Export .ics
          </Button>
          {canImport && (
            <Button variant="secondary" iconLeft={<UploadIcon />} onClick={importIcs}>
              Import .ics
            </Button>
          )}
        </Stack>

        {deletable && (
          <Stack direction="row" justify="end" className="border-t border-separator pt-4">
            <Button variant="destructive" iconLeft={<TrashIcon />} onClick={deleteCalendar}>
              Delete calendar
            </Button>
          </Stack>
        )}
      </Stack>
    </Modal>
  );
}

const KIND_LABEL: Record<Grant['kind'], string> = {
  holistic: 'Holistic-Gruppe',
  contax: 'Kontaktgruppe',
  adhoc: 'Eigene Gruppe',
};

const EXTERNAL_CONSENT = {
  title: 'Externe Kontakte in dieser Gruppe',
  description:
    'Externe (ohne Konto) erreichst du nur über einen öffentlichen Link — jeder mit dem Link kann diesen Kalender lesen. Bestätigen aktiviert den Link; abbrechen teilt nur mit internen Mitgliedern.',
  danger: true,
  confirmLabel: 'Öffentlichen Link aktivieren',
} as const;

// Sharing manages a calendar's ACL grants: share with a Holistic group (the caller's own OS groups),
// a contax personal group (live), or an ad-hoc member set scoped to this calendar. Each grantee has
// a view/edit level. When a share includes external (non-account) contacts, they are only reachable
// via the public webcal link, so the owner is asked to consent (or exclude them).
function Sharing({
  api,
  apiFor,
  ui,
  calendar,
  webcal,
  onCopy,
}: {
  api: ServiceApiClient;
  apiFor: ServiceContextProps['apiFor'];
  ui: ServiceContextProps['ui'];
  calendar: Calendar;
  webcal: string;
  onCopy: (value: string, label: string) => void;
}) {
  const [grants, setGrants] = useState<Grant[] | null>(null);
  const [pickQuery, setPickQuery] = useState('');
  const [adhocOpen, setAdhocOpen] = useState(false);
  const [adhocName, setAdhocName] = useState('');
  const [adhocMembers, setAdhocMembers] = useState<ContactOption[]>([]);
  const [adhocBusy, setAdhocBusy] = useState(false);

  const grantsPath = `calendars/grants?calendar=${encodeURIComponent(calendar.id)}`;

  async function reload() {
    try {
      const res = await api.get<GrantsResp>(grantsPath);
      setGrants(res.grants ?? []);
    } catch {
      setGrants([]);
    }
  }
  useEffect(() => {
    let active = true;
    api.get<GrantsResp>(grantsPath).then(
      (r) => active && setGrants(r.grants ?? []),
      () => active && setGrants([]),
    );
    return () => {
      active = false;
    };
  }, [api, calendar.id, grantsPath]);

  // The add picker searches both live group sources: the caller's own Holistic (OS) groups from
  // icaly, and their contax personal groups from the contax directory (browser session).
  async function searchGroups(q: string): Promise<AutocompleteOption[]> {
    const query = q.trim().toLowerCase();
    const out: AutocompleteOption[] = [];
    try {
      const hg = await api.get<{ groups: string[] }>('shareable-groups');
      for (const g of hg.groups ?? []) {
        if (!query || g.toLowerCase().includes(query)) {
          out.push({ id: 'h:' + g, label: g, sublabel: 'Holistic-Gruppe', data: { kind: 'holistic', ref: g, label: g } });
        }
      }
    } catch {
      /* ignore — the source is optional */
    }
    if (query) {
      try {
        const cg = await apiFor('contax').get<{ groups?: { id: string; name: string; memberCount: number }[] }>(
          `lookup?q=${encodeURIComponent(q)}&includeGroups=1`,
        );
        for (const g of cg.groups ?? []) {
          out.push({ id: 'c:' + g.id, label: g.name, sublabel: 'Kontaktgruppe', data: { kind: 'contax', ref: g.id, label: g.name } });
        }
      } catch {
        /* ignore */
      }
    }
    return out;
  }

  // The external (non-account) member addresses of a contax group — they can only be reached via
  // the public link, and are the addresses we offer to email the subscription link to.
  async function contaxExternals(grpId: string): Promise<string[]> {
    try {
      const res = await apiFor('contax').get<{ contacts: ContactOption[] }>(`groups/${encodeURIComponent(grpId)}/members`);
      return (res.contacts ?? []).filter((c) => !c.username && c.email).map((c) => c.email);
    } catch {
      return [];
    }
  }

  async function post(path: string, body: Record<string, unknown>, okMsg?: string) {
    try {
      await api.post(path, { calendar: calendar.id, ...body });
      if (okMsg) ui.toast({ title: okMsg, variant: 'success' });
      await reload();
      return true;
    } catch (e) {
      ui.toast({ title: 'Freigabe fehlgeschlagen', description: (e as Error).message, variant: 'error' });
      return false;
    }
  }

  async function addPicked(opt: AutocompleteOption) {
    const data = opt.data as { kind: 'holistic' | 'contax'; ref: string; label: string };
    let publicExt = false;
    let externals: string[] = [];
    if (data.kind === 'contax') {
      externals = await contaxExternals(data.ref);
      if (externals.length > 0) {
        publicExt = await ui.confirm(EXTERNAL_CONSENT);
        if (!publicExt) externals = [];
      }
    }
    await post(
      'calendars/grants',
      { kind: data.kind, ref: data.ref, label: data.label, level: 'view', publicExt, notifyExternals: externals, feedUrl: webcal },
      'Freigabe hinzugefügt',
    );
  }

  async function createAdhoc() {
    if (!adhocName.trim()) {
      ui.toast({ title: 'Ein Name ist nötig', variant: 'error' });
      return;
    }
    const members = adhocMembers.map((m) => m.username).filter((u): u is string => !!u);
    const externals = adhocMembers.filter((m) => !m.username && m.email).map((m) => m.email);
    let publicExt = false;
    if (externals.length > 0) {
      publicExt = await ui.confirm(EXTERNAL_CONSENT);
    }
    setAdhocBusy(true);
    const ok = await post(
      'calendars/grants',
      { kind: 'adhoc', ref: '', label: adhocName.trim(), level: 'view', publicExt, members, notifyExternals: publicExt ? externals : [], feedUrl: webcal },
      'Freigabe hinzugefügt',
    );
    setAdhocBusy(false);
    if (ok) {
      setAdhocOpen(false);
      setAdhocName('');
      setAdhocMembers([]);
    }
  }

  async function searchContacts(q: string): Promise<ContactOption[]> {
    try {
      const res = await apiFor('contax').get<{ contacts: ContactOption[] }>(`lookup?q=${encodeURIComponent(q)}`);
      return res.contacts ?? [];
    } catch {
      return [];
    }
  }

  const anyPublic = (grants ?? []).some((g) => g.publicExt);

  return (
    <Field label="Freigeben">
      <Stack gap={3}>
        <Autocomplete
          value={pickQuery}
          onChange={setPickQuery}
          onSearch={searchGroups}
          onSelect={(o) => {
            setPickQuery('');
            void addPicked(o);
          }}
          placeholder="Mit Gruppe teilen …"
          minChars={0}
        />

        {grants === null ? (
          <Stack direction="row" align="center" gap={2}>
            <Spinner />
            <Text color="secondary">Lädt …</Text>
          </Stack>
        ) : grants.length === 0 ? (
          <Text variant="caption" color="tertiary">
            Noch nicht geteilt.
          </Text>
        ) : (
          <Stack gap={1}>
            {grants.map((g) => (
              <Stack key={g.id} direction="row" align="center" justify="between" gap={2} wrap>
                <Stack direction="row" align="center" gap={2}>
                  <Text>{g.label || g.ref}</Text>
                  <Badge variant="neutral">{KIND_LABEL[g.kind]}</Badge>
                </Stack>
                <Stack direction="row" align="center" gap={2}>
                  <SegmentedControl
                    value={g.level}
                    onChange={(v) => void post('calendars/grants/update', { id: g.id, level: v, publicExt: g.publicExt })}
                    options={[
                      { value: 'view', label: 'Ansehen' },
                      { value: 'edit', label: 'Bearbeiten' },
                    ]}
                  />
                  <IconButton label="Entfernen" variant="ghost" size="sm" onClick={() => void post('calendars/grants/delete', { id: g.id })}>
                    <TrashIcon />
                  </IconButton>
                </Stack>
              </Stack>
            ))}
          </Stack>
        )}

        {adhocOpen ? (
          <Box className="rounded-md border border-separator p-3">
            <Stack gap={3}>
              <Input value={adhocName} onChange={(e) => setAdhocName(e.target.value)} placeholder="Gruppenname" autoFocus />
              <ContactPicker value={adhocMembers} onChange={setAdhocMembers} onSearch={searchContacts} placeholder="Mitglieder …" />
              <Stack direction="row" justify="end" gap={2}>
                <Button variant="ghost" size="sm" onClick={() => setAdhocOpen(false)} disabled={adhocBusy}>
                  Abbrechen
                </Button>
                <Button variant="primary" size="sm" onClick={createAdhoc} loading={adhocBusy}>
                  Erstellen
                </Button>
              </Stack>
            </Stack>
          </Box>
        ) : (
          <Box>
            <Button variant="secondary" size="sm" onClick={() => setAdhocOpen(true)}>
              Eigene Gruppe …
            </Button>
          </Box>
        )}

        {anyPublic && webcal && (
          <Stack direction="row" gap={2} align="center">
            <Box className="grow">
              <Input className="w-full" value={webcal} readOnly onFocus={(e) => e.target.select()} />
            </Box>
            <Button variant="secondary" onClick={() => onCopy(webcal, 'Öffentlicher Link')}>
              Link kopieren
            </Button>
          </Stack>
        )}
      </Stack>
    </Field>
  );
}

// AppPasswords manages the per-user CalDAV app passwords: list, create (token shown once) and
// revoke. Scoped by the share right at the call site.
function AppPasswords({ api, ui }: { api: ServiceApiClient; ui: ServiceContextProps['ui'] }) {
  const [list, setList] = useState<AppPassword[] | null>(null);
  const [label, setLabel] = useState('');
  const [busy, setBusy] = useState(false);
  const [fresh, setFresh] = useState<CreatedAppPassword | null>(null);

  async function reload() {
    try {
      const res = await api.get<AppPasswordsResp>('apppasswords');
      setList(res.passwords ?? []);
    } catch {
      setList([]);
    }
  }

  useEffect(() => {
    let active = true;
    api.get<AppPasswordsResp>('apppasswords').then(
      (res) => active && setList(res.passwords ?? []),
      () => active && setList([]),
    );
    return () => {
      active = false;
    };
  }, [api]);

  async function create() {
    setBusy(true);
    try {
      const res = await api.post<CreatedAppPassword>('apppasswords', { label: label.trim() || 'Calendar app' });
      setFresh(res);
      setLabel('');
      await reload();
    } catch (e) {
      ui.toast({ title: 'Could not create app password', description: (e as Error).message, variant: 'error' });
    } finally {
      setBusy(false);
    }
  }

  async function remove(id: string) {
    try {
      await api.post('apppasswords/delete', { id });
      if (fresh?.password.id === id) setFresh(null);
      await reload();
    } catch (e) {
      ui.toast({ title: 'Could not revoke', description: (e as Error).message, variant: 'error' });
    }
  }

  return (
    <Field label="App passwords">
      <Stack gap={3}>
        {fresh && (
          <Box className="rounded-md border border-separator bg-fill/5 p-3">
            <Stack gap={2}>
              <Text variant="caption" color="secondary">
                Copy this now — it is shown only once. Use it as the password in your calendar app.
              </Text>
              <Stack direction="row" gap={2}>
                <Box className="grow">
                  <Input className="w-full font-mono" value={fresh.token} readOnly onFocus={(e) => e.target.select()} />
                </Box>
                <Button
                  variant="secondary"
                  onClick={async () => {
                    try {
                      await navigator.clipboard.writeText(fresh.token);
                      ui.toast({ title: 'App password copied', variant: 'success' });
                    } catch {
                      ui.toast({ title: 'Copy failed', variant: 'error' });
                    }
                  }}
                >
                  Copy
                </Button>
              </Stack>
            </Stack>
          </Box>
        )}

        <Stack direction="row" gap={2}>
          <Box className="grow">
            <Input className="w-full" value={label} onChange={(e) => setLabel(e.target.value)} placeholder="Label (e.g. iPhone)" />
          </Box>
          <Button variant="primary" onClick={create} loading={busy}>
            Create
          </Button>
        </Stack>

        {list === null ? (
          <Stack direction="row" align="center" gap={2}>
            <Spinner />
            <Text color="secondary">Loading…</Text>
          </Stack>
        ) : list.length === 0 ? (
          <Text variant="caption" color="tertiary">
            No app passwords yet.
          </Text>
        ) : (
          <Stack gap={1}>
            {list.map((p) => (
              <Stack key={p.id} direction="row" align="center" justify="between" gap={2}>
                <Text>{p.label || 'Calendar app'}</Text>
                <Stack direction="row" align="center" gap={2}>
                  <Text variant="caption" color="tertiary">
                    {new Date(p.created).toLocaleDateString()}
                  </Text>
                  <Button variant="ghost" size="sm" onClick={() => remove(p.id)}>
                    Revoke
                  </Button>
                </Stack>
              </Stack>
            ))}
          </Stack>
        )}
      </Stack>
    </Field>
  );
}
