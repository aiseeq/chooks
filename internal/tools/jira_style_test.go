package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/chooks/internal/core"
)

func newJiraStyleTool(t *testing.T) *JiraStyleTool {
	t.Helper()
	tool, err := NewJiraStyleTool(core.ToolConfig{Enabled: true}, testLogger(t))
	if err != nil {
		t.Fatalf("NewJiraStyleTool: %v", err)
	}
	return tool
}

func validateJiraCommand(t *testing.T, command string) *core.ValidationResult {
	t.Helper()
	return validateJiraCommandIn(t, "", command)
}

// validateJiraCommandIn проверяет команду, запущенную в каталоге cwd
func validateJiraCommandIn(t *testing.T, cwd, command string) *core.ValidationResult {
	t.Helper()
	result, err := newJiraStyleTool(t).ValidateTool(context.Background(), &core.ToolInput{
		ToolName: "Bash",
		Command:  command,
		CWD:      cwd,
	})
	if err != nil {
		t.Fatalf("ValidateTool: %v", err)
	}
	return result
}

func TestJiraStyleBlocksEmDashInInlinePayload(t *testing.T) {
	result := validateJiraCommand(t,
		`curl -s -X POST --data-binary '{"body":"Готово — выкачено"}' "$JIRA_BASE_URL/rest/api/3/issue/PROJ-123/comment"`)
	if result.IsValid {
		t.Fatal("длинное тире в теле комментария должно блокироваться")
	}
}

func TestJiraStyleBlocksEscapedEmDash(t *testing.T) {
	result := validateJiraCommand(t,
		`curl -X PUT -d '{"text":"a — b"}' https://x.atlassian.net/rest/api/3/issue/PROJ-1/comment/42`)
	if result.IsValid {
		t.Fatal("длинное тире в escape-форме должно блокироваться")
	}
}

func TestJiraStyleBlocksMarkdownInPayloadFile(t *testing.T) {
	payload := filepath.Join(t.TempDir(), "comment.json")
	if err := os.WriteFile(payload, []byte(`{"body":"**Итог:** сделано"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	result := validateJiraCommand(t,
		`curl -s -X POST --data-binary @`+payload+` "$JIRA_BASE_URL/rest/api/3/issue/PROJ-123/comment"`)
	if result.IsValid {
		t.Fatal("markdown в файле с телом комментария должен блокироваться")
	}
}

func TestJiraStyleAllowsPlainComment(t *testing.T) {
	result := validateJiraCommand(t,
		`curl -s -X POST --data-binary '{"body":"Выкачено на UAT, версия 2.0.123"}' "$JIRA_BASE_URL/rest/api/3/issue/PROJ-123/comment"`)
	if !result.IsValid {
		t.Fatalf("обычный комментарий не должен блокироваться: %+v", result.Violations)
	}
}

func TestJiraStyleIgnoresNonCommentRequests(t *testing.T) {
	result := validateJiraCommand(t,
		`echo "тире — в обычной команде" && curl "$JIRA_BASE_URL/rest/api/3/issue/PROJ-123?fields=summary"`)
	if !result.IsValid {
		t.Fatal("команда без комментария в Jira не должна блокироваться")
	}
}

func TestJiraStyleIgnoresReadingComments(t *testing.T) {
	result := validateJiraCommand(t,
		`curl -s "$JIRA_BASE_URL/rest/api/3/issue/PROJ-123/comment" | python3 render.py # — сверка`)
	if result.IsValid {
		return
	}
	// GET за комментариями не содержит тела, но тире в остальной команде не повод блокировать чтение
	t.Fatal("чтение комментариев не должно блокироваться")
}

// jiraComment собирает команду, постящую комментарий с телом в формате
// Jira REST v2 ({"body": "..."}): текст экранируется как JSON-строка
func jiraComment(t *testing.T, text string) string {
	t.Helper()
	body, err := json.Marshal(map[string]string{"body": text})
	if err != nil {
		t.Fatal(err)
	}
	return `curl -s -X POST --data-binary '` + string(body) + `' "$JIRA_BASE_URL/rest/api/2/issue/PROJ-7/comment"`
}

// jiraCommentFile пишет тело комментария в файл и собирает команду, которая
// его отправляет
func jiraCommentFile(t *testing.T, adf string) string {
	t.Helper()
	payload := filepath.Join(t.TempDir(), "comment.json")
	if err := os.WriteFile(payload, []byte(adf), 0o600); err != nil {
		t.Fatal(err)
	}
	return `curl -s -X POST --data-binary @` + payload + ` "$JIRA_BASE_URL/rest/api/3/issue/PROJ-7/comment"`
}

func TestJiraStyleBlocksFormViolations(t *testing.T) {
	cases := map[string]string{
		"ёлочки":                    `В карточке «Итог за день» теперь сумма без комиссии`,
		"лапки":                     `Статус “в обработке” больше не залипает`,
		"немецкие кавычки":          `Кнопка „Выгрузить“ появилась у всех ролей`,
		"X, а не Y":                 `Повторный прогон перезаписывает запись, а не копит версии`,
		"X, а не только Y":          `Вкладка открыта всем ролям, а не только админу`,
		"это не X, а Y":             `Это не ошибка расчёта, а реальная просадка стратегии`,
		"не из X, а из Y":           `Форма берёт варианты не из зашитого списка, а из ответа провайдера`,
		"приветствие":               `Привет, выкатил на UAT, можно проверять`,
		"приветствие после имени":   `@Иван Петров привет. Фид ордеров теперь идёт с прода`,
		"добрый день":               `Добрый день! Отчёт за вчера пересчитан`,
		"здравствуйте":              `Здравствуйте, ключ перевыпущен`,
		"числительное":              `Осталось три расхождения в журнале`,
		"числительное с заглавной":  `Две причины занижения закрыты`,
		"числительное в падеже":     `Из двух операций у нас нет только одной`,
		"десяток":                   `Прогон занимает двадцать секунд`,
		"числительное творительный": `Отчёт собирается тремя запросами`,
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			if result := validateJiraCommand(t, jiraComment(t, text)); result.IsValid {
				t.Fatalf("должно блокироваться: %s", text)
			}
		})
	}
}

