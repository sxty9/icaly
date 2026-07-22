// AgendaView is a chronological, day-grouped list — the densest, most "portioned" view
// (Minimalism maxim) and the natural fallback on small screens.
import { Box, EmptyState, Stack, Text, cn } from '@holistic/ui';
import type { CalEvent } from './types';
import { byStart, colorCss, fmtDayLong, fmtTime } from './helpers';

interface AgendaViewProps {
  events: CalEvent[];
  onEventClick: (ev: CalEvent) => void;
}

export function AgendaView({ events, onEventClick }: AgendaViewProps) {
  if (events.length === 0) {
    return <EmptyState title="Nothing scheduled" description="No events in this range." />;
  }
  const sorted = [...events].sort(byStart);
  const groups = new Map<string, { day: Date; items: CalEvent[] }>();
  for (const ev of sorted) {
    const d = new Date(ev.start);
    const key = `${d.getFullYear()}-${d.getMonth()}-${d.getDate()}`;
    if (!groups.has(key)) groups.set(key, { day: d, items: [] });
    groups.get(key)!.items.push(ev);
  }

  return (
    <Stack gap={4}>
      {[...groups.values()].map(({ day, items }) => (
        <Stack key={day.toISOString()} gap={1}>
          <Text variant="subhead" weight="semibold">
            {fmtDayLong(day)}
          </Text>
          <Stack gap={1}>
            {items.map((ev) => (
              <Box
                key={ev.uid + (ev.recurrenceId ?? '')}
                onClick={() => onEventClick(ev)}
                className="flex cursor-pointer items-center gap-3 rounded-md border border-separator p-2 hover:bg-fill/5"
              >
                <Box className="h-2.5 w-2.5 shrink-0 rounded-full" style={{ backgroundColor: colorCss(ev.color) }} />
                <Text variant="footnote" color="secondary" className="w-28 shrink-0">
                  {ev.allDay ? 'All day' : `${fmtTime(ev.start)} – ${fmtTime(ev.end)}`}
                </Text>
                <Stack gap={0} grow>
                  <Text truncate className={cn(ev.status === 'CANCELLED' && 'line-through opacity-50')}>
                    {ev.summary || '(no title)'}
                  </Text>
                  {ev.location && (
                    <Text variant="caption" color="tertiary" truncate>
                      {ev.location}
                    </Text>
                  )}
                </Stack>
              </Box>
            ))}
          </Stack>
        </Stack>
      ))}
    </Stack>
  );
}
