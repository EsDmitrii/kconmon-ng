import { useId, useState, type FormEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ErrorLine, queryErrorMessage, ROW_ACTION, RowActionLabel, SectionCard } from "@/components/settings-section";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Table, TBody, Td, Th, THead, Tr } from "@/components/ui/table";
import { useAuth } from "@/hooks/use-auth";
import { useConfirmStep } from "@/hooks/use-confirm-step";
import { useDisclosureFocus } from "@/hooks/use-disclosure-focus";
import { useSubmitGuard } from "@/hooks/use-submit-guard";
import { createUser, listRoles, listUsers, resetUserPassword, updateUser } from "@/lib/api";
import { useT } from "@/lib/i18n";
import { usersDict } from "@/lib/i18n/dict/users";
import { passwordTooShort, USERNAME_PATTERN } from "@/lib/password-policy";
import { useWriteGuard } from "@/lib/timemachine";
import type { ConsoleUser } from "@/lib/types";

/** USERS_ANCHOR is the section's deep-link target, the idiom the tokens section uses. */
export const USERS_ANCHOR = "users";

/** The built-in roles are compiled into the server; GET /api/v1/rbac/roles lists only custom ones. */
const BUILTIN_ROLES = ["viewer", "operator", "alert-editor", "admin"];

const USERS_KEY = ["users"] as const;

function useRoleNames(): string[] {
  const custom = useQuery({ queryKey: ["rbac-roles"], queryFn: listRoles, staleTime: 60_000 });
  const names = (custom.data ?? []).map((r) => r.name).filter((n) => !BUILTIN_ROLES.includes(n));
  return [...BUILTIN_ROLES, ...names];
}

function CreateUserForm({ roles, onDone }: { roles: string[]; onDone: () => void }) {
  const t = useT(usersDict);
  const qc = useQueryClient();
  const guard = useWriteGuard();
  const usernameId = useId();
  const displayNameId = useId();
  const passwordId = useId();
  const roleId = useId();
  const errorId = useId();
  const [username, setUsername] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [password, setPassword] = useState("");
  const [role, setRole] = useState("viewer");
  const [error, setError] = useState<string>();
  const { submitting, begin, end } = useSubmitGuard();

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    setError(undefined);
    if (!USERNAME_PATTERN.test(username)) {
      setError(t("form.usernameInvalid"));
      return;
    }
    if (passwordTooShort(password)) {
      setError(t("form.tooShort"));
      return;
    }
    if (!begin()) return;
    try {
      await createUser({ username, password, role, ...(displayName.trim() ? { displayName: displayName.trim() } : {}) });
      await qc.invalidateQueries({ queryKey: USERS_KEY });
      onDone();
    } catch (err) {
      setError(queryErrorMessage(err, t("form.failed")));
      end();
    }
  }

  return (
    <Card asChild className="p-4 sm:p-6">
      <form onSubmit={handleSubmit} className="flex max-w-2xl flex-col gap-4">
        <h3 className="type-section">{t("form.create")}</h3>
        <div className="grid grid-cols-[minmax(0,1fr)] gap-4 sm:grid-cols-2">
          <div className="flex min-w-0 flex-col gap-1 text-[13px]">
            <label htmlFor={usernameId} className="text-muted-foreground">
              {t("form.username")}
            </label>
            <Input
              id={usernameId}
              value={username}
              maxLength={64}
              autoComplete="off"
              aria-describedby={`${usernameId}-help`}
              onChange={(e) => setUsername(e.target.value)}
              className="w-full"
            />
            <span id={`${usernameId}-help`} className="text-xs leading-relaxed text-muted-foreground">
              {t("form.usernameHelp")}
            </span>
          </div>
          <div className="flex min-w-0 flex-col gap-1 text-[13px]">
            <label htmlFor={displayNameId} className="text-muted-foreground">
              {t("form.displayName")}
            </label>
            <Input
              id={displayNameId}
              value={displayName}
              autoComplete="off"
              aria-describedby={`${displayNameId}-help`}
              onChange={(e) => setDisplayName(e.target.value)}
              className="w-full"
            />
            <span id={`${displayNameId}-help`} className="text-xs leading-relaxed text-muted-foreground">
              {t("form.displayNameHelp")}
            </span>
          </div>
          <div className="flex min-w-0 flex-col gap-1 text-[13px]">
            <label htmlFor={passwordId} className="text-muted-foreground">
              {t("form.password")}
            </label>
            <Input
              id={passwordId}
              type="password"
              value={password}
              autoComplete="new-password"
              aria-describedby={`${passwordId}-help`}
              onChange={(e) => setPassword(e.target.value)}
              className="w-full"
            />
            <span id={`${passwordId}-help`} className="text-xs leading-relaxed text-muted-foreground">
              {t("form.passwordHelp")}
            </span>
          </div>
          <div className="flex min-w-0 flex-col gap-1 text-[13px]">
            <label htmlFor={roleId} className="text-muted-foreground">
              {t("form.role")}
            </label>
            <Select id={roleId} value={role} onChange={(e) => setRole(e.target.value)} className="w-full">
              {roles.map((r) => (
                <option key={r} value={r}>
                  {r}
                </option>
              ))}
            </Select>
          </div>
        </div>

        {error ? <ErrorLine id={errorId}>{error}</ErrorLine> : null}

        <div className="flex flex-wrap gap-2">
          <Button type="submit" loading={submitting} {...guard}>
            {t("form.createButton")}
          </Button>
          {/* Cancel touches nothing, so it stays live while the Time Machine is engaged. */}
          <Button type="button" variant="outline" onClick={onDone}>
            {t("cancel")}
          </Button>
        </div>
      </form>
    </Card>
  );
}

