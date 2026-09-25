import { defineDict, type Dictionary } from "@/lib/i18n";

/**
 * users: the Settings section that manages local accounts (auth.mode=local) and the self-service
 * password change in the user menu. Row actions follow dict/settings.ts's tokens rows: the VERB is
 * what shows, the sentence with the user's name is the accessible name and the title.
 */

const en = {
  "title": "Users",
  "blurb":
    "Local accounts for this console. Passwords are at least 12 characters, and a reset or a change signs that user out of every other session. The last user who can manage users cannot be disabled or given a role without that permission.",
  "loading": "Loading users…",
  "unavailable": "Could not load users.",
  "listAria": "Local users",
  "new": "Add user",
  "you": "you",
  "cancel": "Cancel",

  "head.user": "User",
  "head.role": "Role",
  "head.status": "Status",
  "head.actions": "Actions",
  "status.active": "active",
  "status.disabled": "disabled",

  "form.create": "Add a user",
  "form.username": "Username",
  "form.usernameHelp": "Letters, digits and . _ @ - (up to 64). It is what the user signs in with.",
  "form.usernameInvalid": "The username takes letters, digits and . _ @ - only, up to 64 characters.",
  "form.displayName": "Display name",
  "form.displayNameHelp": "Shown in the user menu; the username when left empty.",
  "form.password": "Password",
  "form.passwordHelp": "At least 12 characters.",
  "form.tooShort": "The password needs at least 12 characters.",
  "form.role": "Role",
  "form.createButton": "Create user",
  "form.failed": "Could not create the user.",

  "row.role": "Role of {user}",
  "row.roleFailed": "Could not change the role of {user}.",
  "row.disable.verb": "Disable",
  "row.disable": "Disable {user}",
  "row.confirmDisable.verb": "Confirm disable",
  "row.confirmDisable": "Confirm disabling {user}: they are signed out on their next request",
  "row.enable.verb": "Enable",
  "row.enable": "Enable {user}",
  "row.statusFailed": "Could not update {user}.",
  "row.reset.verb": "Reset password",
  "row.reset": "Reset the password of {user}",

  "reset.label": "New password for {user}",
  "reset.submit": "Set password",
  "reset.failed": "Could not set the password of {user}.",
  "reset.done": "Password set. {user} is signed out of every session.",
} as const;

export type UsersKey = keyof typeof en;

export const usersDict: Dictionary<UsersKey> = defineDict(en, {
  "title": "Пользователи",
  "blurb":
    "Локальные учётные записи консоли. Пароль не короче 12 символов; после сброса или смены пароля все остальные сеансы пользователя завершаются. Последнего, кто может управлять пользователями, нельзя отключить или перевести на роль без этого права.",
  "loading": "Загрузка пользователей…",
  "unavailable": "Не удалось загрузить пользователей.",
  "listAria": "Локальные пользователи",
  "new": "Добавить пользователя",
  "you": "вы",
  "cancel": "Отмена",

  "head.user": "Пользователь",
  "head.role": "Роль",
  "head.status": "Состояние",
  "head.actions": "Действия",
  "status.active": "активен",
  "status.disabled": "отключён",

  "form.create": "Новый пользователь",
  "form.username": "Имя входа",
  "form.usernameHelp": "Латиница, цифры и . _ @ - (до 64 символов). С ним пользователь входит в консоль.",
  "form.usernameInvalid": "Имя входа: только латиница, цифры и . _ @ -, не длиннее 64 символов.",
  "form.displayName": "Отображаемое имя",
  "form.displayNameHelp": "Показывается в меню пользователя; если пусто, берётся имя входа.",
  "form.password": "Пароль",
  "form.passwordHelp": "Не короче 12 символов.",
  "form.tooShort": "Пароль должен быть не короче 12 символов.",
  "form.role": "Роль",
  "form.createButton": "Создать пользователя",
  "form.failed": "Не удалось создать пользователя.",

  "row.role": "Роль пользователя {user}",
  "row.roleFailed": "Не удалось сменить роль пользователя {user}.",
  "row.disable.verb": "Отключить",
  "row.disable": "Отключить {user}",
  "row.confirmDisable.verb": "Подтвердить отключение",
  "row.confirmDisable": "Подтвердите отключение {user}: сеанс завершится при следующем запросе",
  "row.enable.verb": "Включить",
  "row.enable": "Включить {user}",
  "row.statusFailed": "Не удалось изменить пользователя {user}.",
  "row.reset.verb": "Сбросить пароль",
  "row.reset": "Сбросить пароль пользователя {user}",

  "reset.label": "Новый пароль для {user}",
  "reset.submit": "Задать пароль",
  "reset.failed": "Не удалось задать пароль пользователя {user}.",
  "reset.done": "Пароль задан. Все сеансы пользователя {user} завершены.",
});
