# i18n — how to translate a surface

The console speaks **en** and **ru**. It is hand-rolled: a React context, a
hook, and one typed dictionary per surface. No dependency, no build step, no
extraction tooling, no JSON files.

Read `index.tsx`'s module doc for the *why*. This page is the *how*, and it is
aimed at whoever is translating a page next.

---

## The 60-second version

```tsx
// 1. web/src/lib/i18n/dict/overview.ts   ← your surface, your file, nobody else's
import { defineDict, type Dictionary } from "@/lib/i18n";

const en = {
  "title": "Overview",
  "worstPairs.title": "Worst pairs",
  "worstPairs.empty": "Nothing is failing right now.",
  "alerts.firing": "{count} firing",
} as const;

export type OverviewKey = keyof typeof en;

export const overviewDict: Dictionary<OverviewKey> = defineDict(en, {
  "title": "Обзор",
  "worstPairs.title": "Худшие пары",
  "worstPairs.empty": "Сейчас ничего не сбоит.",
  "alerts.firing": "{count} активных",
});
```

```tsx
// 2. web/src/pages/overview.tsx
import { useT } from "@/lib/i18n";
import { overviewDict } from "@/lib/i18n/dict/overview";

export function OverviewPage() {
  const t = useT(overviewDict);
  return <PageShell title={t("title")}>…{t("alerts.firing", { count: n })}…</PageShell>;
}
```

That is the entire API surface you need: `useT(dict)` → `t(key, vars?)`, plus
`useLocale()` if you are building another switcher (you are not — there is one,
in Settings).

---

## Rules

**One file per surface. Yours.** `dict/<surface>.ts`, named after the page or
the shared component. Never add your keys to someone else's file and never
create a shared "common.ts" — several agents translate in parallel, and a file
two of them edit is a merge conflict waiting to happen. Duplicating a short
string across two dictionaries is cheaper than sharing one.

**`en` is the source table.** It must hold the string **byte-for-byte as it
renders today**, because ~1600 existing assertions read those bytes and the
default locale is always `en`. Moving a string into `t()` must not change it.
If a string genuinely needs rewording, that is a separate change with its own
test update — do not smuggle it in behind a translation.

**`defineDict` is not optional.** It is what makes a missing or misspelled
Russian key a compile error instead of a runtime surprise:

```
dict/overview.ts:14:41 - error: Property '"worstPairs.empty"' is missing in type …
```

A hand-written `{ en, ru }` object literal type-checks while quietly missing
half its Russian. Always `defineDict`.

**Key naming.** `dot.case`, grouped by the thing on screen, most general
segment first: `worstPairs.title`, `worstPairs.empty`, `form.name.label`,
`error.notFound`. Name the key after the **role** of the string, not its
content — `worstPairs.empty`, never `nothingIsFailing`, so rewording the
sentence does not orphan the key.

**Interpolation, not concatenation.** `t("alerts.firing", { count: n })` over
`t("alerts.pre") + n + t("alerts.post")`. Russian word order is not English
word order, and a sentence assembled from fragments cannot be reordered by the
translation. Placeholders are `{name}`; an unknown one renders verbatim so a
typo is visible.

**No plural machinery — use separate keys.** Russian has three forms
(1 узел / 2 узла / 5 узлов) and this module deliberately ships no
`Intl.PluralRules`. Where a count is unavoidable, declare the forms as keys and
pick between them in the component:

```ts
"nodes.one":  "{count} node",    // ru: "{count} узел"
"nodes.few":  "{count} nodes",   // ru: "{count} узла"
"nodes.many": "{count} nodes",   // ru: "{count} узлов"
```

```ts
const n = count % 100, d = count % 10;
const key = n >= 11 && n <= 14 ? "nodes.many" : d === 1 ? "nodes.one" : d >= 2 && d <= 4 ? "nodes.few" : "nodes.many";
```

Better still: sidestep it. "Узлов: 5" and "5 · узлы" need no plural at all, and
a table header or a badge usually reads fine as a bare noun.

---

## What you must NOT translate

Data is data. If the string came from the server or names a machine thing, it
renders verbatim in both languages:

- **problem+json** `title` and `detail`, and every API error message. The server
  wrote a sentence about what it refused; a paraphrase in Russian would be this
  console inventing what the backend said.
- **Names of things**: nodes, pairs, zones, targets, check definitions, webhook
  endpoints, metric names, label names, PromQL, event kinds, permission strings
  (`webhooks:manage`), role names, config keys (`console.retention.*`).
- **Protocol and tool names**: MTR, TCP, UDP, ICMP, DNS, HTTP, PromQL,
  Prometheus, Kubernetes.
