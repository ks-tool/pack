package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/ks-tool/pack/bundle"
	"github.com/ks-tool/pack/plan"
	"github.com/ks-tool/pack/render"
)

func newPlanCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "plan", Short: "the saga: a first installation, or the upgrade between two versions", Args: cobra.NoArgs}
	fs := cmd.Flags()
	from := fs.String("from", "", "the installed version: a composition or component directory or registry reference; absent — the plan of a first installation")
	to := fs.String("to", "", "the target version: a directory or a registry reference")
	plainHTTP := fs.Bool("plain-http", false, "the registry speaks plain HTTP")
	valuesFile := fs.String("values", "", "values document (YAML)")
	name := fs.String("name", "release", "installation name")
	tf := addTargetFlags(fs)
	savepoints := fs.String("savepoints", "", "override the composition's savepoints: auto | none | key1,key2")
	noRollback := fs.String("no-rollback", "", "keys where an automatic rollback stops (\"all\" rolls nothing back)")
	strict := fs.Bool("strict", false, "refuse a plan with an irreversible step")
	outFile := fs.StringP("output", "o", "", "write the plan as YAML here, besides printing it")
	var skip multiFlag
	fs.Var(&skip, "skip", "exclude a key from the install, even if the values document or the whole-bundle fallback would otherwise include it; refused if another installed key still requires it; repeatable")
	cmd.RunE = func(*cobra.Command, []string) error {
		if len(*to) == 0 {
			return errors.New("plan: --to is required (--from is the installed version; without it the plan is a first installation)")
		}
		var v render.Values
		if len(*valuesFile) > 0 {
			raw, err := os.ReadFile(*valuesFile)
			if err != nil {
				return err
			}
			if err := yaml.Unmarshal(raw, &v); err != nil {
				return fmt.Errorf("%s: %w", *valuesFile, err)
			}
		}
		v.Skip = append(v.Skip, skip...)
		inst, closeTarget, err := tf.configure(*name)
		if err != nil {
			return err
		}
		defer closeTarget()
		ctx := render.Context{Installation: *name, Target: inst}
		toB, err := openPack(*to, *plainHTTP)
		if err != nil {
			return fmt.Errorf("--to: %w", err)
		}
		toOut, err := render.Render(toB, v, ctx)
		if err != nil {
			return fmt.Errorf("--to: %w", err)
		}
		var fromB *bundle.Bundle
		var fromOut *render.Release
		if len(*from) > 0 {
			fromB, err = openPack(*from, *plainHTTP)
			if err != nil {
				return fmt.Errorf("--from: %w", err)
			}
			fromOut, err = render.Render(fromB, v, ctx) // best effort: the old schema may not take the new values
			if err != nil {
				fromOut = nil
			}
		}
		policy := plan.Policy{Savepoints: *savepoints, Strict: *strict}
		switch *noRollback {
		case "":
		case "all":
			policy.NoRollbackAll = true
		default:
			policy.NoRollback = strings.Split(*noRollback, ",")
		}
		p, err := plan.Build(plan.Input{From: fromB, To: toB, FromOut: fromOut, ToOut: toOut, Policy: policy})
		if err != nil {
			return err
		}
		fmt.Print(p.Text())
		if len(*outFile) > 0 {
			b, _ := yaml.Marshal(p)
			if err := os.WriteFile(*outFile, b, 0o644); err != nil {
				return err
			}
		}
		return nil
	}
	return cmd
}
