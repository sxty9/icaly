// SubscribePanel exposes three ways to connect a calendar: the read-only webcal feed (copy URL
// into Apple/Google/Thunderbird), .ics import/export, and — for two-way sync — CalDAV with a
// per-user app password. The feed token is the calendar's own capability credential, so the
// webcal URL is shown to anyone who can view it; importing requires the edit right and managing
// app passwords requires the share right.
import { useEffect, useState } from 'react';
import {
  Box,
  Button,
  DownloadIcon,
  Field,
  Input,
  Modal,
  Spinner,
  Stack,
  Text,
  UploadIcon,
  type ServiceApiClient,
  type ServiceContextProps,
} from '@holistic/ui';
import type { AppPassword, AppPasswordsResp, Calendar, CreatedAppPassword } from './types';
import { pickTextFile, webcalURL } from './helpers';

interface SubscribePanelProps {
  api: ServiceApiClient;
  ui: ServiceContextProps['ui'];
  calendar: Calendar;
  canImport: boolean;
  canShare: boolean;
  username: string;
  onChanged: () => void;
  onClose: () => void;
}

export function SubscribePanel({ api, ui, calendar, canImport, canShare, username, onChanged, onClose }: SubscribePanelProps) {
  const feedUrl = calendar.feedToken ? api.url(`feeds/${calendar.feedToken}.ics`) : '';
  const webcal = feedUrl ? webcalURL(feedUrl) : '';
  const davUrl = api.url('dav/');

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
      onChanged();
    } catch (e) {
      ui.toast({ title: 'Import failed', description: (e as Error).message, variant: 'error' });
    }
  }

  function exportIcs() {
    window.open(api.url(`events/export?calendar=${encodeURIComponent(calendar.id)}`), '_blank');
  }

  return (
    <Modal open onOpenChange={(o) => !o && onClose()} title={`Subscribe & share — ${calendar.name}`} size="md">
      <Stack gap={5}>
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
      </Stack>
    </Modal>
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
