package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/ks-tool/pack/sdk/platform"
)

// newSchemaCmd prints the vocabulary the tool ships, or a target's platform.
func newSchemaCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "schema", Short: "print the x-pack-* vocabulary, or the platform schema a target joins to the pack's"}
	cmd.AddCommand(&cobra.Command{
		Use:   "vocabulary",
		Short: "the x-pack-* vocabulary (horchestra://pack/v1)",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			_, err := os.Stdout.Write(platform.PackV1)
			return err
		},
	})
	pl := &cobra.Command{Use: "platform", Short: "the platform slot: the pack's half joined to the target's", Args: cobra.NoArgs}
	tf := addTargetFlags(pl.Flags())
	pl.RunE = func(*cobra.Command, []string) error {
		inst, closeTarget, err := tf.configure("schema")
		if err != nil {
			return err
		}
		defer closeTarget()
		slot, err := platform.Merge(inst.PlatformSchema()) // the pack's half joined to the target's
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(append(slot, '\n'))
		return err
	}
	cmd.AddCommand(pl)
	return cmd
}
