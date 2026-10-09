package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/ks-tool/pack/bundle"
	"github.com/ks-tool/pack/plan"
	"github.com/ks-tool/pack/render"
	"github.com/ks-tool/pack/saga"
	"github.com/ks-tool/pack/target"
)

// runRollback takes an installation back: an unfinished saga is undone —
// every step of it in effect, the units behind savepoints too — and a
// finished one is followed by a saga to the version it replaced.
func runRollback(args []string) error {
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	plainHTTP := fs.Bool("plain-http", false, "the registry speaks plain HTTP")
	name := fs.String("name", "", "installation name")
	tf := addTargetFlags(fs)
	dryRun := fs.Bool("dry-run", false, "say what would be done, do nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(*name) == 0 {
		return errors.New("rollback: --name is required")
	}
	inst, closeTarget, err := tf.configure(*name)
	if err != nil {
		return err
	}
	defer closeTarget()
	x, ok := inst.(target.Executor)
	if !ok {
		return fmt.Errorf("target %s does not perform steps (no %q capability)", tf.target(), target.CapExecute)
	}
	ctx := render.Context{Installation: *name, Target: inst}
	rec, err := saga.Read(x)
	if err != nil {
		return err
	}
	if rec == nil || (len(rec.Pack) == 0 && !rec.Unfinished()) {
		return fmt.Errorf("nothing is installed as %s on %s", *name, tf.target())
	}
	sctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	events := func(s string) { fmt.Println(s) }

	if rec.Unfinished() {
		sg := rec.Saga
		fromB, fromRel, err := recorded(rec.Pack, rec.Values, ctx, *plainHTTP)
		if err != nil {
			return fmt.Errorf("the installed version %s: %w", rec.Pack, err)
		}
		toB, toRel, err := recorded(sg.Pack, sg.Values, ctx, *plainHTTP)
		if err != nil {
			return fmt.Errorf("the saga's version %s: %w", sg.Pack, err)
		}
		p, err := plan.Build(plan.Input{From: fromB, To: toB, FromOut: fromRel, ToOut: toRel, Policy: sg.Policy})
		if err != nil {
			return err
		}
		fmt.Printf("undoing the saga towards %s %s (%s): steps in effect %v\n", sg.Pack, sg.Version, rec.State, sg.Done)
		if *dryRun {
			fmt.Print(p.Text())
			return nil
		}
		run := &saga.Run{Plan: p, From: fromRel, To: toRel, Exec: x, Record: rec, Events: events}
		if err := run.Undo(sctx); err != nil {
			return err
		}
		if len(rec.Pack) == 0 {
			fmt.Printf("%s is not installed any more\n", *name)
		} else {
			fmt.Printf("back to %s %s\n", rec.Pack, rec.Version)
		}
		return nil
	}

	prev := rec.Previous
	if prev == nil {
		return fmt.Errorf("%s: no previous version is recorded — nothing to roll back to", *name)
	}
	curB, curRel, err := recorded(rec.Pack, rec.Values, ctx, *plainHTTP)
	if err != nil {
		return fmt.Errorf("the installed version %s: %w", rec.Pack, err)
	}
	prevB, prevRel, err := recorded(prev.Pack, prev.Values, ctx, *plainHTTP)
	if err != nil {
		return fmt.Errorf("the previous version %s: %w", prev.Pack, err)
	}
	p, err := plan.Build(plan.Input{From: curB, To: prevB, FromOut: curRel, ToOut: prevRel})
	if err != nil {
		return err
	}
	fmt.Print(p.Text())
	if *dryRun {
		return nil
	}
	rec.Saga = &saga.Progress{Pack: prev.Pack, Version: prev.Version, Values: prev.Values, Given: prev.Values, Started: time.Now().UTC()}
	run := &saga.Run{Plan: p, From: curRel, To: prevRel, Exec: x, Record: rec, Events: events}
	if err := run.Execute(sctx); err != nil {
		return err
	}
	fmt.Printf("back to %s %s\n", prev.Pack, prev.Version)
	return nil
}

// recorded opens a version the record names and renders it with its
// recorded values; an empty reference is no version — nil, nil.
func recorded(ref string, v render.Values, ctx render.Context, plainHTTP bool) (*bundle.Bundle, *render.Release, error) {
	if len(ref) == 0 {
		return nil, nil, nil
	}
	b, err := openPack(ref, plainHTTP)
	if err != nil {
		return nil, nil, err
	}
	rel, err := render.Render(b, v, ctx)
	if err != nil {
		return nil, nil, err
	}
	return b, rel, nil
}
