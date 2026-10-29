# Stop-хук: фраза о неотправленном тексте без reddy history в ходе — не выпускать

Статус: в работе
Исполнитель: chooks-master

## Вход
Задание work-admin 29.10 ~12:10 CET, итог — work-admin (коммит, где включено, проверка на живом ходе). Решение пользователя: user-word ~/.claude/projects/-home-aisee-work/8afdef91-67e6-4cb4-bdfc-80b5edaeed74.jsonl 'Хорошо, включай' (29.10 12:06:03 CET) на описание work-admin 12:05:37. Сухой прогон work-admin: /home/aisee/.cache/claude-tmp/claude-1000/-home-aisee-work/8afdef91-67e6-4cb4-bdfc-80b5edaeed74/scratchpad/dryrun/{unsent.ts,hits.txt}: 52 ответа с фразой, 9 остановок, 2 ложные.

## Сделать
- Подкоманда `chooks unsent-check` (Stop), пакет `internal/unsentcheck`.
- Фраза — регэксп unsent.ts:7, кроме полной формы причастия («не отправленный текст» — описание, не статус; ложная 24.10).
- Ход — от последней реплики человека или письма сессии (как в скрипте); в нём искать Bash с `reddy … history`.
- Фраза есть, проверки нет — `decision: block` с подсказкой открыть чат адресата; `stop_hook_active` и субагент — пропуск (без петли).
- Строка в chooks.log на каждую найденную фразу (blocked / let through).
- Регистрация: configs/settings-snippet.json, README, ~/.claude/settings.json (Stop, рядом с question-check).

## Чем проверить
- `graft commit`: зелёный гейт; тесты на 7 верных и 2 ложных примера hits.txt и на границу хода.
- Живой ход: `claude -p` с фразой без reddy history — остановка и строка в chooks.log; с reddy history — пропуск.

## Журнал
- 2026-10-29 12:06 открыт — chooks-master
- 2026-10-29 12:07 в работе — chooks-master
- 2026-10-29 12:25 note — chooks-master: код и тесты; регэксп Go по 52 фразам hits.txt: из 9 STOP остаются 8 (7 верных + glint 27.10 «ответ не отправлен» про сессию), ложная 24.10 ушла; среди OK не ловятся 2 причастия («ещё не отправленных черновиков/текстов нет») и обрезанная в hits.txt «не отправл…». Хук stop об остановке не знает — звонок на конец хода остаётся (в README).
