package hookctx

import (
	"archive/zip"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// $ZONEINFO и TZ=Local запроса демона: пояс — тот, что time.LoadLocation и
// time.initLocal дали бы процессу хука с этим env, а не env демона
func TestLocationRequestZoneinfo(t *testing.T) {
	data, err := os.ReadFile("/usr/share/zoneinfo/Asia/Kathmandu")
	if err != nil {
		t.Fatal("нет системной базы поясов:", err)
	}
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "zi", "Test"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "zi", "Test", "Zone"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	archive, err := os.Create(filepath.Join(cwd, "zi.zip"))
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(archive)
	entry, err := writer.CreateHeader(&zip.FileHeader{Name: "Test/Zone", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	at := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		env    []string
		offset int
	}{
		{[]string{"TZ=Test/Zone", "ZONEINFO=zi"}, 5*3600 + 45*60},
		{[]string{"TZ=Test/Zone", "ZONEINFO=" + filepath.Join(cwd, "zi.zip")}, 5*3600 + 45*60},
		{[]string{"TZ=Test/Zone"}, 0},
		{[]string{"TZ=Asia/Kathmandu", "ZONEINFO=missing"}, 5*3600 + 45*60},
	} {
		request := New(nil, nil, nil, tc.env, cwd, 1, 2)
		if _, offset := at.In(request.Location()).Zone(); offset != tc.offset {
			t.Errorf("env %q: offset %d, want %d", tc.env, offset, tc.offset)
		}
	}
	if _, err := os.Stat("/usr/share/zoneinfo/Local"); os.IsNotExist(err) {
		request := New(nil, nil, nil, []string{"TZ=Local"}, cwd, 1, 2)
		if loc := request.Location(); loc != time.UTC {
			t.Errorf("TZ=Local: %v, want UTC as time.initLocal without a Local zone file", loc)
		}
	}
}

// Без TZ: у CLI — time.Local процесса (его и взял бы хук), у демона —
// /etc/localtime, как time.initLocal процесса хука без TZ
func TestLocationWithoutTZ(t *testing.T) {
	cli := Context{process: true}
	if loc := cli.Location(); loc != time.Local {
		t.Errorf("CLI without TZ: %v, want time.Local", loc)
	}
	data, err := os.ReadFile("/etc/localtime")
	if err != nil {
		t.Skip("нет /etc/localtime:", err)
	}
	want, err := time.LoadLocationFromTZData("", data)
	if err != nil {
		t.Fatal(err)
	}
	// Пояс исполнителя — не /etc/localtime: запрос без TZ не должен его брать
	executor := time.Local
	time.Local = time.FixedZone("EXECUTOR", 12345)
	t.Cleanup(func() { time.Local = executor })
	daemon := New(nil, nil, nil, nil, "/", 1, 2)
	for _, at := range []time.Time{time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC), time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)} {
		_, wantOffset := at.In(want).Zone()
		if _, offset := at.In(daemon.Location()).Zone(); offset != wantOffset {
			t.Errorf("request without TZ at %v: offset %d, want %d of /etc/localtime", at, offset, wantOffset)
		}
	}
}

// $ZONEINFO исполнителя запросу не принадлежит: запрос без своего ZONEINFO
// берёт пояс из системной базы, как процесс хука без $ZONEINFO. Пакет time
// читает $ZONEINFO один раз за процесс, поэтому проверка идёт в свежем
// процессе теста с $ZONEINFO исполнителя с самого старта
func TestLocationIgnoresExecutorZoneinfo(t *testing.T) {
	if dir := os.Getenv("CHOOKS_EXECUTOR_ZONEINFO_CHILD"); dir != "" {
		checkExecutorZoneinfo(t, dir)
		return
	}
	dir := t.TempDir()
	utc, err := os.ReadFile("/usr/share/zoneinfo/UTC")
	if err != nil {
		t.Fatal(err)
	}
	nepal, err := os.ReadFile("/usr/share/zoneinfo/Asia/Kathmandu")
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"America/New_York": utc, "Chooks/ExecutorOnly": nepal} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLocationIgnoresExecutorZoneinfo$", "-test.count=1")
	cmd.Env = append(os.Environ(), "ZONEINFO="+dir, "CHOOKS_EXECUTOR_ZONEINFO_CHILD="+dir)
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "executor zoneinfo checked") {
		t.Fatalf("child: %v\n%s", err, out)
	}
}

func checkExecutorZoneinfo(t *testing.T, dir string) {
	// Пояс есть и в системной базе: берётся оттуда (-5 ч зимой), не из
	// подделки исполнителя (UTC)
	req := New(nil, nil, nil, []string{"TZ=America/New_York"}, dir, 1, 2)
	if _, offset := time.Date(2026, 1, 1, 12, 0, 0, 0, req.Location()).Zone(); offset != -18000 {
		t.Fatalf("request TZ=America/New_York without ZONEINFO: offset %d, want -18000: executor $ZONEINFO used", offset)
	}
	// Пояса нет нигде, кроме $ZONEINFO исполнителя: у процесса хука его нет
	// вовсе, и пояс — UTC
	req = New(nil, nil, nil, []string{"TZ=Chooks/ExecutorOnly"}, dir, 1, 2)
	if _, offset := time.Date(2026, 1, 1, 12, 0, 0, 0, req.Location()).Zone(); offset != 0 {
		t.Fatalf("request TZ=Chooks/ExecutorOnly found only in executor $ZONEINFO: offset %d, want 0 (UTC)", offset)
	}
	fmt.Println("executor zoneinfo checked")
}
