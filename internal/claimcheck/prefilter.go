package claimcheck

import (
	"regexp"
	"strings"
)

// Предфильтр без модели: ход без признаков утверждения о факте (только вопрос,
// план или согласие) судье не идёт. Пропуск предфильтра — пропуск хука, поэтому
// признаки берутся с запасом: число или дата, имя файла или путь, слово
// состояния

var (
	digitRE = regexp.MustCompile(`\pN`)
	// fileRE — имя с расширением (main.go, design.md) или путь от / и ~/
	fileRE       = regexp.MustCompile(`(?:^|[^\pL\pN_])(?:[A-Za-z0-9_-]+\.[A-Za-z][A-Za-z0-9]{0,5}(?:$|[^\pL\pN_])|~?/[\pL\pN_.-]+)`)
	letterWordRE = regexp.MustCompile(`\pL+`)
)

// stateWords — слова состояния из замысла («работает», «упало», «нет», «есть»,
// «прошёл», «готово») в ходовых формах
var stateWords = map[string]bool{
	"работает": true, "работают": true, "работал": true, "работала": true, "работало": true, "работали": true,
	"заработал": true, "заработала": true, "заработало": true, "заработали": true,
	"упал": true, "упала": true, "упало": true, "упали": true, "падает": true, "падают": true,
	"нет": true, "есть": true,
	"прошёл": true, "прошел": true, "прошла": true, "прошло": true, "прошли": true, "проходит": true, "проходят": true,
	"готово": true, "готов": true, "готова": true, "готовы": true,
}

// HasFactSignal — в тексте есть признак утверждения о факте и его надо судить
func HasFactSignal(text string) bool {
	if digitRE.MatchString(text) || fileRE.MatchString(text) {
		return true
	}
	for _, w := range letterWordRE.FindAllString(strings.ToLower(text), -1) {
		if stateWords[w] {
			return true
		}
	}
	return false
}
