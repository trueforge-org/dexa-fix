---
applyTo: "pkg/frontend/templates/**,pkg/frontend/static/**"
paths:
  - "pkg/frontend/templates/**"
  - "pkg/frontend/static/**"
description: "Rules for HTML templates, htmx and static files: Content-Security-Policy, escaping, anchors, in-app documentation."
---

# Templates and static files

These rules apply to `pkg/frontend/templates/*.html` and `pkg/frontend/static/*`. They add to [`frontend.instructions.md`](frontend.instructions.md).

## Security headers constrain the markup

Every response carries `Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'`.

- **No inline scripts and no inline event handlers** (`onclick=`, `<script>...</script>`). Put code in a file under `static/` and load it with `<script src>`; use htmx attributes (`hx-post`, `hx-get`, `hx-trigger`, ...) for behaviour.
- The one place the policy is widened is `render`, for a page with `Track` set (optional analytics and ads, `tracking.go`): the hosts of the services an administrator turned on are added, and nothing else changes. Load third-party code only from `static/consent.js`, only after consent.
- Inline `style=` attributes are tolerated for a few existing sizing cases; do not add more, and no inline `<style>` blocks. Put styles in `static/app.css`.
- Forms and htmx requests go to **same-origin** URLs. Cross-origin POSTs are refused by `http.CrossOriginProtection`; there is no CSRF token to add.
- Do not load anything from a CDN or another origin.

## Templates

- `html/template` escapes for the context; never convert user data to `template.HTML` (or `JS`, `URL`) to "fix" escaping.
- Static assets are referenced with the `asset` function (`{{ asset "app.css" }}`), which adds a content hash for cache busting. Do not hard-code `/static/...?v=`.
- Pages are listed in `pages` in `server.go`; a new template needs an entry and a `pageData` field, not a new data path around the backend.
- Help text uses the `help` function; keep it short and in the voice of the surrounding page.
- Do not put secrets, tokens, recovery codes or other one-time values in URLs or hidden fields that outlive the response. One-time values (new token, recovery codes, QR code data) are rendered from the response body only.
- Passkey and htmx scripts call the existing JSON routes (`/passkey/...`); keep response shapes (`{"error": ...}`, `{"redirect": ...}`).

## Forms, buttons and sections

Settings, Admin and the add-entry pages share one layout, so a control is always where you expect it:

- Every form ends with a `div.form-actions` (right-aligned, wraps, has a gap). The primary button is last. Do not put a button in a grid cell of a `.form-row`, and do not leave a bare `<form><button>` in a card.
- A secondary or destructive button that belongs to the same form goes in the same `.form-actions` with `class="start"` to sit on the left. If its action is a different form, give the button `form="<id>"` and an empty `<form id>` next to it.
- Destructive actions in a form use `class="danger"`. `button.link` (red text) is only for row actions inside a table (Revoke, Remove a passkey, Delete a user or entry).
- A card with more than one form or topic is split into `.section`s, each starting with an `<h3>` (a `.card-head` with an `<h3>` and a status pill when it has a state). One `<h2>` per card, with at most one `{{help}}` badge on it.
- Put the help text under the heading as `p.help`, or as `<small>` after the input it explains. Use `.narrow` and `label.inline` instead of inline `style=` for short number inputs.

## In-app documentation

`templates/docs.html` is the documentation inside the app. It mirrors `docs/`. When behaviour, flags, routes, settings or deployment steps change, update both. Every `href="#..."` needs a matching `id="..."`; `docs_test.go` checks it. Add new sections to the table of contents at the top.
