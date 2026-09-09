package asset

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/simulot/immich-go/app"
	"github.com/simulot/immich-go/immich"
	cliflags "github.com/simulot/immich-go/internal/cliFlags"
	"github.com/simulot/immich-go/internal/gen"
	"github.com/spf13/cobra"
)

// listCmd selects assets on the server and reports them, so that a batch of them
// can be reviewed and handed to a conversion pipeline.
//
// The filter flags deliberately carry the same names and meaning as the ones of
// "upload from-immich" and "archive from-immich".
type listCmd struct {
	client app.Client
	app    *app.Application

	Tags            []string
	Albums          []string
	Make            string
	Model           string
	Country         string
	State           string
	City            string
	OnlyArchived    bool
	OnlyFavorite    bool
	OnlyTrashed     bool
	OnlyNoAlbum     bool
	MinimalRating   int
	IncludePartners bool
	DateRange       cliflags.DateRange
	IncludedType    cliflags.IncludeType

	Export string // Write the report as a CSV file

	// The server reports the path a file has inside its own storage. These two turn
	// it into a path reachable from here, for whoever has the Immich folder mounted.
	ServerPathPrefix string
	LocalPathPrefix  string

	tagIDs   []string
	albumIDs []string
}

func newListCommand(ctx context.Context, a *app.Application) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list [flags]",
		Short: "List the assets matching a set of filters",
		Long: `List the assets matching a set of filters.

The filters are the ones of "upload from-immich". The report goes to the terminal,
and to a CSV file with --export.

The CSV is meant to drive a conversion pipeline: it carries the asset ID, where the
file sits in the server's storage, and an empty new_file column to be filled with
the path of the converted file.`,
		Args: cobra.NoArgs,
	}

	lc := &listCmd{app: a}
	flags := cmd.Flags()
	lc.client.RegisterFlags(flags, "")

	flags.StringSliceVar(&lc.Tags, "from-tags", nil, "Get assets only with those tags, can be used multiple times")
	flags.StringSliceVar(&lc.Albums, "from-albums", nil, "Get assets only from those albums, can be used multiple times")
	flags.StringVar(&lc.Make, "from-make", "", "Get only assets with this make")
	flags.StringVar(&lc.Model, "from-model", "", "Get only assets with this model")
	flags.StringVar(&lc.Country, "from-country", "", "Get only assets from this country")
	flags.StringVar(&lc.State, "from-state", "", "Get only assets from this state")
	flags.StringVar(&lc.City, "from-city", "", "Get only assets from this city")
	flags.BoolVar(&lc.OnlyArchived, "from-archived", false, "Get only archived assets")
	flags.BoolVar(&lc.OnlyFavorite, "from-favorite", false, "Get only favorite assets")
	flags.BoolVar(&lc.OnlyTrashed, "from-trash", false, "Get only trashed assets")
	flags.BoolVar(&lc.OnlyNoAlbum, "from-no-album", false, "Get only assets that are not in any album")
	flags.IntVar(&lc.MinimalRating, "from-minimal-rating", 0, "Get only assets with a rating greater or equal to this value")
	flags.BoolVar(&lc.IncludePartners, "from-partners", false, "Get partner's assets as well")
	flags.Var(&lc.DateRange, "date-range", "Get only assets taken within the specified date range")
	flags.Var(&lc.IncludedType, "include-type", "Single file type to include. (VIDEO or IMAGE) (default: all)")

	flags.StringVar(&lc.Export, "export", "", "Write the report as a CSV file at this path")
	flags.StringVar(&lc.ServerPathPrefix, "server-path-prefix", "", "Prefix of the server's storage path to rewrite (e.g. /usr/src/app/upload)")
	flags.StringVar(&lc.LocalPathPrefix, "local-path-prefix", "", "Prefix to put in its place, as reachable from here (e.g. /mnt/tank/immich)")

	cmd.TraverseChildren = true

	cmd.RunE = func(cmd *cobra.Command, args []string) error { //nolint:contextcheck
		ctx := cmd.Context()
		if err := lc.validate(); err != nil {
			return err
		}
		if err := lc.client.Open(ctx, a); err != nil {
			return err
		}
		defer lc.client.Close() //nolint:errcheck
		return lc.run(ctx)
	}
	return cmd
}