func TestJiraStyleBlocksEscapedCyrillicInADF(t *testing.T) {
	// {"type":"text","text":"Две причины закрыты"}, записанный json.dumps
	// без ensure_ascii=False: кириллица уходит в \uXXXX
	adf := `{"body":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[` +
		`{"type":"text","text":"Две причины закрыты"}]}]}}`
	if result := validateJiraCommand(t, jiraCommentFile(t, adf)); result.IsValid {
		t.Fatal("числительное прописью в escape-форме должно блокироваться")
	}
}

func TestJiraStyleBlocksGreetingAfterMentionNode(t *testing.T) {
	adf := `{"body":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[` +
		`{"type":"mention","attrs":{"id":"acc-1","text":"@Иван Петров"}},` +
		`{"type":"text","text":" привет, напомню про задачу"}]}]}}`
	if result := validateJiraCommand(t, jiraCommentFile(t, adf)); result.IsValid {
		t.Fatal("приветствие после упоминания должно блокироваться")
	}
}

func TestJiraStyleNamesWhatToReplace(t *testing.T) {
	result := validateJiraCommand(t, jiraComment(t, `Две причины закрыты, а не одна`))
	joined := strings.Join(result.Suggestions, "\n")
	for _, want := range []string{"Две причины", ", а не "} {
		if !strings.Contains(joined, want) {
			t.Fatalf("подсказка должна называть фрагмент %q: %s", want, joined)
		}
	}
}

