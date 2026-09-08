package asset

import (
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/simulot/immich-go/immich"
)

// The server hands the capture date back converted to the caller's zone. Whatever
// the machine running immich-go is set to, the date sent back must describe the
// same wall clock, or the asset moves to another hour — and sometimes another day.
func TestCaptureDateOfIsIndependentOfTheLocalZone(t *testing.T) {
	const body = `{
		"id": "8b2c1d3e-0000-4000-8000-000000000001",
		"localDateTime": "2013-05-04T10:00:00.000Z",
		"fileCreatedAt": "2013-05-04T08:00:00.000Z",
		"exifInfo": {
			"dateTimeOriginal": "2013-05-04T10:00:00.000+02:00",
			"timeZone": "Europe/Paris"
		}
	}`

	for _, zone := range []string{"UTC", "Europe/Moscow", "America/Los_Angeles", "Asia/Kathmandu"} {
		t.Run(zone, func(t *testing.T) {
			loc, err := time.LoadLocation(zone)
			if err != nil {
				t.Skipf("zone %s unavailable: %v", zone, err)
			}
			saved := time.Local
			time.Local = loc
			defer func() { time.Local = saved }()

			var a immich.Asset
			if err := json.Unmarshal([]byte(body), &a); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			got := captureDateOf(&a)

			// The wall clock and the offset of the original shot, both preserved.
			if want := "2013-05-04T10:00:00+02:00"; got.Format(time.RFC3339) != want {
				t.Errorf("RFC3339 = %q, want %q", got.Format(time.RFC3339), want)
			}
			// The same instant as the one the server holds.
			if want := time.Date(2013, 5, 4, 8, 0, 0, 0, time.UTC); !got.Equal(want) {
				t.Errorf("instant = %v, want %v", got.UTC(), want)
			}
			// immich.TimeFormat ends with a literal "Z", so it prints the wall clock
			// of whatever zone the value carries. This is what the upload sends as
			// fileCreatedAt, and what the server turns into the timeline date.
			if want := "2013-05-04T10:00:00.000Z"; got.Format(immich.TimeFormat) != want {
				t.Errorf("upload format = %q, want %q", got.Format(immich.TimeFormat), want)
			}
		})
	}
}

func TestCaptureDateOfFallsBackWhenTheTimelineDateIsMissing(t *testing.T) {
	const body = `{
		"id": "8b2c1d3e-0000-4000-8000-000000000002",
		"fileCreatedAt": "1999-12-31T23:00:00.000Z",
		"exifInfo": {"dateTimeOriginal": "2013-05-04T08:00:00.000+00:00"}
	}`

	var a immich.Asset
	if err := json.Unmarshal([]byte(body), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := captureDateOf(&a)
	if want := time.Date(2013, 5, 4, 8, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("got %v, want the dateTimeOriginal %v", got.UTC(), want)
	}

	var empty immich.Asset
	if err := json.Unmarshal([]byte(`{"id":"x","fileCreatedAt":"1999-12-31T23:00:00.000Z"}`), &empty); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, want := captureDateOf(&empty), time.Date(1999, 12, 31, 23, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("got %v, want the fileCreatedAt %v", got.UTC(), want)
	}
}

// The sidecar is the only thing that makes the server date the new file the way the
// old one was dated, so it has to be well formed and carry the offset.
func TestWriteDateSidecar(t *testing.T) {
	date := time.Date(2013, 5, 4, 10, 0, 0, 0, time.FixedZone("Europe/Paris", 2*3600))

	path, cleanup, err := writeDateSidecar("video.mp4", date)
	if err != nil {
		t.Fatalf("writeDateSidecar: %v", err)
	}
	defer cleanup()

	if got, want := filepath.Base(path), "video.mp4.xmp"; got != want {
		t.Errorf("name = %q, want %q", got, want)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var parsed struct {
		DateTimeOriginal string `xml:"RDF>Description>DateTimeOriginal"`
		CreateDate       string `xml:"RDF>Description>CreateDate"`
	}
	// The xpacket processing instructions sit outside the root element, Unmarshal
	// stops at the end of x:xmpmeta and is happy with them.
	if err := xml.Unmarshal(content, &parsed); err != nil {
		t.Fatalf("the sidecar isn't well formed XML: %v\n%s", err, content)
	}
	if want := "2013-05-04T10:00:00+02:00"; parsed.DateTimeOriginal != want {
		t.Errorf("DateTimeOriginal = %q, want %q", parsed.DateTimeOriginal, want)
	}
	if want := "2013-05-04T10:00:00+02:00"; parsed.CreateDate != want {
		t.Errorf("CreateDate = %q, want %q", parsed.CreateDate, want)
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the sidecar survived the cleanup: %v", err)
	}
}

func TestParseUTCOffset(t *testing.T) {
	for _, tc := range []struct {
		name string
		want int
		ok   bool
	}{
		{"UTC+2", 2 * 3600, true},
		{"UTC-5", -5 * 3600, true},
		{"UTC+05:30", 5*3600 + 30*60, true},
		{"UTC-05:30", -(5*3600 + 30*60), true},
		{"+02:00", 2 * 3600, true},
		{"UTC", 0, false},
		{"Europe/Paris", 0, false},
		{"UTC+99", 0, false},
	} {
		got, ok := parseUTCOffset(tc.name)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseUTCOffset(%q) = %d, %t; want %d, %t", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// A zone Immich reports as a bare offset still has to come back as that offset.
func TestCaptureDateOfWithAnOffsetZone(t *testing.T) {
	const body = `{
		"id": "8b2c1d3e-0000-4000-8000-000000000003",
		"localDateTime": "2013-05-04T10:00:00.000Z",
		"exifInfo": {"timeZone": "UTC+05:30"}
	}`

	var a immich.Asset
	if err := json.Unmarshal([]byte(body), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := captureDateOf(&a)
	if want := "2013-05-04T10:00:00+05:30"; got.Format(time.RFC3339) != want {
		t.Errorf("RFC3339 = %q, want %q", got.Format(time.RFC3339), want)
	}
	if want := "2013-05-04T10:00:00.000Z"; got.Format(immich.TimeFormat) != want {
		t.Errorf("upload format = %q, want %q", got.Format(immich.TimeFormat), want)
	}
}
