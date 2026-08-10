import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { AnnotationBar } from "@/components/annotations";
import { MaintenanceBar } from "@/components/maintenance";
import { LOCALE_STORAGE_KEY, LocaleProvider } from "@/lib/i18n";
import { annotationsDict } from "@/lib/i18n/dict/annotations";
import { investigateDict } from "@/lib/i18n/dict/investigate";
import { maintenanceDict } from "@/lib/i18n/dict/maintenance";
import { TimeMachineProvider } from "@/lib/timemachine";
import type { Annotation, MaintenanceWindow } from "@/lib/types";

/**
 * The two SHARED BARS, in both languages.
 *
 * They earn their own file for the reason lib/i18n/README.md's closed-gap note
 * gave: they are shared *bars* rather than shared badges — a form, a list and a
 * count line each — and they are mounted on /explore, on /investigate and on
 * the node, pair and target cards. A string missed here reads wrong on five
 * surfaces, and the two bars must not drift apart from each other either.
 *
 * The English cases are NOT duplicates of components/annotations.test.tsx and
 * components/maintenance.test.tsx: those render with no <LocaleProvider> at
 * all, which lib/i18n defines as English, and that is the property they pin.
 * These mount the provider and assert the bytes did not move.
 */

const json = (body: unknown) =>
  new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

const ALL = ["annotations:read", "annotations:write", "maintenance:read", "maintenance:write"];

function stubFetch(permissions: string[] = ALL) {
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string) => {
      if (String(url).includes("/api/v1/auth/me")) {
        return Promise.resolve(
          json({ subject: { kind: "user", id: "u1", displayName: "Ada", groups: [], roles: [] }, permissions }),
        );
      }
      return Promise.resolve(json({}));
    }),
  );
}

function note(over: Partial<Annotation> = {}): Annotation {
  return {
    id: "a-1",
    startAt: "2026-08-01T11:30:00Z",
    scope: "",
    text: "rolled the gateway",
    createdBy: "user:ada",
    createdAt: "2026-08-01T10:00:00Z",
    ...over,
  };
}

function win(over: Partial<MaintenanceWindow> = {}): MaintenanceWindow {
  return {
    id: "m-1",
    scope: "",
    startAt: "2026-08-01T11:30:00Z",
    endAt: "2026-08-01T11:45:00Z",
    reason: "switch upgrade",
    createdBy: "user:ada",
    createdAt: "2026-08-01T10:00:00Z",
    ...over,
  };
}

/** The bars take their rows as PROPS (the hook is the other half and has its
 *  own suite), so nothing here depends on a list request landing. */
function renderBars({
  locale,
  permissions = ALL,
  annotations = [] as Annotation[],
  windows = [] as MaintenanceWindow[],
  error,
}: {
  locale?: "en" | "ru";
  permissions?: string[];
  annotations?: Annotation[];
  windows?: MaintenanceWindow[];
  error?: Error | null;
} = {}) {
  stubFetch(permissions);
  if (locale) localStorage.setItem(LOCALE_STORAGE_KEY, locale);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <LocaleProvider>
        <TimeMachineProvider>
          <AnnotationBar scope="" annotations={annotations} error={error} onChanged={vi.fn()} />
          <MaintenanceBar scope="" windows={windows} error={error} onChanged={vi.fn()} />
        </TimeMachineProvider>
      </LocaleProvider>
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  // One Map backs localStorage for this whole FILE (vitest.setup.ts).
  localStorage.removeItem(LOCALE_STORAGE_KEY);
});

/* ── English, with the provider mounted ─────────────────────────────────── */

