# icaly

**Holistic service** for calendaring: a Go daemon behind the holistic Caddy proxy plus a dashboard
plugin built on the **`@holistic/ui`** SDK (consumed, never vendored). It stores each user's
calendars as RFC 5545 iCalendar objects, serves an in-app month/week/agenda calendar with live
updates, and exposes the same data over **CalDAV**, a read-only **webcal/ICS** feed, and `.ics`
import/export — so native clients (Apple Calendar, Thunderbird, DAVx⁵) stay in two-way sync.

```
Browser ── https://holistic.local (Caddy, same-origin) ─┐
  ├─ /                       → holistic SPA (bundles this plugin)
  ├─ /api/*                  → holistic backend  (127.0.0.1:8770)
  └─ /api/services/icaly/*   → icalyd (Go)        (127.0.0.1:8780)
```

- **Single sign-on:** the daemon validates the same holistic session (HS256 JWT in the
  `h_access` cookie, secret `/etc/holistic/jwt-secret`) — no separate login. Native CalDAV clients,
  which cannot present the cookie, authenticate over HTTP Basic with a per-user **app password**.
- **Roles = Linux (single source of truth):** admin = membership in the `sudo` group. Admins can do
  everything; finer rights are granted to non-admins per user by `privleg`.
- **Least privilege:** the daemon runs as an unprivileged system user and is sandboxed by systemd.
  It performs no privilege escalation.

## Prerequisites

The [holistic](https://github.com/sxty9/holistic) repo must be present **as a sibling**
(`../holistic`) with the dashboard installed — it provides the `@holistic/ui` SDK and the SPA that
bundles this plugin.

## Quickstart

```bash
cd icaly
sudo ./service setup         # build, wire systemd + Caddy, declare rights, rebuild the SPA
```

After `setup`, the calendar appears in the holistic sidebar. Other commands: `service build`,
`service start|stop|restart`, `service status`, `service update`, `service uninstall [--purge]`.

## Rights (privleg)

Admins can do everything. Each fine-grained right is *declared* in `permissions/icaly.json`, backed
1:1 by a Linux group named `hp_icaly_*` (created by `setup`), and enforced identically in the
backend and the UI as `isAdmin || group ∈ user.groups`. Keep three things in sync:
`permissions/icaly.json` ⇄ `backend/internal/rights` ⇄ the UI right constants in `ui/Dashboard.tsx`.

| Right | Group | Default | Grants |
|---|---|---|---|
| Use calendar | `hp_icaly_view` | on | Read own + shared calendars, subscribe to feeds, RSVP |
| Edit events | `hp_icaly_edit` | on | Create / update / delete events, import `.ics` |
| Share & publish | `hp_icaly_share` | off | Extra calendars, app passwords, public feeds |
| Invite externals | `hp_icaly_invite` | off | Email people off this server (also needs `hp_mail_send`) |
| Delegate access | `hp_icaly_delegate` | off | Act on a calendar delegated by another user |
| Administer calendars | `hp_icaly_admin` | off | Other users' calendars + instance resources (dangerous) |

`default:on` rights are held by everyone until revoked; `default:off` are admin-only until granted.
Enforcement is the same either way.

## HTTP surface

All routes live under `/api/services/icaly/`; error bodies follow holistic's `{"detail": "..."}`
contract. A representative slice:

| Method | Path | Access |
|---|---|---|
| GET | `calendars` · `events` · `event` · `freebusy` | `hp_icaly_view` |
| POST | `events` · `events/delete` · `events/import` | `hp_icaly_edit` |
| POST | `calendars` · `apppasswords` · `apppasswords/delete` | `hp_icaly_share` |
| GET | `events/stream` | `hp_icaly_view` (Server-Sent Events; not CSRF-gated) |
| GET | `feeds/{token}.ics` | capability token — no session |
| \* | `dav/…` | CalDAV: session **or** app-password Basic (`view`; writes need `edit`) |
| POST | `imip/inbound` | shared secret from `maild` — machine-to-machine, never a session |

## Local development

```bash
# Backend
cd backend && go build ./... && go vet ./... && go test ./...

# UI plugin in the holistic dashboard (holistic as a sibling repo)
ln -sfn "$PWD/ui" ../holistic/frontend/external/icaly
( cd ../holistic/frontend && pnpm --filter @holistic/app dev )   # http://localhost:5173
```

UI imports are restricted to `@holistic/ui` + `react` (enforced by holistic's `eslint.services.cjs`
at SPA build time).

## Layout

```
service                     single-file CLI: setup / build / lifecycle
permissions/icaly.json      rights manifest (drop-in for privleg)
backend/                    Go daemon (icalyd)
  cmd/icalyd/                 entry point — listens on 127.0.0.1:8780
  internal/auth/              shared-JWT validation + live group/admin resolution + CSRF
  internal/rights/            the hp_icaly_* group constants (mirror of permissions/icaly.json)
  internal/api/               HTTP routes under /api/services/icaly/
  internal/store/             per-user calendar/event persistence (iCalendar on disk) + change log
  internal/event/             the in-app event/calendar model (the shape the UI + JSON API speak)
  internal/ical/              RFC 5545 (+ 7986) VCALENDAR encode/decode — the on-disk source of truth
  internal/caldav/            CalDAV + WebDAV-Sync surface under dav/
  internal/scheduling/        iTIP invitations, RSVPs and recurring-series edits
  internal/imip/              iMIP (email) calendar-message transport, via maild
  internal/apppass/           per-user CalDAV app passwords (HTTP Basic for native clients)
  internal/geocode/           server-side place-search proxy for the location picker
  internal/push/              per-user change hub feeding the live Server-Sent Events stream
  internal/instance/          resolves a user's calendar-user address + the instance mail domain
ui/                         @holistic/ui plugin (linked into holistic/frontend/external/<id>)
```

### Going further: privileged actions

This service escalates nothing. If it must ever perform OS-level writes, follow the holistic
pattern: a narrow `/usr/local/sbin` wrapper allow-listed in `sudoers.d`, invoked via `sudo -n`,
with `NoNewPrivileges=false` in the unit (see `sxty9/hostek` for a worked example).

## License

MIT — see [LICENSE](LICENSE).
</content>
