# holistic-service-template

A starting point for a **holistic service**: a Go backend behind the holistic Caddy proxy
plus a dashboard plugin built on the **`@holistic/ui`** SDK (consumed, never vendored). The
example ships a working **rights interface** that `privleg` can configure per user.

```
Browser ── https://holistic.local (Caddy, same-origin) ─┐
  ├─ /                          → holistic SPA (bundles this plugin)
  ├─ /api/*                     → holistic backend       (127.0.0.1:8770)
  └─ /api/services/icaly/*  → icalyd (Go)        (127.0.0.1:8780)
```

- **Single sign-on:** the daemon validates the same holistic session (HS256 JWT in the
  `h_access` cookie, secret `/etc/holistic/jwt-secret`) — no separate login.
- **Roles = Linux (single source of truth):** admin = membership in the `sudo` group.
- **Least privilege:** the daemon runs as an unprivileged system user and is sandboxed by
  systemd. It performs no privilege escalation.

## Prerequisites

The [holistic](https://github.com/sxty9/holistic) repo must be present **as a sibling**
(`../holistic`) with the dashboard installed — it provides the `@holistic/ui` SDK and the
SPA that bundles this plugin.

```
git clone git@github.com:sxty9/holistic.git
git clone git@github.com:sxty9/holistic-service-template.git icaly
```

## Quickstart

```bash
cd icaly
./service init mytool        # rename the placeholder 'icaly' → 'mytool' everywhere
sudo ./service setup         # build, wire systemd + Caddy, declare rights, rebuild the SPA
```

After `setup`, the service appears in the holistic sidebar. Other commands: `service build`,
`service start|stop|restart`, `service status`, `service update`, `service uninstall [--purge]`.

## The rights interface (privleg)

Admins can do everything. To offer a **non-admin** user a fine-grained right, the service
*declares* it in `permissions/icaly.json`; `privleg` then toggles it per user. Each right
is backed 1:1 by a Linux group named `hp_*`; `setup` creates the group and the service enforces
the right with `isAdmin || group ∈ user.groups`.

```jsonc
{
  "service": "icaly", "version": 1,
  "categories": [{
    "id": "demo", "label": "Demo",
    "permissions": [{
      "id": "access", "label": "Access privileged data",
      "group": "hp_icaly_demo", "default": false
    }]
  }]
}
```

The group must match `^hp_[a-z0-9][a-z0-9_-]{0,27}$`; each group backs exactly one right.
Pick `default` so a host *without* privleg is unchanged: `default:false` = admin-only until
granted; `default:true` = granted to everyone until revoked. Enforcement is the same either
way. The example wires this end to end:

| Method | Path | Access | Demonstrates |
|---|---|---|---|
| GET | `info` | any signed-in user | public read |
| GET | `data` | admin or `hp_icaly_demo` | rights-gated read |
| POST | `action` | admin or `hp_icaly_demo` | rights-gated write (CSRF) |

Backend enforcement is in `backend/internal/api/api.go` (the group constant lives in
`backend/internal/rights/`); the UI mirrors it with `userHasRight` in `ui/Dashboard.tsx`.

## Local development

```bash
# Backend
cd backend && go build ./... && go vet ./...

# UI plugin in the holistic dashboard (holistic as a sibling repo)
ln -sfn "$PWD/ui" ../holistic/frontend/external/icaly
( cd ../holistic/frontend && pnpm --filter @holistic/app dev )   # http://localhost:5173
```

UI imports are restricted to `@holistic/ui` + `react` (enforced by holistic's
`eslint.services.cjs` at SPA build time).

## Layout

```
service                     single-file CLI: init / setup / build / lifecycle
permissions/icaly.json  rights manifest (drop-in for privleg)
backend/                    Go daemon (icalyd)
  cmd/icalyd/             entry point — listens on 127.0.0.1:8780
  internal/auth/              shared-JWT validation + live group/admin resolution + CSRF
  internal/rights/            the hp_* group(s) this service declares
  internal/api/               HTTP routes under /api/services/icaly/
ui/                         @holistic/ui plugin (linked into holistic/frontend/external/<id>)
```

### Going further: privileged actions

This template escalates nothing. If your service must perform OS-level writes, follow the
holistic pattern: a narrow `/usr/local/sbin` wrapper allow-listed in `sudoers.d`, invoked via
`sudo -n`, with `NoNewPrivileges=false` in the unit (see `sxty9/hostek` for a worked example).

## License

MIT — see [LICENSE](LICENSE).
