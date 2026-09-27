package asset

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/simulot/immich-go/immich"
)

// Columns read from a plan. Everything else the file carries — the ones "asset
// list" exports, or any the user added — is ignored, so the exported file can be
// handed back after filling new_file in.
const (
	colID       = "id"
	colNewFile  = "new_file"
	colFilename = "filename"
)

// planRow is one asset to swap, as read from the CSV.
type planRow struct {
	line     int // Line in the CSV, for error messages
	assetID  string
	file     string // Path of the converted file, absolute
	filename string // Optional per-row override of the name given to the server

	source *immich.Asset // Filled once the row is checked against the server
	skip   string        // Non-empty when the check found nothing to do for this row
}

// readPlan reads the rows to work on. Columns are looked up by name, so the order
// doesn't matter. A row with an empty new_file is skipped, which lets a subset be
// processed by clearing the cells of the others.
func readPlan(path string) ([]*planRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1 // a hand-edited file may have ragged rows
	r.TrimLeadingSpace = true

	header, err := r.Read()
	if err == io.EOF {
		return nil, fmt.Errorf("%s is empty", path)
	}
	if err != nil {
		return nil, fmt.Errorf("can't read %s: %w", path, err)
	}

	columns := map[string]int{}
	for i, name := range header {
		columns[strings.ToLower(strings.TrimSpace(name))] = i
	}
	for _, needed := range []string{colID, colNewFile} {
		if _, ok := columns[needed]; !ok {
			return nil, fmt.Errorf("%s has no %q column, it has: %s", path, needed, strings.Join(header, ", "))
		}
	}

	field := func(record []string, name string) string {
		i, ok := columns[name]
		if !ok || i >= len(record) {
			return ""
		}
		return strings.TrimSpace(record[i])
	}

	var (
		rows     []*planRow
		seenID   = map[string]int{}
		seenFile = map[string]int{}
	)
	for line := 2; ; line++ {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("can't read %s: %w", path, err)
		}

		newFile := field(record, colNewFile)
		if newFile == "" {
			continue // nothing to upload for this asset
		}
		id := field(record, colID)
		if id == "" {
			return nil, fmt.Errorf("%s line %d: %s is set but %s is empty", path, line, colNewFile, colID)
		}

		abs, err := filepath.Abs(newFile)
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, line, err)
		}

		// Two rows pointing at the same asset, or at the same file, are always a
		// mistake, and a costly one once the source assets start being trashed.
		if first, ok := seenID[id]; ok {
			return nil, fmt.Errorf("%s: the asset %s appears on lines %d and %d", path, id, first, line)
		}
		if first, ok := seenFile[abs]; ok {
			return nil, fmt.Errorf("%s: the file %s appears on lines %d and %d", path, newFile, first, line)
		}
		seenID[id], seenFile[abs] = line, line

		rows = append(rows, &planRow{
			line:     line,
			assetID:  id,
			file:     abs,
			filename: field(record, colFilename),
		})
	}

	if len(rows) == 0 {
		return nil, fmt.Errorf("%s has no row with a %s to upload", path, colNewFile)
	}
	return rows, nil
}

// planResult is what happened to one row, reported back as a CSV.
type planResult struct {
	row     *planRow
	newID   string
	skipped string // Non-empty when the row needed nothing done, with the reason
	err     error
}

var resultColumns = []string{"id", "new_id", "new_file", "status", "detail"}

func writeResults(path string, results []planResult) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck

	w := csv.NewWriter(f)
	if err = w.Write(resultColumns); err != nil {
		return err
	}
	for _, r := range results {
		status, message := "ok", ""
		switch {
		case r.err != nil:
			status, message = "failed", r.err.Error()
		case r.skipped != "":
			status, message = "skipped", r.skipped
		}
		if err = w.Write([]string{r.row.assetID, r.newID, r.row.file, status, message}); err != nil {
			return err
		}
	}
	w.Flush()
	if err = w.Error(); err != nil {
		return err
	}
	return f.Close()
}
