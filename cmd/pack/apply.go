package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/ks-tool/pack/bundle"
	"github.com/ks-tool/pack/plan"
	"github.com/ks-tool/pack/registry"
	"github.com/ks-tool/pack/render"
	"github.com/ks-tool/pack/saga"
	"github.com/ks-tool/pack/target"
)

// runApply installs a pack, or upgrades the installation to it: the target
// says what is installed, pack plans the saga and drives it step by step.
func runApply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	packRef := fs.String("pack", "", "the version to install: a composition or component directory, or a registry reference")
	plainHTTP := fs.Bool("plain-http", false, "the registry speaks plain HTTP")
	valuesFile := fs.String("values", "", "values document (YAML)")
	name := fs.String("name", "", "installation name")
	tf := addTargetFlags(fs)
	savepoints := fs.String("savepoints", "", "override the composition's savepoints: auto | none | key1,key2")
	noRollback := fs.String("no-rollback", "", "keys where an automatic rollback stops (\"all\" rolls nothing back)")
	strict := fs.Bool("strict", false, "refuse a plan with an irreversible step")
	dryRun := fs.Bool("dry-run", false, "print the plan against what is installed, perform nothing")
	force := fs.Bool("force", false, "plan from the recorded version although the last saga did not finish")
	provider := fs.String("secretProvider", "", "where the credentials the tool creates come from: generate | env | file:<dir>")
	var secrets, skip multiFlag
	fs.Var(&secrets, "secret", "a password leaf to provision from the provider, as a values pointer; repeatable")
	fs.Var(&skip, "skip", "exclude a key from the install; repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(*packRef) == 0 || len(*name) == 0 {
		return errors.New("apply: --pack and --name are required")
	}

	inst, closeTarget, err := tf.configure(*name)
	if err != nil {
		return err
	}
	defer closeTarget()
	x, ok := inst.(target.Executor)
	if !ok {
		return fmt.Errorf("target %s does not perform steps (no %q capability): pack template renders for it", *tf.name, target.CapExecute)
	}
	ctx := render.Context{Installation: *name, Target: inst}

	// what is installed
	rec, err := saga.Read(x)
	if err != nil {
		return err
	}
	if rec != nil && rec.State != saga.Installed && !*force {
		return fmt.Errorf("the last saga towards %s did not finish (%s, step %d: %s): look at the target, then --force to plan from %s %s",
			rec.Saga.Pack, rec.State, rec.Saga.Failed, rec.Saga.Error, rec.Pack, rec.Version)
	}
	var fromB *bundle.Bundle
	var fromRel *render.Release
	if rec != nil && len(rec.Pack) > 0 {
		if fromB, err = openPack(rec.Pack, *plainHTTP); err != nil {
			return fmt.Errorf("the installed version %s: %w", rec.Pack, err)
		}
		if fromRel, err = render.Render(fromB, rec.Values, ctx); err != nil {
			return fmt.Errorf("the installed version %s: %w", rec.Pack, err)
		}
	}

	// what is wanted
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
	toB, err := openPack(*packRef, *plainHTTP)
	if err != nil {
		return err
	}
	credentials, err := render.Provision(toB, &v, *name, render.Provider(*provider), secrets)
	if err != nil {
		return err
	}
	toRel, err := render.Render(toB, v, ctx)
	if err != nil {
		return err
	}

	policy := plan.Policy{Savepoints: *savepoints, Strict: *strict}
	switch *noRollback {
	case "":
	case "all":
		policy.NoRollbackAll = true
	default:
		policy.NoRollback = strings.Split(*noRollback, ",")
	}
	p, err := plan.Build(plan.Input{From: fromB, To: toB, FromOut: fromRel, ToOut: toRel, Policy: policy})
	if err != nil {
		return err
	}
	fmt.Print(p.Text())
	if *dryRun {
		return nil
	}

	if rec == nil {
		rec = &saga.Record{Name: *name}
	}
	rec.Saga = &saga.Progress{Pack: recordRef(*packRef), Version: toB.Version, Values: v, Policy: policy, Started: time.Now().UTC()}
	sctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	run := &saga.Run{Plan: p, From: fromRel, To: toRel, Credentials: credentials, Exec: x, Record: rec,
		Events: func(s string) { fmt.Println(s) }}
	if err := run.Execute(sctx); err != nil {
		return err
	}
	fmt.Println(strings.Join(strings.Fields(fmt.Sprintf("installed %s %s as %s on %s", toB.Name, toB.Version, *name, *tf.name)), " "))
	return nil
}

// recordRef is how the installed version is found again: a registry
// reference as given, a directory as an absolute path.
func recordRef(ref string) string {
	if registry.IsRef(ref) {
		if _, err := os.Stat(ref); err != nil || strings.HasPrefix(ref, registry.Scheme) {
			return ref
		}
	}
	if abs, err := filepath.Abs(ref); err == nil {
		return abs
	}
	return ref
}
