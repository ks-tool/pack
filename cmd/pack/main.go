// pack is the CLI of the pack tool: check, render, plan and drive a pack's
// installation on a target, through the target's provider.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "pack:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "pack",
		Short: "the package format for horchestra, and the tool that renders and installs it",
		Long: `pack checks, renders, plans and installs a pack — from a directory or a registry
(oci://<host>/<repo>:<tag>; schemas, templates and image configs are read, never image layers)
— on a target, through the target's provider.

Registries: PACK_REGISTRY_USER / PACK_REGISTRY_PASSWORD in the environment for credentials.`,
		SilenceUsage:  true, // a refusal is not a misuse: the error says what is wrong
		SilenceErrors: true, // main prints it, as "pack: …"
	}
	root.AddCommand(newCheckCmd(), newTemplateCmd(), newPlanCmd(), newApplyCmd(), newRollbackCmd(), newSchemaCmd())
	return root
}
