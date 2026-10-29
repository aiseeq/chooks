# glint: гейт на high, fail_on: low снят

Статус: готово
Исполнитель: chooks-master

## Постановка
Задание git-admin 28.10 ~00:13 CET по решению пользователя `glint-threshold` (`~/.claude/decisions.md`, «"Советую 2" - да, верно», 27.10 23:56): гейт коммита — `fail_on` high (умолчание glint), остальные находки проверяет `tk done`. `fail_on: low` ставил b9d91c2 по заданию Сна, без слова пользователя.

## Что сделать
- `.glint.yaml`: убрать `fail_on: low`, `min_severity: low` оставить.
- Коммит называет откатываемый b9d91c2.

## Чем проверить
- `grep fail_on .glint.yaml` — пусто, `grep min_severity .glint.yaml` — `low`.
- `graft commit` проходит.

## Журнал
- 2026-10-28 00:14 открыт — git-admin
- 00:14 строка убрана в worktree `glint-threshold` (в main идёт незакоммиченная правка question-check)
- 2026-10-28 00:16 на проверке — chooks-master
- 2026-10-28 00:16 готово — git-admin: cf12329 в origin/main; grep fail_on .glint.yaml пусто; min_severity low на месте
