package claimcheck

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Вердикты судьи на одно утверждение
const (
	VerdictChecked         = "checked"
	VerdictUncheckedMarked = "unchecked-marked"
	VerdictUnchecked       = "unchecked"
	VerdictWrongTime       = "wrong-time"
)

// Claim — утверждение из ответа агента с вердиктом судьи
type Claim struct {
	Text       string `json:"text"`
	Verdict    string `json:"verdict"`
	Basis      string `json:"basis"`
	CouldCheck string `json:"could_check"`
}

// Verdict — разобранный ответ судьи
type Verdict struct {
	Claims []Claim `json:"claims"`
}

// ParseVerdict разбирает ответ судьи: первый JSON-объект с полем claims,
// возможно в обёртке текста или markdown. Неизвестный вердикт и ответ без
// такого объекта — ошибка разбора: такой ответ не даёт ни flag, ни его
// отсутствия. Ошибка — от первой открывающей скобки: вложенный объект
// оборванного ответа разбирается, но причину не объясняет
func ParseVerdict(answer string) (Verdict, error) {
	verdict, unknown, err := ParseClaims(answer)
	if err != nil {
		return Verdict{}, err
	}
	if len(unknown) > 0 {
		return Verdict{}, fmt.Errorf("claim %d: unknown verdict %q", unknown[0].Index, unknown[0].Verdict)
	}
	return verdict, nil
}

// UnknownClaim — пункт ответа судьи с вердиктом вне четырёх слов и его номер
// в ответе
type UnknownClaim struct {
	Claim
	Index int `json:"index"`
}

// ParseClaims — разбор ответа для хука: пункт с неизвестным вердиктом не
// роняет разбор, а уходит в unknown, верные пункты остаются в Verdict. Хук
// пишет unknown в журнал как сбой проверки: неизвестный пункт мог быть
// единственным непроверенным (противник замысла, сценарий 8)
func ParseClaims(answer string) (Verdict, []UnknownClaim, error) {
	var claims *[]Claim
	var firstErr error
	for i := strings.Index(answer, "{"); i >= 0; {
		var raw struct {
			Claims *[]Claim `json:"claims"`
		}
		err := json.NewDecoder(strings.NewReader(answer[i:])).Decode(&raw)
		switch {
		case err != nil:
			err = fmt.Errorf("judge answer is not JSON: %w", err)
		case raw.Claims == nil:
			err = fmt.Errorf("judge answer has no claims field")
		default:
			claims = raw.Claims
		}
		if firstErr == nil {
			firstErr = err
		}
		if claims != nil {
			break
		}
		next := strings.Index(answer[i+1:], "{")
		if next < 0 {
			break
		}
		i += next + 1
	}
	if claims == nil {
		if firstErr == nil {
			firstErr = fmt.Errorf("judge answer has no JSON object: %q", excerpt(answer, 200))
		}
		return Verdict{}, nil, firstErr
	}
	known := []Claim{}
	var unknown []UnknownClaim
	for i, c := range *claims {
		switch c.Verdict {
		case VerdictChecked, VerdictUncheckedMarked, VerdictUnchecked, VerdictWrongTime:
			known = append(known, c)
		default:
			unknown = append(unknown, UnknownClaim{Claim: c, Index: i})
		}
	}
	return Verdict{Claims: known}, unknown, nil
}

// FlaggedClaims — утверждения, требующие проверки
func (v Verdict) FlaggedClaims() []Claim {
	var out []Claim
	for _, c := range v.Claims {
		if c.Flags() {
			out = append(out, c)
		}
	}
	return out
}

// Flags — утверждение требует проверки: без неё и без пометки или с меткой
// времени, расходящейся с проверкой
func (c Claim) Flags() bool {
	return c.Verdict == VerdictUnchecked || c.Verdict == VerdictWrongTime
}

// Flagged — итог хода: есть утверждение, требующее проверки
func (v Verdict) Flagged() bool {
	for _, c := range v.Claims {
		if c.Flags() {
			return true
		}
	}
	return false
}

// Hits — flag дан на утверждение, совпадающее с размеченным тезисом
func (v Verdict) Hits(target string) bool {
	for _, c := range v.Claims {
		if c.Flags() && SameClaim(c.Text, target) {
			return true
		}
	}
	return false
}

// overlapPercent — доля общих слов от меньшего набора, с которой утверждения
// считаются одним тезисом; overlapMinWords — сколько слов нужно в меньшем
// наборе: иначе одно общее слово короткого утверждения давало бы 100%
const (
	overlapPercent  = 60
	overlapMinWords = 3
)

var claimWordRE = regexp.MustCompile(`[\pL\pN_]+(?:[./-][\pL\pN_]+)*`)

// SameClaim — утверждение судьи и размеченный тезис об одном: одно — подстрока
// другого после схлопывания пробелов и регистра, или у них не меньше 60% общих
// слов от меньшего набора
func SameClaim(claim, target string) bool {
	a, b := normalizeSpace(claim), normalizeSpace(target)
	if a == "" || b == "" {
		return false
	}
	if strings.Contains(a, b) || strings.Contains(b, a) {
		return true
	}
	wa, wb := claimWords(a), claimWords(b)
	small, large := wa, wb
	if len(wb) < len(wa) {
		small, large = wb, wa
	}
	if len(small) < overlapMinWords {
		return false
	}
	common := 0
	for w := range small {
		if large[w] {
			common++
		}
	}
	return common*100 >= overlapPercent*len(small)
}

func normalizeSpace(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// claimWords — слова от двух букв: однобуквенные предлоги и союзы не делают
// тезисы похожими
func claimWords(s string) map[string]bool {
	words := map[string]bool{}
	for _, w := range claimWordRE.FindAllString(s, -1) {
		if utf8.RuneCountInString(w) >= 2 {
			words[w] = true
		}
	}
	return words
}
