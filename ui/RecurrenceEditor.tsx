// RecurrenceEditor is icaly's Google-Calendar-style custom-repeat builder: "every N days/weeks/
// months/years", weekday selection for weekly rules, and an end condition (never / on a date /
// after N occurrences). It edits the RRULE subset in helpers' RecurrenceRule model and emits an
// RRULE string; rules it can't model still pass through the editor as raw text untouched. All
// controls are @holisdk/ui primitives (raw HTML is forbidden in service UIs).
import { useMemo, useState } from 'react';
import { Box, Button, Field, Input, Modal, SegmentedControl, Stack, Text, cn } from '@holisdk/ui';
import { RRULE_WEEKDAYS, buildRRule, isoToDateInput, parseRRule, type RecurEnd, type RecurFreq, type RecurrenceRule } from './helpers';

interface RecurrenceEditorProps {
  initial: string; // an existing RRULE body, or ''
  startISO?: string; // the event's start, to seed a sensible default UNTIL date
  onDone: (rrule: string) => void;
  onClose: () => void;
}

const FREQ_OPTIONS: { value: RecurFreq; label: string }[] = [
  { value: 'DAILY', label: 'Tage' },
  { value: 'WEEKLY', label: 'Wochen' },
  { value: 'MONTHLY', label: 'Monate' },
  { value: 'YEARLY', label: 'Jahre' },
];

export function RecurrenceEditor({ initial, startISO, onDone, onClose }: RecurrenceEditorProps) {
  const seed = useMemo<RecurrenceRule>(
    () => parseRRule(initial) ?? { freq: 'WEEKLY', interval: 1, byday: [], end: { type: 'never' } },
    [initial],
  );
  // A default end date one month after the event start (fallback: today), for the "on date" option.
  const defaultUntil = useMemo(() => {
    const base = startISO ? new Date(startISO) : new Date();
    base.setMonth(base.getMonth() + 1);
    return isoToDateInput(base.toISOString());
  }, [startISO]);

  const [freq, setFreq] = useState<RecurFreq>(seed.freq);
  const [interval, setIntervalN] = useState(String(seed.interval));
  const [byday, setByday] = useState<string[]>(seed.byday);
  const [endType, setEndType] = useState<RecurEnd['type']>(seed.end.type);
  const [until, setUntil] = useState(seed.end.type === 'until' ? seed.end.date : defaultUntil);
  const [count, setCount] = useState(String(seed.end.type === 'count' ? seed.end.count : 10));

  function toggleDay(token: string) {
    setByday((cur) => (cur.includes(token) ? cur.filter((d) => d !== token) : [...cur, token]));
  }

  function done() {
    const n = Math.max(1, parseInt(interval, 10) || 1);
    let end: RecurEnd = { type: 'never' };
    if (endType === 'until') end = { type: 'until', date: until };
    else if (endType === 'count') end = { type: 'count', count: Math.max(1, parseInt(count, 10) || 1) };
    onDone(buildRRule({ freq, interval: n, byday: freq === 'WEEKLY' ? byday : [], end }));
    onClose();
  }

  return (
    <Modal
      open
      onOpenChange={(o) => !o && onClose()}
      title="Benutzerdefinierte Wiederholung"
      size="sm"
      footer={
        <Stack direction="row" justify="end" gap={2}>
          <Button variant="ghost" onClick={onClose}>
            Abbrechen
          </Button>
          <Button variant="primary" onClick={done}>
            Fertig
          </Button>
        </Stack>
      }
    >
      <Stack gap={4}>
        <Field label="Wiederholen alle">
          <Stack direction="row" align="center" gap={2}>
            <Box className="w-20">
              <Input
                type="number"
                min={1}
                value={interval}
                onChange={(e) => setIntervalN(e.target.value)}
                aria-label="Intervall"
              />
            </Box>
            <SegmentedControl value={freq} onChange={(v) => setFreq(v as RecurFreq)} options={FREQ_OPTIONS} />
          </Stack>
        </Field>

        {freq === 'WEEKLY' && (
          <Field label="Wiederholen am">
            <Stack direction="row" gap={1} wrap>
              {RRULE_WEEKDAYS.map((w) => (
                <Box
                  key={w.token}
                  onClick={() => toggleDay(w.token)}
                  className={cn(
                    'flex h-9 w-9 cursor-pointer items-center justify-center rounded-full border text-footnote select-none',
                    byday.includes(w.token)
                      ? 'border-accent bg-accent text-white'
                      : 'border-separator text-text-secondary hover:bg-fill/10',
                  )}
                >
                  {w.label}
                </Box>
              ))}
            </Stack>
          </Field>
        )}

        <Field label="Endet">
          <Stack gap={2}>
            <SegmentedControl
              value={endType}
              onChange={(v) => setEndType(v as RecurEnd['type'])}
              options={[
                { value: 'never', label: 'Nie' },
                { value: 'until', label: 'Am' },
                { value: 'count', label: 'Nach' },
              ]}
            />
            {endType === 'until' && (
              <Input type="date" value={until} onChange={(e) => setUntil(e.target.value)} aria-label="Enddatum" />
            )}
            {endType === 'count' && (
              <Stack direction="row" align="center" gap={2}>
                <Box className="w-20">
                  <Input
                    type="number"
                    min={1}
                    value={count}
                    onChange={(e) => setCount(e.target.value)}
                    aria-label="Anzahl der Termine"
                  />
                </Box>
                <Text color="secondary">Terminen</Text>
              </Stack>
            )}
          </Stack>
        </Field>
      </Stack>
    </Modal>
  );
}
