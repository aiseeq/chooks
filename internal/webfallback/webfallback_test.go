package webfallback

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Текст отказа WebFetch 2.1.310
const forbidden = "The server returned HTTP 403 Forbidden.\n\nThe response body was not retrieved."

func TestReason(t *testing.T) {
	tests := []struct {
		name  string
		fetch Fetch
		want  string
	}{
		{name: "403", fetch: Fetch{Code: 403, Result: forbidden}, want: "HTTP 403"},
		{name: "429", fetch: Fetch{Code: 429}, want: "HTTP 429"},
		{name: "202 — пустая проверка AWS WAF", fetch: Fetch{Code: 202, Result: "The web page content you provided is empty"}, want: "HTTP 202"},
		{name: "404 — не отказ", fetch: Fetch{Code: 404}},
		{name: "обычная страница", fetch: Fetch{Code: 200, Result: "**Key facts**\n- ePayments Systems Ltd is a UK electronic money institution"}},
		{
			name:  "заглушка Anubis",
			fetch: Fetch{Code: 200, Result: "The page you provided doesn't contain the survey. It's an access-denied page from Anubis, a bot-protection system"},
			want:  "страница проверки",
		},
		{
			name:  "заглушка reCAPTCHA по-русски",
			fetch: Fetch{Code: 200, Result: "Загруженная страница не содержит материала. Это заглушка проверки браузера reCAPTCHA с текстом"},
			want:  "страница проверки",
		},
		{
			// Документация Claude Code: «Cloudflare» и «captcha» в глубине текста
			name:  "слово проверки далеко от начала",
			fetch: Fetch{Code: 200, Result: strings.Repeat("Configure cloud environments. ", 20) + "Cloudflare challenge, captcha"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Reason(tt.fetch); got != tt.want {
				t.Errorf("Reason = %q, ожидалось %q", got, tt.want)
			}
		})
	}
}

// fakeRunner запоминает аргументы и отвечает заданным выводом
type fakeRunner struct {
	args   []string
	stdout string
	code   int
	err    error
}

func (f *fakeRunner) run(_ context.Context, args []string) ([]byte, int, error) {
	f.args = args
	return []byte(f.stdout), f.code, f.err
}

func writeText(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "page.txt")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFallbackOpened(t *testing.T) {
	path := writeText(t, "Welcome address\nsource: https://web.archive.org/web/20260208211225/https://www.lb.lt/x\n\nТекст речи.")
	runner := &fakeRunner{stdout: `{"ok":true,"source":"wayback","snapshot":"20260208211225","kind":"html","path":"` + path + `"}`}
	fetch := Fetch{Code: 403, CodeText: "Forbidden", Result: forbidden, URL: "https://www.lb.lt/x", DurationMs: 314}

	updated, outcome, err := Fallback(context.Background(), fetch, runner.run)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(runner.args, []string{"--json", "--timeout", "45s", "https://www.lb.lt/x"}) {
		t.Errorf("аргументы webtext: %q", runner.args)
	}
	if !outcome.Opened || outcome.Source != "wayback" {
		t.Errorf("исход: %+v", outcome)
	}
	for _, want := range []string{"HTTP 403 Forbidden", "wayback 20260208211225, html", "Промпт WebFetch к тексту не применён", "Текст речи."} {
		if !strings.Contains(updated.Result, want) {
			t.Errorf("в ответе нет %q:\n%s", want, updated.Result)
		}
	}
	if updated.URL != fetch.URL || updated.Code != 403 || updated.DurationMs != 314 {
		t.Errorf("поля ответа WebFetch, кроме текста, должны остаться: %+v", updated)
	}
}

func TestFallbackTruncatesLongPage(t *testing.T) {
	path := writeText(t, strings.Repeat("я", resultRunes+500))
	runner := &fakeRunner{stdout: `{"ok":true,"source":"direct","kind":"html","path":"` + path + `"}`}

	updated, _, err := Fallback(context.Background(), Fetch{Code: 403, URL: "https://example.org/a"}, runner.run)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(updated.Result, "первые 30000 из 30500 символов, целиком — "+path) {
		t.Errorf("нет пометки об обрезке: %.300s", updated.Result)
	}
	if got := strings.Count(updated.Result, "я"); got != resultRunes {
		t.Errorf("текста %d символов, ожидалось %d", got, resultRunes)
	}
}

