// Package asset implements the "asset" command and its sub-commands, which work
// on a single asset of the Immich server, designated by its ID.
package asset

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/simulot/immich-go/app"
	"github.com/simulot/immich-go/immich"
	"github.com/spf13/cobra"
)

// assetCmd carries what every sub-command needs: the server connection and the
// asset to work on.
type assetCmd struct {
	AssetID string

	client app.Client
	app    *app.Application
}

func (ac *assetCmd) registerCommonFlags(cmd *cobra.Command) {
	flags := cmd.Flags()
	flags.StringVar(&ac.AssetID, "asset", "", "ID of the asset to work on")
	ac.client.RegisterFlags(flags, "")
	_ = cmd.MarkFlagRequired("asset")
	cmd.TraverseChildren = true
}

// load reads the asset and the albums it belongs to. GetAssetInfo doesn't fill
// the albums, they come from another endpoint.
func (ac *assetCmd) load(ctx context.Context) (*immich.Asset, error) {
	a, err := ac.client.Immich.GetAssetInfo(ctx, ac.AssetID)
	if err != nil {
		return nil, err
	}
	a.Albums, err = ac.client.Immich.GetAssetAlbums(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	return a, nil
}

func NewAssetCommand(ctx context.Context, a *app.Application) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "asset",
		Short: "Inspect and replace individual assets on the server",
	}

	cmd.AddCommand(
		newShowCommand(ctx, a),
		newCloneCommand(ctx, a),
		newReplaceCommand(ctx, a),
	)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return errors.New("you must specify a subcommand to the asset command")
	}
	return cmd
}

// describe prints what the server holds for an asset.
func describe(log *app.Log, a *immich.Asset) {
	log.Message("Asset %s", a.ID)
	log.Message("  file:          %s (%s, %d bytes)", a.OriginalFileName, a.Type, a.ExifInfo.FileSizeInByte)
	if a.ExifInfo.ExifImageWidth > 0 {
		log.Message("  resolution:    %dx%d", a.ExifInfo.ExifImageWidth, a.ExifInfo.ExifImageHeight)
	}
	log.Message("  checksum:      %s", a.Checksum)

	// The timeline groups on localDateTime, the properties panel shows
	// dateTimeOriginal. They can disagree, so print both.
	log.Message("  capture date:  %s", formatDate(captureDateOf(a)))
	log.Message("  timeline date: %s", formatDate(a.LocalDateTime.Time.UTC()))
	if tz := a.ExifInfo.TimeZone; tz != "" {
		log.Message("  time zone:     %s", tz)
	}

	if len(a.Albums) > 0 {
		names := make([]string, len(a.Albums))
		for i, al := range a.Albums {
			names[i] = al.AlbumName
		}
		log.Message("  albums:        %s", strings.Join(names, ", "))
	}
	if len(a.Tags) > 0 {
		names := make([]string, len(a.Tags))
		for i, t := range a.Tags {
			names[i] = t.Name
		}
		log.Message("  tags:          %s", strings.Join(names, ", "))
	}
	if a.ExifInfo.Description != "" {
		log.Message("  description:   %s", a.ExifInfo.Description)
	}
	if a.ExifInfo.Latitude != 0 || a.ExifInfo.Longitude != 0 {
		log.Message("  location:      %f, %f", a.ExifInfo.Latitude, a.ExifInfo.Longitude)
	}
	log.Message("  flags:         favorite=%t archived=%t trashed=%t visibility=%s rating=%d",
		a.IsFavorite, a.IsArchived, a.IsTrashed, a.Visibility, a.Rating)
}

// captureDateOf reproduces the wall clock the server displays for an asset.
//
// Immich keeps the date twice: asset_exif.dateTimeOriginal is an instant, and
// asset.localDateTime is the same wall clock relabelled as UTC, which is what the
// timeline groups on. The API hands dateTimeOriginal back converted to the caller's
// zone, so sending it as-is shifts the wall clock whenever the asset's zone differs
// from ours — enough to move an asset to another day. Rebuild the wall clock from
// localDateTime and read it back in the zone the asset was shot in.
func captureDateOf(a *immich.Asset) time.Time {
	// ImmichTime parses the trailing "Z" as UTC then converts to the local zone:
	// back in UTC, the clock reads exactly what the server displays.
	wall := a.LocalDateTime.Time.UTC()
	if wall.IsZero() {
		if instant := a.ExifInfo.DateTimeOriginal.Time; !instant.IsZero() {
			return instant
		}
		return a.FileCreatedAt.Time
	}
	return time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), wall.Second(), wall.Nanosecond(),
		zoneOf(a))
}

// zoneOf resolves the zone the asset was shot in.
//
// exifInfo.timeZone is the only dependable source for it: ImmichExifTime can't parse
// an offset other than +00:00, because Go spells a numeric zone "-07:00" and the
// client's layout says "+00:00", which only ever matches itself literally. Anything
// shot outside UTC therefore decodes as the zero time.
//
// Falling back to UTC keeps the wall clock right and loses only the zone label,
// which is what happens on a system without a tz database as well.
func zoneOf(a *immich.Asset) *time.Location {
	name := a.ExifInfo.TimeZone
	if name == "" {
		return time.UTC
	}
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	// Immich also reports zones as a plain offset, "UTC+2" or "UTC-05:30".
	if offset, ok := parseUTCOffset(name); ok {
		return time.FixedZone(name, offset)
	}
	return time.UTC
}

// parseUTCOffset reads the "UTC+2", "UTC-05:30" and "+02:00" forms into a number of
// seconds east of UTC.
func parseUTCOffset(name string) (int, bool) {
	s := strings.TrimPrefix(strings.TrimSpace(name), "UTC")
	if s == "" || (s[0] != '+' && s[0] != '-') {
		return 0, false
	}
	sign := 1
	if s[0] == '-' {
		sign = -1
	}
	s = s[1:]

	hours, minutes := s, "0"
	if h, m, found := strings.Cut(s, ":"); found {
		hours, minutes = h, m
	}
	h, err := strconv.Atoi(hours)
	if err != nil {
		return 0, false
	}
	m, err := strconv.Atoi(minutes)
	if err != nil {
		return 0, false
	}
	if h < 0 || h > 14 || m < 0 || m > 59 {
		return 0, false
	}
	return sign * (h*3600 + m*60), true
}

func formatDate(t time.Time) string {
	if t.IsZero() {
		return "not set"
	}
	return t.Format("2006-01-02 15:04:05 -07:00")
}