- **The product name**: `kconmon-ng`.
- **Dates, times and numbers.** Every stamp is already `toLocaleString()` — the
  *viewer's* locale, which stays correct whichever language the chrome is in.
  Do not add formatting options, do not switch on locale, do not touch them.

Your own labels *around* that data are yours: "Node", "Last seen", "Failed
checks", the empty states, the button verbs, the help paragraphs, every
`aria-label`, and every `title` tooltip.

---

## Worked example — before / after

Before, `components/anonymous-banner.tsx`:

```tsx
<span className="min-w-0">
  <span className="font-medium">Anonymous mode.</span>{" "}
  <span className="text-muted-foreground">
    Authentication is disabled — everyone has the fixed role. Do not use in production.
  </span>
</span>
```

After — the component gains one hook call, and the two sentences move into
`dict/chrome.ts` unchanged:

```tsx
export function AnonymousBanner({ mode = "anonymous" }: { mode?: string }) {
  const t = useT(chromeDict);          // before any early return
  if (mode !== "anonymous") return null;
  …
      <span className="min-w-0">
        <span className="font-medium">{t("banner.anonymous.title")}</span>{" "}
        <span className="text-muted-foreground">{t("banner.anonymous.body")}</span>
      </span>
```

```ts
// dict/chrome.ts
"banner.anonymous.title": "Anonymous mode.",
"banner.anonymous.body":
  "Authentication is disabled — everyone has the fixed role. Do not use in production.",
// …and the ru half:
"banner.anonymous.title": "Анонимный режим.",
"banner.anonymous.body":
  "Аутентификация выключена — у всех одна фиксированная роль. Не используйте в продакшене.",
```

`components/timemachine-bar.tsx` is the worked example for **interpolation**
(`t("timemachine.viewing", { at: stamp(at!) })` — the stamp is computed by the
component and passed in, never translated), and `components/app-sidebar.tsx` is
the one for **a key chosen at runtime** (`NAV_KEYS[path]`, with the English
`NavItem.label` as the fallback for a path the dictionary has not been taught).

---

## Writing the Russian

Живой технический русский, не калька. This is a console an on-call engineer
reads at 3am, not a localised brochure.

