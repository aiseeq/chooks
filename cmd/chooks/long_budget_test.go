package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/chooks/internal/hookctx"
)

// Срок запроса (--timeout) в базе ограничивал только pre-tool-use: у
// understand-check и webfetch-fallback бюджет свой (understandBudget,
// webfallback.Budget). Медленный webtext и медленный судья дольше срока
// запроса дают тот же ответ, что быстрые
func TestLongBudgetHooksOutliveRequestTimeout(t *testing.T) {
	verdict, err := json.Marshal(map[string]any{"verdict": "fix", "fixes": []map[string]string{{"quote": "q", "action": "a", "why": "w", "fix": "f"}}})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := json.Marshal(map[string]any{"result": string(verdict), "is_error": false, "usage": map[string]int{}})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, hook, stub, body, input, want string
	}{
		{name: "webtext", hook: hookWebFetchFallback, stub: "webtext",
			body:  "#!/bin/sh\nsleep 0.4\nprintf 'страница целиком' > \"$PWD/page.txt\"\nprintf '%s\\n' '{\"ok\":true,\"source\":\"live\",\"kind\":\"text\",\"path\":\"page.txt\"}'\n",
			input: `{"session_id":"s1","cwd":"@W@","hook_event_name":"PostToolUseFailure","tool_name":"WebFetch","tool_input":{"url":"https://example.org/"},"error":"boom"}`,
			want:  "Страницу открыл webtext"},
		{name: "judge", hook: hookUnderstandCheck, stub: "claude",
			body:  "#!/bin/sh\ncat >/dev/null\nsleep 0.4\nprintf '%s\\n' '" + string(reply) + "'\n",
			input: `{"session_id":"s1","cwd":"@W@","transcript_path":"@W@/t.jsonl","hook_event_name":"PreToolUse","tool_name":"SendMessage","tool_input":{"to":"peter","message":"встреча в субботу"}}`,
			want:  `"permissionDecision":"deny"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			home, work := filepath.Join(root, "home"), filepath.Join(root, "work")
			files := map[string]string{
				home + "/.config/chooks/config.yaml": "logger:\n  level: info\n  output: stderr\ntools:\n  webfetch_fallback: {enabled: true}\n  notifier: {enabled: false}\n  skill_reload: {enabled: false}\n",
				home + "/.config/models/approved":    "opus claude-opus-5-5\n",
				home + "/bin/" + c.stub:              c.body,
				work + "/t.jsonl":                    `{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":"напиши Пете, что встреча в пятницу"},"uuid":"u1","timestamp":"2026-11-22T16:00:00Z","origin":{"kind":"human"},"sessionId":"s1"}` + "\n",
			}
			for path, body := range files {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(filepath.Join(root, "run"), 0o700); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			env := []string{"HOME=" + home, "XDG_CONFIG_HOME=" + home + "/.config", "XDG_RUNTIME_DIR=" + root + "/run",
				"PATH=" + home + "/bin:/usr/bin:/bin", "LANG=C.UTF-8", "TZ=UTC"}
			req := hookctx.New(strings.NewReader(strings.ReplaceAll(c.input, "@W@", work)), &stdout, &stderr, env, work, os.Getpid(), os.Getppid())
			req.Timeout = 50 * time.Millisecond
			if code := executeHook(context.Background(), req, c.hook); code != exitAllowed {
				t.Fatalf("code %d, stderr %s", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), c.want) {
				t.Fatalf("stdout %q, want %q: the request timeout cut the hook budget; stderr %s", stdout.String(), c.want, stderr.String())
			}
		})
	}
}
