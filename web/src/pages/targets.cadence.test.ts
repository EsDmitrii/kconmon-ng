import { describe, expect, it } from "vitest";

import { targetsDict, type TargetsKey } from "@/lib/i18n/dict/targets";
import { translate, type Locale } from "@/lib/i18n";
import { fmtCadence, fmtIntervalNs, intervalParts, type CadencePhrases } from "@/pages/targets";

/**
 * Finding 10: the Russian schedules row read «каждые 1m» — an English unit
 * letter glued to a digit inside a Russian sentence, which is not a form
 * anyone writes. These pin the grammar, and pin that ENGLISH did not move.
 */

const SEC = 1_000_000_000;

function phrases(locale: Locale): CadencePhrases {
  const t = (key: TargetsKey) => translate(targetsDict, locale, key);
  return {
    interval: (interval) => translate(targetsDict, locale, "schedules.cadence.interval", { interval }),
    every: {
      second: t("schedules.cadence.every.second"),
      minute: t("schedules.cadence.every.minute"),
      hour: t("schedules.cadence.every.hour"),
    },
    unit: {
      second: t("schedules.cadence.unit.second"),
      minute: t("schedules.cadence.unit.minute"),
      hour: t("schedules.cadence.unit.hour"),
    },
  };
}

const en = (ns: number) => fmtCadence(ns, "en", phrases("en"));
const ru = (ns: number) => fmtCadence(ns, "ru", phrases("ru"));

describe("intervalParts", () => {
  it("walks the same s/m/h ladder fmtIntervalNs walks", () => {
    expect(intervalParts(30 * SEC)).toEqual({ value: 30, unit: "second" });
    expect(intervalParts(60 * SEC)).toEqual({ value: 1, unit: "minute" });
    expect(intervalParts(5 * 60 * SEC)).toEqual({ value: 5, unit: "minute" });
    expect(intervalParts(2 * 3600 * SEC)).toEqual({ value: 2, unit: "hour" });
  });

  it("reports nothing for a zero cadence, the case fmtIntervalNs renders as an em dash", () => {
    expect(intervalParts(0)).toBeNull();
    expect(fmtIntervalNs(0)).toBe("—");
  });
});

describe("fmtCadence — English is untouched", () => {
  it("still glues the unit letter onto the number", () => {
    expect(en(30 * SEC)).toBe("every 30s");
    expect(en(60 * SEC)).toBe("every 1m");
    expect(en(5 * 60 * SEC)).toBe("every 5m");
    expect(en(2 * 3600 * SEC)).toBe("every 2h");
  });
});

describe("fmtCadence — Russian reads as Russian", () => {
  it("never prints an English unit letter", () => {
    for (const ns of [10 * SEC, 60 * SEC, 5 * 60 * SEC, 90 * 60 * SEC, 2 * 3600 * SEC]) {
      expect(ru(ns)).not.toMatch(/\d\s*[smh]\b/);
    }
  });

  it("counts with a non-declining abbreviation", () => {
    expect(ru(10 * SEC)).toBe("каждые 10 с");
    expect(ru(5 * 60 * SEC)).toBe("каждые 5 мин");
    expect(ru(2 * 3600 * SEC)).toBe("каждые 2 ч");
  });

  it("takes the singular phrase for exactly one unit, where «каждые 1 мин» would be wrong", () => {
    expect(ru(SEC)).toBe("каждую секунду");
    expect(ru(60 * SEC)).toBe("каждую минуту");
    expect(ru(3600 * SEC)).toBe("каждый час");
  });

  it("falls back to the counted form for a cadence with no parts at all", () => {
    expect(ru(0)).toBe("каждые —");
  });
});
