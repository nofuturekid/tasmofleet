# TasmoFleet design system

Status: binding since the design v2 port. The look is the
**Claude Design v2 prototype** ("v2"): rounded 7/10/12 px surfaces, hairline borders, Geist and Geist
Mono, one blue accent (`oklch` hue 250), light and dark as equal themes. The prototype
(`design/v2/`) is the binding source and is compared pixel by pixel (`make visual`, see
`design/v2/README.md`); the v1 Modernist system (`design/ds-bundle/`) is historical. Wording follows the prototype
(plan D14) except for the rows of the wording table below.

This document is the style guide in the repository: tokens, type, spacing, every component with its
variants and states, the usage rules and the deviations. The same components live on the page
**`/styleguide`** (System → Styleguide, and linked from About), open to every signed-in user: it shows
sample data only (D17). If a component is not on the style guide, it is not part of the design system.

## Where things live

| What | Where |
| --- | --- |
| Tokens | the top of `internal/web/static/app.css`: theme-independent `:root`, then the light block, the no-JS `prefers-color-scheme` block and the explicit dark block |
| Shell | templ `ui.Shell` (`internal/web/ui/shell.templ`): top bar, sub-navigation, user menu, phone header, drawer, crumbs, live indicator and banners, toast region; one navigation model (`ui/nav.go`) feeds all of them |
| Components | `internal/web/ui/components.templ` (props in `components.go`), CSS block "Components (design v2, WP F4)" in `app.css` |
| Style guide page | `internal/web/styleguide.templ` (`sgSections` lists the sections), route `GET /styleguide` |
| Style guide screenshots | `docs/design/styleguide/`, written by `make styleguide-docs` |
| Pages without a prototype screen | templ pages built from the components (plan 3.3), e.g. the notice page (`messagepage.templ`) and the retry page; the live device cells are `cells.templ` |
| Tables, Columns chooser, bulk bar | `tableprefs.go`, `tables.js`, `select.js`, the bulk bar partials (WP F5) |
| Prototype source | `design/v2/` (`template.html`, `prototype-tokens.css`; the self-contained bundle is kept outside the repository) |
| Fonts | `internal/web/static/geist-latin*.woff2`, `geist-mono-latin*.woff2` (OFL, see `NOTICE`), same origin; versioned with `?v=<hash>` and preloaded |

## Tokens

Names are the prototype's with the app's `--color-` prefix (`--bg` is `--color-bg`). Values are copied
verbatim, `oklch()` included. Light is `:root, [data-theme="light"]`; dark is
`:root[data-theme="dark"]`, `[data-theme="dark"]` and, for the no-JS fallback,
`@media (prefers-color-scheme: dark) { :root[data-theme-auto] }`. "same" means the dark theme does not
redefine the token. `tokens_test.go` parses this table, `app.css` and the prototype's token lines and
fails on any difference.

