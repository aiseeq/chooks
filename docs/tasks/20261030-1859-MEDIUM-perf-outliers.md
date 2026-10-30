# chooks.tsv: правило выбросов FORMAT.md (git-admin 30.10 18:58)

Статус: готово
Исполнитель: chooks-master

## Что сделать
- Задание git-admin 30.10 ~18:59 CET: в FORMAT.md, пункт `референс`, правило выбросов — худшие сутки ≥ 2× вторых худших выпадают, референс — вторые худшие; не больше одних суток; выкинутые — строкой в chooks.md (значение, дата, причина или «не установлена»).
- Пересчитать строки hook-ms в /home/aisee/.claude/improve/state/perf/chooks.tsv по суточным рядам из chooks.md («Как мерили: транскрипты»). exec-ms и `job:chooks statusline day` не трогать.
- Коммит в claude-config: `git --git-dir=$HOME/.claude-config.git --work-tree=$HOME`, add явными путями. Итог git-admin: сколько строк изменилось + коммит.

## Приёмка
- Для каждой строки hook-ms: max ряда / второй max < 2 → референс = max; ≥ 2 → референс = второй max и строка в chooks.md.
- `git --git-dir=$HOME/.claude-config.git log -1 --stat` содержит chooks.tsv и chooks.md.

## Журнал
- 2026-10-30 18:59 открыт — chooks-master
- 2026-10-30 19:00 note — chooks-master: claude-config eb5e669: изменилась 1 строка tsv — cli:chooks post-tool-batch 296 → 132 (296/132 = 2,24, сутки 23.10–24.10, причина не установлена), строка в chooks.md. Остальные hook-ms ×1,05–1,22, без изменений. Не запушено (main ahead 2). Итог отправлен git-admin на приёмку.
- 2026-10-30 19:00 на проверке — chooks-master
- 2026-10-30 19:00 готово — git-admin (session chooks-master): git-admin (другая сессия) 30.10 ~19:01 CET: по таблице «Сутки» в chooks.md post-tool-batch 296/132 = 2,24 выпадает, у остальных пяти 1,05–1,22; eb5e669 меняет только chooks.tsv (1 строка) и chooks.md (1 строка)
