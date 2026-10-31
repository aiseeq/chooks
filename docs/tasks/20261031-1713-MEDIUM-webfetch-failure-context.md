# Сетевой сбой WebFetch не вызывает webtext

Статус: открыт

## Что сделать
- Находка F8 критика spec-as-built (тикет 20261031-1534-MEDIUM-spec-as-built; ТЗ /home/aisee/.claude/tools/chooks/spec.md, срез 388c8de; находки — критик Terra worker 1031-1633-spec-critic, 31.10 16:40 CET). Принята chooks-master 31.10.
- Новое поведение хука (не багфикс) - до кода разрешение пользователя на хук webfetch-fallback и противник замысла (dev-process, design-adversary). Сверено мастером по докам Claude Code (code.claude.com/docs/en/hooks, 31.10): PostToolUseFailure принимает hookSpecificOutput.additionalContext рядом с результатом инструмента, updatedToolOutput - только у PostToolUse; README.md:63 «там текст не заменить» верно для замены, но текст webtext можно передать контекстом. Сначала замер: сколько WebFetch с is_error за неделю в транскриптах и какие из них webtext открыл бы.
- **Суть.** Пользователь запросил автоматически вызывать утилиту при неудавшемся fetch, но сетевые и TLS-ошибки оставляют этот путь без автоматической попытки webtext.
- **Доказательство.** Пользователь 25.10: «просто не удалось fetch - автоматом вызываем утилиту» (tl 0bbcc980:11291). Хук зарегистрирован только на PostToolUse в configs/settings-snippet.json:19; README.md:63 указывает, что сетевые ошибки идут в PostToolUseFailure, где ответ не заменяется. ТЗ признаёт границу: /home/aisee/.claude/tools/chooks/spec.md:77.

## Приёмка
- Сначала красный тест, воспроизводящий находку: Сопоставить регистрацию с README и фактическим типом события для отказа DNS или TLS в сохранённом транскрипте. Живую сетевую пробу выполнять не требуется.
- После правки тест зелёный, `graft smoke` зелёный, glint 0.

## Журнал
- 2026-10-31 17:13 открыт — chooks-master