function ResetPasswordForm({ user, onDone }: { user: ConsoleUser; onDone: (ok: boolean) => void }) {
  const t = useT(usersDict);
  const guard = useWriteGuard();
  const inputId = useId();
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string>();
  const { submitting, begin, end } = useSubmitGuard();

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    setError(undefined);
    if (passwordTooShort(password)) {
      setError(t("form.tooShort"));
      return;
    }
    if (!begin()) return;
    try {
      await resetUserPassword(user.id, password);
      onDone(true);
    } catch (err) {
      setError(queryErrorMessage(err, t("reset.failed", { user: user.username })));
      end();
    }
  }

  return (
    <form onSubmit={handleSubmit} className="flex flex-col gap-2 py-1">
      <label htmlFor={inputId} className="text-xs text-muted-foreground">
        {t("reset.label", { user: user.username })}
      </label>
      <div className="flex flex-wrap items-center gap-2">
        <Input
          id={inputId}
          type="password"
          value={password}
          autoComplete="new-password"
          autoFocus
          onChange={(e) => setPassword(e.target.value)}
          className="w-64 max-w-full"
        />
        <Button type="submit" size="sm" loading={submitting} {...guard}>
          {t("reset.submit")}
        </Button>
        <Button type="button" size="sm" variant="ghost" onClick={() => onDone(false)}>
          {t("cancel")}
        </Button>
      </div>
      {error ? (
        <span role="alert" className="text-xs leading-relaxed text-health-bad">
          {error}
        </span>
      ) : null}
    </form>
  );
}

function UserRow({ user, roles, isMe }: { user: ConsoleUser; roles: string[]; isMe: boolean }) {
  const t = useT(usersDict);
  const qc = useQueryClient();
  const guard = useWriteGuard();
  const { confirming, confirmRef, triggerRef, ask, reset } = useConfirmStep();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [notice, setNotice] = useState<string>();
  const [resetting, setResetting] = useState(false);

  async function patch(change: { disabled?: boolean; role?: string }, failed: string) {
    setBusy(true);
    setError(undefined);
    setNotice(undefined);
    try {
      await updateUser(user.id, change);
      await qc.invalidateQueries({ queryKey: USERS_KEY });
    } catch (err) {
      setError(queryErrorMessage(err, failed));
    }
    setBusy(false);
    reset();
  }

  /* A user bound to several roles shows the first; picking one replaces them all, which is what the
     API does. A role the list does not know (deleted since) stays visible rather than silently
     reading as the first option. */
  const current = user.roles[0] ?? "";
  const options = current === "" || roles.includes(current) ? roles : [current, ...roles];
  const name = user.username;

  return (
    <>
      <Tr data-testid="user-row">
        <Td className="font-medium">
          <span className="flex min-w-0 items-center gap-2">
            <span className="block max-w-[10rem] truncate sm:max-w-[14rem]" title={name}>
              {name}
            </span>
            {isMe ? <Badge variant="neutral">{t("you")}</Badge> : null}
          </span>
          {user.displayName && user.displayName !== name ? (
            <span className="block max-w-[14rem] truncate text-xs font-normal text-muted-foreground" title={user.displayName}>
              {user.displayName}
            </span>
          ) : null}
        </Td>
        <Td>
          <Select
            aria-label={t("row.role", { user: name })}
            value={current}
            disabled={busy || guard.disabled}
            title={guard.title}
            onChange={(e) => void patch({ role: e.target.value }, t("row.roleFailed", { user: name }))}
            className="h-8 w-40 max-w-full text-xs"
          >
            {options.map((r) => (
              <option key={r} value={r}>
                {r}
              </option>
            ))}
          </Select>
        </Td>
        <Td>
          <Badge dot variant={user.disabled ? "unknown" : "ok"}>
            {t(user.disabled ? "status.disabled" : "status.active")}
          </Badge>
        </Td>
        <Td className="whitespace-nowrap text-right">
          <span className="inline-flex items-center justify-end gap-1">
            <Button
              size="sm"
              variant="ghost"
              className={ROW_ACTION}
              {...guard}
              aria-label={t("row.reset", { user: name })}
              aria-expanded={resetting}
              onClick={() => {
                setNotice(undefined);
                setResetting((v) => !v);
              }}
            >
              <RowActionLabel text={t("row.reset.verb")} title={t("row.reset", { user: name })} />
            </Button>
            {user.disabled ? (
              <Button
                size="sm"
                variant="ghost"
                className={ROW_ACTION}
                loading={busy}
                {...guard}
                aria-label={t("row.enable", { user: name })}
                onClick={() => void patch({ disabled: false }, t("row.statusFailed", { user: name }))}
              >
                <RowActionLabel text={t("row.enable.verb")} title={t("row.enable", { user: name })} />
              </Button>
            ) : confirming ? (
              <>
                {/* The destructive treatment is reserved for this second click. */}
                <Button
                  ref={confirmRef}
                  size="sm"
                  variant="destructive"
                  className={ROW_ACTION}
                  loading={busy}
                  {...guard}
                  aria-label={t("row.confirmDisable", { user: name })}
                  onClick={() => void patch({ disabled: true }, t("row.statusFailed", { user: name }))}
                >
                  <RowActionLabel text={t("row.confirmDisable.verb")} title={t("row.confirmDisable", { user: name })} />
                </Button>
                <Button size="sm" variant="ghost" className={ROW_ACTION} onClick={reset}>
                  {t("cancel")}
                </Button>
              </>
            ) : (
              <Button
                ref={triggerRef}
                size="sm"
                variant="ghost"
                className={ROW_ACTION}
                {...guard}
                aria-label={t("row.disable", { user: name })}
                onClick={ask}
              >
                <RowActionLabel text={t("row.disable.verb")} title={t("row.disable", { user: name })} />
              </Button>
            )}
          </span>
        </Td>
      </Tr>
      {resetting ? (
        <Tr>
          <Td colSpan={4}>
            <ResetPasswordForm
              user={user}
              onDone={(ok) => {
                setResetting(false);
                if (ok) setNotice(t("reset.done", { user: name }));
              }}
            />
          </Td>
        </Tr>
      ) : null}
      {error || notice ? (
        <Tr>
          <Td colSpan={4}>
            {error ? (
              <span role="alert" className="text-xs leading-relaxed text-health-bad">
                {error}
              </span>
            ) : (
              <span role="status" className="text-xs leading-relaxed text-muted-foreground">
                {notice}
              </span>
            )}
          </Td>
        </Tr>
      ) : null}
    </>
  );
}