// Похожие на нарушения, но допустимые тексты: переписаны с ложных
// срабатываний на реальных комментариях
func TestJiraStyleAllowsLookalikes(t *testing.T) {
	cases := map[string]string{
		"не X, а связка предложений":   `Строка без хэша сопоставиться не может по построению, а нулевая расходы не двигает`,
		"это не глагол, а связка":      `Клиент получает 499, в алерты это не попадает, а настоящий сбой виден как раньше`,
		"не X, а без предлога":         `Поле не пускает дальше недели, а запрос за окном получает отказ`,
		"не из X, а связка":            `Конвертер не возьмёт строку из реестра, а остальные посчитает как обычно`,
		"не в X, а другой предлог":     `Платёж не уходит в обход настроек, а по лимиту отклоняется`,
		"числительное перед союзом":    `Причин было три, и все закрыты`,
		"числительное перед предлогом": `Проверил три из них`,
		"слова с числом внутри":        `Опять просто дважды семинар пятиминутный двадцатый`,
		"цифры":                     `Осталось 3 расхождения, прогон за 20 секунд`,
		"привет не в начале":        `Коллеги из поддержки передали привет, фикс им подошёл`,
		"формат суммы в интерфейсе": `Сумма показана в валюте депозита, например 10,50 EUR`,
		"прямые кавычки":            `Статус "в обработке" больше не залипает`,
		"дефис":                     `Готово - выкачено на UAT`,
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			if result := validateJiraCommand(t, jiraComment(t, text)); !result.IsValid {
				t.Fatalf("не должно блокироваться: %s: %v", text, result.Suggestions)
			}
		})
	}
}

// Команды отправки, в которых маркер стоит в коде рядом с телом, а не в
// тексте комментария: своя проверка перед отправкой, комментарий shell или
// Python, старый текст в замене sed. Сутью переписаны с ложных блокировок
// 09-10.2026
func TestJiraStyleAllowsMarkersInCode(t *testing.T) {
	const send = `curl -s -o /dev/null -w "%{http_code}\n" -u "$JIRA_EMAIL:$JIRA_API_TOKEN" -X POST ` +
		`-H "Content-Type: application/json" --data-binary @$S/c.json "$JIRA_BASE_URL/rest/api/3/issue/PROJ-1/comment"`
	const generator = `python3 - > $S/c.json <<'PY'
import json
def para(t): return {"type":"paragraph","content":[{"type":"text","text":t}]}
print(json.dumps({"body":{"type":"doc","version":1,"content":[para("Сверка снова сходится, допуск 0.1%")]}}))
PY
`
	cases := map[string]string{
		"grep тире в двойных кавычках": generator + `grep -c "—" $S/c.json; ` + send,
		"grep набора символов":         generator + `grep -l '—\|«\|\*\*' $S/c.json; ` + send,
		"grep -P класса символов":      generator + `grep -n -P '[—–«»“”]' $S/c.json && ` + send,
		"assert в генераторе": `python3 - > $S/c.json <<'PY'
import json
t = "Сверка снова сходится, допуск 0.1%"
assert "—" not in t and "–" not in t and "«" not in t
print(json.dumps({"body": t}))
PY
` + send,
		"python -c с проверкой": generator +
			`python3 -c "import sys;s=open('$S/c.json').read();print('dash' if '—' in s else 'clean')" && ` + send,
		"комментарий в генераторе": `python3 - > $S/c.json <<'PY'
import json
# drop the last "Открыто" heading and append new sections
print(json.dumps({"body": "Сверка снова сходится"}))
PY
` + send,
		"комментарий shell в пакете": "# 1. Онбординг\n" + generator + send,
		"старый текст в замене sed":  `sed -i 's/на все три проекта/на все 3 проекта/' $S/c.json; ` + send,
	}
	for name, command := range cases {
		t.Run(name, func(t *testing.T) {
			if result := validateJiraCommand(t, command); !result.IsValid {
				t.Fatalf("маркер в коде не должен блокировать отправку: %v", result.Suggestions)
			}
		})
	}
}

