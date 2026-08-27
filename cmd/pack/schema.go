package main

import (
	"errors"
	"os"

	"github.com/ks-tool/pack/sdk/platform"
)

// runSchema prints the schemas the tool ships as blobs of its release.
func runSchema(args []string) error {
	if len(args) != 1 {
		return errors.New("schema: vocabulary | platform")
	}
	switch args[0] {
	case "vocabulary":
		_, err := os.Stdout.Write(platform.PackV1)
		return err
	case "platform":
		_, err := os.Stdout.Write(platform.PlatformV1)
		return err
	}
	return errors.New("schema: vocabulary | platform")
}
