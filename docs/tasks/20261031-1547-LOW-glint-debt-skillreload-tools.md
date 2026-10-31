# glint: 3 находки в skillreload и tools (до 31.10)

Статус: открыт

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
