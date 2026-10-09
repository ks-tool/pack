// pack is the CLI of the pack tool — phase 1: everything that needs no cluster.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "check":
		err = runCheck(os.Args[2:])
	case "template":
		err = runTemplate(os.Args[2:])
	case "plan":
		err = runPlan(os.Args[2:])
	case "apply":
		err = runApply(os.Args[2:])
	case "schema":
		err = runSchema(os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		_, _ = fmt.Fprintf(os.Stderr, "pack: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "pack:", err)
		os.Exit(1)
	}
}

func usage() {
	_, _ = fmt.Fprint(os.Stderr, `usage:
  pack check [--plain-http] <dir|ref>  author checks on a component directory (schema.json, templates/, image.json),
                                       on a composition directory (its components are the sibling dirs), or on a
                                       pack in a registry (cr://host/repo:tag — schemas and templates are read, no image)
  pack template --pack <dir|ref> --values <file> --name <installation> [--target horchestra] [--set k=v ...]
                [--namespace ns] [-o <dir>] [--plain-http] [--secretProvider generate|env|file:<dir> --secret /key/path/password ...]
                                       render for a target — horchestra: the ApplicationSet and its config Secrets; no cluster needed
  pack plan [--from <dir|ref>] --to <dir|ref> --values <file> [--target t] [--set k=v ...] [--savepoints auto|none|k1,k2]
            [--no-rollback k1,k2|all] [--strict] [-o plan.yaml]
                                       the saga: a first installation, or the upgrade between two versions
  pack apply --pack <dir|ref> --values <file> --name <installation> [--target t] [--set k=v ...] [--savepoints …]
             [--no-rollback …] [--strict] [--dry-run] [--force] [--secretProvider … --secret …]
                                       install or upgrade: the target says what is installed, pack drives the saga
                                       step by step and rolls a failed unit back; the target must perform steps
  registries: PACK_REGISTRY_USER / PACK_REGISTRY_PASSWORD in the environment for credentials
  pack schema vocabulary|platform [--target t]
                                       print the x-pack-* vocabulary, or the schema a target gives the platform half
`)
}
