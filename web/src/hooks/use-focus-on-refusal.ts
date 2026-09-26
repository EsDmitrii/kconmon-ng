import { useEffect, type RefObject } from "react";

/**
 * useFocusOnRefusal moves focus to the first invalid field once a submit is refused, or back to the
 * submit button when the refusal landed on the form as a whole. The button disables itself while
 * the request is in flight, and a focused control that becomes disabled hands focus to <body>,
 * where a keyboard user loses their place.
 */
export function useFocusOnRefusal(formRef: RefObject<HTMLFormElement | null>, errors: object | undefined): void {
  useEffect(() => {
    const form = formRef.current;
    if (!form || errors === undefined || Object.keys(errors).length === 0) return;
    const target =
      form.querySelector<HTMLElement>('[aria-invalid="true"]') ??
      form.querySelector<HTMLElement>('button[type="submit"]:not(:disabled)');
    target?.focus();
  }, [formRef, errors]);
}
