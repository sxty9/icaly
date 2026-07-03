# CLAUDE.md

Template for a **holistic service**. A developer clones it, runs `./service init <name>`, and
builds out the backend + dashboard plugin. The holistic SDK (`@holistic/ui`) is **consumed
only** — never vendored or modified here.

## Where things are

- `service` — the CLI. Auto-detects the service id from `permissions/<id>.json`; owns
  `init`/`setup`/lifecycle and generates the systemd unit, Caddy route and rights drop-in inline.
- `backend/internal/auth/auth.go` — shared-JWT (`h_access`) validation, live OS group/admin
  resolution, CSRF. Service-agnostic; reuse as-is.
- `backend/internal/api/api.go` — the HTTP surface under `/api/services/<id>/`. The `guard`
  helper does auth → optional right → optional CSRF. Add routes here.
- `backend/internal/rights/` — the `hp_*` group constant(s); mirror `permissions/<id>.json`.
- `ui/index.tsx` — default-exports the `ServicePlugin`; `id` MUST equal the manifest `service`.
- `ui/Dashboard.tsx` — the plugin UI; renders **only** `@holistic/ui`, gates with `userHasRight`.

## Rules

- Enforce every right as `isAdmin || group ∈ user.groups`, in both the backend and the UI.
- Keep three things in sync: `permissions/<id>.json` ⇄ `internal/rights` ⇄ the UI right constant.
- UI may import only `@holistic/ui` and `react` (holistic's `eslint.services.cjs` enforces it).
- The daemon runs unprivileged and escalates nothing. Privileged work needs a narrow sudo
  wrapper (see `sxty9/hostek`), not blanket sudo.

## Verify (from the repo root)

```bash
(cd backend && go build ./... && go vet ./...)
python3 ../holistic/services/dashboard/lib/holistic-perms.py validate ./permissions
```

<holistic_architecture_maxims>
Du arbeitest im "Holistic" Services-Ökosystem. Validiere JEDE Implementierung gegen diese Maximen, bevor du Code schreibst oder änderst:

<maxim name="Single Source of Truth">
- Jede Datenabfrage und jedes Setzen von Daten ist atomar.
- Existiert für die Entität bereits ein Zugangspunkt? Zwingend wiederverwenden. Baue niemals parallele Datenpfade.
</maxim>

<maxim name="Reuse before Build">
1. Suche die Komponente im Holistic SDK.
2. Wenn ähnlich vorhanden: Erweitere die SDK-Komponente.
3. Wenn nicht vorhanden, aber domänenübergreifend: Baue sie im SDK.
4. Nur wenn hochspezifisch: Baue sie lokal in diesem Service.
</maxim>

<maxim name="Uniformity">
- Code-Struktur: Syntaktischer Aufbau, Code-Layout, Naming-Conventions und Repository-Skeletons müssen exakt den anderen Holistic-Services entsprechen.
- CLI: Jeder Service stellt eine CLI bereit. Diese muss sich in Syntax und Semantik strikt an den Holistic-Standard halten. Nutze bestehende Holistic-Services als Referenz.
- Rechtesystem: Einheitliches, symmetrisches Design. Jeder Service stellt ein Rechte-Manifest für den zentralen Privilege Service bereit. Feingranularität ist nur dort erlaubt, wo sie fachlich zwingend geboten ist.
</maxim>

<maxim name="Minimalism">
- Das System ist "intuitiv by Design". Keine Hilfstexte, Notes oder Tooltips im UI (außer bei extremen Spezialfällen).
- Präsentiere Daten nicht im Überfluss, sondern strikt portioniert und bedarfsgerecht.
</maxim>
</holistic_architecture_maxims>

<decision_rule>
Führe keine Änderungen durch, die Redundanz schaffen oder UI-Bloat erzeugen. Bevorzuge Refactoring des SDKs gegenüber lokalem Custom-Code.
</decision_rule>
