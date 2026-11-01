# Сон 01.11 tests: TestRun_UnreadableTranscript падает при TMPDIR на диске; DNS-проба; разбор тестов

Статус: готово
Исполнитель: chooks-master

## Что сделать
- Задание dream-master 01.11 (обход tests, /home/aisee/.claude/improve/runs/2026-11-01/round-tests.md:11,21).
- Найти причину падения `TestRun_UnreadableTranscript` (internal/unsentcheck/check_test.go:185, check.go:130-135) на копии 585d92b при TMPDIR на диске; исправить обработку или тест без ослабления проверки.
- Повторить DNS-пробу (unshare + resolv-offline) всего набора.
- Первый разбор тестов chooks: стадии, что поймали за месяц, дубли, пустые тесты и мутации фиксов, сеть, время; профиль CPU/память через graft exec --heavy, сборка отдельно.

## Приёмка
- `TMPDIR=<папка на диске> graft exec --heavy --local -- go test -count=1 -run '^TestRun_UnreadableTranscript$' ./internal/unsentcheck` (без --local heavy уходит на pc без TMPDIR) зелёный, и тест краснеет на мутации обработки ошибки чтения.
- DNS-проба: все пакеты OK.

## Журнал
- 2026-11-01 02:41 открыт — chooks-master
- 2026-11-01 02:58 note — chooks-master: Причина: на btrfs размер пустого каталога 0 -> окно 0, ReadAt пустого буфера без syscall и без ошибки; на tmpfs размер 40 -> EISDIR. Фикс: общий internal/tailscan (Scan/Clip) с отказом на не-обычный файл; unsentcheck и questioncheck - на нём, дубль цикла хвоста снят. Мутация (снять IsRegular) краснит TestScan_DirectoryIsError и TestRun_UnreadableTranscript при TMPDIR на btrfs. DNS-проба (unshare, resolv 127.0.0.9, GODEBUG=netdns=go, GOPROXY=off, TMPDIR на btrfs): DNS connection refused, 15 пакетов OK, 290 тестов pass. Время: прогон 2,96 с wall / 6,8 с user при тёплой сборке, холодная сборка 21,2 с wall / 104 с user / 298 MB RSS; самые долгие тесты 0,44 с.
- 2026-11-01 02:58 в работе — chooks-master
- 2026-11-01 03:27 note — chooks-master: Разбор тестов (боец Luna 1101-0305, 2 захода, отчёт /home/aisee/.cache/claude-tmp/claude-1000/-home-aisee-git-chooks/0f7d5c09-e2d3-4f09-bf78-3b04ec6368d5/scratchpad/tests-review-1101.md): стадии - gate=smoke(go build+go test)+glint, test вне gate - дубля нет; graft stats с 25.10 при load<16: gate 83 прогона, wall мед 3,9 с, CPU 5,8 с, TREE 170 MB, отказы 1%; smoke 101, 1,4 с, 2,6 с CPU. Фиксы октября 30, без теста 3 (b93d10d nolint, fa2a662 конфиг, 94ccb84 statusline). Ручные мутации 8 фиксов: красные 6 (d5b09e6, 29190dc, 2f013e8, e3262f2, 810056e, c501f70), зелёные 2: a541698 (Debug-лог, не поведение) и 361cf1b на tmpfs - тест каталога ловит любую ошибку, на tmpfs её даёт ReadAt (EISDIR); усилить: метка ErrNotRegular и errors.Is. Пустых тестов нет, дублей нет. Покрытие 72,5%; ниже 60% с фиксами - cmd/chooks 29,3% (4 фикса), без фиксов - internal/desktop 28,6%. Сеть и время - выше (DNS-проба).
- 2026-11-01 03:30 note — chooks-master: Независимая проверка (суб-агент, бриф scratchpad 0f7d5c09 review-brief.md): п.1-4 прошли (btrfs и tmpfs зелёные; мутация IsRegular краснит на btrfs; окна 1..32 МБ по 40-МБ файлу; ReadAt только в tailscan.go). Находки: на tmpfs мутация зелёная (EISDIR) - усилено: tailscan.ErrNotRegular, оба теста проверяют errors.Is; мутация теперь краснит TestScan_DirectoryIsError и TestRun_UnreadableTranscript и на /tmp, и на btrfs. graft exec --heavy уводит go test на pc без TMPDIR - команда приёмки исправлена на --local; мои доказательства шли локально (мутация - прямой go test, DNS-проба - unshare: «on this machine»).
- 2026-11-01 03:34 готово — review-subagent-ad6941a (session chooks-master): Суб-агент-проверяющий (бриф scratchpad 0f7d5c09 review-brief.md, 2 прохода, прогоны --local): 361cf1b+4ae9f49 - TestRun_UnreadableTranscript и TestScan_DirectoryIsError зелёные на tmpfs и btrfs; мутация IsRegular краснит оба на обеих ФС (tmpfs: «is a directory» вместо ErrNotRegular; btrfs: err=nil); окна 1..32 МБ на 40-МБ файле; ReadAt только в tailscan.go:42. DNS-проба полного набора - зелёная (журнал). Разбор тестов - в журнале.