func (lc *listCmd) validate() error {
	if (lc.ServerPathPrefix == "") != (lc.LocalPathPrefix == "") {
		return fmt.Errorf("--server-path-prefix and --local-path-prefix go together, set both or neither")
	}
	if lc.Export != "" {
		// The export is meant to be edited by hand afterwards, don't clobber it.
		if _, err := os.Stat(lc.Export); err == nil {
			return fmt.Errorf("%s exists already, remove it or export to another path", lc.Export)
		}
	}
	lc.DateRange.SetTZ(lc.app.GetTZ())
	return nil
}

func (lc *listCmd) run(ctx context.Context) error {
	if err := lc.resolveTags(ctx); err != nil {
		return err
	}
	if err := lc.resolveAlbums(ctx); err != nil {
		return err
	}

	found, err := lc.search(ctx)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		lc.app.Log().Message("No asset matches those filters.")
		return nil
	}

	lc.printTable(found)

	if lc.Export != "" {
		if err = lc.exportCSV(found); err != nil {
			return err
		}
		lc.app.Log().Message("Wrote %s", lc.Export)
	}
	return nil
}

func (lc *listCmd) search(ctx context.Context) ([]*immich.Asset, error) {
	so := immich.SearchOptions().WithExif()

	if !lc.OnlyArchived && !lc.OnlyTrashed && !lc.OnlyFavorite {
		so.All()
	} else {
		if lc.OnlyArchived {
			so.WithOnlyArchived()
		}
		if lc.OnlyTrashed {
			so.WithOnlyTrashed()
		}
		if lc.OnlyFavorite {
			so.WithOnlyFavorite()
		}
	}

	if lc.Make != "" {
		so.WithOnlyMake(lc.Make)
	}
	if lc.Model != "" {
		so.WithOnlyModel(lc.Model)
	}
	if lc.Country != "" {
		so.WithOnlyCountry(lc.Country)
	}
	if lc.State != "" {
		so.WithOnlyState(lc.State)
	}
	if lc.City != "" {
		so.WithOnlyCity(lc.City)
	}
	if lc.OnlyNoAlbum {
		so.WithNotInAlbum()
	} else if len(lc.albumIDs) > 0 {
		so.WithAlbums(lc.albumIDs...)
	}
	if lc.DateRange.IsSet() {
		so.WithDateRange(lc.DateRange)
	}
	if r := min(max(0, lc.MinimalRating), 5); r > 1 {
		so.WithMinimalRate(r)
	}
	if len(lc.tagIDs) > 0 {
		so.WithTags(lc.tagIDs...)
	}

	// GetFilteredAssetsFn fans the search out over several queries and calls back
	// from all of them at once.
	var (
		lock  sync.Mutex
		found []*immich.Asset
	)
	wanted := string(lc.IncludedType)
	err := lc.client.Immich.GetFilteredAssetsFn(ctx, so, func(a *immich.Asset) error {
		if !lc.IncludePartners && a.OwnerID != lc.client.User.ID {
			return nil
		}
		if wanted != "" && !strings.EqualFold(a.Type, wanted) {
			return nil
		}
		lock.Lock()
		defer lock.Unlock()
		found = append(found, a)
		return nil
	})
	if err != nil {
		return nil, err
	}

	// The queries run concurrently, so the order they come back in is arbitrary.
	sort.Slice(found, func(i, j int) bool {
		di, dj := captureDateOf(found[i]), captureDateOf(found[j])
		if !di.Equal(dj) {
			return di.Before(dj)
		}
		if found[i].OriginalFileName != found[j].OriginalFileName {
			return found[i].OriginalFileName < found[j].OriginalFileName
		}
		return found[i].ID < found[j].ID
	})
	return found, nil
}

