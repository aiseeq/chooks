# перевести unsentcheck, corpus_sweep, pgrep_ancestors на пакет akit «вызвана ли программа» (mvdan/sh)

Статус: в работе
Исполнитель: chooks-master

## Что сделать
Этап 1 (сейчас):
1. unsentcheck (`internal/unsentcheck/check.go`): `chatCall` - через `shellcmd.Find(command, "reddy")`: вызов есть, если у какого-то Call первый аргумент, не начинающийся с `-`, равен `history`. Регулярки `callStart`, `historyArgs`, `reddyHistory`, `reddyVar` убрать. Ошибка `shellcmd` - вызова нет; число таких команд в ходе - поле `ParseErrors int` в `Result`, хук пишет его в лог (`cmd/chooks/unsentcheck.go`, при `ParseErrors > 0` - Warn с сессией).
2. corpus_sweep (`internal/tools/corpus_sweep.go`): поиск в команде - по `shellcmd.Calls` (имя из `corpusSearchTools` у любого Call, включая вложенные через обёртки и `bash -c`); слова корпуса - как сейчас из `parseShell` (присваивания, списки `for … in`, `cd`) плюс `Args` всех Call (так видны пути внутри `graft exec -- bash -c '…'`). Ошибка `shellcmd` - поиск определяется по-старому (`parseShell`), в вердикт поле `ParseError string`.
3. Тесты: все нынешние зелёные без правки ожиданий; новые - 4 пропуска сухого прогона: `graft exec --heavy -- bash -c 'find /home/aisee/.claude/projects -name "*.jsonl" | xargs -0 grep -l x'`, `cd ~/.claude/projects; graft exec --heavy -- bash -c 'find . -name "*.jsonl" -print0 | xargs -0 rg x'`, `timeout 270 graft exec --heavy -- bash -c 'cd /home/aisee/.claude/projects; find . -name "*.jsonl" | xargs rg x'`, `/home/aisee/.claude/improve/moment-frame.sh exec /tmp/t bash -c 'grep -rl x /home/aisee/.claude/projects'` - все Blocked; unsentcheck - `bash -c 'reddy history chat'` и `graft exec -- reddy history chat` - вызов.
4. Сверка на корпусе 3100 команд (`/home/aisee/.claude/improve/runs/2026-10-30/corpus-search-dryrun.jsonl`): старый и новый `chatCall` и вердикт corpus_sweep построчно, все расхождения - разбор: где неправ пакет - akit-master с командой.
Этап 2: pgrep_ancestors - после `Call.Offset` в akit (просьба akit-master 30.10 04:2x CET).

## Приёмка
- `graft gate` зелёный; `go test ./internal/unsentcheck ./internal/tools ./cmd/chooks` зелёные, ожидания старых тестов не менялись (`git diff` по `*_test.go` - только добавления).
- Сверка п. 4: файл расхождений с разбором, ни одного расхождения «старое верно, новое неверно» без письма akit-master.
- Установка (`graft install`) - после сверки: поведение то же, отличия - только исправленные ошибки разбора (decisions.md hook-bugfix-no-ask).
- Независимая проверка: суб-агент по тикету.

## Журнал
- 2026-10-30 03:31 открыт — chooks-master
- 2026-10-30 03:31 note — chooks-master: обещано akit-master 30.10 03:3x CET; его тикет /home/aisee/git/akit/docs/tasks/20261030-0247-LOW-shell-call.md. rm_guard и write_guard остаются на parseShell (повторяют разбор Claude Code, 4007fba). Проверка: тесты check_test.go, pgrep_ancestors_test.go, corpus_sweep_test.go зелёные на пакете + сухой прогон corpus-search-dryrun.jsonl без расхождений, кроме заявленных
- 2026-10-30 03:31 ждёт — chooks-master — выход пакета shell-call в akit (письмо akit-master)
- 2026-10-30 04:20 в работе — chooks-master
- 2026-10-30 04:20 note — chooks-master: akit v1.9.0 shellcmd вышел (письмо akit-master ~04:2x CET). Этап 1 - unsentcheck и corpus_sweep (боец Luna); этап 2 - pgrep_ancestors, ждёт Call.Offset (попросил akit-master). Ошибка разбора shellcmd = вызова нет (shellcmd ошибается только там, где и bash -n: bash такую команду не выполнит), с записью в лог хука
