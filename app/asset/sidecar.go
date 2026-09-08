package asset

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// xmpTemplate is the smallest sidecar exiftool reads back as a DateTimeOriginal.
//
// It holds the date and nothing else on purpose: the date is the only field the
// server can't be told about through the API afterwards. Everything else is set
// with a plain asset update once the file is uploaded.
const xmpTemplate = "<?xpacket begin=\"\uFEFF\" id=\"W5M0MpCehiHzreSzNTczkc9d\"?>\n" + `<x:xmpmeta xmlns:x="adobe:ns:meta/" x:xmptk="immich-go">
 <rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">
  <rdf:Description rdf:about=""
    xmlns:exif="http://ns.adobe.com/exif/1.0/"
    xmlns:xmp="http://ns.adobe.com/xap/1.0/">
   <exif:DateTimeOriginal>%[1]s</exif:DateTimeOriginal>
   <xmp:CreateDate>%[1]s</xmp:CreateDate>
  </rdf:Description>
 </rdf:RDF>
</x:xmpmeta>
<?xpacket end="w"?>
`

// writeDateSidecar writes a sidecar carrying captureDate next to the upload.
//
// The server sets asset.localDateTime, the field the timeline groups on, only when
// it extracts the metadata of the file, and the file produced by a transcoder
// carries the date of the transcoding — or none at all. Uploading a sidecar is what
// makes the extraction see the right date: it explicitly prefers the dates of a
// sidecar over the ones embedded in the media, and drops the embedded ones.
//
// Returns the path of the sidecar and a function removing the directory holding it.
func writeDateSidecar(name string, captureDate time.Time) (string, func(), error) {
	dir, err := os.MkdirTemp("", "immich-go-sidecar-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	path := filepath.Join(dir, name+".xmp")
	content := fmt.Sprintf(xmpTemplate, captureDate.Format(time.RFC3339))
	if err = os.WriteFile(path, []byte(content), 0o600); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return path, cleanup, nil
}
