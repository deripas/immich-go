package asset

import (
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/simulot/immich-go/immich"
)

func TestServerPathRewrite(t *testing.T) {
	a := &immich.Asset{OriginalPath: "/usr/src/app/upload/upload/user/ab/cd/asset.mp4"}

	// Without the prefixes the path is reported as the server sees it.
	lc := &listCmd{}
	if got, want := lc.serverPath(a), "/usr/src/app/upload/upload/user/ab/cd/asset.mp4"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	lc = &listCmd{ServerPathPrefix: "/usr/src/app/upload", LocalPathPrefix: "/mnt/tank/immich"}
	if got, want := lc.serverPath(a), "/mnt/tank/immich/upload/user/ab/cd/asset.mp4"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// An external library sits outside the upload folder: leave it alone.
	other := &immich.Asset{OriginalPath: "/library/external/asset.mp4"}
	if got, want := lc.serverPath(other), "/library/external/asset.mp4"; got != want {
		t.Errorf("got %q, want the path untouched %q", got, want)
	}
}

func TestExportCSV(t *testing.T) {
	const body = `{
		"id": "8b2c1d3e-0000-4000-8000-000000000001",
		"originalFileName": "stereo.mp4",
		"type": "VIDEO",
		"checksum": "Wg8B1w==",
		"originalPath": "/usr/src/app/upload/upload/u/ab/cd/stereo.mp4",
		"localDateTime": "2013-05-04T10:00:00.000Z",
		"exifInfo": {
			"timeZone": "Europe/Paris",
			"exifImageWidth": 3840,
			"exifImageHeight": 1080,
			"fileSizeInByte": 123456789
		}
	}`

	var a immich.Asset
	if err := json.Unmarshal([]byte(body), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	path := filepath.Join(t.TempDir(), "plan.csv")
	lc := &listCmd{
		Export:           path,
		ServerPathPrefix: "/usr/src/app/upload",
		LocalPathPrefix:  "/mnt/tank/immich",
	}
	if err := lc.exportCSV([]*immich.Asset{&a}); err != nil {
		t.Fatalf("exportCSV: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("got %d records, want a header and one row", len(records))
	}

	header, row := records[0], records[1]
	if len(header) != len(csvColumns) {
		t.Fatalf("header has %d columns, want %d", len(header), len(csvColumns))
	}
	got := map[string]string{}
	for i, name := range header {
		got[name] = row[i]
	}

	for _, tc := range []struct{ column, want string }{
		{"id", "8b2c1d3e-0000-4000-8000-000000000001"},
		{"name", "stereo.mp4"},
		{"type", "VIDEO"},
		{"capture_date", "2013-05-04T10:00:00+02:00"},
		{"timeline_date", "2013-05-04T10:00:00Z"},
		{"width", "3840"},
		{"height", "1080"},
		{"size", "123456789"},
		{"checksum", "Wg8B1w=="},
		{"server_path", "/mnt/tank/immich/upload/u/ab/cd/stereo.mp4"},
		{"new_file", ""},
	} {
		if got[tc.column] != tc.want {
			t.Errorf("%s = %q, want %q", tc.column, got[tc.column], tc.want)
		}
	}
}

// The export is meant to be filled in by hand afterwards, so a second run must not
// silently overwrite the work.
func TestExportCSVRefusesToClobber(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.csv")
	if err := os.WriteFile(path, []byte("mine\n"), 0o600); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	lc := &listCmd{Export: path}
	if err := lc.exportCSV(nil); err == nil {
		t.Fatal("exportCSV overwrote an existing file")
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(content) != "mine\n" {
		t.Errorf("the file was modified: %q", content)
	}
}