<!-- tokens:start -->
| Token | Light | Dark |
| --- | --- | --- |
| `--color-bg` | `#f4f5f7` | `#0d1013` |
| `--color-surface` | `#fff` | `#14181c` |
| `--color-surface-2` | `#f0f2f4` | `#1a1f24` |
| `--color-surface-3` | `#e5e8ec` | `#242a31` |
| `--color-border` | `#dfe3e7` | `#262c33` |
| `--color-border-strong` | `#c2c9d0` | `#3a424b` |
| `--color-text` | `#13171c` | `#e4e7ea` |
| `--color-text-2` | `#48535e` | `#a9b2bb` |
| `--color-text-3` | `#5c6772` | `#939da7` |
| `--color-accent` | `oklch(0.5 0.13 250)` | `oklch(0.7 0.12 250)` |
| `--color-accent-hover` | `oklch(0.44 0.13 250)` | `oklch(0.77 0.11 250)` |
| `--color-accent-bg` | `oklch(0.95 0.025 250)` | `oklch(0.27 0.05 250)` |
| `--color-accent-fg` | `oklch(0.45 0.13 250)` | `oklch(0.8 0.1 250)` |
| `--color-on-accent` | `#fff` | `#0b1015` |
| `--color-ok` | `oklch(0.63 0.14 155)` | `oklch(0.72 0.15 155)` |
| `--color-ok-fg` | `oklch(0.44 0.1 155)` | `oklch(0.8 0.13 155)` |
| `--color-ok-bg` | `oklch(0.95 0.035 155)` | `oklch(0.27 0.05 155)` |
| `--color-warn` | `oklch(0.76 0.14 75)` | `oklch(0.8 0.14 80)` |
| `--color-warn-fg` | `oklch(0.47 0.1 65)` | `oklch(0.85 0.12 85)` |
| `--color-warn-bg` | `oklch(0.96 0.045 85)` | `oklch(0.29 0.05 75)` |
| `--color-danger` | `oklch(0.58 0.19 27)` | `oklch(0.68 0.18 25)` |
| `--color-danger-fg` | `oklch(0.49 0.17 27)` | `oklch(0.8 0.13 25)` |
| `--color-danger-bg` | `oklch(0.96 0.025 27)` | `oklch(0.29 0.07 25)` |
| `--color-danger-solid` | `oklch(0.52 0.19 27)` | `oklch(0.55 0.17 25)` |
| `--color-neutral` | `#98a1ab` | `#6b7581` |
| `--color-neutral-fg` | `#4f5964` | `#b0b8c1` |
| `--color-neutral-bg` | `#eceff2` | `#242a31` |
| `--color-term-bg` | `#12161b` | `#090c0f` |
| `--color-term-fg` | `#d3d9df` | same |
| `--color-term-dim` | `#8b95a1` | same |
| `--color-term-cmd` | `#93c3ff` | same |
| `--color-term-err` | `#ff9d8f` | same |
| `--color-grip-hover` | `color-mix(in oklab, var(--color-accent) 30%, transparent)` | `color-mix(in oklab, var(--color-accent) 30%, transparent)` |
| `--shadow-lg` | `0 14px 44px rgba(16, 24, 32, 0.18), 0 2px 6px rgba(16, 24, 32, 0.08)` | `0 14px 44px rgba(0, 0, 0, 0.6)` |
| `--color-console-rule` | `rgba(255, 255, 255, 0.08)` | same |
| `--color-console-line` | `rgba(255, 255, 255, 0.14)` | same |
<!-- tokens:end -->

`--color-info*` does not exist: informational banners and badges use the accent set
(`--color-accent-bg` / `--color-accent-fg`), as the prototype's "info" toasts do. A filled destructive
button uses `--color-danger-solid`.

Theme-independent tokens (all on `:root`):

| Group | Tokens |
| --- | --- |
| Fonts | `--font-sans: "Geist", ui-sans-serif, system-ui, sans-serif`; `--font-mono: "Geist Mono", ui-monospace, Menlo, monospace`. Body is `400 14px/1.45` (`--fs-base`, `--lh-body`) |
| Type sizes | `--fs-h1` 22px (weight 600), `--fs-h2-lg` 15px, `--fs-h2` 14px, `--fs-h2-sm` 13px, `--fs-body` 13.5px, `--fs-ui` 13px, `--fs-meta` 12.5px, `--fs-small` 12px, `--fs-badge` 11.5px, `--fs-stat` 28px (the stat tile value, `600 28px/1`, tabular figures; measured in F4) |
| Radii | `--r-control` 7px, `--r-card` 10px, `--r-panel` 12px, `--r-banner` 9px, `--r-code` 8px, `--r-sm` 6px, `--r-badge` 5px, `--r-xs` 4px, `--r-pill` 999px; dots and avatars use `50%` |
| Spacing | `--sp-N` is N pixels, for the values the prototype uses: 1-12, 14, 16, 18, 20, 22, 24, 28, 32, 36, 40. The prototype is not on a 4px grid, nothing is rounded |
| Card gap | `--card-gap` (= `--sp-12`): the space between sibling cards, down and across (the device overview, its State changes and Transactions row); `device-layout.spec.ts` fails when two cards come closer |
| Controls | `--ctl` 34px (40px on phones), `--ctl-sm` 28px, `--ctl-lg` 40px, `--ctl-pad` 14px, `--ctl-pad-sm` 10px |
| Layout | `--container` 1640px, `--page-pad: clamp(12px, 2.4cqw, 32px)`, `--gutter` 16px, `--hair-w` 1px |
| Inline values made tokens | `--backdrop`, `--shadow-knob`, `--shadow-bar`, `--color-console-rule` (theme independent), `--color-grip-hover` (per theme: it mixes the accent) |
| Layers | `--z-sticky` 10, `--z-menu` 20, `--z-header` 30, `--z-header-menu` 40, `--z-dialog` 1000, `--z-toast` 1001 |

