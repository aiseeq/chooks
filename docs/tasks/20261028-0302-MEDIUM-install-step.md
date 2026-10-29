# Установка бинаря шагом graft {install: bin/chooks}

Статус: готово
Исполнитель: chooks-master

## Вход
Задание git-admin 28.10 ~01:0x CET (лид 27.10, пункт D; решение пользователя `tl 2dc3ff44:2495`). Форма и проверка — `~/git/docs/install-step.md`, пилот akit ad1c782.

## Что сделать
- `.graft.yaml`, задача install: `cp`+`mv` бинаря заменить шагом `{install: bin/chooks}`, sh-шаг с конфигом оставить.
- README «Установка»: ссылка `graft@v0.4.0` (пин) и устаревшее «кладётся рядом как config.yaml.new».
- Комментарий гейта про `fail_on: low` устарел после cf12329.
- `graft commit`, затем `graft install`.

## Приёмка
- Вывод `graft install`: строка `graft: install: chooks <rev7> -> ~/bin/chooks`.
- `go version -m ~/bin/chooks | grep vcs`: `vcs.modified=false`, ревизия — коммит правки.
- `chooks version` печатает версию.

## Журнал
- 2026-10-28 03:02 открыт — chooks-master
- 2026-10-28 03:02 note — chooks-master: правка .graft.yaml и README; дальше graft commit и graft install
- 2026-10-28 03:02 note — chooks-master: коммит 679a040; glint medium unused-internal-export на BaseTool.SupportedTools — ложное (метод интерфейса core.ToolValidator через встраивание), отправлено glint-master
- 2026-10-28 03:03 note — chooks-master: graft install: «graft: install: chooks ff28e4f -> ~/bin/chooks» (повтор — unchanged), vcs.revision=ff28e4f vcs.modified=false, chooks v0.0.0-143-gff28e4f. Отказ первого install: журнал тикета грязнил дерево — тикет закоммичен до установки
- 2026-10-28 03:03 на проверке — chooks-master — ждёт проверки git-admin; tk done упрётся в ложную находку glint (glint-master)
- 2026-10-28 03:04 note — git-admin: проверка git-admin: принято (.graft.yaml {install: bin/chooks}, ~/bin/chooks ff28e4f modified=false); tk done отказал на ложной glint unused-internal-export tool.go:36 — у glint-master; done после фикса правила
- 2026-10-28 03:07 готово — git-admin: git-admin 28.10: проверил .graft.yaml и ~/bin/chooks (ff28e4f, vcs.modified=false); ложная находка glint снята glint-master 37bb4d9
- 2026-10-28 03:08 готово — git-admin: git-admin: {install: bin/chooks}, ~/bin/chooks ff28e4f modified=false; ложная glint снята в a839d21
