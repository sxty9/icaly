// MonthView renders the classic 6×7 month grid from @holistic/ui primitives (no raw HTML).
// Each cell lists that day's event instances (recurrences are pre-expanded by the backend),
// capped with a "+N more" overflow. Clicking a day starts a new event; clicking a chip edits.
import { type MouseEvent } from 'react';
import { Box, Grid, Stack, Text, cn } from '@holistic/ui';
import type { CalEvent } from './types';
import { WEEKDAYS, colorCss, eventOnDay, fmtTime, monthMatrix, sameDay, startOfDay } from './helpers';

interface MonthViewProps {
  anchor: Date;
  events: CalEvent[];
  onDayClick: (day: Date) => void;
  onEventClick: (ev: CalEvent) => void;
}

export function MonthView({ anchor, events, onDayClick, onEventClick }: MonthViewProps) {
  const days = monthMatrix(anchor);
  const today = new Date();
  const month = anchor.getMonth();

  return (
    <Stack gap={0}>
      <Grid cols={7} gap={0}>
        {WEEKDAYS.map((w) => (
          <Box key={w} className="px-2 py-1 text-center">
            <Text variant="caption" color="tertiary" weight="semibold">
              {w}
            </Text>
          </Box>
        ))}
      </Grid>
      <Grid cols={7} gap={0}>
        {days.map((day) => {
          const dayEvents = events
            .filter((ev) => eventOnDay(ev, day))
            .sort((a, b) => new Date(a.start).getTime() - new Date(b.start).getTime());
          const visible = dayEvents.slice(0, 3);
          const overflow = dayEvents.length - visible.length;
          const isToday = sameDay(day, today);
          const inMonth = day.getMonth() === month;

          return (
            <Box
              key={day.toISOString()}
              onClick={() => onDayClick(startOfDay(day))}
              className={cn(
                'min-h-[6.5rem] cursor-pointer border-b border-r border-separator p-1 transition-colors hover:bg-fill/5',
                !inMonth && 'bg-fill/[0.03]',
              )}
            >
              <Stack gap={1}>
                <Box className="flex justify-end">
                  <Text
                    variant="caption"
                    weight={isToday ? 'bold' : 'normal'}
                    className={cn(
                      isToday && 'flex h-5 w-5 items-center justify-center rounded-full bg-accent text-white',
                      !inMonth && !isToday && 'opacity-40',
                    )}
                  >
                    {day.getDate()}
                  </Text>
                </Box>
                {visible.map((ev) => (
                  <Box
                    key={ev.uid + (ev.recurrenceId ?? '')}
                    onClick={(e: MouseEvent) => {
                      e.stopPropagation();
                      onEventClick(ev);
                    }}
                    className={cn(
                      'flex items-center gap-1 rounded-sm px-1 py-0.5 hover:opacity-80',
                      ev.status === 'CANCELLED' && 'line-through opacity-50',
                    )}
                    style={{ backgroundColor: `${colorCss(ev.color)}22` }}
                  >
                    <Box className="h-1.5 w-1.5 shrink-0 rounded-full" style={{ backgroundColor: colorCss(ev.color) }} />
                    <Text variant="caption" truncate>
                      {ev.allDay ? ev.summary : `${fmtTime(ev.start)} ${ev.summary}`}
                    </Text>
                  </Box>
                ))}
                {overflow > 0 && (
                  <Text variant="caption" color="tertiary" className="px-1">
                    +{overflow} more
                  </Text>
                )}
              </Stack>
            </Box>
          );
        })}
      </Grid>
    </Stack>
  );
}
