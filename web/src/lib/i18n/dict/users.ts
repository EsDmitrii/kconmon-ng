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
  "roles.gated": "Custom roles are not listed: reading them needs rbac:manage. The built-in roles can still be assigned.",
  "roles.unavailable": "Custom roles could not be loaded, so only the built-in ones are offered ({reason}).",
  "roles.noAnswer": "no answer from the server",
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
  "row.confirmDisable": "Confirm disabling {user}: they are signed out on their next request and their API tokens are revoked",
  "row.enable.verb": "Enable",
  "row.enable": "Enable {user}",
  "row.statusFailed": "Could not update {user}.",
  "row.delete.verb": "Delete",
  "row.delete": "Delete {user}",
  "row.confirmDelete.verb": "Confirm delete",
  "row.confirmDelete": "Confirm deleting {user}: the account and its API tokens are gone for good, and the username is free again",
  "row.deleteFailed": "Could not delete {user}.",
  "row.reset.verb": "Reset password",
  "row.reset": "Reset the password of {user}",
  "row.changeOwn.verb": "Change password",
  "row.changeOwn": "Change your own password with the current one; a reset would sign you out of this session",

  "self.roleWarning":
    "This is your own account. A role without users:manage takes this section away from you, and you cannot give the permission back yourself.",
  "self.confirmRole.verb": "Change my role",
  "self.confirmRole": "Confirm changing your own role to {role}",

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
  "roles.gated": "Пользовательские роли не показаны: для их чтения нужно право rbac:manage. Встроенные роли назначать можно.",
  "roles.unavailable": "Не удалось загрузить пользовательские роли, поэтому доступны только встроенные ({reason}).",
  "roles.noAnswer": "сервер не ответил",
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
  "row.confirmDisable": "Подтвердите отключение {user}: сеанс завершится при следующем запросе, а токены API этого пользователя будут отозваны",
  "row.enable.verb": "Включить",
  "row.enable": "Включить {user}",
  "row.statusFailed": "Не удалось изменить пользователя {user}.",
  "row.delete.verb": "Удалить",
  "row.delete": "Удалить {user}",
  "row.confirmDelete.verb": "Подтвердить удаление",
  "row.confirmDelete": "Подтвердите удаление {user}: учётная запись и её токены API исчезнут насовсем, а имя пользователя освободится",
  "row.deleteFailed": "Не удалось удалить пользователя {user}.",
  "row.reset.verb": "Сбросить пароль",
  "row.reset": "Сбросить пароль пользователя {user}",
  "row.changeOwn.verb": "Сменить пароль",
  "row.changeOwn": "Сменить свой пароль, указав текущий; сброс завершил бы и этот сеанс",

  "self.roleWarning":
    "Это ваша учётная запись. Роль без права users:manage уберёт у вас этот раздел, и вернуть себе это право вы уже не сможете.",
  "self.confirmRole.verb": "Сменить свою роль",
  "self.confirmRole": "Подтвердите смену своей роли на {role}",

  "reset.label": "Новый пароль для {user}",
  "reset.submit": "Задать пароль",
  "reset.failed": "Не удалось задать пароль пользователя {user}.",
  "reset.done": "Пароль задан. Все сеансы пользователя {user} завершены.",
});
