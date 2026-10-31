# Хуки: отказ окнам, ждущим ввода (AskUserQuestion, PermissionRequest, Elicitation)

Статус: готово
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
- 2026-10-29 10:05 note — chooks-master: Регистрация сделана лидом по «да» 28.10 16:44 (claude-config 43bd621, тикет лида no-prompt-hook-settings). Живые пробы: AskUserQuestion в сессии лида 580bcc76 (bypass) отклонён, строка prompt denied в chooks.log 10:02:21. PermissionRequest: `claude -p --permission-mode default` — curl отклонён, агент получил текст message дословно, строка в логе 10:03:40 (сессия 2d20f0bf); в auto классификатор пропустил curl без окна, хук не вызывался; в bypass окна нет. Elicitation — только образец stdin (лид 10:01:51), живого MCP с elicitation нет.
- 2026-10-29 10:05 готово — chooks-master
- 2026-10-31 02:40 note — chooks-master: Elicitation — живая проба 31.10 02:39 CET (задание Сна 31.10, d884df93af; тикет 20261031-0235-MEDIUM-rm-loop-name-review): MCP-сервер stdio, инструмент просит ввод через elicitation/create (scratchpad 1ae2fb1f elicit/server.ts), `claude -p --model opus --mcp-config` Claude Code 2.1.314. С хуком: сервер получил {"action":"decline"} через 47 мс, chooks.log 02:39:31 «prompt denied» session=480e699c hook=Elicitation mcp_server=elicitprobe mode=form. Контроль с --settings disableAllHooks: {"action":"cancel"}, строки в логе нет (сессия 0634c12b). Интерактивное окно не пробовалось: в -p окна нет, decline хука приходит раньше окна.
