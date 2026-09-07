// Package replace implements the "replace" command: it swaps the file of an
// existing asset for a local one, while keeping the metadata the server holds.
//
// Immich has removed the replace endpoint (PUT /assets/{id}/original) in the v3
// series. The supported sequence is now:
//
//  1. upload the new file, the server gives it a brand new ID,
//  2. PUT /assets/copy to carry over what the server knows how to copy,
//  3. patch by hand what the copy leaves behind,
//  4. delete the asset that has been replaced.
package replace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/simulot/immich-go/app"
	"github.com/simulot/immich-go/immich"
	"github.com/simulot/immich-go/internal/assets"
	"github.com/simulot/immich-go/internal/filetypes"
	"github.com/simulot/immich-go/internal/fshelper"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type ReplaceCmd struct {
	AssetID  string // ID of the asset to be replaced
	FileName string // Name given to the server, defaults to the local file name

	client app.Client
	app    *app.Application
}

func (rc *ReplaceCmd) RegisterFlags(flags *pflag.FlagSet) {
	flags.StringVar(&rc.AssetID, "asset", "", "ID of the asset to replace")
	flags.StringVar(&rc.FileName, "filename", "", "Original file name to give to the server (default: the local file name)")
}

func NewReplaceCommand(ctx context.Context, a *app.Application) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "replace <file> [flags]",
		Short: "Replace the file of an existing asset, keeping its metadata",
		Long: `Replace the file of an existing asset by a local file.

The new file is uploaded, the metadata of the replaced asset is carried over to
it, and the replaced asset is moved to the trash.

Albums, stack, shared links, sidecar and the favorite flag are copied by the
server. The capture date, the description, the rating, the GPS coordinates, the
visibility and the tags are copied by immich-go, because the server doesn't.
Faces aren't carried over: the server runs its recognition again on the new file.

Use --dry-run to see what would be done without touching the server.`,
		Args: cobra.ExactArgs(1),
	}

	rc := &ReplaceCmd{app: a}
	rc.RegisterFlags(cmd.Flags())
	rc.client.RegisterFlags(cmd.Flags(), "")
	_ = cmd.MarkFlagRequired("asset")
	cmd.TraverseChildren = true

	cmd.RunE = func(cmd *cobra.Command, args []string) error { //nolint:contextcheck
		ctx := cmd.Context()
		if err := rc.client.Open(ctx, a); err != nil {
			return err
		}
		defer rc.client.Close() //nolint:errcheck
		return rc.run(ctx, args[0])
	}
	return cmd
}

func (rc *ReplaceCmd) run(ctx context.Context, fileName string) error {
	old, err := rc.client.Immich.GetAssetInfo(ctx, rc.AssetID)
	if err != nil {
		return fmt.Errorf("can't get the asset %s: %w", rc.AssetID, err)
	}
	if old.IsTrashed {
		return fmt.Errorf("the asset %s is in the trash, restore it before replacing it", old.ID)
	}

	// GetAssetInfo doesn't fill the albums, they come from another endpoint.
	old.Albums, err = rc.client.Immich.GetAssetAlbums(ctx, old.ID)
	if err != nil {
		return fmt.Errorf("can't get the albums of the asset %s: %w", old.ID, err)
	}

	newAsset, err := rc.prepare(old, fileName)
	if err != nil {
		return err
	}
	defer newAsset.Close() //nolint:errcheck

	// Hash the file before anything else: it tells whether there is something to
	// replace at all, and it makes the dry-run report meaningful.
	checksum, err := newAsset.GetChecksum()
	if err != nil {
		return fmt.Errorf("can't read %s: %w", fileName, err)
	}

	rc.report(old, newAsset)

	onServer, err := rc.client.Immich.GetAssetsByHash(ctx, checksum)
	if err != nil {
		return fmt.Errorf("can't check whether %s is already on the server: %w", fileName, err)
	}
	for _, a := range onServer {
		if a.ID == old.ID {
			return fmt.Errorf("%s is the file of the asset %s already, nothing to replace", fileName, old.ID)
		}
		return fmt.Errorf("%s is already on the server as the asset %s (%s)", fileName, a.ID, a.OriginalFileName)
	}

	return rc.replace(ctx, old, newAsset)
}