No element is a size container (F3 as built): `cqw` in `--page-pad` resolves against the viewport
width including the scrollbar, which is what the prototype's root measures, and fixed elements
(bulk bar, toasts, drawer) stay positioned against the window.

## Type

Body is `400 14px/1.45 var(--font-sans)`. Each component writes its exact `font` shorthand from the
prototype's markup in its CSS block; the recurring sizes:

| Use | Font |
| --- | --- |
| Page title (h1) | `600 22px/1.2`, letter-spacing -0.015em |
| Error page title | `600 26px/1.2`, -0.02em |
| Sign-in card title | `600 20px/1.2` |
| Dialog title | `600 16px/1.3` |
| Empty state heading | `600 17px` |
| Card and section heading (h2) | `600 14px`; `600 13px` in device overview cards; `600 15px` in release notes |
| Stat tile value | `600 28px/1` (`--fs-stat`), tabular figures, -0.02em |
| List rows, inputs | `400 13.5px` |
| Buttons, tabs | `500 13px` (primary `600 13px`); tabs and top-bar groups `500 13.5px` |
| Field labels, secondary lines | `500 12.5px` / `400 12.5px` |
| Help, error lines, hints | `400 12px` |
| Badges, counters | `500 11.5px` |
| Mono values (addresses, versions, ids) | `400 12.5px var(--font-mono)`; transaction ids `500 12px` |

## Spacing, shape and layout

- Spacing tokens are named by pixel value (`--sp-N`); the prototype is not on a 4 px grid and nothing
  is rounded. The gaps that recur: 8 between buttons, 10 inside cards, 14 between cards, 16 card
  padding and below the page header.
- Radii: controls 7, cards 10, dialogs and the auth card 12, banners and toasts 9, code 8, menu items
  6, badges 5, pills 999, dots and avatars 50 %.
- Heights: controls 34 px (40 px on phones), small buttons 32, top bar 52, sub-navigation 40, phone
  header 56, row targets 44.
- `main` is at most 1640 px wide with `clamp(12px, 2.4cqw, 32px)` gutters and 120 px at the bottom.
- Shadows: `--shadow-lg` for menus, dialogs and toasts only; `--shadow-knob` for switches.

## Components

Every component is a templ component in `internal/web/ui/components.templ` with typed props (values
are text, templ escapes them; no `templ.Raw`, no style attribute) and one CSS block in `app.css`
("Components (design v2, WP F4)"). `/styleguide` shows each section below, the first three as the
prototype's UI-states screen; `make styleguide-docs` writes the screenshots embedded here
(`docs/design/styleguide/<section>-<desktop|phone>-<light|dark>.png`). `styleguide_doc_test.go` fails
when a section of `/styleguide` has no entry here or no screenshot, so run `make styleguide-docs` and
commit the images after changing a component.

### Inline banners

A dot, a bold lead, the text and at most one action, in the tones ok, accent (information), warn and danger. Above the page content, under the page header; a banner stays until its cause is gone. A failure the user has to act on is `role=alert`, everything else `role=status`. The shell's flash, the setup notice and the connection banners are banners too. `ui.ErrorSummary` is the danger banner of a rejected form ("Fix 1 field: ... Nothing was saved yet.").

Component: `ui.Banner(BannerData{Tone, Title, Text, Action, Alert})`.

![Inline banners, desktop, light](../design/styleguide/banners-desktop-light.png)

