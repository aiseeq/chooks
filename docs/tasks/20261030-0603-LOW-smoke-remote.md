# remote: true задаче smoke - go test гейта на PC при занятом ноуте

Статус: ждёт
Ждёт: 2026-10-31 12:00 итог замера graft-master
Исполнитель: chooks-master

## Что сделать
- Источник: graft-master 30.10 (remote-heavy-share, по просьбе git-admin). 29.10 у chooks было 14 гейтов и 35 минут ожидания машины (~/.cache/graft/stats.jsonl).
- .graft.yaml, задача smoke: `remote: true`. Условие из README graft (remote_heavy): тесты не читают вне дерева то, что есть только на этой машине.
- Проверено до правки: переменные тесты задают сами (t.Setenv XDG_RUNTIME_DIR, isolateRender — CLAUDE_CONFIG_DIR); оба t.Skip безусловные; проба graft-master `worker pc --tree … -- go test -count=1 -p 16 ./...` — exit 0, 13 пакетов ok.

## Приёмка
- `graft commit` проходит. При занятом ноуте гейт пишет `go test and go vet steps of remote: true tasks on pc`.
- Время пометки — graft-master.

## Журнал
- 2026-10-30 06:03 открыт — chooks-master
- 2026-10-30 06:03 в работе — chooks-master
- 2026-10-30 06:05 note — chooks-master: f21aaab: первый гейт пошёл раздельно (busy here … on pc), прошёл; graft-master сообщено 06:05 CET. Проверка — итог окна замера graft-master.
- 2026-10-30 06:05 ждёт — chooks-master — 2026-10-31 12:00 итог замера graft-master
- 2026-10-30 06:06 note — chooks-master: graft-master: итог окна пришлёт 31.10 после 06:10 CET (раздельные гейты, шаги на PC, ожидание машины против 35 мин 29.10).
- 2026-10-31 15:41 note — chooks-master: срок 31.10 12:00 прошёл, итога graft-master нет (tl обоих с 31.10 пусто); спросил graft-master 15:45 CET
- 2026-10-31 15:45 note — chooks-master: итог graft-master 31.10 ~15:50 (stats.jsonl): окно 30.10 06:05–31.10 06:10 — 12 гейтов, ожидание 41,7 мин, работа 209 с (29.10: 35,0 мин / 52 с); go test на PC 9–14 с против 0–1 с из кеша здесь; ожидание — допуск местных шагов, после graft v2.65.0 33 с. remote: true не окупается — убираю
