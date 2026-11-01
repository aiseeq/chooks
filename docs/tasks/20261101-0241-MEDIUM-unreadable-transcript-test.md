# Сон 01.11 tests: TestRun_UnreadableTranscript падает при TMPDIR на диске; DNS-проба; разбор тестов

Статус: в работе
Исполнитель: chooks-master

## Что сделать
- Задание dream-master 01.11 (обход tests, /home/aisee/.claude/improve/runs/2026-11-01/round-tests.md:11,21).
- Найти причину падения `TestRun_UnreadableTranscript` (internal/unsentcheck/check_test.go:185, check.go:130-135) на копии 585d92b при TMPDIR на диске; исправить обработку или тест без ослабления проверки.
- Повторить DNS-пробу (unshare + resolv-offline) всего набора.
- Первый разбор тестов chooks: стадии, что поймали за месяц, дубли, пустые тесты и мутации фиксов, сеть, время; профиль CPU/память через graft exec --heavy, сборка отдельно.

## Приёмка
- `TMPDIR=<папка на диске> graft exec --heavy -- graft test -- -count=1 -run '^TestRun_UnreadableTranscript$'` зелёный, и тест краснеет на мутации обработки ошибки чтения.
- DNS-проба: все пакеты OK.

## Журнал
- 2026-11-01 02:41 открыт — chooks-master
- 2026-11-01 02:58 note — chooks-master: Причина: на btrfs размер пустого каталога 0 -> окно 0, ReadAt пустого буфера без syscall и без ошибки; на tmpfs размер 40 -> EISDIR. Фикс: общий internal/tailscan (Scan/Clip) с отказом на не-обычный файл; unsentcheck и questioncheck - на нём, дубль цикла хвоста снят. Мутация (снять IsRegular) краснит TestScan_DirectoryIsError и TestRun_UnreadableTranscript при TMPDIR на btrfs. DNS-проба (unshare, resolv 127.0.0.9, GODEBUG=netdns=go, GOPROXY=off, TMPDIR на btrfs): DNS connection refused, 15 пакетов OK, 290 тестов pass. Время: прогон 2,96 с wall / 6,8 с user при тёплой сборке, холодная сборка 21,2 с wall / 104 с user / 298 MB RSS; самые долгие тесты 0,44 с.
- 2026-11-01 02:58 в работе — chooks-master