/** UsersSection manages local accounts (auth.mode=local) for users:manage holders. */
export function UsersSection() {
  const t = useT(usersDict);
  const guard = useWriteGuard();
  const { me } = useAuth();
  const roles = useRoleNames();
  const [creating, setCreating] = useState(false);
  // The keyboard across the button↔form swap; see hooks/use-disclosure-focus.
  const createFocus = useDisclosureFocus(creating);
  const query = useQuery({ queryKey: USERS_KEY, queryFn: listUsers });
  const users = query.data ?? [];

  const createButton =
    creating || query.isPending ? null : (
      <Button
        ref={createFocus.triggerRef}
        size="sm"
        {...guard}
        onClick={() => {
          createFocus.onOpen();
          setCreating(true);
        }}
      >
        {t("new")}
      </Button>
    );

  return (
    <div className="flex flex-col gap-5">
      {creating ? (
        <div ref={createFocus.panelRef} tabIndex={-1}>
          <CreateUserForm
            roles={roles}
            onDone={() => {
              createFocus.onClose();
              setCreating(false);
            }}
          />
        </div>
      ) : null}

      <SectionCard id={USERS_ANCHOR} title={t("title")} blurb={t("blurb")} action={createButton}>
        {query.isError ? (
          <ErrorLine onRetry={() => void query.refetch()}>{queryErrorMessage(query.error, t("unavailable"))}</ErrorLine>
        ) : null}
        {query.isPending ? (
          <div role="status" aria-live="polite" className="mt-4 flex flex-col gap-2">
            <span className="sr-only">{t("loading")}</span>
            <Skeleton className="h-10 w-full" />
          </div>
        ) : null}
        {users.length > 0 ? (
          <Table
            variant="dense"
            aria-label={t("listAria")}
            containerClassName="mt-4"
            className="[&_td:not(:last-child)]:pr-3 [&_th:not(:last-child)]:pr-3"
          >
            <THead>
              <Tr>
                <Th>{t("head.user")}</Th>
                <Th>{t("head.role")}</Th>
                <Th>{t("head.status")}</Th>
                <Th className="text-right">
                  <span className="sr-only">{t("head.actions")}</span>
                </Th>
              </Tr>
            </THead>
            <TBody>
              {users.map((u) => (
                <UserRow key={u.id} user={u} roles={roles} isMe={me?.subject?.id === u.id} />
              ))}
            </TBody>
          </Table>
        ) : null}
      </SectionCard>
    </div>
  );
}
