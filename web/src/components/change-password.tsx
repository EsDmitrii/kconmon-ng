import { createContext, useCallback, useContext, useEffect, useId, useState, type FormEvent, type ReactNode } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Modal } from "@/components/ui/modal";
import { useSubmitGuard } from "@/hooks/use-submit-guard";
import { changeOwnPassword, isWrongCurrentPassword, queryErrorMessage } from "@/lib/api";
import { useT } from "@/lib/i18n";
import { userMenuDict } from "@/lib/i18n/dict/user-menu";
import { passwordTooShort } from "@/lib/password-policy";

/**
 * ChangePasswordDialog is the signed-in user's own password change (auth.mode=local). The server
 * answers with a fresh session cookie for this browser and leaves every other session of the user
 * stale, so after a success there is nothing more to do here than say so.
 */
export function ChangePasswordDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const t = useT(userMenuDict);
  const formId = useId();
  const currentId = useId();
  const nextId = useId();
  const repeatId = useId();
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [repeat, setRepeat] = useState("");
  const [error, setError] = useState<string>();
  const [done, setDone] = useState(false);
  const { submitting, begin, end } = useSubmitGuard();

  /* A reopened dialog starts clean: the previous attempt's passwords must not sit in its fields. */
  useEffect(() => {
    if (open) return;
    setCurrent("");
    setNext("");
    setRepeat("");
    setError(undefined);
    setDone(false);
  }, [open]);

  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    setError(undefined);
    if (passwordTooShort(next)) {
      setError(t("password.tooShort"));
      return;
    }
    if (next !== repeat) {
      setError(t("password.mismatch"));
      return;
    }
    if (next === current) {
      setError(t("password.same"));
      return;
    }
    if (!begin()) return;
    try {
      await changeOwnPassword(current, next);
      setCurrent("");
      setNext("");
      setRepeat("");
      setDone(true);
    } catch (err) {
      setError(isWrongCurrentPassword(err) ? t("password.wrong") : queryErrorMessage(err, t("password.failed")));
    }
    end();
  }

  const footer = done ? (
    <div className="flex justify-end">
      <Button type="button" onClick={onClose}>
        {t("password.close")}
      </Button>
    </div>
  ) : (
    <div className="flex flex-wrap justify-end gap-2">
      <Button type="button" variant="outline" onClick={onClose} disabled={submitting}>
        {t("password.cancel")}
      </Button>
      <Button type="submit" form={formId} loading={submitting}>
        {t("password.submit")}
      </Button>
    </div>
  );

  return (
    /* Not dismissible while the change is in flight: the request cannot be taken back, and its
       answer belongs to this attempt. */
    <Modal
      open={open}
      onClose={onClose}
      dismissible={!submitting}
      title={t("password")}
      description={t("password.description")}
      footer={footer}
    >
      {done ? (
        <p role="status" className="text-sm leading-relaxed text-health-ok">
          {t("password.done")}
        </p>
      ) : (
        <form id={formId} onSubmit={handleSubmit} className="flex flex-col gap-4">
          <div className="flex flex-col gap-1 text-[13px]">
            <label htmlFor={currentId} className="text-muted-foreground">
              {t("password.current")}
            </label>
            <Input
              id={currentId}
              type="password"
              value={current}
              autoComplete="current-password"
              onChange={(e) => setCurrent(e.target.value)}
            />
          </div>
          <div className="flex flex-col gap-1 text-[13px]">
            <label htmlFor={nextId} className="text-muted-foreground">
              {t("password.next")}
            </label>
            <Input
              id={nextId}
              type="password"
              value={next}
              autoComplete="new-password"
              aria-describedby={`${nextId}-help`}
              onChange={(e) => setNext(e.target.value)}
            />
            <span id={`${nextId}-help`} className="text-xs leading-relaxed text-muted-foreground">
              {t("password.nextHelp")}
            </span>
          </div>
          <div className="flex flex-col gap-1 text-[13px]">
            <label htmlFor={repeatId} className="text-muted-foreground">
              {t("password.repeat")}
            </label>
            <Input
              id={repeatId}
              type="password"
              value={repeat}
              autoComplete="new-password"
              onChange={(e) => setRepeat(e.target.value)}
            />
          </div>
          {error ? (
            <p role="alert" className="text-sm leading-relaxed text-health-bad">
              {error}
            </p>
          ) : null}
        </form>
      )}
    </Modal>
  );
}

const PasswordDialogContext = createContext<(() => void) | null>(null);

/**
 * PasswordDialogHost mounts the one ChangePasswordDialog above both renderings of the sidebar, so
 * the dialog outlives the drawer (and the UserMenu in it), which unmounts when the viewport crosses
 * md.
 */
export function PasswordDialogHost({ children }: { children: ReactNode }) {
  const [open, setOpen] = useState(false);
  const show = useCallback(() => setOpen(true), []);
  const close = useCallback(() => setOpen(false), []);
  return (
    <PasswordDialogContext.Provider value={show}>
      {children}
      <ChangePasswordDialog open={open} onClose={close} />
    </PasswordDialogContext.Provider>
  );
}

/** useShowPasswordDialog is the host's opener, or null outside a PasswordDialogHost. */
export function useShowPasswordDialog(): (() => void) | null {
  return useContext(PasswordDialogContext);
}