// После 429 сайт второй раз не спрашивается, отчёт о неудаче дописывается к
// ответу WebFetch
func TestFallbackRateLimitedNotOpened(t *testing.T) {
	runner := &fakeRunner{code: 1, stdout: `{"ok":false,"steps":[{"step":"wayback","result":"no 200 snapshot"}],"hint":"Найди копию через WebSearch по заголовку."}`}
	original := "The server returned HTTP 429 Too Many Requests."

	updated, outcome, err := Fallback(context.Background(), Fetch{Code: 429, Result: original, URL: "https://liquipedia.net/starcraft2/X"}, runner.run)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(runner.args, "--no-direct") {
		t.Errorf("после 429 нужен --no-direct: %q", runner.args)
	}
	if outcome.Opened {
		t.Error("страница не открыта, а исход Opened")
	}
	want := original + "\n\nwebtext тоже не открыл страницу (wayback: no 200 snapshot). Найди копию через WebSearch по заголовку."
	if updated.Result != want {
		t.Errorf("ответ:\n%s\nожидался:\n%s", updated.Result, want)
	}
}

func TestFallbackLeavesResponse(t *testing.T) {
	tests := []struct {
		name    string
		fetch   Fetch
		runner  fakeRunner
		wantErr bool
	}{
		{name: "обычный ответ — утилита не зовётся", fetch: Fetch{Code: 200, Result: "текст", URL: "https://a.org"}},
		{name: "нет утилиты", fetch: Fetch{Code: 403, URL: "https://a.org"}, runner: fakeRunner{err: ErrNoWebtext}, wantErr: true},
		{name: "поломка утилиты", fetch: Fetch{Code: 403, URL: "https://a.org"}, runner: fakeRunner{code: 2, stdout: "webtext: bad url"}, wantErr: true},
		{name: "нечитаемый вывод", fetch: Fetch{Code: 403, URL: "https://a.org"}, runner: fakeRunner{stdout: "not json"}, wantErr: true},
		{name: "файла нет", fetch: Fetch{Code: 403, URL: "https://a.org"}, runner: fakeRunner{stdout: `{"ok":true,"path":"/nonexistent/x.txt"}`}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			updated, _, err := Fallback(context.Background(), tt.fetch, tt.runner.run)
			if updated != nil {
				t.Errorf("ответ должен остаться как был, а подменён: %+v", updated)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("ошибка %v, ожидалась: %v", err, tt.wantErr)
			}
			if tt.fetch.Code == 200 && tt.runner.args != nil {
				t.Error("на обычный ответ утилита не должна зваться")
			}
		})
	}
	if _, _, err := Fallback(context.Background(), Fetch{Code: 403, URL: "https://a.org"}, (&fakeRunner{err: ErrNoWebtext}).run); !errors.Is(err, ErrNoWebtext) {
		t.Errorf("нет утилиты — ErrNoWebtext, а %v", err)
	}
}

func TestLogURLDropsQuery(t *testing.T) {
	if got := LogURL("https://api.example.org/v1/x?token=secret#frag"); got != "api.example.org/v1/x" {
		t.Errorf("LogURL = %q", got)
	}
}

// Текст отказа домена самим Claude Code (WebFetch 31.10)
const domainBlocked = "Claude Code is unable to fetch from www.reddit.com"

func TestFailureContextOpened(t *testing.T) {
	path := writeText(t, "Thread\nsource: wayback\n\nТекст обсуждения.")
	runner := &fakeRunner{stdout: `{"ok":true,"source":"wayback","snapshot":"20250801092245","kind":"html","path":"` + path + `"}`}

	text, outcome, err := FailureContext(context.Background(), "https://www.reddit.com/r/x/comments/1/", domainBlocked, runner.run)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(runner.args, []string{"--json", "--timeout", "45s", "https://www.reddit.com/r/x/comments/1/"}) {
		t.Errorf("аргументы webtext: %q", runner.args)
	}
	if !outcome.Opened || outcome.Source != "wayback" || outcome.Reason != domainBlocked {
		t.Errorf("исход: %+v", outcome)
	}
	// Дата снимка видна агенту: архив бывает старым
	for _, want := range []string{"WebFetch упал (" + domainBlocked + ")", "wayback 20250801092245, html", "Промпт WebFetch к тексту не применён", "Текст обсуждения."} {
		if !strings.Contains(text, want) {
			t.Errorf("в контексте нет %q:\n%s", want, text)
		}
	}
}