// Текст комментария в генераторе той же команды, который строками JSON не
// разобрать: строки Python в одинарных кавычках, многострочный аргумент
// функции shell, новый текст замены sed
func TestJiraStyleBlocksTextInGenerator(t *testing.T) {
	const send = ` curl -s -X POST --data-binary @$S/c.json "$JIRA_BASE_URL/rest/api/3/issue/PROJ-1/comment"`
	cases := map[string]string{
		"строка Python в одинарных кавычках": `python3 - > $S/c.json <<'EOF'
import json
P = lambda t: {"type":"paragraph","content":[{"type":"text","text":t}]}
print(json.dumps({"body":{"type":"doc","version":1,"content":[P('На копии данных десять одновременных запусков отвечают за 4.3 с')]}}))
EOF
` + send,
		"многострочный аргумент shell": `mk() { python3 -c 'import json,sys; print(json.dumps({"body": sys.argv[1]}))' "$1" > $S/c.json; }
mk "Кнопка поддержки на проде с 05.10.
Как проверить: внизу страницы нажать «Написать в поддержку», откроется почта."
` + send,
		"тире в строке генератора": `python3 - > $S/c.json <<'EOF'
import json
print(json.dumps({"body": "в колонке Slippage — статус расчёта"}))
EOF
` + send,
		"новый текст замены sed": `sed -i 's/на все 3 проекта/на все три проекта/' $S/c.json;` + send,
		"заголовок в теле":       jiraComment(t, "# Итог\nВыкачено на UAT"),
	}
	for name, command := range cases {
		t.Run(name, func(t *testing.T) {
			if result := validateJiraCommand(t, command); result.IsValid {
				t.Fatal("нарушение в тексте комментария должно блокироваться")
			}
		})
	}
}

