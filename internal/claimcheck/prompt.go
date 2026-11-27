package claimcheck

import _ "embed"

// JudgePrompt — промпт судьи v3: копия ~/docs/claim-check-20261122/luna-data/prompt-v3.md.
// Пользователь 22.11 23:41 CET (tl 27d21ddd:21337): «Включайте хук на luna high v3»
//
//go:embed prompt-v3.md
var JudgePrompt string

// Schema — строгая JSON-схема ответа судьи для `codex exec --output-schema`:
// claims[] с text, verdict из четырёх слов, basis и could_check. Строгая схема
// снижает, но не исключает неизвестный вердикт: ParseClaims его всё равно
// отделяет
//
//go:embed verdict-schema.json
var Schema []byte

//go:embed prompt-v6.md
var JudgePromptV6 string

//go:embed verdict-schema-v6.json
var SchemaV6 []byte

// JudgePromptV7 — v6 и вторая задача: вопрос пользователю или черновик людям
// без строки ❓ или «Проверил: <id>». Пользователь 27.11 18:38 CET
// (tl 27d21ddd:63954) на карточку tl 27d21ddd:63950: «1» — включить v7
//
//go:embed prompt-v7.md
var JudgePromptV7 string

//go:embed verdict-schema-v7.json
var SchemaV7 []byte

// FinalOnly — промпт судит только итог хода (v6 и v7): вход — вызовы хода
// без вывода, письма SendMessage не судятся
func FinalOnly(prompt string) bool {
	return prompt == "v6" || prompt == "v7"
}
