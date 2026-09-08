package asset

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/simulot/immich-go/app"
	"github.com/simulot/immich-go/immich"
	"github.com/simulot/immich-go/internal/assets"
	"github.com/simulot/immich-go/internal/filetypes"
	"github.com/simulot/immich-go/internal/fshelper"
	"github.com/spf13/cobra"
)

// swapCmd backs both "clone" and "replace": the two run the same sequence, and
// only differ by what happens to the source asset at the end.
//
// Immich removed the replace endpoint (PUT /assets/{id}/original) in the v3 series.
// The supported way of putting a new file behind an existing asset is now:
//
//  1. upload the new file, the server gives it a brand new ID,
//  2. PUT /assets/copy, for what the server knows how to carry over,
//  3. patch by hand what the copy leaves behind,
//  4. dispose of the source asset.
type swapCmd struct {
	assetCmd

	FileName    string // Name given to the server, defaults to the local file name
	Stack       bool   // Stack the new asset with the source one
	trashSource bool   // Set by "replace", not a flag
}

func newCloneCommand(ctx context.Context, a *app.Application) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clone <file> [flags]",
		Short: "Upload a file and give it the metadata of an existing asset",
		Long: `Upload a local file and carry the metadata of an existing asset over to it.

The source asset is left untouched, so both can be compared in the web interface.
Use --stack to have the server show them together. Use "asset replace" to do the
same and move the source asset to the trash.`,
		Args: cobra.ExactArgs(1),
	}

	sc := &swapCmd{assetCmd: assetCmd{app: a}}
	sc.registerFlags(cmd)
	cmd.Flags().BoolVar(&sc.Stack, "stack", false, "Stack the new asset with the source one, to compare them side by side")

	cmd.RunE = func(cmd *cobra.Command, args []string) error { //nolint:contextcheck
		return sc.run(cmd.Context(), a, args[0])
	}
	return cmd
}

func newReplaceCommand(ctx context.Context, a *app.Application) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "replace <file> [flags]",
		Short: "Replace the file of an existing asset, keeping its metadata",
		Long: `Replace the file of an existing asset by a local file.

The new file is uploaded, the metadata of the replaced asset is carried over to it,
and the replaced asset is moved to the trash. Use "asset clone" to keep it.

Albums, stack, shared links, sidecar and the favorite flag are copied by the server.
The capture date, the description, the rating, the GPS coordinates and the tags are
copied by immich-go, because the server doesn't. Faces aren't carried over: the
server runs its recognition again on the new file.`,
		Args: cobra.ExactArgs(1),
	}

	sc := &swapCmd{assetCmd: assetCmd{app: a}, trashSource: true}
	sc.registerFlags(cmd)

	cmd.RunE = func(cmd *cobra.Command, args []string) error { //nolint:contextcheck
		return sc.run(cmd.Context(), a, args[0])
	}
	return cmd
}

func (sc *swapCmd) registerFlags(cmd *cobra.Command) {
	sc.registerCommonFlags(cmd)
	cmd.Flags().StringVar(&sc.FileName, "filename", "", "Original file name to give to the server (default: the local file name)")
}

func (sc *swapCmd) run(ctx context.Context, a *app.Application, fileName string) error {
	if err := sc.client.Open(ctx, a); err != nil {
		return err
	}
	defer sc.client.Close() //nolint:errcheck

	source, err := sc.load(ctx)
	if err != nil {
		return fmt.Errorf("can't get the asset %s: %w", sc.AssetID, err)
	}
	if source.IsTrashed {
		return fmt.Errorf("the asset %s is in the trash, restore it before using it as a source", source.ID)
	}

	newAsset, cleanup, err := sc.prepare(source, fileName)
	if err != nil {
		return err
	}
	defer cleanup()
	defer newAsset.Close() //nolint:errcheck

	// Hash the file before anything else: it tells whether there is something to do
	// at all, and it makes the dry-run report meaningful.
	checksum, err := newAsset.GetChecksum()
	if err != nil {
		return fmt.Errorf("can't read %s: %w", fileName, err)
	}

	sc.report(source, newAsset)

	onServer, err := sc.client.Immich.GetAssetsByHash(ctx, checksum)
	if err != nil {
		return fmt.Errorf("can't check whether %s is already on the server: %w", fileName, err)
	}
	for _, other := range onServer {
		if other.ID == source.ID {
			return fmt.Errorf("%s is the file of the asset %s already, nothing to do", fileName, source.ID)
		}
		return fmt.Errorf("%s is already on the server as the asset %s (%s)", fileName, other.ID, other.OriginalFileName)
	}

	return sc.swap(ctx, source, newAsset)
}