func (lc *listCmd) resolveTags(ctx context.Context) error {
	if len(lc.Tags) == 0 {
		return nil
	}
	tags, err := lc.client.Immich.GetAllTags(ctx)
	if err != nil {
		return err
	}

	unknown := []string{}
	for _, wanted := range lc.Tags {
		found := false
		for _, t := range tags {
			if t.Value == wanted {
				lc.tagIDs = gen.AddOnce(lc.tagIDs, t.ID)
				found = true
			}
		}
		if !found {
			unknown = append(unknown, wanted)
		}
	}
	if len(unknown) == 0 {
		return nil
	}

	available := make([]string, len(tags))
	for i, t := range tags {
		available[i] = t.Value
	}
	return fmt.Errorf("unknown tag(s): %s, available tag(s): %s", strings.Join(unknown, ", "), strings.Join(available, ", "))
}

func (lc *listCmd) resolveAlbums(ctx context.Context) error {
	if len(lc.Albums) == 0 {
		return nil
	}
	albums, err := lc.client.Immich.GetAllAlbums(ctx)
	if err != nil {
		return err
	}

	unknown := []string{}
	for _, wanted := range lc.Albums {
		found := false
		for _, al := range albums {
			if al.AlbumName == wanted {
				lc.albumIDs = gen.AddOnce(lc.albumIDs, al.ID)
				found = true
			}
		}
		if !found {
			unknown = append(unknown, wanted)
		}
	}
	if len(unknown) == 0 {
		return nil
	}

	available := make([]string, len(albums))
	for i, al := range albums {
		available[i] = al.AlbumName
	}
	return fmt.Errorf("unknown album(s): %s, available album(s): %s", strings.Join(unknown, ", "), strings.Join(available, ", "))
}

// serverPath returns where the file of an asset sits, rewritten to be reachable
// from here when the prefixes are given.
func (lc *listCmd) serverPath(a *immich.Asset) string {
	if lc.ServerPathPrefix == "" {
		return a.OriginalPath
	}
	// An external library sits outside the upload folder: leave those paths alone
	// rather than prepending a prefix that doesn't apply to them.
	rest, found := strings.CutPrefix(a.OriginalPath, lc.ServerPathPrefix)
	if !found {
		return a.OriginalPath
	}
	return lc.LocalPathPrefix + rest
}

func (lc *listCmd) printTable(found []*immich.Asset) {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)

	fmt.Fprintln(w, "ID\tNAME\tTYPE\tCAPTURE DATE\tSIZE\tRESOLUTION")
	var total int64
	for _, a := range found {
		resolution := ""
		if a.ExifInfo.ExifImageWidth > 0 {
			resolution = fmt.Sprintf("%dx%d", a.ExifInfo.ExifImageWidth, a.ExifInfo.ExifImageHeight)
		}
		total += a.ExifInfo.FileSizeInByte
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n",
			a.ID, a.OriginalFileName, a.Type,
			captureDateOf(a).Format(time.DateTime), a.ExifInfo.FileSizeInByte, resolution)
	}
	_ = w.Flush()

	log := lc.app.Log()
	for line := range strings.SplitSeq(strings.TrimRight(b.String(), "\n"), "\n") {
		log.Message("%s", line)
	}
	log.Message("%d asset(s), %d bytes in total", len(found), total)
}

var csvColumns = []string{
	"id", "name", "type",
	"capture_date", "timeline_date",
	"width", "height", "size", "checksum",
	"server_path", "new_file",
}

func (lc *listCmd) exportCSV(found []*immich.Asset) error {
	f, err := os.OpenFile(lc.Export, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck

	w := csv.NewWriter(f)
	if err = w.Write(csvColumns); err != nil {
		return err
	}

	for _, a := range found {
		// Both dates: they disagree whenever the capture date was edited after the
		// server extracted the metadata, and that is worth seeing before a batch run.
		record := []string{
			a.ID,
			a.OriginalFileName,
			a.Type,
			formatCSVDate(captureDateOf(a)),
			formatCSVDate(a.LocalDateTime.Time.UTC()),
			strconv.Itoa(a.ExifInfo.ExifImageWidth),
			strconv.Itoa(a.ExifInfo.ExifImageHeight),
			strconv.FormatInt(a.ExifInfo.FileSizeInByte, 10),
			a.Checksum,
			lc.serverPath(a),
			"", // new_file, to be filled with the path of the converted file
		}
		if err = w.Write(record); err != nil {
			return err
		}
	}

	w.Flush()
	if err = w.Error(); err != nil {
		return err
	}
	return f.Close()
}

func formatCSVDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
