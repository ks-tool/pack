package main

import (
	"errors"
	"flag"
	"fmt"
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
		t, ok := targets[*tf.name]
		if !ok {
			return fmt.Errorf("--target %q: unknown", *tf.name)
		}
		inst, err := t.Configure("schema", nil)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(inst.PlatformSchema())
		return err
	}
	return errors.New("schema: vocabulary | platform [--target t]")
}
