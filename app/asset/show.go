package asset

import (
	"context"

	"github.com/simulot/immich-go/app"
	"github.com/spf13/cobra"
)

func newShowCommand(ctx context.Context, a *app.Application) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show [flags]",
		Short: "Print what the server holds for an asset",
		Long: `Print the metadata the server holds for an asset.

Both dates are printed: the capture date, shown in the properties panel, and the
timeline date the asset is grouped on. They can disagree — the server computes the
timeline date only when it extracts the metadata of a file, and never recomputes it
when the capture date is edited afterwards.`,
		Args: cobra.NoArgs,
	}

	sc := &assetCmd{app: a}
	sc.registerCommonFlags(cmd)
	_ = cmd.MarkFlagRequired("asset")

	cmd.RunE = func(cmd *cobra.Command, args []string) error { //nolint:contextcheck
		ctx := cmd.Context()
		if err := sc.client.Open(ctx, a); err != nil {
			return err
		}
		defer sc.client.Close() //nolint:errcheck

		asset, err := sc.load(ctx)
		if err != nil {
			return err
		}
		describe(a.Log(), asset)
		return nil
	}
	return cmd
}