- Prefer the noun an engineer would actually say: Investigate → **Расследование**,
  Time Machine → **Машина времени**, Alerting → **Оповещения**, Explore →
  **Метрики** (not «Исследование» — that is Investigate's word).
- Imperative for buttons, as in English: «Сохранить», «Удалить», «Применить».
- Keep the sentence short. English UI copy that runs long usually runs ~15%
  longer in Russian; if it no longer fits the card, shorten the Russian rather
  than widening the layout.
- Do not translate a term the operator will next type into `kubectl`, a PromQL
  box, or a config file.
- One word per concept, everywhere. If your page calls a pair «пара», the next
  page does too.

---

## Testing your surface

The default locale is `en`, always, and `useT` does **not** require a provider
— so every existing test of your page keeps passing untouched, and you should
not add `LocaleProvider` to a test that only asserts English.

Add one case that proves the Russian is wired, wrapping in the provider and
seeding the stored choice:

```tsx
import { LocaleProvider, LOCALE_STORAGE_KEY } from "@/lib/i18n";

it("renders the page in Russian", async () => {
  localStorage.setItem(LOCALE_STORAGE_KEY, "ru");
  render(<LocaleProvider><OverviewPage /></LocaleProvider>);
  expect(await screen.findByRole("heading", { name: "Обзор" })).toBeInTheDocument();
});
```

**Clear the key afterwards.** `vitest.setup.ts` backs `localStorage` with one
`Map` per test *file*, so a locale left behind leaks into every later test in
that file:

```ts
afterEach(() => localStorage.removeItem(LOCALE_STORAGE_KEY));
```

`lib/i18n/index.test.tsx` and `lib/i18n/chrome.test.tsx` are the reference
tests: the first pins the contract (default, persistence, fallback,
interpolation), the second pins the chrome in both languages.

---

## Closed gaps

Kept here rather than deleted, because the *shape* of the answer is the thing
to copy the next time a surface is not just prose.

- **The command palette — CLOSED.** It was English because an entry's title is
  not only a label, it is the *search corpus* `scoreCommand` ranks against, and
  `commands.test.ts` pins English titles as ranking fixtures. Translating the
  display alone would have moved the search target with it.

  The rule that resolved it, now implemented in `lib/commands.ts`:

  > **Display follows the locale. Search matches BOTH languages, in either
  > locale.**

  Concretely: `Command.title` stays the English **source** field — the display
  fallback and half the corpus — and `Command.titleRu` is added beside it.
  `commandTitle(cmd, locale)` is the one place display is decided.
  `scoreCommand` walks both titles and a keyword list holding one English blob
  and one Russian blob, so "matrix" and «матрица» find the same row whichever
  language the console is in. An operator's fingers do not switch language when
  the interface does, and a palette that answered only to the language of the
  moment would be a worse tool than the English-only one.

  Three things that were not obvious and are worth knowing:

  - **The word-boundary class had to learn Cyrillic.** `/[a-z0-9]/` put every
    Cyrillic letter *outside* a word, so any position inside a Russian word
    read as a boundary and the boundary rung silently collapsed for one of the
    two languages. It is `/[a-z0-9Ѐ-ӿ]/` now; English scoring is untouched.
  - **The tie-break sorts by the DISPLAYED title**, so `searchCommands` takes a
    locale — defaulted to `en`, which is what keeps every existing two-argument
    call and fixture unchanged. Sorting «Матрица» by its hidden English name
    would put an alphabet nobody is reading in charge of the list.
  - **`CommandGroup` stayed an English union.** It is a type before it is a
    word; `GROUP_KEYS` maps it to what it renders as, for both the visible
    header and the `role="group"` name.

  Nav TITLES come from `dict/chrome.ts` through `NAV_KEYS` — the table the
  sidebar renders from — so the palette cannot call a surface something the
  sidebar does not.

- **`nav.ts`'s per-item `description` — CLOSED**, with the palette, as
  promised. The English half stays byte-for-byte in `dict/palette.ts` (it is
  `nav.ts`'s own sentence, and `commands.test.ts` asserts the registry carries
  it verbatim); the Russian half is both the sidebar's tooltip and the other
  half of the search corpus. A tooltip an operator can read in Russian and then
  not find by typing what they read would have been worse than either alone.

- **`components/annotations.tsx` and `components/maintenance.tsx` — CLOSED**,
  together, as this file said they had to be. `dict/annotations.ts` and
  `dict/maintenance.ts` are twins the way the components are, and the short
  strings they repeat («Отмена», «Подтвердить удаление», «Область») are the
  duplication the One-file-per-surface rule asks for, not an oversight. Two
  things there were worth knowing:

  - **A form that looks a field up by its own label has to look it up by the
    TRANSLATED one.** `CreateAnnotationForm.focusField` finds the wrong field
    with `[aria-label="…"]`, and a selector still hunting for `"Note"` in a
    Russian console finds nothing and drops focus on `<body>`. It is fed
    `t("form.note")` now — the same key the control was rendered with.
  - **A default that became a `t()` call cannot stay in the parameter list.**
    `MaintenanceBar`'s `createLabel = "＋ maintenance"` moved into the body as
    `createLabel ?? t("bar.create")`: a hook does not run in a default
    expression, and the one caller that passes a verb (Investigate) still wins.

- **The pure modules — CLOSED**, all four, with the trailing-translator pattern
  the wave had already used three times: an optional last parameter defaulting
  to an English `enT`, so every existing call, fixture and English-pinned
  assertion answers the same bytes. `lib/investigation-sources.ts` (the scope
  refusals, `commitWindow`, `CLAMPED_BANNER`, and the words the timeline mappers
  put *around* the server's rows), `lib/matrix-cells.ts`'s `cellSummary`,
  `lib/annotations.ts`'s `outsideWindowNote` and `lib/utils.ts`'s
  `ADHOC_ADDRESS_ERROR`.

  Three shapes from that work are worth copying:

  - **A constant that is also a table entry reads from the table.**
    `CLAMPED_BANNER` and `ADHOC_ADDRESS_ERROR` are now
    `dict.en["…"]` — the rule `lib/timemachine.tsx`'s
    `TIME_MACHINE_DISABLED_REASON` already followed, so the constant a test pins
    and the string a page renders cannot drift.
  - **One sentence, two dictionaries, no shared file.** `outsideWindowNote` is
    written once and called by both bars. Its parameter is typed
    `Translate<"created.outsideWindow">` — the ONE key it asks for — and a
    translator over a wider key union is assignable to a narrower one, so each
    bar passes its own `t` and each dictionary keeps its own copy of the words.
  - **Translation must not move an identity.** Every `ref.id` in the timeline is
    built from a row id or a label set and never from a title, so
    `mergeTimeline`'s dedupe, `pinKey` and every permalink read the same bytes
    in both languages. `CuratedChart.id` was likewise split from its title
    before `titleRu` was added, and `SignalChart` gained an `id` prop because
    its chart id used to be derived from the English words.

- **The last five surfaces — CLOSED**, all of them: `pages/login.tsx`,
  `components/user-menu.tsx`, `components/recent-changes.tsx`,
  `components/investigate-entry.tsx` and `components/stub-page.tsx`, with one
  small dictionary each (`dict/login.ts`, `dict/user-menu.ts`,
  `dict/recent-changes.ts`, `dict/investigate-entry.ts`, `dict/stub-page.ts`)
  and `residue.test.tsx` pinning both halves.

  Five files rather than four keys added to `dict/cards.ts`, even though three
  of the five are mounted BY the object cards: two of them are shared rails
  that the node, pair and target cards each mount with a different filter, and
  a rail is a surface in the sense the One-file-per-surface rule means —
  the same call `dict/annotations.ts` and `dict/maintenance.ts` already made.
  `cards.ts`'s `target.changesNote` stays where it is: that one is the CARD
  explaining its own filter, not the rail speaking.

  Four things worth copying:

  - **English can repeat itself where Russian must not.** `login.tsx` says
    "Sign in" twice on one card — heading and button — and Russian says «Вход»
    then «Войти», because a heading is a noun and a button is an imperative.
    Two keys with identical English values is the right shape, not a duplicate
    to be collapsed; `en` is a source table, not a deduplicated one.
  - **A dictionary that borrows must borrow VISIBLY.**
    `dict/investigate-entry.ts` takes «Открытые инциденты» from
    `dict/overview.ts` and «Открыт» / «Расследовать» from `dict/investigate.ts`,
    and `residue.test.tsx` asserts the equalities rather than trusting the
    reader — one word per concept is a property, so it gets a test. The ENGLISH
    halves stay apart ("Incidents need…" here, "Open incidents need…" there),
    because those are the bytes each file renders today and moving a string is
    not a translation.
  - **The honest lines share a stem with the ones already written.** The rail's
    database sentence opens on the same four words `dict/overview.ts`'s
    `db.note` opens on («Истории нужна база»), and both permission refusals end
    on its «запрос не отправлялся» — two more equalities `residue.test.tsx`
    pins, and the reason an operator meeting the second of these recognises it
    as the same kind of answer rather than a new problem.
  - **A hook goes above every early return, including the ones that are whole
    branches.** `LoginPage` returns in three places — pending, redirect-home,
    and the entire OIDC card — so `useT` sits with the other hooks at the top,
    and the error path (`handleSubmit`) reads its fallback from the same `t`.
    The server's problem+json still wins whenever there is one, verbatim.

  Not closed by this, and deliberately: `StubPage`'s `title` and `description`
  are props from `nav.ts` by way of `routes.tsx`, and they are translated where
  the nav is (`NAV_KEYS`, `NAV_DESC_KEYS`) rather than a second time inside the
  component.

## Known gaps

The honest residue: what an operator still reads in English with the console set
to Russian. Every SURFACE has been taken now — none of the four below is a page
or a component of ours with a sentence left in it. Three stay for a reason the
entry gives; the fourth is a third-party dependency.

- **The two ECharts SERIES names** — `lib/annotations.ts`'s
  `ANNOTATION_SERIES_NAME` and `MAINTENANCE_SERIES_NAME` ("Maintenance"). They
  appear in the chart legend. Left English DELIBERATELY: a legend toggles a
  series *by name*, both are exported constants tests read as identity, and
  translating one of two would be worse than translating none. (There were
  three: `CURSOR_SERIES_NAME` went with the markLine it named, when the time
  cursor became one shared DOM line per page — see `lib/chart-cursor.tsx`.)
- **`@xyflow/react`'s built-in `<Controls>`** on `/topology` — zoom in, zoom
  out, fit view, toggle interactivity. Third-party aria-labels; the component
  takes overrides, so this is a small, self-contained job.
- **The browser's own controls** — the native `<input type="date">` and
  `type="time"` inside the DateTimePicker's manual fields, and every native
  `<select>` popup. Those follow the BROWSER's locale, not this switch, and
  that is correct: they are the platform's chrome, not ours.
- **`index.html`'s `<title>`** — "kconmon-ng Console". It stays: the tag is
  written before any React code runs, before `LocaleProvider` has read
  `localStorage`, so translating it would mean a title that flickers from one
  language to the other on every load. Half of it is the product name and the
  other half is what this thing IS — an identity, in the tab strip, beside
  whatever else the operator has open. Nothing sets `document.title` per route,
  and nothing should start doing so just to move one word.

Not gaps, and deliberately so: the ad-hoc address PLACEHOLDERS on `/mtr` and
`/diagnostics` ("10.0.0.1 or https://example.test/health") are literals an
operator copies, `pages/targets.tsx`'s `DEFINITION_FIELD_PHRASES` matches the
SERVER's own text and is data, and `CuratedChart.title` stays the English source
field that `chartTitle(chart, locale)` chooses display from.
