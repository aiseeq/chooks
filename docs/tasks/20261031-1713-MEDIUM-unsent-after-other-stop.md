# Остановка другим Stop-хуком отключает unsent-check

Статус: открыт

## Что сделать
- Находка F4 критика spec-as-built (тикет 20261031-1534-MEDIUM-spec-as-built; ТЗ /home/aisee/.claude/tools/chooks/spec.md, срез 388c8de; находки — критик Terra worker 1031-1633-spec-critic, 31.10 16:40 CET). Принята chooks-master 31.10.
- Код хука unsent-check: починка бага хука, install после проверки. Сверено мастером: internal/unsentcheck/check.go:76-83 пропускает любой stop_hook_active; обоснование в комментарии («подсказка уже была») верно, только если остановил сам unsent-check. Частоту оценить до правки: сколько раз question-check останавливал ход (~/.local/state/chooks/question-check.jsonl) и сколько продолжений после них содержат фразу claim (internal/unsentcheck/check.go:24).
- **Суть.** После остановки любым Stop-хуком unsent-check пропускает следующий конец хода, даже если сам прежде ещё не проверил утверждение о неотправленном тексте. Это позволяет пропустить новое утверждение, появившееся в продолжении после question-check.
- **Доказательство.** internal/unsentcheck/check.go:73 безусловно пропускает `stop_hook_active`, тогда как question-check сам возвращает блокировку в cmd/chooks/questioncheck.go:87. Оба хука подключаются отдельно: configs/settings-snippet.json:40 и /home/aisee/.claude/tools/chooks/spec.md:19.

## Приёмка
- Сначала красный тест, воспроизводящий находку: Вызвать `unsentcheck.Run` с `StopHookActive: true` и `LastMessage: "текст не отправлен"`: результат `Skip: "stop hook active"` получается до чтения транскрипта. Сопоставить с блокировкой question-check на предыдущем Stop; живой судья не нужен.
- После правки тест зелёный, `graft smoke` зелёный, glint 0.

## Журнал
- 2026-10-31 17:13 открыт — chooks-master
