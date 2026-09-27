package asset

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePlan(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plan.csv")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// The plan is the file "asset list" exported, with new_file filled in and whatever
// else the user kept or added. Columns are found by name, extras are ignored.
func TestReadPlan(t *testing.T) {
	path := writePlan(t, `id,name,type,capture_date,size,server_path,new_file
aaa,one.mp4,VIDEO,2013-05-04T10:00:00+02:00,12,/srv/one.mp4,out/one_left.mp4
bbb,two.mp4,VIDEO,2014-05-04T10:00:00+02:00,13,/srv/two.mp4,
ccc,three.mp4,VIDEO,2015-05-04T10:00:00+02:00,14,/srv/three.mp4,out/three_left.mp4
`)

	rows, err := readPlan(path)
	if err != nil {
		t.Fatalf("readPlan: %v", err)
	}
	// The row with an empty new_file is skipped, not an error.
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].assetID != "aaa" || rows[1].assetID != "ccc" {
		t.Errorf("got ids %q and %q, want aaa and ccc", rows[0].assetID, rows[1].assetID)
	}
	// Line numbers are the ones of the file, so an error points at the right row.
	if rows[0].line != 2 || rows[1].line != 4 {
		t.Errorf("got lines %d and %d, want 2 and 4", rows[0].line, rows[1].line)
	}
	if !filepath.IsAbs(rows[0].file) {
		t.Errorf("the path was not resolved: %q", rows[0].file)
	}
	if !strings.HasSuffix(rows[0].file, filepath.Join("out", "one_left.mp4")) {
		t.Errorf("got %q, want it to end with out/one_left.mp4", rows[0].file)
	}
}

func TestReadPlanColumnOrderAndExtras(t *testing.T) {
	path := writePlan(t, `new_file, whatever ,ID,filename
out/a.mp4,noise,aaa,keep-this-name.mp4
`)
	rows, err := readPlan(path)
	if err != nil {
		t.Fatalf("readPlan: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].assetID != "aaa" {
		t.Errorf("id = %q, want aaa", rows[0].assetID)
	}
	if rows[0].filename != "keep-this-name.mp4" {
		t.Errorf("filename = %q, want keep-this-name.mp4", rows[0].filename)
	}
}

func TestReadPlanRejects(t *testing.T) {
	for _, tc := range []struct {
		name, content, wants string
	}{
		{"no id column", "name,new_file\none.mp4,out/a.mp4\n", `"id" column`},
		{"no new_file column", "id,name\naaa,one.mp4\n", `"new_file" column`},
		{"empty id", "id,new_file\n,out/a.mp4\n", "line 2"},
		{"nothing to do", "id,new_file\naaa,\n", "no row"},
		{"empty file", "", "empty"},
		// Two rows on the same asset would replace it twice, the second time against
		// an asset that is already in the trash.
		{"duplicate asset", "id,new_file\naaa,out/a.mp4\naaa,out/b.mp4\n", "lines 2 and 3"},
		{"duplicate file", "id,new_file\naaa,out/a.mp4\nbbb,out/a.mp4\n", "lines 2 and 3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readPlan(writePlan(t, tc.content))
			if err == nil {
				t.Fatal("readPlan accepted it")
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error %q doesn't mention %q", err, tc.wants)
			}
		})
	}
}

func TestWriteResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.csv")
	results := []planResult{
		{row: &planRow{assetID: "aaa", file: "/out/a.mp4"}, newID: "new-aaa"},
		{row: &planRow{assetID: "bbb", file: "/out/b.mp4"}, err: os.ErrNotExist},
	}
	if err := writeResults(path, results); err != nil {
		t.Fatalf("writeResults: %v", err)
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
	if len(records) != 3 {
		t.Fatalf("got %d records, want a header and two rows", len(records))
	}
	if got, want := records[1], []string{"aaa", "new-aaa", "/out/a.mp4", "ok", ""}; !equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if records[2][3] != "failed" || records[2][4] == "" {
		t.Errorf("the failed row lost its status or message: %v", records[2])
	}

	// A previous record of what happened must not be overwritten.
	if err := writeResults(path, results); err == nil {
		t.Error("writeResults clobbered an existing file")
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
