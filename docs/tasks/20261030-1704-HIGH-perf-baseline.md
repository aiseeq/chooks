# Первый референс скорости chooks (perf-baseline-1030, задание git-admin)

Статус: в работе
Исполнитель: chooks-master

## Вход
- git-admin 30.10 17:03 CET, его тикет /home/aisee/git/docs/tasks/20261030-1137-HIGH-perf-baseline-git.md, лид /home/aisee/docs/tasks/20261030-1136-MEDIUM-perf-baseline-1030.md.
- Слово пользователя: `user-word ~/.claude/projects/-home-aisee/d753e3a8-1750-4371-bdef-a4263a5bf0f7.jsonl 'собирать статистику работы всех'` (30.10 11:34:56).
- Формат: /home/aisee/.claude/improve/state/perf/FORMAT.md (перечитать перед коммитом), образцы akit/graft/glint там же.

## Что сделать
- chooks.tsv и chooks.md в /home/aisee/.claude/improve/state/perf/: время каждого хука на вызов (PreToolUse, PostToolBatch, UserPromptSubmit, Stop ×3, Notification, statusline, no-prompt, webfetch-fallback) и CPU за сутки.
- Источники: stop_hook_summary.hookInfos[].durationMs в транскриптах (все Stop), attachment hook_success.durationMs (только хуки с выводом), локальный прогон бинаря с реальным stdin, число вызовов за сутки по транскриптам.
- Кандидаты ×2 — только назвать с замером.
- Коммит в claude-config: `git --git-dir=$HOME/.claude-config.git --work-tree=$HOME`, add явными путями.

## Приёмка
- Обе таблицы по FORMAT.md: шапка tsv, без пустых полей, у каждой строки с командой — одно число и код 0 (или 3) за < 60 с.
- Независимая проверка суб-агентом: команды tsv, числа референса против источника.

## Журнал
- 2026-10-30 17:04 открыт — chooks-master
- 2026-10-30 17:11 в работе — chooks-master
- 2026-10-30 17:11 в работе — chooks-master
- 2026-10-30 17:11 note — chooks-master: замер 17:05–17:15 CET: stop_hook_summary.hookInfos[].durationMs за 7 дней (суточные p95 stop 47–110 мс, question-check 98–4278, unsent-check 69–84); локально p50 на вызов: statusline 10–12 мс (CPU 12), pre-tool-use 7, post-tool-batch 9, user-prompt-submit 10, chooks version 5 (CPU 6, init пакетов 3.4–4.7 мс: регэкспы textstyle/tools/questioncheck/unsentcheck); bpftrace 120 с (17:07:40–17:09:41): 909 запусков statusline из 932 (7.6/с при 17 сессиях); судья latency_ms около 6 с, 12–70 вызовов в сутки. tl --raw длительности хуков не печатает — скрипт в репо: боец Luna, scripts/perf/hook-ms.sh и exec-ms.sh с тестом, worktree perf-scripts
- 2026-10-30 17:19 note — chooks-master: боец 1030-1712-perf-scripts (Luna, 5 мин) принят: scripts/perf/hook-ms.sh, exec-ms.sh, testdata, perf_test.go; ревью мастера: hook-ms глушил ошибки rg/jq и звал date на запись — ошибки rg видны (код 2), время в jq; exec-ms требовал TMPDIR — mktemp -t. Сверка: суточные hook-ms по окнам до 17:05:48 совпали с моим счётом (stop 89 58 102 47 48 110 105, webfetch-fallback p50 724 834 889); каждая команда ~0.14 с