Also: [desktop, dark](../design/styleguide/banners-desktop-dark.png) · [phone, light](../design/styleguide/banners-phone-light.png) · [phone, dark](../design/styleguide/banners-phone-dark.png)

### Toasts

Bottom right, newest last. Success and information fade after 4.5 s; warnings and failures stay until dismissed and carry ×. Sources: the server flash after a redirect (an inline banner without JavaScript), the htmx event `toast` (`HX-Trigger`) and any button with `data-toast` (and `data-toast-tone`). The in-flow `ui.ToastStatic` (style guide) can show an action before ×; live toasts have none.

Component: `toast.js`, `ui.ToastStatic` (in the flow, style guide only).

![Toasts, desktop, light](../design/styleguide/toasts-desktop-light.png)

Also: [desktop, dark](../design/styleguide/toasts-desktop-dark.png) · [phone, light](../design/styleguide/toasts-phone-light.png) · [phone, dark](../design/styleguide/toasts-phone-dark.png)

### Live connection indicator

Top bar, right of the navigation, only on pages that hold the event stream (S6). Live pulses green, Reconnecting... pulses amber faster and shows the warn banner with the attempt and the next try, Offline is red and shows the danger banner with Retry now. The pulse stops under reduced motion.

Component: shell (`live.js`), `ui.LivePill` (style guide).

![Live connection indicator, desktop, light](../design/styleguide/live-desktop-light.png)

Also: [desktop, dark](../design/styleguide/live-desktop-dark.png) · [phone, light](../design/styleguide/live-phone-light.png) · [phone, dark](../design/styleguide/live-phone-dark.png)

### Buttons

Default (surface, strong border, `500 13px`), primary (accent, `600 13px`, padding 14), danger (outlined text in danger-fg), danger-solid (the confirming button of a destructive dialog), ghost, link (text only, 32 px target) and the icon button (⋯). Height `--ctl` (34 px, 40 px on phones), small 32 px. A Btn with `Href` is a link (it only navigates or opens the dialog named by `Dialog`); without it a `type=button` that never submits by accident. One action, one component; the confirming button repeats the trigger's verb and tone.

