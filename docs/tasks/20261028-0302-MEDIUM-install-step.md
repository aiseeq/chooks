# Установка бинаря шагом graft {install: bin/chooks}

Статус: открыт

## Вход
Задание git-admin 28.10 ~01:0x CET (лид 27.10, пункт D; решение пользователя `tl 2dc3ff44:2495`). Форма и проверка — `~/git/docs/install-step.md`, пилот akit ad1c782.

## Что сделать
- `.graft.yaml`, задача install: `cp`+`mv` бинаря заменить шагом `{install: bin/chooks}`, sh-шаг с конфигом оставить.
- README «Установка»: ссылка `graft@v0.4.0` (пин) и устаревшее «кладётся рядом как config.yaml.new».
- Комментарий гейта про `fail_on: low` устарел после 560c189.
- `graft commit`, затем `graft install`.

## Приёмка
- Вывод `graft install`: строка `graft: install: chooks <rev7> -> ~/bin/chooks`.
- `go version -m ~/bin/chooks | grep vcs`: `vcs.modified=false`, ревизия — коммит правки.
- `chooks version` печатает версию.

## Журнал
- 2026-10-28 03:02 открыт — chooks-master
- 2026-10-28 03:02 note — chooks-master: правка .graft.yaml и README; дальше graft commit и graft install
- 2026-10-28 03:02 note — chooks-master: коммит 294271a; glint medium unused-internal-export на BaseTool.SupportedTools — ложное (метод интерфейса core.ToolValidator через встраивание), отправлено glint-master
