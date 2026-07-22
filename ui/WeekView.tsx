// WeekView lays out the seven days of the anchor's week as columns, each listing its event
// instances. A full time-of-day grid is a later refinement; this column list is the live,
// portioned week overview for Phase 1a.
import { type MouseEvent } from 'react';
import { Box, Grid, Stack, Text, cn } from '@holistic/ui';
import type { CalEvent } from './types';
import { addDays, colorCss, eventsOnDay, fmtTime, sameDay, startOfDay, startOfWeek } from './helpers';

interface WeekViewProps {
  anchor: Date;
  events: CalEvent[];
  onDayClick: (day: Date) => void;
  onEventClick: (ev: CalEvent) => void;
}

export function WeekView({ anchor, events, onDayClick, onEventClick }: WeekViewProps) {
  const start = startOfWeek(anchor);
  const days = Array.from({ length: 7 }, (_, i) => addDays(start, i));
  const today = new Date();

  return (
    <Grid cols={7} gap={0}>
      {days.map((day) => {
        const dayEvents = eventsOnDay(events, day);
        const isToday = sameDay(day, today);
        return (
          <Box
            key={day.toISOString()}
            onClick={() => onDayClick(startOfDay(day))}
            className="min-h-[18rem] cursor-pointer border-b border-r border-separator p-1 hover:bg-fill/5"
          >
            <Stack gap={1}>
              <Box className="flex flex-col items-center py-1">
                <Text variant="caption" color="tertiary">
                  {day.toLocaleDateString(undefined, { weekday: 'short' })}
                </Text>
                <Text
                  variant="subhead"
                  weight={isToday ? 'bold' : 'normal'}
                  className={cn(isToday && 'flex h-7 w-7 items-center justify-center rounded-full bg-accent text-white')}
                >
                  {day.getDate()}
                </Text>
              </Box>
              {dayEvents.map((ev) => (
                <Box
                  key={ev.uid + (ev.recurrenceId ?? '')}
                  onClick={(e: MouseEvent) => {
                    e.stopPropagation();
                    onEventClick(ev);
                  }}
                  className={cn(
                    'rounded-sm px-1 py-0.5 hover:opacity-80',
                    ev.status === 'CANCELLED' && 'line-through opacity-50',
                  )}
                  style={{ backgroundColor: `${colorCss(ev.color)}22`, borderLeft: `3px solid ${colorCss(ev.color)}` }}
                >
                  <Text variant="caption" weight="medium" truncate>
                    {ev.summary || '(no title)'}
                  </Text>
                  {!ev.allDay && (
                    <Text variant="caption" color="tertiary" truncate>
                      {fmtTime(ev.start)}
                    </Text>
                  )}
                </Box>
              ))}
            </Stack>
          </Box>
        );
      })}
    </Grid>
  );
}
