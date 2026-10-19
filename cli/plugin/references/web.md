# Web analytics and heatmaps - one script tag on the site

One script tag in the page's `<head>` gives the owner page views, visitors,
referrers, countries, devices and heatmaps (clicks, rage clicks, mouse
movement, scroll depth) per page and device. `npx upcontrol web` mints the key
inside the tag and prints it; you place the tag where every page shares it.

## When

The repo holds a website or a front-end people visit - plain HTML pages, Next,
Vite/React/Vue/Svelte, Astro, Nuxt, SvelteKit, Remix, Gatsby, Hugo, Jekyll, a
WordPress theme - or the user asks for analytics, heatmaps, "where do users
click", "how far do they scroll", "top pages", "where do visitors come from".

## Find the address

Look for the production address in this order: a `CNAME` file,
`vercel.json` / `netlify.toml` / `fly.toml`, `package.json` `homepage`, the
framework's site config (`astro.config.*` `site`, `next-sitemap` `siteUrl`,
`nuxt.config` `site.url`, Hugo `baseURL`, Jekyll `url`), then `README`. Not
found - ask the user one question: what is the site's public address?

Find the local dev address too (the dev server's port in `package.json`
scripts or the framework config, for example `localhost:5173`). The first run
must name every address, the dev one included: the key is bound to exactly the
addresses that run names, and a rerun never widens it.

## Mint

```
npx upcontrol web <address> <dev address>
```

It prints the tag. A rerun reuses `UPCONTROL_PUBLIC_KEY` from `.env` and says
so: it covers only the addresses it was minted for, whatever the rerun names.
To add one, revoke that key in the app (Sources, Manage keys), remove the
`UPCONTROL_PUBLIC_KEY` line from `.env` and rerun naming every address.

On a site-only install (nothing in the repo imports the SDK), if `init` added
`@upcontrol/sdk` to `package.json`, remove that line, because the script tag
does not use it: left in, the lockfile goes stale and a frozen install on CI
fails.

The key inside the tag is a public, origin-bound key meant to be in page HTML, so writing it into a committed layout file is correct. This is
the ONE key you may write into code (rule 6, topic `rules`); a `uc_live_` key
never goes into code.

## Place it

Inside `<head>`, once, in the layout every page shares:

| Stack | Where |
|---|---|
| Next.js app router | `app/layout.tsx`: `<Script src=… data-key=… strategy="afterInteractive" />` from `next/script`, in `<head>` or `<body>` |
| Next.js pages router | `pages/_document.tsx`, inside `<Head>` |
| Vite / CRA / plain SPA | `index.html`, `<head>` |
| Astro | the base layout's `<head>`, e.g. `src/layouts/Layout.astro` |
| Nuxt | `nuxt.config` `app.head.script: [{ src, defer: true, 'data-key': … }]` |
| SvelteKit | `src/app.html` |
| Remix / React Router framework | `app/root.tsx`, `<head>` |
| Gatsby | `gatsby-ssr` `onRenderBody`, `setHeadComponents` |
| Hugo | the base template's head partial |
| Jekyll | `_includes/head.html` |
| WordPress theme | `header.php`, before `wp_head()` |
| plain HTML | every page's `<head>`, or the shared include |

An SPA needs nothing more: route changes are recorded by the script.

Never add the tag twice, never in a component that mounts per page, never
behind a consent gate that does not exist. The script sets no cookies and
stores nothing, so it needs no banner of its own.

## Mark signed-in visitors (when the site has sign-in)

The script cannot tell a member from a stranger, or new from returning: it
keeps nothing on the device. If the site has accounts, tell it, so the owner
can read the map of members and of anonymous visitors apart:

- A server-rendered layout that knows the session: add
  `data-segment="member"` or `data-segment="anon"` to the same tag.
- A single-page app: put the queue stub before the tag, then call it when the
  session is known and again on sign-in and sign-out:

  ```html
  <script>window.upcontrol=window.upcontrol||function(){(upcontrol.q=upcontrol.q||[]).push(arguments)};</script>
  ```

  ```js
  upcontrol('segment', user ? 'member' : 'anon');
  ```

A segment is 1-24 of `a-z`, `0-9`, `_` and `-`, never an id, an email or a
name, and a project keeps its first four, so pick them on purpose (`member`,
`anon`, `trial`, `new` for an account younger than a week). Never send a test
value from a dev machine: it takes one of the four for good. The source of a
visit (search, social, campaign, referral, direct) is worked out by the script
itself and needs nothing.

## Name the clicks that matter (optional)

A funnel from a page to an action needs the action as an event. On the
control itself, nothing else:

```html
<a href="/signup" data-uc-event="cta_click" data-uc-plan="pro">Start free trial</a>
```

or from code, `upcontrol('track', 'cta_click', { plan: 'pro' })` (the queue
stub above makes it safe before the script loads). These share the page view's
visitor, so `Page /pricing → cta_click` is one funnel within a day. Names are
1-40 characters, never `uc.`; fields are short strings, never an email.

To credit signups and payments to a campaign, have the signup form carry
`upcontrol('source')` (it returns the visit's `source`, `utm_*` and `landing`)
to the backend, and send it on the server's own event:
`track('signup', { source, campaign: utm_campaign })`. The visit and the
account are never joined; the event carries where it came from.

## Verify

Ask the user to open the site once in their own browser, then run
`npx upcontrol verify --web`: it reports `page views arriving`. The dev address
counts only if it was passed to the web run that minted the key; otherwise the
tag must be deployed before verify can see a view. Do not open it with a
headless browser yourself: known bots and headless browsers are dropped at the
door, so that visit never counts. `--web` lets page views pass on their own, for
a site-only install with no SDK; without it verify waits for the SDK's marker,
so a visit to the site never hides an SDK that did not connect (topic `verify`).

## Offer the board

Topic `dashboard`: a `heatmap` card (top pages with their engaged time, rage and
dead clicks, a Heatmap button per row), a `stat` of
`{ "source": "web", "name": "engaged" }` titled `Engaged time`, plus `breakdown`
cards of `uc.pageview` by `referrer`, `country` and `device`. Apply
with `npx upcontrol board --add`. If the user wants the site on a board of its
own, `npx upcontrol board --new "Website"` makes one and prints its id: name
that id with `--board <id>` on every later write, or the cards land on the
project's first board instead.

## What the owner gets

Page views, visitors (a daily-rotating hash, no cookie), referrers, UTM tags,
countries, devices, the page each view came from, and engaged time per page
(the tab visible and in use); heatmaps of clicks, rage clicks, dead clicks,
mouse movement and scroll depth per page and device, opened from the Heatmap
card onto the live site, where hovering an element shows how many views
clicked it, and where the map filters by segment (when the site marks them)
and by the kind of source the visit came from.
A plan records a number of visits a month: past it the tag records nothing
until the next month and the app says so. Heatmaps are kept for the plan's
busiest pages.
