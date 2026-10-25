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
