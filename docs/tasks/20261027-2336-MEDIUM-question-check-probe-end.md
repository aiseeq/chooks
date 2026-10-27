# question-check: после итога пробы 30.10 убрать или продлить

Статус: открыт

## Постановка
Ожидание к сроку. Проба `question-checker` (`~/.claude/improve/state/experiments.tsv`, строка 10) идёт до 30.10. После 30.10 23:59 по местному `question-check` сам становится no-op (`internal/questioncheck/session.go`, `Until`). Итог пробы подводит Сон; что делать с кодом, решает пользователь.

## Что сделать
- Срок 31.10: прочитать итог пробы (experiments.tsv, отчёт Сна).
- Проба принята: снять `Until` и гейт сессий по решению; срок продлён — сдвинуть `Until`; отклонена — убрать подкоманды `question-check` и `question-bench`, пакет `internal/questioncheck`, запись в settings (образец в configs) и разделы README и CLAUDE.md.
- Записи журнала делить по `judge_rev`: rev 3 (порядок реплики и предыдущего ответа) с 27.10 ~23:40 CET, тикет `question-check-reply-order`.

## Чем проверить
- `graft commit`: зелёный гейт.
- После `graft install`: `chooks question-check` на событии Stop из сессии lead ведёт себя по решению (журнал `~/.local/state/chooks/question-check.jsonl`, лог `~/.claude/logs/chooks.log`).

## Журнал
- 2026-10-27 23:36 открыт — chooks-master
