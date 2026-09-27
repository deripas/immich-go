package asset

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

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
	FromCSV     string // Work on the rows of this plan instead of a single asset
	Result      string // Write the outcome of a plan as a CSV file
	trashSource bool   // Set by "replace", not a flag
}

func newCloneCommand(ctx context.Context, a *app.Application) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clone [<file>] [flags]",
		Short: "Upload a file and give it the metadata of an existing asset",
		Long: `Upload a local file and carry the metadata of an existing asset over to it.

The source asset is left untouched, so both can be compared in the web interface.
Use --stack to have the server show them together. Use "asset replace" to do the
same and move the source asset to the trash.

Give --asset and a file to work on a single asset, or --from-csv to work on a plan
exported by "asset list" and filled in with the converted files.`,
		Args: cobra.MaximumNArgs(1),
	}

	sc := &swapCmd{assetCmd: assetCmd{app: a}}
	sc.registerFlags(cmd)
	cmd.Flags().BoolVar(&sc.Stack, "stack", false, "Stack the new asset with the source one, to compare them side by side")

	cmd.RunE = func(cmd *cobra.Command, args []string) error { //nolint:contextcheck
		return sc.run(cmd.Context(), a, args)
	}
	return cmd
}

func newReplaceCommand(ctx context.Context, a *app.Application) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "replace [<file>] [flags]",
		Short: "Replace the file of an existing asset, keeping its metadata",
		Long: `Replace the file of an existing asset by a local file.

The new file is uploaded, the metadata of the replaced asset is carried over to it,
and the replaced asset is moved to the trash. Use "asset clone" to keep it.

Albums, stack, shared links, sidecar and the favorite flag are copied by the server.
The capture date, the description, the rating, the GPS coordinates and the tags are
copied by immich-go, because the server doesn't. Faces aren't carried over: the
server runs its recognition again on the new file.

Give --asset and a file to work on a single asset, or --from-csv to work on a plan
exported by "asset list" and filled in with the converted files.`,
		Args: cobra.MaximumNArgs(1),
	}

	sc := &swapCmd{assetCmd: assetCmd{app: a}, trashSource: true}
	sc.registerFlags(cmd)

	cmd.RunE = func(cmd *cobra.Command, args []string) error { //nolint:contextcheck
		return sc.run(cmd.Context(), a, args)
	}
	return cmd
}

func (sc *swapCmd) registerFlags(cmd *cobra.Command) {
	sc.registerCommonFlags(cmd)
	flags := cmd.Flags()
	flags.StringVar(&sc.FileName, "filename", "", "Original file name to give to the server (default: the local file name)")
	flags.StringVar(&sc.FromCSV, "from-csv", "", "Work on every row of this plan, reading the id and new_file columns")
	flags.StringVar(&sc.Result, "result", "", "Write the outcome of the plan as a CSV file at this path")
}

func (sc *swapCmd) run(ctx context.Context, a *app.Application, args []string) error {
	if err := sc.validate(args); err != nil {
		return err
	}
	if err := sc.client.Open(ctx, a); err != nil {
		return err
	}
	defer sc.client.Close() //nolint:errcheck

	if sc.FromCSV != "" {
		return sc.runPlan(ctx)
	}
	return sc.runOne(ctx, args[0])
}

func (sc *swapCmd) validate(args []string) error {
	switch {
	case sc.FromCSV == "" && (sc.AssetID == "" || len(args) == 0):
		return fmt.Errorf("give --asset and a file to work on a single asset, or --from-csv to work on a plan")
	case sc.FromCSV != "" && (sc.AssetID != "" || len(args) > 0):
		return fmt.Errorf("--from-csv carries the assets and the files, don't pass --asset or a file as well")
	case sc.FromCSV == "" && sc.Result != "":
		return fmt.Errorf("--result reports on a plan, it goes with --from-csv")
	}
	if sc.FromCSV != "" {
		// Fail on a missing plan before opening a connection.
		if _, err := os.Stat(sc.FromCSV); err != nil {
			return err
		}
	}
	if sc.Result != "" {
		// The result is the record of what happened, don't lose a previous one.
		if _, err := os.Stat(sc.Result); err == nil {
			return fmt.Errorf("%s exists already, remove it or write the result to another path", sc.Result)
		}
	}
	return nil
}