// prepare builds the asset to be uploaded, seeded with the metadata of the asset
// being replaced. A transcoder output carries none of it, so everything the
// server should keep has to be put back explicitly.
func (rc *ReplaceCmd) prepare(old *immich.Asset, fileName string) (*assets.Asset, error) {
	name, err := filepath.Abs(fileName)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(name)
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		return nil, fmt.Errorf("%s is a directory", fileName)
	}

	originalFileName := filepath.Base(name)
	if rc.FileName != "" {
		originalFileName = rc.FileName
	}
	if t := rc.client.Immich.SupportedMedia().TypeFromName(originalFileName); t != filetypes.TypeImage && t != filetypes.TypeVideo {
		return nil, fmt.Errorf("the file %q isn't a media type supported by the server", originalFileName)
	}

	// The capture date is what the upload sends as fileCreatedAt. Take the one the
	// server holds: it may have been fixed by hand, and the new file doesn't have it.
	captureDate := old.ExifInfo.DateTimeOriginal.Time
	if captureDate.IsZero() {
		captureDate = old.FileCreatedAt.Time
	}

	rating := old.Rating
	if rating == 0 {
		rating = old.ExifInfo.Rating
	}

	a := &assets.Asset{
		File:             fshelper.FSName(os.DirFS(filepath.Dir(name)), filepath.Base(name)),
		OriginalFileName: originalFileName,
		FileSize:         int(st.Size()),
		FileDate:         st.ModTime(),
		CaptureDate:      captureDate,
		Description:      old.ExifInfo.Description,
		Favorite:         old.IsFavorite,
		Archived:         old.IsArchived,
		Rating:           rating,
		Latitude:         old.ExifInfo.Latitude,
		Longitude:        old.ExifInfo.Longitude,
		Visibility:       assets.Visibility(old.Visibility),
	}
	for _, t := range old.Tags {
		a.Tags = append(a.Tags, t.AsTag())
	}
	return a, nil
}

// replace runs the upload / copy / patch / delete sequence. The replaced asset is
// deleted last: should any step fail, both assets are left on the server and their
// IDs are reported, so nothing is lost.
func (rc *ReplaceCmd) replace(ctx context.Context, old *immich.Asset, newAsset *assets.Asset) error {
	log := rc.app.Log()

	ar, err := rc.client.Immich.AssetUpload(ctx, newAsset)
	if err != nil {
		return fmt.Errorf("can't upload %s: %w", newAsset.File.Name(), err)
	}
	if ar.Status == immich.UploadDuplicate {
		return fmt.Errorf("the server has rejected %s as a duplicate of the asset %s", newAsset.File.Name(), ar.ID)
	}
	newAsset.ID = ar.ID
	log.Message("Uploaded %s as the asset %s", newAsset.File.Name(), newAsset.ID)

	// Albums, stack, shared links, sidecar and the favorite flag.
	if err = rc.client.Immich.CopyAsset(ctx, old.ID, newAsset.ID); err != nil {
		return fmt.Errorf("can't copy the asset %s onto %s, both are on the server now: %w", old.ID, newAsset.ID, err)
	}
	log.Message("Copied the albums, the stack, the shared links and the sidecar of %s", old.ID)

	// What the server's copy leaves behind.
	upd := immich.UpdAssetField{
		Description:      newAsset.Description,
		Latitude:         newAsset.Latitude,
		Longitude:        newAsset.Longitude,
		Rating:           newAsset.Rating,
		DateTimeOriginal: newAsset.CaptureDate,
	}
	if v := string(newAsset.Visibility); v != "" {
		upd.Visibility = v
	}
	if _, err = rc.client.Immich.UpdateAsset(ctx, newAsset.ID, upd); err != nil {
		return fmt.Errorf("can't set the metadata of the asset %s, both are on the server now: %w", newAsset.ID, err)
	}
	log.Message("Set the capture date, the description, the rating and the location of %s", newAsset.ID)

	if len(newAsset.Tags) > 0 {
		tagIDs := make([]string, len(newAsset.Tags))
		for i, t := range newAsset.Tags {
			tagIDs[i] = t.ID
		}
		if _, err = rc.client.Immich.BulkTagAssets(ctx, tagIDs, []string{newAsset.ID}); err != nil {
			return fmt.Errorf("can't tag the asset %s, both are on the server now: %w", newAsset.ID, err)
		}
		log.Message("Tagged %s with %s", newAsset.ID, tagNames(newAsset.Tags))
	}

	// force=false: the replaced asset goes to the trash, not away.
	if err = rc.client.Immich.DeleteAssets(ctx, []string{old.ID}, false); err != nil {
		return fmt.Errorf("can't trash the replaced asset %s, both are on the server now: %w", old.ID, err)
	}
	log.Message("Moved the replaced asset %s to the trash", old.ID)
	log.Message("The asset %s has been replaced by %s", old.ID, newAsset.ID)
	return nil
}

