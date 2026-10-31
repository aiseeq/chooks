# ТЗ chooks «как сделано» + критика вторым агентом

Статус: на проверке
Исполнитель: chooks-master

## Что сделать
- Задание лида через git-admin 31.10 15:33 CET (письмо git-admin → chooks-master). Бриф: /home/aisee/docs/reverse-spec/brief.md. Образец — пилот aguard /home/aisee/git/aguard/docs/tasks/20261031-1448-MEDIUM-spec-as-built.md.
- Слово пользователя: `user-word /home/aisee/.claude/projects/-home-aisee/8e927714-df71-45db-8bfd-a672b6f32c8d.jsonl 'Вся информация агентам'` (31.10 15:30:59) — агентам всё, без отсечки по дате; `… 'грузить терру там где можно'` (15:32:51) — автор и критик Terra, Opus только если Terra не справилась.
- Автор: боец Terra (`worker run --model terra --class документация`) пишет docs/spec.md: цель, что делает и решает каждый хук и подкоманда, пороги и откуда они, кому и что пишет, отказы и восстановление, границы; ссылки файл:строка.
- Критик: свежий боец Terra (`--class ревью`), docs/spec.md без вывода автора, всё доступное; находки 5 видов с доказательством и проверкой.
- Разбор: принято → tk, отклонено → причина; проверка разбора свежим агентом.
- Репо публичное: в docs/spec.md нет имён и данных рабочих проектов (~/.config/graft/private-terms), цитаты пользователя — без них.

## Приёмка
- accept.sh автора (scratchpad мастера, spec/accept.sh): разделы, ≥100 разных ссылок файл:строка разрешаются, покрыты cmd/chooks и все internal/*, все подкоманды main.go, у пунктов источник, 0 private-terms, правки только docs/spec.md; код 0; на неполной заготовке код 1.
- ТЗ в /home/aisee/.claude/tools/chooks/spec.md (не в публичном репо, git-admin 31.10).
- Файл находок критика; у каждой — решение (тикет или причина).
- Проверка разбора свежим агентом — в журнал.
- Итог git-admin строкой: N, M, K, главные 1–2, время и цена автора и критика.

## Журнал
- 2026-10-31 15:34 открыт — chooks-master
- 2026-10-31 15:34 в работе — chooks-master
- 2026-10-31 15:38 note — chooks-master: автор: боец Terra 1031-1538-spec-as-built запущен 15:38 CET, бриф и accept.sh — scratchpad chooks-master 4c5ae22d…/scratchpad/spec/; accept.sh проверен на заготовке: код 1 (разделы, ссылки, 10 пакетов, 29 подкоманд, пункт без источника, private-term)
- 2026-10-31 15:40 note — chooks-master: смена намерения: git-admin 31.10 ~15:45 — репо публичное, ТЗ с цитатами, tl id и домашними путями не в репо, а в /home/aisee/.claude/tools/chooks/spec.md (как akit). Автор пишет docs/spec.md в worktree (мера та же), после приёмки — перенос туда, в main не коммитить; критику дать путь к нему
- 2026-10-31 15:42 note — chooks-master: очередь 15:42: автор ждёт машину — не застой: 14 бойцов раскатки ТЗ впускаются по одному (graft exec --heavy --settle 30s, замок load с 15:41:53), машина free (runnable 8/22)
- 2026-10-31 16:34 note — chooks-master: автор принят 16:27: мера 0, 319 ссылок, 6 сверены по коду; 33 мин (22 — очередь), 0.106% недели OpenAI. ТЗ → /home/aisee/.claude/tools/chooks/spec.md (+строка среза 388c8de). accept.sh: база правок origin/main → HEAD (мой e10b09b сдвинул origin/main посреди работы автора). Критик Terra запущен, дерево spec-critic
- 2026-10-31 16:34 note — chooks-master: критик: worker 1031-1633-spec-critic, 16:34 CET
- 2026-10-31 17:14 note — chooks-master: критик принят мерой 17:11: 9 находок, 32 мин, 0.047% недели OpenAI. Разбор: scratchpad 4c5ae22d…/scratchpad/spec/triage.md — принято 6 (F1 ТЗ и F2 README исправлены сразу; F3 F4 F5 F8 → тикеты 20261031-1713-*), отклонено 3 (F6 F7 F9 с причиной). Дальше — проверка разбора свежим агентом
- 2026-10-31 17:16 note — verifier-subagent: проверка разбора: ПРИНЯТО — 9/9 с решением; тикеты 1713-* (F3 F4 F5 F8) несут Суть/Доказательство дословно, «Как проверить» в приёмке; F1 spec.md:17 = hooks.yaml:45 и config.go:101 (false), установленный config.yaml:12-13 true; F2 README 25e55b7 «от 99%» = WeekEmptyPercent 99.0 (choose.go:16,104); F6: 0 строк причины в chooks.log и .1, и все 3022 решения Stop с 27.10 несут оба поля background_tasks и session_crons, смешанный nil не встречался, ветка - звонок, не пропуск; F7 = механизм тикета 1233 (Truncate+WriteAt, пустой → Fresh молча), нового нет; F9: forecast.go:5-10 и 8abab4f1:18997/19021 — цвет = хватит ли недели до сброса; F3 (scanEvents/chatCall — только текст Bash) и F5 (cleanupStaleStates по mtime) подтверждены по коду; private-terms 0 во всех 4
- 2026-10-31 17:17 note — chooks-master: итог отправлен git-admin 17:18 CET: N=9, M=6, K=4 (+2 сразу), отклонено 3; F8: приёмка тикета — замер, не тест (замечание проверяющего)
- 2026-10-31 17:17 на проверке — chooks-master
