# pgrep_ancestors: вложенные вызовы по Call.Offset akit v1.13.0, снять shellCommandScript и scriptSource

Статус: готово
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
- 2026-10-30 05:57 готово — verifier-subagent (session chooks-master): независимая проверка, суб-агент со свежим контекстом: свой разбор снят (grep пуст), вставка по Call.Offset (pgrep_ancestors.go:32,59), ssh по Via — мутация роняет 4 теста; go test tools и cmd/chooks ok; дифф тестов — только добавления; сверка 944 у e341e3b~1 и e341e3b — 0 расхождений, совпала с new-pgrep-v122; 47 проб: +5 исправлений (склейки, bash -c --, продолжение строки), -3 случая $'…' (Offset -1 в akit, в корпусе 0, akit-master сообщено); glint без находок. Копии: /home/aisee/.cache/claude-tmp/verify-pgrep-offset/
