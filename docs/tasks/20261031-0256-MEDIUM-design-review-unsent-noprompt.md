# Обход design Сна 31.10: unsent-check без транскрипта, мёртвая ветка no-prompt AskUserQuestion

Статус: в работе
Исполнитель: chooks-master

## Что сделать
- Задание dream-master 31.10 ~02:56 CET (обход design, ревизор Opus), итог — git-admin, копия dream-master.
1. unsent-check: ночные `claude -p --no-session-persistence` (~/git/dream/run.sh) дают ERROR «unsent check failed … open transcript: no such file» (10 из 32 строк unsent в chooks.log, 30.10 03:08–03:48). Нет файла транскрипта — пропуск `Skip: "no transcript"` (Debug), не ERROR; прочие ошибки чтения — ERROR как было. Починка бага хука — без вопроса (decisions.md hook-bugfix-no-ask).
2. no-prompt: ветка PreToolUse AskUserQuestion мёртвая с 29.10 12:57 (decisions.md no-blocking-prompts: AskUserQuestion в permissions.deny, хук снят; settings.json PreToolUse без no-prompt — проверено 31.10). Убрать ветку из cmd/chooks/noprompt.go, тест, README.md:72-83, configs/settings-snippet.json, справку main.go:74.
3. Регэксп «ещё ждут отправки» — к сведению, без действия.

## Приёмка
- `graft smoke` зелёный; тест unsentcheck: нет файла → Skip "no transcript", без ошибки.
- `grep -n AskUserQuestion README.md configs/settings-snippet.json cmd/chooks/main.go cmd/chooks/noprompt.go` — пусто (кроме объяснения, что окно снято permissions.deny).
- После `graft install`: `echo '{"stop_hook_active":false,"last_assistant_message":"текст не ушёл","transcript_path":"/nonexistent"}' | chooks unsent-check` — без ERROR в chooks.log.

## Журнал
- 2026-10-31 02:56 открыт — chooks-master
- 2026-10-31 02:57 в работе — chooks-master
- 2026-10-31 02:57 note — chooks-master: Правка: unsentcheck.Run — os.ErrNotExist → Skip "no transcript" (Debug), прочие ошибки — ERROR (тест TestRun_UnreadableTranscript); no-prompt — ветка PreToolUse и askDeniedReason сняты, PreToolUse AskUserQuestion теперь ошибка регистрации (тест), README/snippet/справка без AskUserQuestion. go test unsentcheck, cmd/chooks зелёные. Дальше graft commit, graft install (починка бага хука и чистка мёртвой ветки, hook-bugfix-no-ask).