// report prints what is going to happen, so that --dry-run is readable.
func (rc *ReplaceCmd) report(old *immich.Asset, newAsset *assets.Asset) {
	log := rc.app.Log()

	log.Message("Replacing the asset %s:", old.ID)
	log.Message("  file:         %s (%s, %d bytes)", old.OriginalFileName, old.Type, old.ExifInfo.FileSizeInByte)
	if old.ExifInfo.ExifImageWidth > 0 {
		log.Message("  resolution:   %dx%d", old.ExifInfo.ExifImageWidth, old.ExifInfo.ExifImageHeight)
	}
	log.Message("  capture date: %s", formatDate(old.ExifInfo.DateTimeOriginal.Time))
	log.Message("  checksum:     %s", old.Checksum)
	if len(old.Albums) > 0 {
		names := make([]string, len(old.Albums))
		for i, al := range old.Albums {
			names[i] = al.AlbumName
		}
		log.Message("  albums:       %s", strings.Join(names, ", "))
	}
	if len(old.Tags) > 0 {
		tags := make([]assets.Tag, len(old.Tags))
		for i, t := range old.Tags {
			tags[i] = t.AsTag()
		}
		log.Message("  tags:         %s", tagNames(tags))
	}
	if old.ExifInfo.Description != "" {
		log.Message("  description:  %s", old.ExifInfo.Description)
	}
	if old.ExifInfo.Latitude != 0 || old.ExifInfo.Longitude != 0 {
		log.Message("  location:     %f, %f", old.ExifInfo.Latitude, old.ExifInfo.Longitude)
	}
	log.Message("  flags:        favorite=%t archived=%t visibility=%s rating=%d", old.IsFavorite, old.IsArchived, old.Visibility, old.Rating)

	log.Message("by the file %s:", newAsset.File.Name())
	log.Message("  uploaded as:  %s (%d bytes)", newAsset.OriginalFileName, newAsset.FileSize)
	log.Message("  checksum:     %s", newAsset.Checksum)
	log.Message("  capture date: %s (taken from the replaced asset)", formatDate(newAsset.CaptureDate))
	log.Message("The replaced asset will be moved to the trash.")
	log.Message("The faces of the replaced asset aren't carried over, the server will detect them again.")
}

func formatDate(t time.Time) string {
	if t.IsZero() {
		return "not set"
	}
	return t.Format(time.DateTime)
}

func tagNames(tags []assets.Tag) string {
	names := make([]string, len(tags))
	for i, t := range tags {
		names[i] = t.Name
	}
	return strings.Join(names, ", ")
}
