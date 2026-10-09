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
		slot, err := platform.Merge(inst.PlatformSchema()) // the pack's half joined to the target's
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(append(slot, '\n'))
		return err
	}
	return errors.New("schema: vocabulary | platform [--target t]")
}