// prepare builds the asset to be uploaded, seeded with the metadata of the source
// asset. A transcoder output carries none of it, so everything the server should
// keep has to be put back explicitly.
func (sc *swapCmd) prepare(source *immich.Asset, fileName string) (*assets.Asset, func(), error) {
	noCleanup := func() {}

	name, err := filepath.Abs(fileName)
	if err != nil {
		return nil, noCleanup, err
	}
	st, err := os.Stat(name)
	if err != nil {
		return nil, noCleanup, err
	}
	if st.IsDir() {
		return nil, noCleanup, fmt.Errorf("%s is a directory", fileName)
	}

	originalFileName := filepath.Base(name)
	if sc.FileName != "" {
		originalFileName = sc.FileName
	}
	if t := sc.client.Immich.SupportedMedia().TypeFromName(originalFileName); t != filetypes.TypeImage && t != filetypes.TypeVideo {
		return nil, noCleanup, fmt.Errorf("the file %q isn't a media type supported by the server", originalFileName)
	}

	captureDate := captureDateOf(source)

	rating := source.Rating
	if rating == 0 {
		rating = source.ExifInfo.Rating
	}

	a := &assets.Asset{
		File:             fshelper.FSName(os.DirFS(filepath.Dir(name)), filepath.Base(name)),
		OriginalFileName: originalFileName,
		FileSize:         int(st.Size()),
		FileDate:         st.ModTime(),
		CaptureDate:      captureDate,
		Description:      source.ExifInfo.Description,
		Favorite:         source.IsFavorite,
		Archived:         source.IsArchived,
		Rating:           rating,
		Latitude:         source.ExifInfo.Latitude,
		Longitude:        source.ExifInfo.Longitude,
	}
	for _, t := range source.Tags {
		a.Tags = append(a.Tags, t.AsTag())
	}

	// The sidecar is what fixes the date the timeline groups on. Without it the
	// metadata extraction reads the date out of the new file and overwrites it.
	if !captureDate.IsZero() {
		sidecar, cleanup, err := writeDateSidecar(originalFileName, captureDate)
		if err != nil {
			return nil, noCleanup, fmt.Errorf("can't write the sidecar: %w", err)
		}
		a.FromSideCar = &assets.Metadata{
			File:      fshelper.FSName(os.DirFS(filepath.Dir(sidecar)), filepath.Base(sidecar)),
			DateTaken: captureDate,
		}
		return a, cleanup, nil
	}
	return a, noCleanup, nil
}