// Формы отправки комментария: тело в файле видно при любой записи пути
// (-d@f, --json @f, относительный путь от cwd или после cd, $(cat f), вход
// curl из файла или cat), нераскрытый файл - отказ, файл из heredoc этой же
// команды не читается. Команды без вызова curl с адресом комментария и
// признаком записи не проверяются
func TestJiraStyleSendForms(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("bad.json", `{"body":"Готово — выкачено. **Важно**: проверь."}`)
	write("clean.json", `{"body":"Выкатил на stage, проверь вход."}`)
	write("bad.txt", "Готово — выкачено.")
	// Прежнее тело файла, который команда перезаписывает: читать его
	// нельзя, на отправку уйдёт новый текст
	write("stale.json", `{"body":"Готово — выкачено."}`)
	// Тот же файл body.json в двух каталогах: в good чистый, в dir с
	// нарушением. Переменная с прежним значением good и присваиванием dir,
	// значение которого видно только при запуске, должна дать отказ
	good := filepath.Join(dir, "good")
	if err := os.Mkdir(good, 0o700); err != nil {
		t.Fatal(err)
	}
	write("good/body.json", `{"body":"Выкатил на stage, проверь вход."}`)
	write("body.json", `{"body":"Готово — выкачено."}`)
	const (
		auth = `curl -s -u "$JIRA_EMAIL:$JIRA_API_TOKEN" -H 'Content-Type: application/json' `
		url  = ` "$JIRA_BASE_URL/rest/api/3/issue/PROJ-1/comment"`
	)
	bad, clean := filepath.Join(dir, "bad.json"), filepath.Join(dir, "clean.json")
	heredoc := func(path, body string) string {
		return "cat > " + path + " <<'JSON'\n" + body + "\nJSON\n"
	}
	cases := []struct {
		name string
		cwd  string
		cmd  string
		want string // "" - пропуск, иначе тип первого нарушения
	}{
		{"-d @абсолютный", "/", auth + "-X POST -d @" + bad + url, "jira_comment_style"},
		{"-d@ слитно", "/", auth + "-d@" + bad + url, "jira_comment_style"},
		{"--json @", "/", auth + "--json @" + bad + url, "jira_comment_style"},
		{"связка -sSd", "/", `curl -sSd @` + bad + url, "jira_comment_style"},
		{"относительный от cwd", dir, auth + "--data-binary @bad.json" + url, "jira_comment_style"},
		{"cd перед curl", "/", "cd " + dir + " && " + auth + "-d @bad.json" + url, "jira_comment_style"},
		{"$(cat файл)", "/", auth + `-X POST -d "$(cat ` + bad + `)"` + url, "jira_comment_style"},
		{"вход из файла", "/", auth + "-X POST -d @-" + url + " < " + bad, "jira_comment_style"},
		{"cat в конвейере", "/", "cat " + bad + " | " + auth + "-X POST -d @-" + url, "jira_comment_style"},
		{"правка /comment/ID", "/", auth + "--request PUT -d @" + bad + ` "$JIRA_BASE_URL/rest/api/3/issue/PROJ-1/comment/10001"`, "jira_comment_style"},
		{"переменная из команды", "/", "S=" + dir + "; " + auth + "-d @$S/bad.json" + url, "jira_comment_style"},
		{"переменная после отправки", "/", auth + "-d @$S/bad.json" + url + "; S=" + dir, "jira_comment_payload"},
		{"переменная в той же подоболочке", "/", "(S=" + dir + "; " + auth + "-d @$S/bad.json" + url + ")", "jira_comment_style"},
		{"переменная из закрытой подоболочки", "/", "(S=" + dir + "); " + auth + "-d @$S/bad.json" + url, "jira_comment_payload"},
		{"переменная из звена конвейера", "/", "S=" + dir + " | true; " + auth + "-d @$S/bad.json" + url, "jira_comment_payload"},
		{"переменная дописана +=", "/", "S=" + filepath.Dir(dir) + "; S+=/" + filepath.Base(dir) + "; " + auth + "-d @$S/bad.json" + url, "jira_comment_style"},
		{"переменная из тела функции", "/", "f() { S=" + dir + "; }; f; " + auth + "-d @$S/bad.json" + url, "jira_comment_payload"},
		{"вызов функции меняет прежнее значение", "/", "S=" + good + "; f() { S=" + dir + "; }; f; " + auth + "-d @$S/body.json" + url, "jira_comment_payload"},
		{"элемент массива поверх прежнего", "/", "S=" + good + "; S[0]=" + dir + "; " + auth + "-d @$S/body.json" + url, "jira_comment_payload"},
		{"переменная for поверх прежней", "/", "S=" + good + "; for S in " + dir + "; do :; done; " + auth + "-d @$S/body.json" + url, "jira_comment_payload"},
		// Тело функции присваивает S: ссылка вне тела после определения не
		// раскрывается, даже если функция не вызвана (её вызовет alias, trap
		// DEBUG/ERR, mapfile -C или другая функция) - цена правила
		{"функция объявлена, не вызвана", "/", "S=" + good + "; f() { S=" + dir + "; }; " + auth + "-d @$S/body.json" + url, "jira_comment_payload"},
		{"command f не вызывает функцию", "/", "S=" + good + "; f() { S=" + dir + "; }; command f; " + auth + "-d @$S/body.json" + url, "jira_comment_payload"},
		{"вызов функции в закрытой подоболочке", "/", "S=" + good + "; f() { S=" + dir + "; }; (f); " + auth + "-d @$S/body.json" + url, "jira_comment_payload"},
		{"функция f() ( … ) не меняет S", "/", "S=" + good + "; f() ( S=" + dir + " ); f; " + auth + "-d @$S/body.json" + url, ""},
		{"функция меняет S в подоболочке тела", "/", "S=" + good + "; f() { (S=" + dir + "); }; f; " + auth + "-d @$S/body.json" + url, ""},
		{"отправка в другой функции после вызова f", "/", "S=" + good + "; f() { S=" + dir + "; }; g() { " + auth + "-d @$S/body.json" + url + "; }; f; g", "jira_comment_payload"},
		{"alias на функцию", "/", "S=" + good + "; f() { S=" + dir + "; }; alias g=f\ng; " + auth + "-d @$S/body.json" + url, "jira_comment_payload"},
		{"trap DEBUG на функцию", "/", "S=" + good + "; f() { S=" + dir + "; }; trap f DEBUG; " + auth + "-d @$S/body.json" + url, "jira_comment_payload"},
		{"trap ERR на функцию", "/", "S=" + good + "; f() { S=" + dir + "; }; trap f ERR; false; " + auth + "-d @$S/body.json" + url, "jira_comment_payload"},
		{"mapfile -C на функцию", "/", "S=" + good + "; f() { S=" + dir + "; }; mapfile -C f -c 1 A <<< x; " + auth + "-d @$S/body.json" + url, "jira_comment_payload"},
		{"lastpipe: последнее звено конвейера", "/", "shopt -s lastpipe; S=" + good + "; true | S=" + dir + "; " + auth + "-d @$S/body.json" + url, "jira_comment_payload"},
		{"без lastpipe звено конвейера закрыто", "/", "S=" + good + "; true | S=" + dir + "; " + auth + "-d @$S/body.json" + url, ""},
		{"отправка в теле функции, присвоено до вызова", "/", "S=" + good + "; g() { " + auth + "-d @$S/body.json" + url + "; }; S=" + dir + "; g", "jira_comment_payload"},
		{"отправка в теле функции, присвоено до определения", "/", "S=" + good + "; g() { " + auth + "-d @$S/body.json" + url + "; }; g", ""},
		{"lastpipe через переменную", "/", "o=lastpipe; shopt -s $o; S=" + good + "; true | S=" + dir + "; " + auth + "-d @$S/body.json" + url, "jira_comment_payload"},
		{"lastpipe из подстановки", "/", `shopt -s "$(echo lastpipe)"; S=` + good + "; true | S=" + dir + "; " + auth + "-d @$S/body.json" + url, "jira_comment_payload"},
		{"lastpipe через eval переменной", "/", `c='shopt -s lastpipe'; eval "$c"; S=` + good + "; true | S=" + dir + "; " + auth + "-d @$S/body.json" + url, "jira_comment_payload"},
		{"lastpipe в теле функции", "/", "f() { shopt -s lastpipe; }; f; S=" + good + "; true | S=" + dir + "; " + auth + "-d @$S/body.json" + url, "jira_comment_payload"},
		{"lastpipe: звено после отправки", "/", "shopt -s lastpipe; S=" + good + "; " + auth + "-d @$S/body.json" + url + "; true | S=" + dir, ""},
		{"lastpipe включён после конвейера", "/", "S=" + good + "; true | S=" + dir + "; shopt -s lastpipe; " + auth + "-d @$S/body.json" + url, ""},
		{"lastpipe включён в подоболочке", "/", "(shopt -s lastpipe); S=" + good + "; true | S=" + dir + "; " + auth + "-d @$S/body.json" + url, ""},
		{"lastpipe: первое звено", "/", "shopt -s lastpipe; S=" + good + "; S=" + dir + " | true; " + auth + "-d @$S/body.json" + url, ""},
		{"lastpipe: конвейер в подоболочке", "/", "shopt -s lastpipe; S=" + good + "; (true | S=" + dir + "); " + auth + "-d @$S/body.json" + url, ""},
		// ${ ; } и ${| ; } (bash 5.3) идут в текущей оболочке: присваивание
		// внутри видно после подстановки
		{"присвоено в ${ ; }", "/", "S=" + good + "; x=${ S=" + dir + "; }; " + auth + "-d @$S/body.json" + url, "jira_comment_style"},
		{"присвоено в ${| ; }", "/", "S=" + good + "; x=${| S=" + dir + "; }; " + auth + "-d @$S/body.json" + url, "jira_comment_style"},
		// Here-doc звена или coproc: команда после ; на строке с <<EOF
		// идёт в текущей оболочке, не в звене
		{"here-doc в первом звене, присвоено после ;", "/", "S=" + good + "; cat <<EOF | cat >/dev/null; S=" + dir + "\nx\nEOF\n" + auth + "-d @$S/body.json" + url, "jira_comment_style"},
		{"here-doc у coproc, присвоено после ;", "/", "S=" + good + "; coproc cat <<EOF; S=" + dir + "\nx\nEOF\n" + auth + "-d @$S/body.json" + url, "jira_comment_style"},
		{"here-doc в звене, присвоено после &&", "/", "S=" + good + "; cat <<EOF | cat >/dev/null && S=" + dir + "\nx\nEOF\n" + auth + "-d @$S/body.json" + url, "jira_comment_style"},
		{"функция присваивает и отправляет, чистый", "/", "f() { S=" + good + "; " + auth + "-d @$S/body.json" + url + "; }; f", ""},
		{"функция присваивает и отправляет, плохой", "/", "f() { S=" + dir + "; " + auth + "-d @$S/body.json" + url + "; }; f", "jira_comment_style"},
		{"в теле вызов другой функции между присваиванием и отправкой", "/", "f() { S=" + good + "; g; " + auth + "-d @$S/body.json" + url + "; }; g() { S=" + dir + "; }; f", "jira_comment_payload"},
		{"trap DEBUG на другую функцию, отправка в теле", "/", "f() { S=" + good + "; " + auth + "-d @$S/body.json" + url + "; }; g() { S=" + dir + "; }; trap g DEBUG; f", "jira_comment_payload"},
		{"присвоено в подоболочке тела, вызов той же функции", "/", "S=" + good + "; f() { (S=" + good + "; :); " + auth + "-d @$S/body.json" + url + "; }; S=" + dir + "; f", "jira_comment_payload"},
		// trap f DEBUG перед curl вернул бы good: тело f присваивает S, ссылка
		// после определения не раскрывается и после прямого присваивания
		{"присвоено после вызова функции", "/", "S=" + good + "; f() { S=" + good + "; }; f; S=" + dir + "; " + auth + "-d @$S/body.json" + url, "jira_comment_payload"},
		{"файл переписан после отправки", "/", auth + "-d @" + bad + url + "; " + heredoc(bad, `{"body":"ok"}`), "jira_comment_style"},
		{"адрес в переменной", "/", `U="$JIRA_BASE_URL/rest/api/3/issue/PROJ-1/comment"; ` + auth + "-d @" + bad + ` "$U"`, "jira_comment_style"},
		{"perl -i поверх старого", "/", "perl -pi -e 's/ — / - /' " + dir + "/stale.json && " + auth + "-d @" + dir + "/stale.json" + url, ""},
		{"perl -M без правки на месте", "/", "perl -Mwarnings -ne 'print' " + bad + " && " + auth + "-d @" + bad + url, "jira_comment_style"},
		{"heredoc с нарушением", "/", heredoc(dir+"/new-bad.json", `{"body":"Готово — выкачено"}`) + auth + "-d @" + dir + "/new-bad.json" + url, "jira_comment_style"},
		{"нераскрытая переменная", "/", auth + "-X POST -d @$S/c.json" + url, "jira_comment_payload"},
		{"файла нет", "/", auth + "-X POST -d @" + dir + "/missing.json" + url, "jira_comment_payload"},
		{"относительный без cwd", "", auth + "-d @clean.json" + url, "jira_comment_payload"},
		{"каталог после cd $X", "/", "cd $X && " + auth + "-d @clean.json" + url, "jira_comment_payload"},
		{"правка задачи, не комментария", "/", auth + "-X PUT -d @" + bad + ` "$JIRA_BASE_URL/rest/api/3/issue/PROJ-1"`, ""},
		{"правка задачи и чтение комментариев", "/", auth + "-X PUT -d @" + bad + ` "$JIRA_BASE_URL/rest/api/3/issue/PROJ-1"; curl -s` + url, ""},
		{"чтение комментариев", "/", auth + "-X GET" + url + ` | grep -c "Готово — выкачено"`, ""},
		{"grep по адресу", "/", `grep -rn "rest/api/3/issue/PROJ-1/comment" docs/ -X POST # заметка — не отправка`, ""},
		{"echo адреса в файл", "/", `echo 'curl -d @x.json $JIRA_BASE_URL/rest/api/3/issue/PROJ-1/comment — пример' > ` + dir + "/note.txt", ""},
		{"чистый файл", "/", auth + "-X POST -d @" + clean + url, ""},
		{"чистый относительный", dir, auth + "-X POST -d @clean.json" + url, ""},
		{"чистый heredoc поверх старого", "/", heredoc(dir+"/stale.json", `{"body":"Выкатил на stage, проверь вход."}`) + auth + "-d @" + dir + "/stale.json" + url, ""},
		{"sed -i поверх старого", "/", "sed -i 's/ — / - /' " + dir + "/stale.json; " + auth + "-d @" + dir + "/stale.json" + url, ""},
		{"jq --rawfile в конвейере", "/", "jq -n --rawfile b " + dir + "/bad.txt '{body:$b}' | " + auth + "-X POST -d @-" + url, "jira_comment_style"},
		{"jq по файлу в конвейере", dir, "jq -c . bad.json | " + auth + "-d @-" + url, "jira_comment_style"},
		{"cat | jq | curl", "/", "cat " + bad + " | jq . | " + auth + "-d @-" + url, "jira_comment_style"},
		{"python с файлом в конвейере", "/", `python3 -c 'import json,sys; print(json.dumps({"body": open(sys.argv[1]).read()}))' ` + dir + "/bad.txt | " + auth + "-d @-" + url, "jira_comment_style"},
		{"jq < файл в конвейере", "/", "jq -c . < " + bad + " | " + auth + "-d @-" + url, "jira_comment_style"},
		{"cat нет файла в конвейере", "/", "cat " + dir + "/missing.json | " + auth + "-d @-" + url, "jira_comment_payload"},
		{"jq без файла-аргумента", "/", "jq -n \"{body:1}\" | " + auth + "-d @-" + url, ""},
		{"< обрывает конвейер", "/", "cat " + bad + " | jq -c . < " + clean + " | " + auth + "-d @-" + url, ""},
		{"чистый jq в конвейере", "/", "jq -c . " + clean + " | " + auth + "-d @-" + url, ""},
		{"--upload-file", "/", auth + "-X POST --upload-file " + bad + url, "jira_comment_style"},
		{"-T на /comment/ID", "/", auth + "-T " + bad + ` "$JIRA_BASE_URL/rest/api/3/issue/PROJ-1/comment/10001"`, "jira_comment_style"},
		{"генератор в конвейере", "/", `printf '{"body":"Выкатил на stage"}' | ` + auth + "-d @-" + url, ""},
		{"--data-urlencode не тело", "/", auth + "--data-urlencode q@" + dir + "/missing.json" + url, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := validateJiraCommandIn(t, tc.cwd, tc.cmd)
			got := ""
			if !result.IsValid && len(result.Violations) > 0 {
				got = result.Violations[0].Type
			}
			if got != tc.want || result.IsValid != (tc.want == "") {
				t.Fatalf("нарушение %q (пропуск %v), ожидалось %q: %v", got, result.IsValid, tc.want, result.Suggestions)
			}
		})
	}
}

