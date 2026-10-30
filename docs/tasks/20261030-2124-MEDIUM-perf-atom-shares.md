# chooks.md: доли профиля statusline по обоим событиям perf (git-admin 30.10)

Статус: готово
Исполнитель: chooks-master

## Что сделать
- Задание git-admin 30.10 ~19:06 CET: доли профиля statusline в /home/aisee/.claude/improve/state/perf/chooks.md могли быть посчитаны только по cpu_atom (первый раздел perf report). Пересчитать по всем выборкам (`perf script`, кадр в стеке), править отчёт и вывод о кандидате ×2 при заметном сдвиге. Метод — /home/aisee/.claude/skills/go-performance/commands.md, «3. Профили».
- Профиль: scratchpad 5de41ca4 perf/perf.data. Счёт: scratchpad 1ae2fb1f share.awk.

## Приёмка
- `perf script -i …/perf.data -F comm,event,ip,sym` + share.awk: доли по comm=chooks, обоим событиям, совпадают с chooks.md «Кандидаты ×2».
- Коммит claude-config содержит только chooks.md.

## Журнал
- 2026-10-30 21:24 открыт — chooks-master
- 2026-10-30 21:24 note — chooks-master: claude-config 50a4072: доли по 11 784 выборкам (atom 5391 + core 6393): doInit1 20,5/52,4/37,8 % (atom/core/все), regexp 16,1/43,6/31,0, textstyle 7,3/20,3/14,4, tools 2,9/10,1/6,8, questioncheck 4,0/7,9/6,2, unsentcheck 0,8/3,5/2,3, _dl_start 7,5/4,5/5,8, statusline 1,4/8,0/5,0. Старые доли совпадали с колонкой atom. Кандидат ×2 в силе, ожидание 12 → 4,6–5,5 мс. Итог отправлен git-admin.
- 2026-10-30 21:24 на проверке — chooks-master
- 2026-10-30 21:24 готово — git-admin (session chooks-master): git-admin (другая сессия) 30.10: сам прогнал perf script по тому же perf.data + share.awk — 11 784 выборки (atom 5 391, core 6 393); doInit1 37,8 %, regexp.compile 31,0, textstyle 14,4, tools 6,8, questioncheck 6,2, unsentcheck 2,3, _dl_start 5,8, statusline 5,0; сходится с chooks.md на claude-config 50a4072
