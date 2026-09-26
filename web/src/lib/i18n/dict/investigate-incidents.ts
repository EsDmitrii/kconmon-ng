import { defineDict, type Dictionary } from "@/lib/i18n";

/**
 * investigate-incidents — pages/investigate-incidents.tsx, the "Saved incidents" card on
 * /investigate: every saved incident, open and resolved, with its status filter and pager.
 * Incident titles and scopes are data and stay as written.
 */

const en = {
  "title": "Saved incidents",
  "at": "as of {at}",
  "filter.aria": "Incident status",
  "filter.all": "All",
  "filter.open": "Open",
  "filter.resolved": "Resolved",
  "status.open": "Open",
  "status.resolved": "Resolved",
  "scope.global": "global",
  "opened": "opened {at}",
  "resolvedAt": " · resolved {at}",
  "loading": "Loading incidents…",
  "empty.all": "No incident has been saved yet. Save an investigation as an incident and it is listed here.",
  "empty.open": "No open incidents.",
  "empty.resolved": "No resolved incidents.",
  "empty.at": "No incident saved by the instant on screen matches this filter.",
  "denied": "Saved incidents need incidents:read — none was requested.",
  "noDatabase": "Incidents are stored — set database.dsnFile (Helm: database.existingSecret). Nothing was requested.",
  "failed": "Saved incidents could not be loaded: {error}",
  "failed.generic": "the request failed",
  "olderFailed": "The older page could not be loaded.",
  "loadOlder": "Load older",
  "loadingOlder": "Loading…",
  "subject": "incidents",
} as const;

export type InvestigateIncidentsKey = keyof typeof en;

export const investigateIncidentsDict: Dictionary<InvestigateIncidentsKey> = defineDict(en, {
  "title": "Сохранённые инциденты",
  "at": "на {at}",
  "filter.aria": "Статус инцидента",
  "filter.all": "Все",
  "filter.open": "Открытые",
  "filter.resolved": "Закрытые",
  "status.open": "Открыт",
  "status.resolved": "Закрыт",
  "scope.global": "глобальный",
  "opened": "открыт {at}",
  "resolvedAt": " · закрыт {at}",
  "loading": "Загрузка инцидентов…",
  "empty.all": "Сохранённых инцидентов пока нет. Сохраните расследование как инцидент, и он появится здесь.",
  "empty.open": "Открытых инцидентов нет.",
  "empty.resolved": "Закрытых инцидентов нет.",
  "empty.at": "Среди инцидентов, сохранённых к показанному моменту, под этот фильтр ничего не подходит.",
  "denied": "Сохранённым инцидентам нужно право incidents:read, которого у роли нет, так что запрос не отправлялся.",
  "noDatabase": "Инциденты хранятся в базе, задайте database.dsnFile (Helm: database.existingSecret). Запрос не отправлялся.",
  "failed": "Не удалось загрузить сохранённые инциденты: {error}",
  "failed.generic": "запрос не выполнен",
  "olderFailed": "Более старую страницу загрузить не удалось.",
  "loadOlder": "Показать более старые",
  "loadingOlder": "Загрузка…",
  "subject": "Инциденты",
});