Component: `ui.Button(Btn{Label, Kind, Href, Type, Name, Value, Small, Icon, Aria, Disabled, Dialog})`. `Name` and `Value` make a submit button that sends a value (the console's quick commands); `Aria` is the accessible name of an icon button.

![Buttons, desktop, light](../design/styleguide/buttons-desktop-light.png)

Also: [desktop, dark](../design/styleguide/buttons-desktop-dark.png) · [phone, light](../design/styleguide/buttons-phone-light.png) · [phone, dark](../design/styleguide/buttons-phone-dark.png)

### Badges, status dots and avatars

Badges are 20 px tinted labels (`500 11.5px`, radius 5) for states, results and counters. A status is a dot with its word (Online / Offline / Unknown), never the colour alone. Tones: ok, warn, danger, neutral, accent (`.t-*` classes); an unknown tone renders neutral. The avatar shows the initials; its colour follows the username, so a user keeps one colour everywhere.

Component: `ui.Badge(tone, label)`, `ui.StatusDot(tone, label)`, `ui.Avatar(name, username)`.

![Badges, status dots and avatars, desktop, light](../design/styleguide/badges-desktop-light.png)

Also: [desktop, dark](../design/styleguide/badges-desktop-dark.png) · [phone, light](../design/styleguide/badges-phone-light.png) · [phone, dark](../design/styleguide/badges-phone-dark.png)

### Cards and key/value rows

A card is a surface with a hairline border and radius 10: padded (16 px, h2 `600 14px`, an optional line under it) or a list card (a header row with the title and a link, then rows separated by hairlines). Key/value rows are a 118 px label column and the value; addresses, MACs, versions and ids are monospace.

Component: `ui.Card(title, sub)`, `ui.ListCard(title, linkLabel, linkHref)`, `ui.KV(rows)`.

![Cards and key/value rows, desktop, light](../design/styleguide/cards-desktop-light.png)

Also: [desktop, dark](../design/styleguide/cards-desktop-dark.png) · [phone, light](../design/styleguide/cards-phone-light.png) · [phone, dark](../design/styleguide/cards-phone-dark.png)

### Form fields

Label above (`500 12.5px`, text-2), the control (34 px, radius 7, strong border), then the help line (`400 12px`, text-3) or, when the server rejected the value, the error (`400 12px`, danger-fg) instead of the help. A rejected input carries `aria-invalid` and `aria-describedby` pointing at its message; validation stays on the server. Mono fields for addresses and ports. Forms lay out in a grid of 260 px columns (`gap 14px 18px`); the style guide sample is at most 820 px wide, the add-user form 760 px.

Component: `ui.Field(FieldData{...})`, `ui.SelectField`, `ui.ErrorSummary`.

![Form fields, desktop, light](../design/styleguide/forms-desktop-light.png)

Also: [desktop, dark](../design/styleguide/forms-desktop-dark.png) · [phone, light](../design/styleguide/forms-phone-light.png) · [phone, dark](../design/styleguide/forms-phone-dark.png)

### Choices and switches

Radio cards (auth method, backup keep or drop) are `label > input[type=radio]` with a title and a line; the checked card takes the accent tint. Segmented controls (Advisory / Required, Operator / Admin, Side by side / Unified) are radio inputs in one frame, so they work without JavaScript; the theme switch in the shell is the same look. Checkboxes are 16 px in the accent colour. A setting switch is a checkbox with `role=switch` (38 x 22, knob `--shadow-knob`); the power switch of a row is 34 x 20 inside a 48 x 44 px target and disabled while the device is offline.

Component: `ui.RadioCards`, `ui.Segmented`, `ui.Check`, `ui.Toggle`, `ui.PowerSwitch`.

![Choices and switches, desktop, light](../design/styleguide/controls-desktop-light.png)

Also: [desktop, dark](../design/styleguide/controls-desktop-dark.png) · [phone, light](../design/styleguide/controls-phone-light.png) · [phone, dark](../design/styleguide/controls-phone-dark.png)

### Stat tiles and fleet health

A tile is a link to the filtered list: dot and label (`500 12.5px`), the value (`--fs-stat`, tabular figures) and a hint. Tiles sit in a grid of 150 px columns. The health bar shows online, offline and unknown as three segments; their widths come from the server as SVG attributes (no inline style under the CSP).

Component: `ui.StatTile(StatTileData{...})`, `ui.HealthBar(ok, off, unk)`; `ui.ComplianceBar(match, deviate, unknown)` draws a policy rule's compliance the same way.

![Stat tiles and fleet health, desktop, light](../design/styleguide/tiles-desktop-light.png)

Also: [desktop, dark](../design/styleguide/tiles-desktop-dark.png) · [phone, light](../design/styleguide/tiles-phone-light.png) · [phone, dark](../design/styleguide/tiles-phone-dark.png)

### Attention list and transactions

"Needs attention" lists address-change proposals first, with two inline actions (POST forms with the CSRF token), then devices with a dot, the reason and ›. The transaction card shows id, title, state badge, the three-colour progress (done, failed, running) and a summary; history rows show id, title, result badge and age. A row's progress is a mini bar with the percentage.

Component: `ui.AttentionList`, `ui.TxnCard`, `ui.TxnRow`, `ui.SegBar`, `ui.MiniBar`.

![Attention list and transactions, desktop, light](../design/styleguide/lists-desktop-light.png)

Also: [desktop, dark](../design/styleguide/lists-desktop-dark.png) · [phone, light](../design/styleguide/lists-phone-light.png) · [phone, dark](../design/styleguide/lists-phone-dark.png)

### Page header, crumbs and tabs

The page header is the h1 (`600 22px/1.2`), a subtitle line and the page's buttons on the right. The crumbs sit above it (`/` separators); the shell draws them from the navigation unless the page passes its own trail. Tabs are links with an underline (`inset 0 -2px 0` accent) and an optional count; a tab that cannot be used now is disabled with the reason as its title. On a phone the tab row scrolls sideways.

Component: `ui.PageHeader(title, sub)`, `ui.Crumbs(trail, base)`, `ui.Tabs(label, tabs)`.

![Page header, crumbs and tabs, desktop, light](../design/styleguide/tabs-desktop-light.png)

Also: [desktop, dark](../design/styleguide/tabs-desktop-dark.png) · [phone, light](../design/styleguide/tabs-phone-light.png) · [phone, dark](../design/styleguide/tabs-phone-dark.png)

### Menus

A row menu is a `<details>` with the ⋯ button and a 250 px panel (radius 10, `--shadow-lg`); items are links (`400 13px`), destructive ones in danger-fg after a rule, disabled ones in text-3 with the reason on a second line. `menu.js` keeps one menu open, closes on Escape and on a click elsewhere; `up` opens above the button (the server marks the last rows, the script decides by the viewport). The style guide shows menus open in the flow with `data-static`, which every script leaves alone.

Component: `ui.RowMenu(label, items, up)`, the shell's user menu and drawer.

![Menus, desktop, light](../design/styleguide/menus-desktop-light.png)

Also: [desktop, dark](../design/styleguide/menus-desktop-dark.png) · [phone, light](../design/styleguide/menus-phone-light.png) · [phone, dark](../design/styleguide/menus-phone-dark.png)

### Dialogs

A confirmation dialog has the title (`600 16px`), the consequence, optionally the list of what changes or a command input, Cancel and the verb (danger-solid for destructive actions, primary otherwise); it posts with the CSRF token. `dialog.js` opens it from any `[data-dialog]` and closes the menu it was opened from; without JavaScript the trigger's link goes to the confirmation page (`ui.ConfirmPage`, the same look). The step-up dialog ("Confirm it's you") offers the identity provider's re-authentication and, for local accounts, the password.

Component: `ui.Dialog(DialogData{...})`, `ui.StepUp(StepUpData{...})`, `ui.ConfirmPage(ConfirmData{...})`.

![Dialogs, desktop, light](../design/styleguide/dialogs-desktop-light.png)

Also: [desktop, dark](../design/styleguide/dialogs-desktop-dark.png) · [phone, light](../design/styleguide/dialogs-phone-light.png) · [phone, dark](../design/styleguide/dialogs-phone-dark.png)

### Empty, no match and loading

An empty list shows an icon tile, a heading, one line and up to three buttons in a dashed frame. A filter without results says so and offers Clear filters. The skeleton is only for lazily loaded fragments (D12); server-rendered pages never show it. Its shimmer stops under reduced motion.

Component: `ui.Empty(title, text)`, `ui.NoMatch(text, clearHref)`, `ui.Skeleton(label, rows)`.

![Empty, no match and loading, desktop, light](../design/styleguide/states-desktop-light.png)

Also: [desktop, dark](../design/styleguide/states-desktop-dark.png) · [phone, light](../design/styleguide/states-phone-light.png) · [phone, dark](../design/styleguide/states-phone-dark.png)

### Error page

The status code (mono), a title (`600 26px`), what happened, the path that failed and two ways on. Used for 404, 403 ("Not allowed") and a feature that is switched off.

Component: `ui.ErrorPage(code, title, body, path, primary, secondary)`.

![Error page, desktop, light](../design/styleguide/errors-desktop-light.png)

Also: [desktop, dark](../design/styleguide/errors-desktop-dark.png) · [phone, light](../design/styleguide/errors-phone-light.png) · [phone, dark](../design/styleguide/errors-phone-dark.png)

### Sign-in card

Sign-in, setup and password pages: the brand above, a 420 px card (radius 12, padding 24) with the h1 (`600 20px`), banners, fields and the button, and the footer "TasmoFleet x.y.z · host" in mono (D15).

Component: `ui.AuthCard(base, version, host)`.

![Sign-in card, desktop, light](../design/styleguide/auth-desktop-light.png)

Also: [desktop, dark](../design/styleguide/auth-desktop-dark.png) · [phone, light](../design/styleguide/auth-phone-light.png) · [phone, dark](../design/styleguide/auth-phone-dark.png)

### Console

A dark panel (`--color-term-*`): a header with the live dot, the connection and Clear; timestamped lines (commands in term-cmd, refusals in term-err); the prompt form; quick commands are submit buttons that send their command (no JavaScript). The placeholder and the quick commands never offer `Status 0` (D9).

Component: `ui.Console(ConsoleData{...})`.

![Console, desktop, light](../design/styleguide/console-desktop-light.png)

Also: [desktop, dark](../design/styleguide/console-desktop-dark.png) · [phone, light](../design/styleguide/console-phone-light.png) · [phone, dark](../design/styleguide/console-phone-dark.png)

### Backup comparison

Side by side (setting, older, newer; changed rows tinted warn) or unified (− and + lines in the danger and ok tints). The layout and "show all" are query parameters, so the comparison works without JavaScript.

Component: `ui.Diff(DiffData{...})`.

![Backup comparison, desktop, light](../design/styleguide/diff-desktop-light.png)

Also: [desktop, dark](../design/styleguide/diff-desktop-dark.png) · [phone, light](../design/styleguide/diff-phone-light.png) · [phone, dark](../design/styleguide/diff-phone-dark.png)

### Release notes

Headings, paragraphs, lists, code blocks and the breaking-change box in the warn tone. Release notes are text only: nothing from the feed becomes markup (the firmware/notes.go contract).

Component: `ui.Notes(blocks)`.

![Release notes, desktop, light](../design/styleguide/notes-desktop-light.png)

Also: [desktop, dark](../design/styleguide/notes-desktop-dark.png) · [phone, light](../design/styleguide/notes-phone-light.png) · [phone, dark](../design/styleguide/notes-phone-dark.png)

## Usage rules (must not regress)

- **Theme.** `ui.Shell` renders `<html data-theme-auto>` unless the user chose Light or Dark (stored
  per user, D11). Dark tokens apply only under `:root[data-theme="dark"]` or, without JavaScript,
  under `prefers-color-scheme: dark` while `data-theme-auto` is present, so "Auto" follows the system
  live. The token blocks also select `[data-theme]`, so a part of a page can show the other theme.
- **CSRF.** A GET fallback never sits in a POST form that carries the `csrf` field. The
  fallback of a dialog trigger is a link (`a ... href=".../delete" data-dialog="..."`) or its
  own GET form without a token. No GET request of the app carries `csrf=` in its URL
  (`e2e/tests/design-port.spec.ts`, `deviceconfirm_test.go`).
- **Fixed bars stay inside the viewport.** The bulk bar never clips its last button at 375 px;
  layout guard (a) checks every fixed or sticky element.
- **Phone breakpoint is 45rem (720 px)**: phone layout up to `max-width: 44.99rem`, desktop from
  `min-width: 45rem`, the prototype's switch (`S.w < 720`); the Columns chooser and `tables.js` use
  the same 45rem. Other breakpoints remain: 60rem (the bulk bar folds into one Actions menu) and
  72rem (the audit table layout, the two-column settings grid).
- **Folds.** `details.fold` holds something secondary and is closed at every width unless it has
  `open` (About's effective configuration, account and user editing, a rule's Unjudged devices, the
  system container settings). No page uses `details.m-fold` any more (the phone-only fold of the
  first design port); its CSS is left over.
- **Form errors sit at the input.** A handler that rejects a value sets the input's name
  (`ErrField`) next to a human sentence (`Error`). `ui.Field` (and a page's own inputs) mark that
  input (`aria-invalid`, `aria-describedby` to the sentence right after it), the focus goes there,
  and the page leaves its banner out, so the message appears once. Errors that belong to no input (rate limit,
  stale page, a failed device call) stay in the banner with no `ErrField`. Go error chains
  (`auth: ...`, `tasmota: ... HTTP 404`) never reach the browser: a small mapping per area turns
  them into copy (`auth.inputProblem`, `formProblem`, `ruleProblem`, `deviceProblem`), the details
  go to the log. Row forms that repeat per row keep their errors in the banner. `fielderror_test.go`
  guards the pattern on the pages.
- **One action, one component.** Actions are buttons; links only navigate. Destructive actions
  confirm exactly once, in the dialog (or its page without JavaScript), and the confirming
  button repeats the trigger's verb and tone.
- **No inline styles or scripts** (strict CSP). Colours, spacing, radii and type come from tokens.
- **A dialog shared by many rows** (the device list) is one `dialog#dlg-row` that `dialog.js`
  fills from the `data-dlg-*` attributes of the link that opened it, so the list does not carry
  a dialog per device. The device list does not reload while a menu or dialog is open or rows
  are selected (`select.js`).
- **Dialog openers.** Opening a dialog closes any open Actions menu (`dialog.js`). When the
  confirming verb is itself "Cancel ..." the opener sets `data-dlg-dismiss` ("Keep running") so
  the two buttons never read alike.
- **Fonts.** Geist and Geist Mono, self-hosted (`geist-*.woff2`, variable `wght`, SIL OFL 1.1,
  `geist-OFL.txt`). Kerning is the browser default, as in the prototype; the space after a capital
  keeps its width (`shell-css.spec.ts`).
- **Tokens are the prototype's.** Colours, shadows and the font stacks are copied value for value
  (`design/v2/prototype-tokens.css`); `tokens_test.go` fails when `app.css`, that file or the table
  below differ. v1 names (`--color-success*`, `--color-divider`, `--space-*`, the generated ramps) do
  not exist and get no alias.

- **Navigation is the same for every role** (F4, S7). Every role sees all groups and pages; each page
  enforces its own permission (403). A group button leads to the first page the role may open, so it
  never ends on a 403 (an operator's System button opens the style guide). Feature switches still
  decide: a missing service has no page and no entry.
- **Components take text, not markup.** No `templ.Raw`, no style attribute, no inline script; sizes
  from data are SVG attributes. `components_test.go` feeds a hostile string through every
  text-carrying component.

## Wording

The prototype's wording is the default (D14); the product name is "TasmoFleet" in all UI and docs text
(D15). The deliberate changes (kept in sync with the plan, section 0.1):

| Prototype term | Our term | Reason |
| --- | --- | --- |
| Enforce (policy severity) | Required | "Enforce" suggests TasmoFleet writes the value to devices. It never does: a deviation is counted and preselected for Make compliant. |
| Warn (policy severity) | Advisory | Our `warn` is the stronger level; reusing the word would invert its meaning. |
| `Status 0` (console quick command and placeholder) | `Status 5` | Safety rule 2: `Status 0` bursts corrupt ESP8266 heap and replies (D9). |
| "tasmofleet" (product name in text) | TasmoFleet | D15. Identifiers stay lower case. |

## Deviations from the prototype

The structure deviations S1-S7 are listed with their reasons in the plan (section 0.2); in short:

1. S1: `/styleguide` (the prototype's UI-states screen, open to every signed-in user) is the last
   item of the System menu and linked from About.
2. S2: the Columns chooser pins left or right (F5, P2).
3. S3: the policy live check can be paused (N3, N21).
4. S4: About keeps the effective configuration and the source line below its two cards.
5. S5: the phone drawer ends with the account, What's new, the Light / Dark / Auto switch and Sign out.
6. S6: the live indicator and the connection banners only on pages that hold the event stream.
7. S7: the navigation is the same for every role; pages enforce their permission. The style guide
   continues below the prototype's screen with the component gallery (the screen itself is the
   prototype's to the pixel, the gallery starts where the prototype's page ends).
8. The device list's Restart uses the shared `dlg-row` dialog; its trigger is a link (`a`), not a
   `formmethod="get"` button, because of the CSRF rule. Delete is a link to the confirmation page.
9. `font-variant-numeric: tabular-nums` is set where the prototype sets it (stat values, progress
   percentages), not on `body`.
10. Geist Mono ships without the box-drawing subset of the prototype bundle; the app draws no box
    characters.
11. Pages without a prototype screen (plan 3.3: notices such as "Nothing to retry", the retry
    confirmation, backup view, prune, restore progress and others) are built from the
    components above (page header, card, confirmation card, banner, buttons); they have no
    pixel reference and are checked by their handler tests, e2e specs and the layout guards.
