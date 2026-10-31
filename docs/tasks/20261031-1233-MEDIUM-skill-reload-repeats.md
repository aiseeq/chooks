# skill reload: доля ложных повторов уведомлений (задание dream-master, noticed.md 31.10 карточка 2)

Статус: готово
Исполнитель: chooks-master

## Что сделать
- Источник: /home/aisee/.claude/improve/runs/2026-10-31/noticed.md карточка 2; окно 30.10 02:19 – 31.10 03:12 CET, 269 строк «skill reload notified» в /home/aisee/.claude/logs/chooks.log.
- Для пар повторных уведомлений одной сессии/агента по одному файлу проверить, менялся ли файл между ними (git-история файлов инструкций, mtime, дифф в транскрипте).
- Оценить долю ложных повторов (файл не менялся) и повторов не по задаче.
- Хук менять только по доказанным случаям (баг хука - без вопроса, decisions hook-bugfix-no-ask).

## Приёмка
- Таблица пар с вердиктом в журнале тикета; итог одной строкой dream-master.

## Журнал
- 2026-10-31 12:33 открыт — chooks-master
- 2026-10-31 12:42 note — chooks-master: Разбор: /home/aisee/.cache/claude-tmp/claude-1000/-home-aisee-git-chooks/c5641e2e-b590-4b7c-b74e-111fb2876e07/scratchpad/extract.sh (события транскриптов) + /home/aisee/.cache/claude-tmp/claude-1000/-home-aisee-git-chooks/c5641e2e-b590-4b7c-b74e-111fb2876e07/scratchpad/pairs.pl (пары) -> /home/aisee/.cache/claude-tmp/claude-1000/-home-aisee-git-chooks/c5641e2e-b590-4b7c-b74e-111fb2876e07/scratchpad/pairs.tsv. Окно: 349 выдач по файлам (= журнал), 136 первых, 213 повторных пар: 98 новая правка (другое mtime); 100 после сжатия контекста - invoked_skills подкладывает исходный снимок вызова (хэш = снимку Skill, проба scanTranscript 91253a33: 8375289c на 11:27 и 11:59), у агента в контексте снова старый текст, повтор законный; 13 - правки в ту же минуту и продолжение overflow->разделы, законно; 2 ложных (graft 91253a33 worker-model 11:59->12:36, work 06e8c51e write-as-user 10:57->13:16 CET): между ними hook_cancelled timedOut chooks (7,1 и 4,6 с при таймауте 3 с), второй дифф с LoadedAt исходного вызова - хук перечитал транскрипт с нуля на пустом состоянии; воспроизведено Checker на префиксах транскрипта со свежим StateDir. Причина: save() = Truncate(0)+WriteAt, убийство по таймауту между ними оставляет пустой файл, openState молча берёт пустой как Fresh. 19 пустых файлов в ~/.local/state/chooks/skill-reload с 25.10. Не чинил: 2 из 349 (0,6%), правило «имеет значение?»; лечение при росте - запись во временный файл + rename при блокировке отдельного .lock.
- 2026-10-31 12:42 на проверке — chooks-master — итог посчитан, ждёт независимой сверки двух ложных случаев
- 2026-10-31 12:50 note — chooks-master: Поправка по сверке: 19 пустых файлов состояния — с 24.10, у всех нет транскрипта (видимо, claude -p без сохранения), механизм они не подтверждают. Не исключён второй путь молчаливого сброса: смена transcript_path (ветка state.Transcript != transcript в update()). Оба таймаута пришлись на остановки ввода-вывода (aguard io 30.10 12:27:37 6,7 с — btrfs relocate; 13:16:37 1,3 с — snapper).
- 2026-10-31 12:50 готово — chooks-master-verifier (session chooks-master): Независимая сверка суб-агентом Opus по брифу тикета, 31.10 12:51 CET: 1) оба ложных повтора подтверждены — побайтно одинаковые диффы (md5 009dcc38…, d8518e0a…), между выдачами нет Skill/Read/Edit/compact, есть hook_cancelled timedOut (смещения 25678338, 7750563); в окне таймаутов хука ровно два; 2) save() Truncate(0)+WriteAt и молчаливый Fresh на пустом файле подтверждены по state.go/check.go, испорченного JSON в журнале 0; 3) повтор после сжатия законен: invoked_skills по смещению 23818540 = снимку Skill по смещению 22885906 (sha256 30d770a0), в rendered старая строка. Файлы: /home/aisee/.cache/claude-tmp/skillreload-verify/
