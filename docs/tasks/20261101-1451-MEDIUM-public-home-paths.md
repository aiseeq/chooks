# Домашние пути пользователя в публичном репо вне docs/tasks

Статус: в работе
Исполнитель: chooks-master

## Что сделать
- Факт git-admin 01.11 ~14:5x CET: в публичном репо есть `/home/aisee` вне docs/tasks: тесты cmd/chooks/corpussweep_test.go, internal/tools/{corpus_sweep,write_guard}_test.go, internal/unsentcheck/check_test.go, фикстуры scripts/perf/testdata/*.json.
- Своя находка там же: слаг `-home-aisee…` (write_guard_test, corpus_sweep_test, README.md:37, комментарии saidbench.go, said.go), `pgrep -u aisee` (pgrep_ancestors_test).
- Тесты: строки — данные, код берёт home аргументом или из заглушки env → заменить на нейтральные `/home/u`, `-home-u`, `-u u`.
- Фикстуры perf: cwd/current_dir/project_dir → метка `@REPO@`, exec-ms.sh подставляет корень репо (как `@TRANSCRIPT@`), чтобы statusline мерился по живому git-дереву, как сейчас.
- README и комментарии — без имени пользователя.
- Тикеты docs/tasks не трогать: переезжают в закрытый репо (решение лида, вариант A, команда от git-admin позже).

## Приёмка
- `git grep -n aisee -- ':!docs/tasks' | grep -v github.com/aiseeq` — пусто.
- `graft smoke` зелёный.
- `scripts/perf/exec-ms.sh --runs 1 statusline scripts/perf/testdata/statusline.json` печатает число; вход после подстановки содержит путь репо.

## Журнал
- 2026-11-01 14:51 открыт — chooks-master
- 2026-11-01 14:52 в работе — chooks-master
- 2026-11-01 14:52 note — chooks-master: Сделано: /home/aisee→/home/u, -home-aisee→-home-u, pgrep -u u; README и комментарии без имени; perf-фикстуры cwd/current_dir/project_dir → @REPO@, exec-ms.sh подставляет корень репо (проверено awk: cwd=/home/aisee/git/chooks при прогоне здесь). git grep aisee вне docs/tasks (кроме github.com/aiseeq) — пусто; graft smoke зелёный; exec-ms.sh --runs 1 statusline → 11.5 мс.
