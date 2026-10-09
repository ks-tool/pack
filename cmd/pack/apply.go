package main

import (
	"context"
	"encoding/json"
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
// says what is installed, pack plans the saga and drives it step by step. A
// saga the record shows unfinished is continued from its last savepoint —
// the same command again — unless --from-start runs it from its first step.
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
	fromStart := fs.Bool("from-start", false, "run an unfinished saga from its first step instead of continuing from its last savepoint, or start a new one over it")
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
	given, err := copyValues(v)
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
	resume := rec.Unfinished() && !*fromStart
	if resume {
		sg := rec.Saga
		if sg.Pack != recordRef(*packRef) || !sameJSON(sg.Given, given) || !sameJSON(sg.Policy, policy) {
			return fmt.Errorf("a saga towards %s did not finish (%s at step %d: %s): the same command continues it from its last savepoint; this one asks for something else — --from-start to start over",
				sg.Pack, rec.State, sg.Failed, sg.Error)
		}
	}
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
	if resume {
		fmt.Printf("continuing the saga towards %s from its last savepoint\n", rec.Saga.Pack)
	} else {
		rec.Saga = &saga.Progress{Pack: recordRef(*packRef), Version: toB.Version, Values: v, Given: given, Policy: policy, Started: time.Now().UTC()}
	}
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

// copyValues is a deep copy of a values document, through JSON.
func copyValues(v render.Values) (render.Values, error) {
	var out render.Values
	raw, err := json.Marshal(v)
	if err != nil {
		return out, err
	}
	return out, json.Unmarshal(raw, &out)
}

// sameJSON compares two documents as their JSON.
func sameJSON(a, b any) bool {
	ra, err1 := json.Marshal(a)
	rb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ra) == string(rb)
}
