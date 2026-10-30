# remote: true задаче smoke - go test гейта на PC при занятом ноуте

Статус: в работе
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
