# glint: 3 находки в skillreload и tools (до 31.10)

Статус: готово
Исполнитель: chooks-master

## Что сделать
- `glint check .` 31.10 15:55 CET на e10b09b: 3 находки, не связанные с текущими тикетами.
  - medium internal/skillreload/dryrun.go:520 reimplemented-stdlib — `sortedKeys` = `slices.Sorted(maps.Keys(m))`.
  - low internal/skillreload/tracker.go:353 unused-param `now` у `tracker.check`.
  - low internal/tools/model_guard.go:242 unused-param `path` у `ModelGuardTool.reject` (вызовы :44, :57).
- Код хуков skillreload и model-guard: правка без смены поведения, `graft install` — по правилу CLAUDE.md проекта (разрешение на хук).

## Приёмка
- `glint check .` — 0 находок; `graft smoke` зелёный.

## Журнал
- 2026-10-31 15:47 открыт — chooks-master
- 2026-11-01 00:03 note — chooks-master: Исправлено: sortedKeys → slices.Sorted(maps.Keys) на месте вызовов, сняты неиспользуемые now (tracker.check, tracker.candidates) и path (ModelGuardTool.reject). glint check ./... 0 находок, graft smoke зелёный. Не установлено (правило проекта: install хуков skillreload/model-guard - по разрешению; поведение не меняется).
- 2026-11-01 00:08 note — chooks-master: Независимая проверка (суб-агент, 01.11 00:05): glint 0, smoke зелёный, git show 4a29706 - поведение не изменилось (порядок ключей тот же). Установлен вместе с d5b09e6 (chooks v0.0.0-202-gd5b09e6).
- 2026-11-01 00:08 готово — subagent-acceptance (session chooks-master): суб-агент 01.11 00:05 CET: glint check ./... No issues (159 файлов), graft smoke exit 0, git show 4a29706 - поведение не изменилось