describe("the two bars in English", () => {
  it("still say exactly what they always said", async () => {
    renderBars({ locale: "en", annotations: [note()], windows: [win()] });

    expect(await screen.findByText("1 annotation in this window · scope global")).toBeInTheDocument();
    /* findBy, not getBy: the maintenance bar is hidden until /auth/me answers
       — permission HIDES it, and that gate is the twin's one real difference. */
    expect(await screen.findByText("1 maintenance window in this window · scope global")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "＋ annotate" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "＋ maintenance" })).toBeInTheDocument();
    expect(screen.getByRole("list", { name: "Annotations in this window" })).toBeInTheDocument();
    expect(screen.getByRole("list", { name: "Maintenance windows in this window" })).toBeInTheDocument();
  });

  it("keep the plural the English count line always had", async () => {
    renderBars({ locale: "en", annotations: [note(), note({ id: "a-2" })] });
    expect(await screen.findByText("2 annotations in this window · scope global")).toBeInTheDocument();
    expect(await screen.findByText("0 maintenance windows in this window · scope global")).toBeInTheDocument();
  });
});

/* ── Russian ────────────────────────────────────────────────────────────── */

describe("the annotation bar in Russian", () => {
  it("counts, captions the scope and names its list", async () => {
    renderBars({ locale: "ru", annotations: [note()] });
    expect(await screen.findByText("1 заметка в этом интервале · область глобальная")).toBeInTheDocument();
    expect(screen.getByRole("list", { name: "Заметки в этом интервале" })).toBeInTheDocument();
  });

  it("picks the THREE Russian count forms, where English has two", async () => {
    const rows = (n: number) => Array.from({ length: n }, (_, i) => note({ id: `a-${i}` }));

    renderBars({ locale: "ru", annotations: rows(2) });
    expect(await screen.findByText(/^2 заметки в этом интервале/)).toBeInTheDocument();
    cleanup();

    renderBars({ locale: "ru", annotations: rows(5) });
    expect(await screen.findByText(/^5 заметок в этом интервале/)).toBeInTheDocument();
    cleanup();

    // 11–14 take the `many` form whatever their last digit says.
    renderBars({ locale: "ru", annotations: rows(11) });
    expect(await screen.findByText(/^11 заметок в этом интервале/)).toBeInTheDocument();
  });

  it("translates every label in the create form, and the field the picker is named by", async () => {
    renderBars({ locale: "ru" });
    fireEvent.click(await screen.findByRole("button", { name: "＋ заметка" }));

    const form = await screen.findByRole("form", { name: "Новая заметка" });
    /* The scope line is three nodes — a word, the value in its own bold span,
       and the rest — so it is asserted as the sentence it renders as. */
    expect(within(form).getByText(/^Область/)).toHaveTextContent("Область глобальная задана этим экраном.");
    expect(within(form).getByText("Начало")).toBeInTheDocument();
    expect(within(form).getByText("Конец (необязательно)")).toBeInTheDocument();
    expect(within(form).getByText("Оставьте пустым, если отмечаете один момент.")).toBeInTheDocument();
    // The picker's own accessible name, and the textarea's — both are the keys
    // focusField() now looks the wrong field up by.
    expect(within(form).getByRole("button", { name: "Конец" })).toBeInTheDocument();
    expect(within(form).getByLabelText("Заметка")).toBeInTheDocument();
    expect(within(form).getByRole("button", { name: "Создать заметку" })).toBeInTheDocument();
    expect(within(form).getByRole("button", { name: "Отмена" })).toBeInTheDocument();
  });

  it("refuses an empty note in Russian, and moves focus to the field it named", async () => {
    renderBars({ locale: "ru" });
    fireEvent.click(await screen.findByRole("button", { name: "＋ заметка" }));
    fireEvent.click(await screen.findByRole("button", { name: "Создать заметку" }));

    expect(await screen.findByRole("alert")).toHaveTextContent("Нужен текст заметки.");
    // The lookup is fed the TRANSLATED label; a selector still hunting for
    // "Note" would have dropped focus on <body>.
    expect(document.activeElement).toBe(screen.getByLabelText("Заметка"));
  });

  it("says the honest thing when the read failed, rather than claiming zero", async () => {
    renderBars({ locale: "ru", error: new Error("boom") });
    expect(await screen.findByText("Заметки недоступны.")).toBeInTheDocument();
    expect(screen.queryByText(/0 заметок/)).toBeNull();
  });

  it("uses the console's confirm-delete idiom on a row", async () => {
    renderBars({ locale: "ru", annotations: [note()] });
    fireEvent.click(await screen.findByRole("button", { name: "Удалить заметку: rolled the gateway" }));
    expect(
      await screen.findByRole("button", { name: "Подтвердить удаление заметки: rolled the gateway" }),
    ).toBeInTheDocument();
    // The button's accessible NAME is the aria-label (it carries the note's
    // own text); the words on its face are the shared idiom.
    expect(screen.getByText("Подтвердить удаление")).toBeInTheDocument();
  });
});

