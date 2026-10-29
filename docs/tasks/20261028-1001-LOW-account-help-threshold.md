# chooks account: справка пишет 98%, порог WeekEmptyPercent 99

Статус: готово
Исполнитель: chooks-master

## Вход
Находка Сна 28.10 через lead: справка `chooks account` (cmd/chooks/account.go:31) — «меньше 98%», порог internal/accounts/choose.go WeekEmptyPercent = 99 (с 22.10).

## Что сделать

## Приёмка
- `chooks account -h` после установки показывает 99%; grep '98%' по cmd/ пуст.

## Сделать
- Справка: «меньше 99%».

## Журнал
- 2026-10-28 10:01 открыт — chooks-master
- 2026-10-28 10:18 на проверке — chooks-master — 642eb0d, установлен: chooks account -h — «меньше 99%».
- 2026-10-28 10:22 готово — git-admin: git-admin 28.10: chooks account -h → 99%, константа WeekEmptyPercent = 99.0 (internal/accounts/choose.go:17), 642eb0d
