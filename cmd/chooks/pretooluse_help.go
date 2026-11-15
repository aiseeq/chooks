package main

// preToolUseLong — справка pre-tool-use: какие проверки идут перед вызовом
// инструмента, что каждая отклоняет и почему. Карточки инструментов агентов
// собираются из справки, так агент видит причину отказа до вызова
const preToolUseLong = `Проверки перед вызовом инструмента (PreToolUse). Отказ — код 2, причина и
подсказка — в stderr, их читает модель. Проверку включает ключ
tools.<имя>.enabled в config.yaml. Без ключа включены git_guard, rsync_guard,
pgrep_ancestors и understand_check, остальные выключены.

Отказывают:
  kill_guard (Bash, Monitor) — kill, pkill, killall, killall5, цель которых до
    запуска не проверить (PID из переменной или $(…), цели через xargs и
    parallel, скрипт из ввода) или которые заденут саму сессию: PID 1, kill -1,
    kill 0, группа процессов, предки хука (claude, оболочка, терминал), pkill -f
    с шаблоном, совпадающим с текстом своей же команды. Одна такая команда гасит
    все сессии Claude.
  rm_guard (Bash, Monitor) — rm и rmdir, на которых Claude Code сам остановит
    команду запросом «Dangerous rm operation on possibly-empty variable path»:
    запрос не снимается и в bypassPermissions и ждёт пользователя. Отказ даёт
    переписанную форму.
  write_guard (Bash, Monitor) — запись файла через here-doc, тело которого
    Claude Code примет за rm по пустой переменной и остановит тем же запросом.
  rsync_guard (Bash, Monitor) — rsync с --delete, --del или --delete-*, если
    назначение раскрывается при запуске (переменная, подстановка): пустое
    значение удалит не то. "${R:?}" и литеральный путь проходят.
  git_guard (Bash, Monitor) — суб-агентам, не основной сессии: git-команды,
    которые стирают незакоммиченную работу или меняют запушенное (reset --hard,
    restore, checkout пути, clean, stash, force-push, удаление ветки). Основная
    сессия делает их по «да» пользователя.
  model_guard (Agent, Bash, Monitor) — модель не из ~/.config/models/approved:
    model у Agent, --model, --settings и --agents у claude, codex и opencode,
    переменные ANTHROPIC_MODEL и подобные.
  jira_style (Bash) — curl с комментарием в Jira, текст которого написан мимо
    скилла write-as-user: длинное тире, markdown, типографские кавычки, оборот
    «X, а не Y», приветствие, число прописью.

Не отказывает:
  pgrep_ancestors (Bash, Monitor) — дописывает -A к pgrep -f: иначе pgrep
    находит оболочку своей же команды, и цикл «ждать, пока процесс кончится»
    не кончается.

Отдельной подкомандой, своей строкой хука в settings.json:
  understand-check (ключ understand_check; SendMessage, Edit, Write, Bash) —
    судья без контекста
    беседы сверяет письмо другой сессии и правку инструкций с репликой
    пользователя. Расхождение — отказ один раз; тот же вызов повтором проходит.`
