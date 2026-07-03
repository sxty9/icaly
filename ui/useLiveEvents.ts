// useLiveEvents backs the calendar's live updates. The backend's change-log seq drives an SSE
// stream (events/stream); we open it as the primary signal and refetch the visible range on any
// "changed" frame. useLiveQuery polling is the fallback: a slow 60s safety net while SSE is
// healthy, dropping to 4s when the stream is down (plan: SSE + polling fallback). EventSource is
// authenticated by the same-origin session cookie — no header, no CSRF (plan m7).
import { useEffect, useRef, useState } from 'react';
import { useLiveQuery, type ServiceApiClient } from '@holistic/ui';
import type { CalEvent, EventsResp } from './types';

export interface LiveEvents {
  events: CalEvent[];
  loading: boolean;
  error: Error | null;
  live: boolean; // true while the SSE stream is connected
  refresh: () => void;
}

export function useLiveEvents(api: ServiceApiClient, calendar: string, from: Date, to: Date): LiveEvents {
  const [live, setLive] = useState(false);
  const fromISO = from.toISOString();
  const toISO = to.toISOString();

  const q = useLiveQuery<EventsResp>(
    () =>
      api.get<EventsResp>(
        `events?calendar=${encodeURIComponent(calendar)}&start=${encodeURIComponent(fromISO)}&end=${encodeURIComponent(toISO)}`,
      ),
    live ? 60000 : 4000,
    [calendar, fromISO, toISO, live],
  );

  // Keep the latest refresh in a ref so the long-lived SSE listener always calls the current one.
  const refreshRef = useRef(q.refresh);
  refreshRef.current = q.refresh;

  useEffect(() => {
    let closed = false;
    let es: EventSource | null = null;
    try {
      es = new EventSource(api.url('events/stream'));
    } catch {
      setLive(false);
      return;
    }
    const onOpen = () => !closed && setLive(true);
    const onError = () => !closed && setLive(false); // EventSource auto-reconnects; polling covers the gap
    const onChanged = (e: MessageEvent) => {
      let forThisCal = true;
      try {
        const data = JSON.parse(e.data) as { calendar?: string };
        if (data && data.calendar) forThisCal = data.calendar === calendar;
      } catch {
        /* malformed frame: refresh anyway */
      }
      if (forThisCal) refreshRef.current();
    };
    es.addEventListener('open', onOpen);
    es.addEventListener('error', onError);
    es.addEventListener('changed', onChanged as EventListener);
    return () => {
      closed = true;
      if (es) es.close();
    };
  }, [api, calendar]);

  return {
    events: q.data?.events ?? [],
    loading: q.loading,
    error: q.error,
    live,
    refresh: q.refresh,
  };
}