// runOne works on the single asset given on the command line.
func (sc *swapCmd) runOne(ctx context.Context, fileName string) error {
	source, err := sc.load(ctx)
	if err != nil {
		return fmt.Errorf("can't get the asset %s: %w", sc.AssetID, err)
	}
	if source.IsTrashed {
		return fmt.Errorf("the asset %s is in the trash, restore it before using it as a source", source.ID)
	}
	if err = checkFile(fileName, sc.client.Immich.SupportedMedia()); err != nil {
		return err
	}

	_, err = sc.processOne(ctx, source, fileName, sc.FileName, true)
	return err
}

// runPlan works on every row of a CSV plan.
//
// The plan is checked as a whole before anything is uploaded: a typo on the last
// row is worth knowing about before the first asset has been replaced.
func (sc *swapCmd) runPlan(ctx context.Context) error {
	log := sc.app.Log()

	rows, err := readPlan(sc.FromCSV)
	if err != nil {
		return err
	}
	if err = sc.checkPlan(ctx, rows); err != nil {
		return err
	}
	sc.reportPlan(rows)

	results := make([]planResult, 0, len(rows))
	var stopped error
	for i, row := range rows {
		if row.skip != "" {
			log.Message("[%d/%d] %s (%s) skipped: %s", i+1, len(rows), row.source.OriginalFileName, row.assetID, row.skip)
			results = append(results, planResult{row: row, skipped: row.skip})
			continue
		}

		log.Message("[%d/%d] %s (%s) <- %s", i+1, len(rows), row.source.OriginalFileName, row.assetID, row.file)

		newID, err := sc.processOne(ctx, row.source, row.file, row.filename, false)

		// A row that needs nothing done to it is not a failure: re-running a plan
		// that was partly applied is the normal way to finish it.
		var nothing nothingToDo
		if errors.As(err, &nothing) {
			log.Message("        skipped: %s", nothing.reason)
			results = append(results, planResult{row: row, newID: newID, skipped: nothing.reason})
			continue
		}

		results = append(results, planResult{row: row, newID: newID, err: err})
		if err != nil {
			// --on-errors decides whether one bad row ends the run.
			if stop := sc.app.ProcessError(fmt.Errorf("%s line %d: %w", sc.FromCSV, row.line, err)); stop != nil {
				stopped = stop
				break
			}
		}
	}

	done, skipped, failed := 0, 0, 0
	for _, r := range results {
		switch {
		case r.err != nil:
			failed++
		case r.skipped != "":
			skipped++
		default:
			done++
		}
	}
	verb := "cloned"
	if sc.trashSource {
		verb = "replaced"
	}
	log.Message("%d asset(s) %s, %d skipped, %d failed, %d not attempted",
		done, verb, skipped, failed, len(rows)-len(results))

	if sc.Result != "" {
		if err = writeResults(sc.Result, results); err != nil {
			return err
		}
		log.Message("Wrote %s", sc.Result)
	}
	return stopped
}

// checkPlan resolves every row against the file system and the server, so that the
// run either starts on solid ground or doesn't start at all.
func (sc *swapCmd) checkPlan(ctx context.Context, rows []*planRow) error {
	sm := sc.client.Immich.SupportedMedia()
	problems := []string{}

	for _, row := range rows {
		source, err := sc.client.Immich.GetAssetInfo(ctx, row.assetID)
		if err != nil {
			problems = append(problems, fmt.Sprintf("line %d: can't get the asset %s: %v", row.line, row.assetID, err))
			continue
		}

		// Whether the row still needs work is decided before the file is looked at:
		// a row that was applied already may well have had its converted file
		// cleaned up since, and that must not hold the rest of the plan back.
		if source.IsTrashed {
			row.source = source
			row.skip = "the source asset is in the trash, it was most likely replaced already"
			continue
		}

		if err = checkFile(row.file, sm); err != nil {
			problems = append(problems, fmt.Sprintf("line %d: %v", row.line, err))
			continue
		}

		source.Albums, err = sc.client.Immich.GetAssetAlbums(ctx, source.ID)
		if err != nil {
			problems = append(problems, fmt.Sprintf("line %d: can't get the albums of %s: %v", row.line, row.assetID, err))
			continue
		}
		row.source = source
	}

	if len(problems) > 0 {
		return fmt.Errorf("%s doesn't check out, nothing was uploaded:\n  %s", sc.FromCSV, strings.Join(problems, "\n  "))
	}
	return nil
}

