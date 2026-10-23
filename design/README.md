# Design system source and prototype history

This folder holds the source of the tasmofleet design system and the Claude Design
prototype it came from. It was moved here from the local-only repo `tasmofleet-design`
(history not kept).

**What binds the app:** the admin page `/styleguide` in the running app (built from the real
partials) and `docs/spec/design-system.md`. `design/` is the design-system source and the
prototype history; the app does not read it at build time. `internal/web/static/app.css` is
a copy of `ds-bundle/styles.css` plus `patterns.css` plus page rules. If the two differ,
the style guide in the app wins.

## Layout

| Path | What |
| --- | --- |
| `ds-bundle/` | The upload root for Claude Design: `styles.css` (generated), `components/`, `foundations/`, `templates/` (the four prototype pages and `templates/nav-patterns/` with `PORT.md`, the binding port guide, `README.md`, `patterns.css`), `fonts/` (Archivo, OFL), `assets/`, `theme.js`, `theme.json`, `thumbnail.html` |
| `v2/` | The second prototype (design v2 port): `template.html`, the bundle, the reference manifest; see `v2/README.md` |
| `tools/` | Build and check scripts (Python 3 standard library, Node for `shots.mjs`) |
| `.design-sync/` | `config.json` (sync settings), `NOTES.md` (sync log), `conventions.md` (tokens and class vocabulary) |

## Build

`ds-bundle/styles.css` is generated. Edit `tools/styles.src.css`, then, from this folder:

```sh
python3 -I tools/build.py         # styles.src.css -> ds-bundle/styles.css (adds ramps, dark tokens, state hooks)
python3 -I tools/contrast.py      # WCAG contrast of the token pairs, both themes
python3 -I tools/theme_json.py    # ds-bundle/theme.json
python3 -I tools/pages.py         # regenerates the preview pages
node tools/shots.mjs              # screenshots of the preview pages (needs Playwright; output not committed)
```

The scripts find their files relative to their own location, so they work from any
directory. Do not commit screenshots or `__pycache__`.

## Re-sync with Claude Design

The upload root is `ds-bundle/`; the project id is not committed (the sync asks for it). Run
`/design-sync` in Claude Code from this folder. The first sync after the move re-verifies
everything (there is no `_ds_sync.json` anchor yet). Known template defects are listed in
`.design-sync/NOTES.md`.

## Moving a change into the app

1. Change `tools/styles.src.css` (or the prototype pages) and rebuild.
2. Port the change to `internal/web/static/app.css` and the templates following
   `ds-bundle/templates/nav-patterns/PORT.md`.
3. Make sure the component shows on `/styleguide`; a component that is not there is not
   part of the design system.

## Licence

The Archivo fonts in `ds-bundle/fonts/` are under the SIL Open Font License; `OFL.txt` stays
next to them (see also `NOTICE` at the repo root).
