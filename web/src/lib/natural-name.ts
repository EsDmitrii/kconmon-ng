/**
 * byNaturalName orders node names the way an operator reads them: m02 before m10, and m2 before m10
 * too. A plain codepoint sort puts m10 above m2, because "1" sorts before "2" long before either is
 * read as a number.
 */
const byNaturalName = new Intl.Collator(undefined, { numeric: true, sensitivity: "base" });

/** compareNaturalName is byNaturalName as a sort callback, ties broken by codepoint so two names the
 *  collator calls equal (case, accents) still land in one stable order. */
export function compareNaturalName(a: string, b: string): number {
  const byName = byNaturalName.compare(a, b);
  return byName !== 0 ? byName : a < b ? -1 : a > b ? 1 : 0;
}