func (sc *swapCmd) reportPlan(rows []*planRow) {
	log := sc.app.Log()

	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "#\tID\tCURRENT NAME\tNEW FILE")
	todo := 0
	for i, row := range rows {
		newFile := row.file
		if row.skip != "" {
			newFile = "(skipped: " + row.skip + ")"
		} else {
			todo++
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", i+1, row.assetID, row.source.OriginalFileName, newFile)
	}
	_ = w.Flush()
	for line := range strings.SplitSeq(strings.TrimRight(b.String(), "\n"), "\n") {
		log.Message("%s", line)
	}

	skipped := len(rows) - todo
	if sc.trashSource {
		log.Message("%d asset(s) to replace, %d skipped. The source of each replaced one goes to the trash.", todo, skipped)
	} else {
		log.Message("%d asset(s) to clone, %d skipped. Every source is left untouched.", todo, skipped)
	}
}

// nothingToDo marks a row that needs nothing done to it. Re-running a plan that was
// applied in part is the normal way to finish it, so in a plan this is a skip. On a
// single asset the caller asked for something precise, and gets told it didn't happen.
type nothingToDo struct {
	reason string
}

func (e nothingToDo) Error() string { return e.reason }

// processOne uploads fileName in place of source. detailed asks for the full
// metadata block, which is worth printing for a single asset but not for a plan of
// a hundred.
func (sc *swapCmd) processOne(ctx context.Context, source *immich.Asset, fileName, overrideName string, detailed bool) (string, error) {
	newAsset, cleanup, err := sc.prepare(source, fileName, overrideName)
	if err != nil {
		return "", err
	}
	defer cleanup()
	defer newAsset.Close() //nolint:errcheck

	// Hash the file before anything else: it tells whether there is something to do
	// at all, and it makes the dry-run report meaningful.
	checksum, err := newAsset.GetChecksum()
	if err != nil {
		return "", fmt.Errorf("can't read %s: %w", fileName, err)
	}

	if detailed {
		sc.report(source, newAsset)
	}

	onServer, err := sc.client.Immich.GetAssetsByHash(ctx, checksum)
	if err != nil {
		return "", fmt.Errorf("can't check whether %s is already on the server: %w", fileName, err)
	}
	for _, other := range onServer {
		if other.ID == source.ID {
			return "", nothingToDo{fmt.Sprintf("%s is the file of the asset %s already", fileName, source.ID)}
		}
		return other.ID, nothingToDo{fmt.Sprintf("%s is already on the server as the asset %s (%s)", fileName, other.ID, other.OriginalFileName)}
	}

	if err = sc.swap(ctx, source, newAsset); err != nil {
		return newAsset.ID, err
	}
	return newAsset.ID, nil
}

// checkFile makes sure a path points at a file the server would accept.
func checkFile(fileName string, sm filetypes.SupportedMedia) error {
	st, err := os.Stat(fileName)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return fmt.Errorf("%s is a directory", fileName)
	}
	if t := sm.TypeFromName(filepath.Base(fileName)); t != filetypes.TypeImage && t != filetypes.TypeVideo {
		return fmt.Errorf("the file %q isn't a media type supported by the server", filepath.Base(fileName))
	}
	return nil
}

// prepare builds the asset to be uploaded, seeded with the metadata of the source
// asset. A transcoder output carries none of it, so everything the server should
// keep has to be put back explicitly.
func (sc *swapCmd) prepare(source *immich.Asset, fileName, overrideName string) (*assets.Asset, func(), error) {
	noCleanup := func() {}

	name, err := filepath.Abs(fileName)
	if err != nil {
		return nil, noCleanup, err
	}
	sm := sc.client.Immich.SupportedMedia()
	if err = checkFile(name, sm); err != nil {
		return nil, noCleanup, err
	}
	st, err := os.Stat(name)
	if err != nil {
		return nil, noCleanup, err
	}

	originalFileName := filepath.Base(name)
	if overrideName != "" {
		originalFileName = overrideName
		// The name given to the server drives the type it stores the asset as.
		if t := sm.TypeFromName(originalFileName); t != filetypes.TypeImage && t != filetypes.TypeVideo {
			return nil, noCleanup, fmt.Errorf("the name %q isn't a media type supported by the server", originalFileName)
		}
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
