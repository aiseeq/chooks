# corpus-sweep-check - сухой прогон проверки поиска по корпусу транскриптов

Статус: открыт

## Вход
- Проблема: Bash-поиски rg/grep/find/xargs по всему корпусу транскриптов (`~/.claude/projects`, `~/.codex`, `~/.local/state/worker`) ночью 30.10 вызвали фризы: суб-агенты Сна 02:27–02:28, бойцы 01:56 и 02:15 (по dream-master). Правило в промпте («`tl`, не grep по jsonl») их не удержало.
- Метод: проверка вызова по разбору команды, сначала сухой прогон на выборке за 7 дней. В хуки не ставить: хук и правка settings - только с «да» пользователя, предлагает лид.
- Дешевле: правило в промпте уже есть и не держит.
- Инструмент: chooks, подкоманда `corpus-sweep-check` (без установки в хуки).
- Цена: ~1 ч (боец Luna + ревью), прогон по файлу - секунды.
- Если не делать: фризы машины от поисков по корпусу повторяются.
- Источник: задание git-admin 30.10 ~02:45 CET (письмо, задание Сна «аудит процесса»). Вход для прогона готовит dream-master к 08:30 CET (тикет /home/aisee/git/dream/docs/tasks/20261030-0234-MEDIUM-corpus-search-dryrun.md), самому не собирать.
- Итог: git-admin и dream-master - сколько вызовов остановила бы, сколько из них законных (точечный `rg` по одному файлу, `tl`), какие поиски пропустила.

## Что сделать
1. `internal/tools/corpus_sweep.go`: `CheckCorpusSweep(command, home string) CorpusSweepVerdict` с полями `Blocked bool`, `Sweeps []string`, `Points []string` (пути как в слове команды после снятия кавычек, в порядке появления).
   - Разбор - `parseShell` + `withInlineScripts` (sh/bash -c). Тела here-doc не смотреть.
   - Поиск в команде: имя утилиты (`commandWords`, `commandName`) из {rg, grep, egrep, fgrep, zgrep, ugrep, ag, find, fd, xargs} или слово `xargs` среди слов до имени утилиты (`xargs -0 rg`: `skipWrappers` съедает `rg` как значение `-0`).
   - Слово корпуса: снятое `unquoteWord` слово любой команды (и списка `for … in`, и присваивания), которое начинается с корня; у присваивания `X=…` и ключа `--opt=…` - часть после первого `=`. Корень: префикс `~/`, `$HOME/`, `${HOME}/`, `<home>/` или без префикса (относительный, слово начинается с него) + одно из `.claude/projects`, `.codex`, `.local/state/worker`, дальше конец слова или `/`. `~/.claude/skills`, `~/.codex-old` - не корпус.
   - Точечный (Points): нет `*?[{` и последний сегмент (без хвостового `/`, с хвостовым - каталог) длиннее корня и содержит `.` не первым символом (`5de4.jsonl`, `config.toml`, `$S.jsonl`). Иначе - Sweeps (корень, каталог, glob).
   - Blocked = в команде есть поиск и есть хоть один Sweeps. Points учитываются всегда, Sweeps - только при поиске (иначе пусто).
2. `cmd/chooks/corpussweep.go`: подкоманда `chooks corpus-sweep-check --input FILE [--field command]` - JSONL на вход, строка - объект, команда в поле `--field`. На stdout - JSONL: исходный объект + `"corpus_sweep": {"blocked":…,"sweeps":[…],"points":[…]}`. На stderr - сводка: всего, остановлено, точечных среди пропущенных, без корпуса. Нечитаемая строка или нет поля строкой - ошибка с номером строки, код 1. Хук не регистрировать.
3. Прогон на файле dream-master: счёт, ручной разбор остановленных (законные?) и пропущенных с корпусом, итог git-admin и dream-master.

## Приёмка
- `go test ./internal/tools -run CorpusSweep` - тесты `internal/tools/corpus_sweep_test.go` (мастер, до бойца) зелёные; файл тестов не менять.
- `graft gate` зелёный (сборка, весь `go test`, glint).
- `chooks corpus-sweep-check --input <образец>` на образце из 3 строк печатает 3 строки JSONL и сводку; строка без поля - код 1.
- Независимая проверка: суб-агент с этим тикетом, прогон по файлу dream-master и выборочная сверка вердиктов.

## Журнал
- 2026-10-30 03:00 открыт — chooks-master
- 2026-10-30 03:02 note — chooks-master: тесты приёмки `internal/tools/corpus_sweep_test.go` написаны мастером; код - боец Luna (класс фича-инструмента, маршрут luna).
- 2026-10-30 03:02 note — chooks-master: боец Luna 1030-0302-corpus-sweep-check запущен, worktree /home/aisee/.cache/worktrees/chooks/corpus-sweep-check, тесты приёмки там (sha256 af0a6b3c…)
