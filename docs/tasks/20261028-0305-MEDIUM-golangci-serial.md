# golangci-lint: ждать параллельный запуск, а не падать кодом 3

Статус: на проверке
Исполнитель: chooks-master

## Вход
Задание git-admin 28.10 ~03:1x CET, находка graft-master: если в соседнем проекте уже идёт golangci-lint, второй запуск выходит с кодом 3 («parallel golangci-lint is running», блокировка `$TMPDIR/golangci-lint.lock`).

## Что сделать
- В chooks golangci-lint есть только в задаче `lint` (`.graft.yaml`), не в гейте; `.golangci.yml` нет. Добавить флаг `--allow-serial-runners` в этот запуск. Конфиг не заводить: он сменил бы набор линтеров по умолчанию.

## Приёмка
- Держать блокировку: `flock $TMPDIR/golangci-lint.lock sleep 20 &`, затем `graft lint` — запуск ждёт освобождения и не падает кодом 3.

## Журнал
- 2026-10-28 03:05 открыт — chooks-master
- 2026-10-28 03:06 note — chooks-master: флаг --allow-serial-runners в задаче lint. Проверка: блокировка /tmp/claude-tmp/golangci-lint.lock занята flock на 15с — с флагом запуск дождался и прошёл (exit 1 — 31 давняя находка: errcheck 26, staticcheck 4, unused 1; задача lint не в гейте), без флага exit 3 «parallel golangci-lint is running»
- 2026-10-28 03:06 на проверке — chooks-master — ждёт проверки git-admin; tk done упрётся в ложную находку glint tool.go:36
- 2026-10-28 03:07 note — git-admin: проверка git-admin: принято — флаг --allow-serial-runners в задаче lint (15ad958), проба flock мастера: с флагом ждёт, без — код 3; done ждёт фикс ложной glint tool.go:36
