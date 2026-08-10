import { defineDict, type Dictionary } from "@/lib/i18n";

/**
 * validation — the sentences lib/utils.ts's own field checks write.
 *
 * ONE key today, and a file rather than a corner of dict/targets.ts or
 * dict/diagnostics.ts because the message is lib/utils.ts's: BOTH of those
 * forms show it, from one constant, so that a definition and a one-shot run
 * refuse the same address in the same words. Split between two page tables it
 * would be two sentences waiting to diverge.
 *
 * NOT the server's refusal. `pages/targets.tsx`'s DEFINITION_FIELD_PHRASES
 * matches the SERVER's problem+json text to place it on a field — that table is
 * DATA and stays byte-for-byte English whatever the interface language is. This
 * is the client's own pre-flight sentence, written here, and it is the only one.
 */

const en = {
  /* Repeats the four accepted SHAPES rather than saying "invalid": an operator
     who typed a wrong thing needs to know what a right thing looks like. The
     shapes themselves — host, IP, host:port, an http(s) URL — are syntax and do
     not translate; the sentence around them does. */
  "adhoc.address":
    "destination address must be a host, an IP, host:port, or an http(s) URL — " +
    "the agent resolves and dials exactly this string",
} as const;

export type ValidationKey = keyof typeof en;

export const validationDict: Dictionary<ValidationKey> = defineDict(en, {
  "adhoc.address":
    "адрес назначения должен быть хостом, IP, host:port или http(s)-ссылкой, " +
    "а агент резолвит и подключается ровно по этой строке",
});
