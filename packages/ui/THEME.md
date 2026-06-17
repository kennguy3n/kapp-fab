# @kapp/ui theme — KChat design language

This document is the contract every downstream screen follows. The look
& feel is modelled on [KChat](https://kchat.com/): a violet primary,
near-black ink, Mona Sans type, and fully-pilled buttons. All values
live as CSS custom properties in
[`src/styles/globals.css`](./src/styles/globals.css) and are exposed to
Tailwind v4 through the `@theme` block (CSS-first config — there is no
`tailwind.config.*`).

## How tokens work

- Semantic CSS variables are declared on `:root` (light) and overridden
  under `.dark`. Toggling the `dark` class on `<html>` re-themes the
  whole app at runtime — no recompile.
- The `@theme` block maps each variable to a Tailwind colour
  (`--color-accent: var(--accent)`), so utilities like `bg-accent`,
  `text-fg`, `border-border` just work.
- **Never rename a token.** ~90 screens reference `--accent`, `--bg`,
  `--fg`, `--success`, etc. Retune values; only ever _add_ new tokens.

## KChat colour mapping

| KChat brand | Hex | Token (light) | Token (dark) | Used for |
| --- | --- | --- | --- | --- |
| Primary violet | `#553BD8` | `--accent: oklch(48% 0.23 277)` | `--accent: oklch(80% 0.11 282)` | Primary buttons, links, active nav, logo |
| Accent hover | — | `--accent-hover: oklch(43% 0.23 277)` | `oklch(85% 0.09 282)` | Hover/pressed accent |
| Light violet | `#BAB2FF` | — | `--accent` (dark) | On-dark CTAs / accents |
| Near-black ink | `#191919` | `--fg: oklch(18% 0 0)` | `--fg: oklch(96% 0 0)` | Headings & body text |
| Surface white | `#FFFFFF` | `--bg: oklch(99% 0 0)` | `--bg: oklch(16% 0 0)` | App background |

The greys (`--fg-muted`, `--fg-subtle`, `--border`) were de-tinted from
the old blue cast toward neutral so the violet accent is the only strong
hue in the UI.

## Typography

- `--font-sans` leads with **Mona Sans** (self-hosted variable font,
  weight axis `200 900`, `font-display: swap`), falling back to Inter /
  system UI. No CDN hotlink, no per-weight files.
- Headings are weight **500** with tight tracking:
  `h1,h2,h3 { font-weight: 500; letter-spacing: -0.02em; }`.

## Radii

- Added `--radius-pill: 9999px` → generates the `rounded-pill` utility.
- **Buttons are pills** (`rounded-pill`). **Form controls stay
  `rounded-md`** (Input / Select / Textarea) — pills are for actions,
  not fields.

## Components

### Button
Pill-shaped, weight 500. Primary = violet fill + white text. API
unchanged (`variant`, `size`, `asChild`, `leadingIcon`, `trailingIcon`).
Focus ring uses `--focus-ring` with a 2px offset against `--bg`.

### Badge — status → variant mapping
Badge is the workhorse for statuses across the app. Map domain statuses
to the semantic variant, never to a raw colour:

| Variant | Token | Typical statuses |
| --- | --- | --- |
| `success` | `--success` | active, paid, completed, approved, in-stock |
| `warning` | `--warning` | pending, draft, low-stock, awaiting |
| `danger` | `--danger` | failed, overdue, cancelled, suspended, out-of-stock |
| `info` | `--info` | new, processing, scheduled, in-progress |
| `accent` | `--accent` | featured / branded emphasis |
| `neutral` / `default` | `--bg-muted` | archived, closed, n/a |
| `outline` | transparent | low-emphasis chips, counts |

### Field + Input/Select/Textarea
`Field` is the single labelled form-row wrapper: it renders the label,
the control, an optional help line and an error message, wires
`htmlFor`/`id`/`aria-describedby`, shows the required marker, and pushes
the control into its invalid state when `error` is set. Pass exactly one
control as the child:

```tsx
<Field label="Email" required help="We'll never share it." error={err}>
  <Input type="email" />
</Field>
```

### Eyebrow — the KChat motif
Monospace, leading-underscore label used above headings/sections:

```tsx
<Eyebrow>Communities</Eyebrow>   // renders _Communities
```

## Dark mode

- Controller: [`apps/web/src/lib/theme.ts`](../../apps/web/src/lib/theme.ts).
- Resolution: explicit choice in `localStorage["kapp.theme"]`, else the
  OS `prefers-color-scheme`. While no explicit choice is stored we keep
  following the OS live; once the user toggles, their choice wins.
- `initTheme()` runs before first render (main.tsx) to avoid a flash;
  components read/observe via `useTheme()`. Toggles in the topbar and the
  profile menu share the same store, so they stay in sync.

## Global shell (apps/web)

The shared chrome lives in `apps/web/src/App.tsx`. The richer
search/collapsible/favorites navigation (`AppSidebarNav`) is built in
apps/web composing the layout-only `@kapp/ui` `Sidebar*` primitives — the
primitives stay generic; the product nav lives next to the route table
(its single source of truth). Tenant display names are resolved via
`useTenantName()` ([`apps/web/src/lib/tenant.ts`](../../apps/web/src/lib/tenant.ts)),
which **never renders a raw UUID** — reuse it instead of reading
`kapp.tenant` directly.
