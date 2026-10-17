# Branding an installation

An installation can give its sign-in page - and, if it wants, any other page -
its own look: a logo, its colours, a background, its typeface. It does it with
one stylesheet and the files that stylesheet uses, without forking or
rebuilding the client.

What this is: CSS over a documented set of hooks and design tokens.

What this is not: a way to change the page's text, add markup, or run script.
The heading still says Lexarii, and the form is the same form. Nothing in the
branding directory can execute.

---

## Turning it on

Put the files in a directory and point `LEXARII_BRANDING_DIR` at it:

```
/srv/lexarii-branding/
├── branding.css        ← the one file the client loads by name
├── logo.svg
├── background.webp
└── fonts/
    └── House.woff2
```

```bash
LEXARII_BRANDING_DIR=/srv/lexarii-branding
```

The server then serves the directory read-only at `/branding/`, and tells the
client so (`"branding": true` in `GET /api/v1/auth/providers`). The client
links `/branding/branding.css`; everything else is loaded by that stylesheet,
by URLs relative to it (`url('logo.svg')`, `url('fonts/House.woff2')`).

The server refuses to start if the path does not exist or is not a directory,
and logs a warning if it has no `branding.css`. Files are read on every
request, so editing them needs no restart; turning branding on or off does.
Under `make dev` the Vite server forwards `/branding/` to the Go server, as it
does `/api/`.

A ready-made example is in [`branding-example/`](branding-example/): copy
`branding.css` and `logo.svg` into your directory and change the colours. It
is not loaded unless you do.

### What is served, and what is not

| Served | Content-Type |
|---|---|
| `.css` | `text/css; charset=utf-8` |
| `.svg` | `image/svg+xml` |
| `.png` | `image/png` |
| `.webp` | `image/webp` |
| `.avif` | `image/avif` |
| `.woff2` | `font/woff2` |
| `.ico` | `image/x-icon` |

Anything else is a 404: other extensions (no HTML, no scripts), a file over
4 MiB (logged, so you can find it), a name or directory beginning with a dot,
and a directory - there are no listings. Subdirectories are fine. Files are
opened inside the directory with `os.OpenInRoot`, so neither `..` nor a
symbolic link can reach anything outside it.

Every response carries `X-Content-Type-Options: nosniff`,
`Cache-Control: no-cache` (revalidated, so a 304 when nothing changed), and a
sandboxed `Content-Security-Policy` with `default-src 'none'`: a stylesheet or
image loaded by the app is unaffected, but an SVG opened directly in a tab
cannot run script on this origin.

`/branding/` is open to visitors who are not signed in, like the rest of the
web client - the sign-in page is what it is for. Do not put anything there you
would not show a stranger.

### Content-Security-Policy

Neither of Lexarii's policies sets `style-src` (the server's header covers
framing, `<base>`, plugins and form targets; the client's adds `script-src`),
so a same-origin stylesheet needs no change to either. Inline styles are not
involved: the link is an ordinary `<link rel="stylesheet">`.

The policies do not stop a branding stylesheet loading fonts or images from
another server, but keep everything in the branding directory anyway: a
third-party URL on the sign-in page tells that server about every visit to it.

### From a program that embeds Lexarii

The two halves are separate, so a program mounting Lexarii's router can supply
branding its own way:

- **Serving the files.** Either set `Router.Branding` to any `http.Handler`
  (the router mounts it at `GET /branding/{path...}`, open like the rest of
  the client; `http.NewBrandingFiles(dir, log)` is what the server itself
  uses), or serve `/branding/` in front of the router yourself. If you serve
  it in front, it must answer visitors who are not signed in.
- **Telling the client.** Set `AuthHandler.Branding = true`. That is all the
  flag is; the client then requests `/branding/branding.css` and nothing else
  by name.

---

## How the client applies it

The root layout asks for the flag once per load of the app and adds the
`<link>` when the answer arrives. It is never waited on: how the app looks
must not hold up what it shows, so on a slow connection the first frame can be
unbranded for a moment.

The link is added after the design tokens and base styles, so a rule with the
same selector as one of theirs wins. Styles belonging to a page that is
navigated to later can arrive after it, which is why the hooks below are built
so that order does not matter for them.

---

## The contract

These names are supported: they will not be renamed or removed without a note
in [UPGRADING.md](UPGRADING.md). Anything else in the markup - the elements
inside the form, Svelte's generated `svelte-…` classes, the nesting - may
change in any release. Style those through the tokens rather than by
selector.

### Every page: `data-page` on `<body>`

`<body data-page="…">` names the current route: its path with slashes turned
to hyphens, parameters by name without brackets, the root as `home`, and a
path that matched no route as `not-found`. Today that is:

| Route | `data-page` |
|---|---|
| `/` | `home` |
| `/signin` | `signin` |
| `/account` | `account` |
| `/ask` | `ask` |
| `/blocked` | `blocked` |
| `/collections`, `/collections/[id]` | `collections`, `collections-id` |
| `/duplicates` | `duplicates` |
| `/notes` | `notes` |
| `/read/[id]` | `read-id` |
| `/subjects` | `subjects` |
| `/suggestions` | `suggestions` |
| `/works/[id]` | `works-id` |

A new page gets a name by the same rule. Prefer to scope everything to the
pages you mean (`body[data-page='signin'] …`); a rule on a bare element
selector reaches the reader too.

### The sign-in page

```html
<body data-page="signin">
  <main data-page="signin">
    <div class="auth-decoration" aria-hidden="true"></div>
    <div class="auth-card">
      <h1 class="auth-brand">Lexarii</h1>
      …the form, the other ways to sign in…
      <nav class="auth-links">…the administrator's links…</nav>
    </div>
  </main>
</body>
```

| Hook | What it is | Default |
|---|---|---|
| `main[data-page='signin']` | the page's root | no styles of its own |
| `.auth-decoration` | an empty element behind the card, for a background image, a gradient or an animation | `position: fixed; inset: 0; z-index: -1; pointer-events: none` and nothing visible |
| `.auth-card` | everything on the page: the heading, the form, the links | `max-width: 22rem; margin: 12vh auto; padding: 0 var(--space-4)` |
| `.auth-brand` | the heading | serif, `--text-2xl` |
| `.auth-links` | the links from `LEXARII_SIGNIN_LINKS`, when there are any | a rule above them |

The defaults of these five are written inside `:where()`, which gives them no
specificity at all, so any rule you write for them wins, wherever your
stylesheet lands in the document. `.auth-links` is only present when links are
configured.

The decoration sits at `z-index: -1`, in front of the page background and
behind everything else. Give it, or `body`, the background. A background on
`main` would be painted over it.

### The design tokens

The app reads every colour, size and duration from custom properties defined
in [`web/src/lib/styles/tokens.css`](../web/src/lib/styles/tokens.css).
Overriding them recolours every page at once, the reader included, and these
names are part of the contract:

| Group | Tokens |
|---|---|
| Colour | `--bg` `--bg-raised` `--bg-sunken` `--fg` `--fg-muted` `--fg-subtle` `--border` `--accent` `--accent-fg` `--focus-ring` `--danger` |
| Over cover art, every theme | `--overlay-track` `--overlay-fill` `--backdrop` |
| Type | `--font-serif` `--font-sans` `--font-mono` `--text-xs` `--text-sm` `--text-base` `--text-lg` `--text-xl` `--text-2xl` `--text-3xl` `--leading-tight` `--leading-normal` `--leading-reading` |
| Spacing | `--space-1` `--space-2` `--space-3` `--space-4` `--space-6` `--space-8` `--space-12` `--space-16` |
| Shape | `--radius-sm` `--radius-md` `--radius-lg` `--shadow-book` `--shadow-raised` |
| Layout | `--measure` `--cover-aspect` |
| Motion | `--ease-out` `--duration-fast` `--duration-page-turn` |

Colours change with the theme, and the themes are selected by more than
`:root`. To override a colour in every theme, use the same selectors
`tokens.css` uses; yours load later, so they win:

```css
:root { --accent: #1d5c63; }                         /* light, and the default */
:root[data-theme='dark'] { --accent: #7cc4cc; }     /* chosen in the app */
@media (prefers-color-scheme: dark) {                /* following the system */
  :root:not([data-theme='light']):not([data-theme='sepia']):not([data-theme='black']) {
    --accent: #7cc4cc;
  }
}
```

`sepia` and `black` are selected as `:root[data-theme='sepia']` and
`:root[data-theme='black']` in the same way.

Think twice before changing `--measure`, the `--leading-*` tokens or
`--font-serif`: the reader uses them for the books themselves.

---

## Motion

Somebody who has asked their system for less motion should get none from your
branding. The app's own durations already shrink to nothing under
`prefers-reduced-motion: reduce`; an animation of your own has to do the same:

```css
body[data-page='signin'] .auth-decoration {
  animation: drift 40s ease-in-out infinite alternate;
}

@media (prefers-reduced-motion: reduce) {
  body[data-page='signin'] .auth-decoration {
    animation: none;
  }
}
```

Animate `transform` and `opacity`, which the browser can do without laying the
page out again; animating sizes or positions costs every frame on the phones
people sign in from.
