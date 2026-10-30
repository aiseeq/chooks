# pgrep_ancestors: вложенные вызовы по Call.Offset akit v1.13.0, снять shellCommandScript и scriptSource

Статус: в работе
Исполнитель: chooks-master

## Что сделать
- Источник: akit-master 30.10 05:5x CET, akit v1.13.0. У вызовов из вложенного скрипта (bash -c '…' и "…", склейки, вложенность, ssh, heredoc-скрипт) Call.Offset — в координатах исходной команды; -1 только у $'…' и у argv0 из переменной или подстановки.
- internal/tools/pgrep_ancestors.go: места вставки -A — только по Call.Offset из одного shellcmd.Calls(command). Убрать nestedPgrepPositions, shellCommandScript, scriptSource, single/doubleQuotedSource, sourceValue.
- Сохранить прежнее правило: pgrep, ушедший по ssh (в Via есть ssh), не трогать — на удалённой машине procps может быть старым. Отказ при `<<` в команде остаётся.

## Приёмка
- Старые тесты pgrep (internal/tools/pgrep_ancestors_test.go, cmd/chooks/pgrep_test.go) зелёные без правки ожиданий.
- Сверка на корпусе pgrep (scratchpad shell-call/pgrep-cmds.jsonl, 944) против new-pgrep-v122.jsonl: расхождений нет, иначе каждое разобрано.
- glint чист. Независимая проверка суб-агентом. graft install (hook-bugfix-no-ask: поведение то же).

## Журнал
- 2026-10-30 05:51 открыт — chooks-master
- 2026-10-30 05:51 в работе — chooks-master
- 2026-10-30 05:53 note — chooks-master: сделано мастером без бойца: правка на 10 минут при полном контексте. akit v1.13.0; места вставки -A — только по Call.Offset одного shellcmd.Calls, nestedPgrepPositions, shellCommandScript и scriptSource убраны, ssh отсекается по Via. Тесты: старые без правки, добавлены ssh pc pgrep -f x и ssh pc -- bash -c; мутация без проверки Via роняет 4 случая с ssh. Сверка pgrep 944 против v1.12.2 — 0 расхождений. По ходу ошибочно откатил незакоммиченный файл через git checkout, восстановил из заготовки, тесты и сверка — после восстановления.