// Весь контекст с длинной ошибкой, заголовком и пометкой — в пределе Claude
// Code, считая UTF-16: эмодзи занимают две единицы
func TestFailureContextFitsHookCap(t *testing.T) {
	page := strings.Repeat("😀я", 8000)
	path := writeText(t, page)
	runner := &fakeRunner{stdout: `{"ok":true,"source":"direct","kind":"html","path":"` + path + `"}`}
	longErr := strings.Repeat("certificate chain error ", 100) + "\nвторая строка"

	text, outcome, err := FailureContext(context.Background(), "https://example.org/a", longErr, runner.run)
	if err != nil {
		t.Fatal(err)
	}
	if got := utf16Len(text); got > 10000 {
		t.Errorf("контекст %d единиц UTF-16, предел Claude Code 10000", got)
	}
	if got := len([]rune(outcome.Reason)); got > errorRunes+1 || strings.Contains(outcome.Reason, "вторая") {
		t.Errorf("ошибка в заголовке не обрезана: %d символов, %q", got, outcome.Reason)
	}
	if !strings.Contains(text, "из 16000 символов, целиком — "+path) {
		t.Errorf("нет пометки об обрезке: %.400s", text)
	}
	if !strings.HasSuffix(text, "😀я") && !strings.HasSuffix(text, "😀") {
		t.Errorf("текст обрезан посреди символа: %q", text[len(text)-8:])
	}
}

func TestFailureContextNotOpened(t *testing.T) {
	runner := &fakeRunner{code: 1, stdout: `{"ok":false,"steps":[{"step":"direct","result":"error: tls: failed to verify certificate"},{"step":"wayback","result":"no snapshot with status 200"}]}`}

	text, outcome, err := FailureContext(context.Background(), "https://developer.bank.example/docs/x", "self signed certificate in certificate chain", runner.run)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Opened {
		t.Error("страница не открыта, а исход Opened")
	}
	want := "WebFetch упал (self signed certificate in certificate chain). webtext тоже не открыл страницу " +
		"(direct: error: tls: failed to verify certificate; wayback: no snapshot with status 200)."
	if text != want {
		t.Errorf("контекст:\n%s\nожидался:\n%s", text, want)
	}
}

// WebFetch намеренно не открывает локальные адреса — хук за него тоже
func TestFailureContextSkipsLocal(t *testing.T) {
	for _, address := range []string{
		"http://localhost:8080/admin", "http://127.0.0.1:8080/admin", "http://intranet/wiki",
		"http://10.1.2.3/", "http://192.168.0.1/", "http://[::1]/", "http://[fe80::1]/", "http://169.254.169.254/latest/meta-data",
		"http://0.0.0.0/",
		// Обходы, найденные приёмкой 01.11: точка в конце, зона localhost,
		// сокращённый IP, CGNAT, имя локальной зоны
		"http://localhost./", "http://app.localhost/", "http://127.1/", "http://0x7f.0.0.1/",
		"http://100.64.0.1/", "http://router.lan/", "http://nas.local/", "http://[::ffff:127.0.0.1]/",
	} {
		runner := &fakeRunner{}
		_, _, err := FailureContext(context.Background(), address, "fetch failed", runner.run)
		if !errors.Is(err, ErrLocalAddress) {
			t.Errorf("%s: ожидался ErrLocalAddress, а %v", address, err)
		}
		if runner.args != nil {
			t.Errorf("%s: webtext вызван на локальный адрес", address)
		}
	}
	for _, address := range []string{"https://132.70.196.109/x", "https://www.reddit.com./r/x", "https://3dnews.ru/x", "https://a.b.io/"} {
		runner := &fakeRunner{code: 1, stdout: `{"ok":false}`}
		if _, _, err := FailureContext(context.Background(), address, "connect ECONNREFUSED", runner.run); err != nil || runner.args == nil {
			t.Errorf("%s: публичный адрес должен идти в webtext: err %v, вызван %v", address, err, runner.args != nil)
		}
	}
}
