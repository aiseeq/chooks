package hookctx

import (
	"archive/zip"
	"os"
	"path/filepath"
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
	daemon := New(nil, nil, nil, nil, "/", 1, 2)
	for _, at := range []time.Time{time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC), time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)} {
		_, wantOffset := at.In(want).Zone()
		if _, offset := at.In(daemon.Location()).Zone(); offset != wantOffset {
			t.Errorf("request without TZ at %v: offset %d, want %d of /etc/localtime", at, offset, wantOffset)
		}
	}
}
