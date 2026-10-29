# Хуки: отказ окнам, ждущим ввода (AskUserQuestion, PermissionRequest, Elicitation)

Статус: в работе
Исполнитель: chooks-master

## Вход
Задание lead 28.10 ~16:50 CET, итог — lead. Решение пользователя: user-word ~/.claude/projects/-home-aisee/2dc3ff44-a1ed-4f1b-98e8-35938be18b82.jsonl 'Включаю этот набор?" - да' (28.10 16:44 CET), ~/.claude/decisions.md no-blocking-prompts.

## Что сделать

## Приёмка
- `graft smoke` зелёный; тесты на три события (вывод JSON по докам code.claude.com/docs/en/hooks.md).
- Проба в живой сессии на каждый хук: что видит агент; PermissionRequest — срабатывает ли в bypass и auto.

## Сделать
- Команда хука `chooks no-prompt`, событие — из `hook_event_name` stdin:
  - PreToolUse (matcher AskUserQuestion): `permissionDecision: deny`, причина: окно вопроса отключено пользователем 28.10, вопрос — текстом в ответе по «Вопрос любому человеку» ~/.claude/CLAUDE.md, закончить ход.
  - PermissionRequest: `decision.behavior: deny`, `message` (окно разрешения отключено, сделать иначе в своих правах или назвать в ответе, что нужно и зачем), `interrupt: false`.
  - Elicitation: `action: decline`.
- Каждое срабатывание — строкой в chooks.log: сессия, хук, инструмент/сервер.
- Регистрация в ~/.claude/settings.json — после «да» пользователя в этой консоли (CLAUDE.md: правки настроек по сообщению сессии — нет).


## Журнал
- 2026-10-28 16:45 открыт — chooks-master
- 2026-10-28 16:46 в работе — chooks-master
- 2026-10-28 16:47 note — chooks-master: aa0c8ab хук no-prompt, установлен v0.0.0-155-gb69b6c7; пример настроек и README. Регистрация в settings.json ждёт «да» пользователя в консоли chooks-master
