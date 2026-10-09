package main

import (
	"errors"
	"flag"
	"os"

	"github.com/ks-tool/pack/sdk/platform"
)

// runSchema prints the vocabulary the tool ships, or a target's platform.
func runSchema(args []string) error {
	if len(args) == 0 {
		return errors.New("schema: vocabulary | platform [--target t]")
	}
	switch args[0] {
	case "vocabulary":
		_, err := os.Stdout.Write(platform.PackV1)
		return err
	case "platform":
		fs := flag.NewFlagSet("schema platform", flag.ContinueOnError)
		tf := addTargetFlags(fs)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		inst, closeTarget, err := tf.configure("schema")
		if err != nil {
			return err
		}
		defer closeTarget()
		_, err = os.Stdout.Write(inst.PlatformSchema())
		return err
	}
	return errors.New("schema: vocabulary | platform [--target t]")
}
