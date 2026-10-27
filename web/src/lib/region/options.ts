// The choices the language, region-format and keyboard pickers offer (the mock's lists,
// design_handoff_v3/screens/region-input-v3.html). The contract takes any `xx-XX` language and any
// xkeyboard-config layout name (control-api.md §Region and Input settings); these are only what the
// UI shows, and a saved value outside them still reads back and shows as itself.

/** [language tag, name in that language]. */
export const LANGUAGES: readonly (readonly [string, string])[] = [
  ["en-GB", "English (UK)"], ["en-US", "English (US)"], ["de-DE", "Deutsch"], ["fr-FR", "Français"],
  ["es-ES", "Español"], ["it-IT", "Italiano"], ["nl-NL", "Nederlands"], ["pl-PL", "Polski"],
  ["pt-BR", "Português (Brasil)"], ["sv-SE", "Svenska"], ["ru-RU", "Русский"], ["ja-JP", "日本語"],
  ["ko-KR", "한국어"], ["zh-CN", "简体中文"],
];

/** [locale tag, name], for dates, numbers and time. */
export const REGION_FORMATS: readonly (readonly [string, string])[] = [
  ["en-GB", "English (United Kingdom)"], ["en-US", "English (United States)"], ["en-AU", "English (Australia)"],
  ["de-DE", "Deutsch (Deutschland)"], ["fr-FR", "Français (France)"], ["es-ES", "Español (España)"],
  ["nl-NL", "Nederlands (Nederland)"], ["sv-SE", "Svenska (Sverige)"], ["ja-JP", "日本語 (日本)"],
];

export interface KeyboardLayoutOption {
  /** xkeyboard-config layout name. */
  layout: string;
  label: string;
  /** xkeyboard-config variant ("" = the basic one) and its name. */
  variants: readonly (readonly [string, string])[];
}

export const KEYBOARDS: readonly KeyboardLayoutOption[] = [
  { layout: "gb", label: "English (UK)", variants: [["", "Default"], ["extd", "Extended (Windows keys)"], ["dvorak", "Dvorak"], ["colemak", "Colemak"]] },
  { layout: "us", label: "English (US)", variants: [["", "Default"], ["intl", "International (dead keys)"], ["dvorak", "Dvorak"], ["colemak", "Colemak"]] },
  { layout: "de", label: "German", variants: [["", "Default"], ["nodeadkeys", "No dead keys"], ["dvorak", "Dvorak"], ["neo", "Neo 2"]] },
  { layout: "fr", label: "French", variants: [["", "Default (AZERTY)"], ["bepo", "BÉPO"], ["afnor", "AFNOR"]] },
  { layout: "es", label: "Spanish", variants: [["", "Default"]] },
  { layout: "latam", label: "Spanish (Latin American)", variants: [["", "Default"]] },
  { layout: "it", label: "Italian", variants: [["", "Default"]] },
  { layout: "se", label: "Swedish", variants: [["", "Default"], ["dvorak", "Dvorak"]] },
  { layout: "jp", label: "Japanese", variants: [["", "Default (106 keys)"]] },
];

/** A date, a number and a clock time as `locale` shows them in `tz`; "" when this browser cannot. */
export function formatExample(locale: string, tz: string, at: Date): string {
  try {
    return [
      new Intl.DateTimeFormat(locale, { timeZone: tz, day: "2-digit", month: "2-digit", year: "numeric" }).format(at),
      new Intl.NumberFormat(locale, { minimumFractionDigits: 2 }).format(1234.56),
      new Intl.DateTimeFormat(locale, { timeZone: tz, hour: "2-digit", minute: "2-digit" }).format(at),
    ].join(" · ");
  } catch {
    return "";
  }
}
