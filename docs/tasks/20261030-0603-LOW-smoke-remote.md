# remote: true задаче smoke - go test гейта на PC при занятом ноуте

Статус: готово
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
- 2026-10-31 15:49 готово — verifier-subagent (session chooks-master): Независимая проверка суб-агентом со свежим контекстом 31.10 15:50 CET по ~/.cache/graft/stats.jsonl(.1), kind=task task=gate top=commit project=/home/aisee/git/chooks: окно 30.10 06:05-31.10 06:10 - 12 гейтов, wait_s 2501 с (queue 2129, load 331, io 41), работа wall-wait 209 с; 29.10 - 14 гейтов, wait 2099 с, работа 52 с. go test ./... на host=pc: 12 прогонов 9,6-14,4 с (один 32,4 с - первый), здесь 58 прогонов с 29.10 в среднем 0,84 с. Холодный кеш: llmark tool/cmd/worker/pc.go:176 rm -rf spot.Dir после разового запуска; GOCACHE=stage/cache - bind $HOME/cacheDir (pcBwrapArgs), cacheDir - этот spot (pc.go:149 worker-cache). e10b09b: в .graft.yaml снят только remote: true у smoke, прочие задачи не тронуты; origin/main = e10b09b. remote не окупается - подтверждено.; glint: 3 findings, debt glint-debt-skillreload-tools
