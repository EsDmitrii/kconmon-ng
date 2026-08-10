import { defineDict, type Dictionary } from "@/lib/i18n";

/**
 * realtime — components/realtime-badge.tsx, the four strings that state the
 * TRANSPORT: the two labels and the two tooltips behind them.
 *
 * Its own file because the badge is not chrome and not a page: /live, /matrix
 * and a run's detail each mount it in their own header, and the sentence about
 * why data may be 15 seconds old must read the same on all three.
 *
 * «Онлайн», the word dict/chrome.ts's nav item uses for the pushed feed — NOT
 * «реальное время», which is the Time Machine's present, the instant you left
 * and return to. Two English "Live"s, two Russian words, and this badge is the
 * feed's one: it says the stream is connected, never that you are un-engaged.
 *
 * "Delayed data" is NOT an error, and the Russian must not make it one: polling
 * is a supported deployment (controller.events.enabled=false), so the badge
 * explains and does not alarm — warn, never bad.
 *
 * NOT HERE: WebSocket, REST, the 15s figure's unit. Protocol names and a
 * number.
 */

const en = {
  "live": "Live",
  "live.title": "Realtime is up: this view is fed by pushed WebSocket updates.",
  "delayed": "Delayed data",
  "delayed.title":
    "Realtime is unavailable — this console replica is not receiving the controller event stream. " +
    "Falling back to 15s REST polling, so data can be up to 15s old.",
} as const;

export type RealtimeKey = keyof typeof en;

export const realtimeDict: Dictionary<RealtimeKey> = defineDict(en, {
  "live": "Онлайн",
  "live.title": "Онлайн-поток работает: экран получает обновления, которые контроллер шлёт по WebSocket.",
  "delayed": "Данные с задержкой",
  "delayed.title":
    "Онлайн-потока нет: эта реплика консоли не получает поток событий контроллера. " +
    "Работает запасной REST-опрос раз в 15 с, так что данные могут отставать до 15 с.",
});