describe("the maintenance bar in Russian", () => {
  it("counts in the three forms and names its list", async () => {
    renderBars({ locale: "ru", windows: [win()] });
    expect(await screen.findByText("1 окно работ в этом интервале · область глобальная")).toBeInTheDocument();
    expect(screen.getByRole("list", { name: "Окна работ в этом интервале" })).toBeInTheDocument();

    cleanup();
    renderBars({ locale: "ru", windows: [win(), win({ id: "m-2" }), win({ id: "m-3" })] });
    expect(await screen.findByText(/^3 окна работ в этом интервале/)).toBeInTheDocument();
  });

  it("translates its create form and the sentence naming the server as arbiter", async () => {
    renderBars({ locale: "ru" });
    fireEvent.click(await screen.findByRole("button", { name: "＋ работы" }));

    const form = await screen.findByRole("form", { name: "Новое окно работ" });
    expect(within(form).getByText("Начало")).toBeInTheDocument();
    expect(within(form).getByText("Должен быть позже начала, иначе сервер откажет.")).toBeInTheDocument();
    expect(within(form).getByLabelText("Причина")).toBeInTheDocument();
    expect(within(form).getByRole("button", { name: "Создать окно работ" })).toBeInTheDocument();
  });

  it("refuses an empty reason in Russian", async () => {
    renderBars({ locale: "ru" });
    fireEvent.click(await screen.findByRole("button", { name: "＋ работы" }));
    fireEvent.click(await screen.findByRole("button", { name: "Создать окно работ" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Нужна причина.");
  });

  it("says the read failed, and HIDES the whole bar without maintenance:read", async () => {
    renderBars({ locale: "ru", error: new Error("boom") });
    expect(await screen.findByText("Окна работ недоступны.")).toBeInTheDocument();

    cleanup();
    renderBars({ locale: "ru", permissions: ["annotations:read"] });
    // Permission HIDES; the annotation bar beside it still renders, so this is
    // the gate and not a failed mount.
    await waitFor(() => expect(screen.getByTestId("annotation-bar")).toBeInTheDocument());
    expect(screen.queryByTestId("maintenance-bar")).toBeNull();
  });
});

/* ── the two tables must agree where they overlap ───────────────────────── */

describe("the twins' shared vocabulary", () => {
  /* outsideWindowNote is written ONCE in lib/annotations.ts and called by both
     bars, each passing its own translator. The duplication is what
     lib/i18n/README.md asks for instead of a shared dictionary; this is the
     cheap half of that trade. */
  it("spells outsideWindowNote identically in both dictionaries", () => {
    expect(maintenanceDict.en["created.outsideWindow"]).toBe(annotationsDict.en["created.outsideWindow"]);
    expect(maintenanceDict.ru["created.outsideWindow"]).toBe(annotationsDict.ru["created.outsideWindow"]);
  });

  it("names the page's own commit button in that sentence", () => {
    // «Расследовать» — dict/investigate.ts's "form.submit". A note pointing at
    // a control that does not exist under that name is worse than no note.
    expect(annotationsDict.ru["created.outsideWindow"]).toContain(investigateDict.ru["form.submit"]);
  });

  it("uses the console's ONE confirm-delete verb", () => {
    const idiom = investigateDict.ru["incident.confirmDelete"];
    expect(annotationsDict.ru["row.confirmDelete"]).toBe(idiom);
    expect(maintenanceDict.ru["row.confirmDelete"]).toBe(idiom);
    expect(annotationsDict.ru["row.delete"]).toBe(maintenanceDict.ru["row.delete"]);
    expect(annotationsDict.ru["scope.global"]).toBe(maintenanceDict.ru["scope.global"]);
  });
});