// Нераскрытый файл называется первым сообщением, маркеры видимой части
// тела идут подсказками следом
func TestJiraStyleNamesUnreadablePayloadFirst(t *testing.T) {
	result := validateJiraCommandIn(t, "/",
		`curl -X POST -d @$S/c.json -d '{"body":"Готово — выкачено"}' "$JIRA_BASE_URL/rest/api/3/issue/PROJ-1/comment"`)
	if result.IsValid || len(result.Violations) < 2 {
		t.Fatalf("ожидался отказ и по файлу, и по маркеру: %+v", result.Violations)
	}
	if result.Violations[0].Type != "jira_comment_payload" || !strings.Contains(result.Violations[0].Suggestion, "$S/c.json") {
		t.Fatalf("первым должен быть нераскрытый файл: %+v", result.Violations[0])
	}
	if !strings.Contains(strings.Join(result.Suggestions, "\n"), "длинное тире") {
		t.Fatalf("маркер видимой части должен остаться в подсказках: %v", result.Suggestions)
	}
}

// testLogger — логгер для тестов пакета: пишет в stderr, который go test
// показывает только при провале
func testLogger(t *testing.T) core.Logger {
	t.Helper()
	logger, err := core.NewLogger(core.LoggerConfig{Level: "debug"})
	if err != nil {
		t.Fatalf("не удалось создать логгер: %v", err)
	}
	return logger
}