// swap runs the upload / copy / patch sequence. The source asset is disposed of
// last: should any step fail, both assets are left on the server and their IDs are
// reported, so nothing is lost.
func (sc *swapCmd) swap(ctx context.Context, source *immich.Asset, newAsset *assets.Asset) error {
	log := sc.app.Log()

	ar, err := sc.client.Immich.AssetUpload(ctx, newAsset)
	if err != nil {
		return fmt.Errorf("can't upload %s: %w", newAsset.File.Name(), err)
	}
	if ar.Status == immich.UploadDuplicate {
		return fmt.Errorf("the server has rejected %s as a duplicate of the asset %s", newAsset.File.Name(), ar.ID)
	}
	newAsset.ID = ar.ID
	log.Message("Uploaded %s as the asset %s", newAsset.File.Name(), newAsset.ID)

	// Albums, stack, shared links, sidecar and the favorite flag.
	if err = sc.client.Immich.CopyAsset(ctx, source.ID, newAsset.ID); err != nil {
		return fmt.Errorf("can't copy the asset %s onto %s, both are on the server now: %w", source.ID, newAsset.ID, err)
	}
	log.Message("Copied the albums, the stack, the shared links and the sidecar of %s", source.ID)

	// What the server's copy leaves behind. Setting the capture date here also locks
	// it, so a later metadata extraction won't take the date of the new file instead.
	upd := immich.UpdAssetField{
		Description:      newAsset.Description,
		Latitude:         newAsset.Latitude,
		Longitude:        newAsset.Longitude,
		Rating:           newAsset.Rating,
		DateTimeOriginal: newAsset.CaptureDate,
	}
	if _, err = sc.client.Immich.UpdateAsset(ctx, newAsset.ID, upd); err != nil {
		return fmt.Errorf("can't set the metadata of the asset %s, both are on the server now: %w", newAsset.ID, err)
	}
	log.Message("Set the capture date, the description, the rating and the location of %s", newAsset.ID)

	if len(newAsset.Tags) > 0 {
		tagIDs := make([]string, len(newAsset.Tags))
		for i, t := range newAsset.Tags {
			tagIDs[i] = t.ID
		}
		if _, err = sc.client.Immich.BulkTagAssets(ctx, tagIDs, []string{newAsset.ID}); err != nil {
			return fmt.Errorf("can't tag the asset %s, both are on the server now: %w", newAsset.ID, err)
		}
		log.Message("Tagged %s with the tags of %s", newAsset.ID, source.ID)
	}

	if sc.Stack {
		if _, err = sc.client.Immich.CreateStack(ctx, []string{newAsset.ID, source.ID}); err != nil {
			return fmt.Errorf("can't stack %s with %s: %w", newAsset.ID, source.ID, err)
		}
		log.Message("Stacked %s with %s, the new asset being the cover", newAsset.ID, source.ID)
	}

	if sc.trashSource {
		// force=false: the replaced asset goes to the trash, not away.
		if err = sc.client.Immich.DeleteAssets(ctx, []string{source.ID}, false); err != nil {
			return fmt.Errorf("can't trash the replaced asset %s, both are on the server now: %w", source.ID, err)
		}
		log.Message("Moved the replaced asset %s to the trash", source.ID)
		log.Message("The asset %s has been replaced by %s", source.ID, newAsset.ID)
		return nil
	}

	log.Message("The asset %s has been cloned to %s, the source is untouched", source.ID, newAsset.ID)
	return nil
}

// report prints what is going to happen, so that --dry-run is readable.
func (sc *swapCmd) report(source *immich.Asset, newAsset *assets.Asset) {
	log := sc.app.Log()

	describe(log, source)
	log.Message("Uploading %s in its place:", newAsset.File.Name())
	log.Message("  uploaded as:   %s (%d bytes)", newAsset.OriginalFileName, newAsset.FileSize)
	log.Message("  checksum:      %s", newAsset.Checksum)
	log.Message("  capture date:  %s (taken from the source asset)", formatDate(newAsset.CaptureDate))
	if newAsset.FromSideCar != nil {
		log.Message("  sidecar:       %s, so the server dates the new file the same way", filepath.Base(newAsset.FromSideCar.File.Name()))
	}
	if sc.Stack {
		log.Message("The two assets will be stacked together.")
	}
	if sc.trashSource {
		log.Message("The source asset will be moved to the trash.")
	} else {
		log.Message("The source asset will be left untouched.")
	}
	log.Message("The faces of the source asset aren't carried over, the server will detect them again.")
}
