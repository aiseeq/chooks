# Сетевой сбой WebFetch не вызывает webtext

Статус: открыт

## Что сделать
- Находка F8 критика spec-as-built (тикет 20261031-1534-MEDIUM-spec-as-built; ТЗ /home/aisee/.claude/tools/chooks/spec.md, срез 388c8de; находки — критик Terra worker 1031-1633-spec-critic, 31.10 16:40 CET). Принята chooks-master 31.10.
- Новое поведение хука (не багфикс) - до кода разрешение пользователя на хук webfetch-fallback и противник замысла (dev-process, design-adversary). Сверено мастером по докам Claude Code (code.claude.com/docs/en/hooks, 31.10): PostToolUseFailure принимает hookSpecificOutput.additionalContext рядом с результатом инструмента, updatedToolOutput - только у PostToolUse; README.md:63 «там текст не заменить» верно для замены, но текст webtext можно передать контекстом. Сначала замер: сколько WebFetch с is_error за неделю в транскриптах и какие из них webtext открыл бы.
- **Суть.** Пользователь запросил автоматически вызывать утилиту при неудавшемся fetch, но сетевые и TLS-ошибки оставляют этот путь без автоматической попытки webtext.
- **Доказательство.** Пользователь 25.10: «просто не удалось fetch - автоматом вызываем утилиту» (tl 0bbcc980:11291). Хук зарегистрирован только на PostToolUse в configs/settings-snippet.json:19; README.md:63 указывает, что сетевые ошибки идут в PostToolUseFailure, где ответ не заменяется. ТЗ признаёт границу: /home/aisee/.claude/tools/chooks/spec.md:77.

## Приёмка
- Сначала замер (до «да» пользователя на хук): сопоставить регистрацию с README и фактическим типом события для отказа DNS или TLS в сохранённом транскрипте. Живую сетевую пробу выполнять не требуется.
- После «да» и правки: тест на событие PostToolUseFailure WebFetch с is_error даёт additionalContext с текстом webtext; `graft smoke` зелёный, glint 0.

## Журнал
- 2026-10-31 17:13 открыт — chooks-master
- 2026-10-31 23:50 note — chooks-master: «да» пользователя на хук: user-word /home/aisee/.claude/projects/-home-aisee/8e927714-df71-45db-8bfd-a672b6f32c8d.jsonl 31.10 23:49:26 «Расширяем webfetch-fallback на сетевые сбои WebFetch? - да» (передал лид). Начат замер is_error WebFetch за 7 дней
- 2026-10-31 23:58 note — chooks-master: Замер 30 дней (скрипт и данные: /home/aisee/.cache/claude-tmp/claude-1000/-home-aisee-git-chooks/ba40d6e2-29aa-4e2e-a905-17d5be6c5c97/scratchpad/wf/w30.err, w30.probe): 1317 WebFetch, 24 is_error, 23 адреса; webtext открыл 14/23 - отказ домена Claude Code 8/8, таймаут 60 с 3/3, TLS 3/8, ECONNREFUSED 1/5; неудачи до 29 с. Доки (hooks.md, 31.10): вход PostToolUseFailure - tool_input, error, is_interrupt, duration_ms; выход - только additionalContext, кап 10 000 символов (больше - файл и превью 2 000).
- 2026-10-31 23:58 note — chooks-master: Противник (terra, worker ask, /home/aisee/.cache/claude-tmp/claude-1000/-home-aisee-git-chooks/ba40d6e2-29aa-4e2e-a905-17d5be6c5c97/scratchpad/wf/adversary.out): принято 4 (localhost/без точки/частные IP не запрашивать), 6 (весь additionalContext с ошибкой и заголовком ≤ 10 000 UTF-16), 7 (дата снимка Wayback в заголовке - тест), 8 (legal research.go:565 передаёт хук флагом --settings - сказать агенту legal). Отклонено 1,2 (тот же путь в PostToolUse, 24 сбоя за месяц), 3 (результат идёт агенту после хука), 5 (промпт не применён и путь к файлу уже в заголовке). План: тот же webfetch-fallback ветвится по hook_event_name; is_interrupt и пустой url - пропуск; регистрация в settings-snippet и ~/.claude/settings.json; README, hooks.yaml, spec.md:77.
- 2026-11-01 00:01 note — chooks-master: Код: ветка PostToolUseFailure в cmd/chooks/webfallback.go:79, FailureContext/checkRemote в internal/webfallback; README, hooks.yaml, settings-snippet, /home/aisee/.claude/tools/chooks/spec.md. go test зелёный, glint по своим файлам 0 (3 давние - тикет glint-debt-skillreload-tools). Коммит.
