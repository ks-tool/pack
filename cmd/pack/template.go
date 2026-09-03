package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/ks-tool/pack/render"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func runTemplate(args []string) error {
	fs := flag.NewFlagSet("template", flag.ContinueOnError)
	packDir := fs.String("pack", "", "composition or component: a directory, or a registry reference (cr://host/repo:tag)")
	plainHTTP := fs.Bool("plain-http", false, "the registry speaks plain HTTP")
	valuesFile := fs.String("values", "", "values document (YAML)")
	name := fs.String("name", "", "installation name — the prefix of everything it creates")
	namespace := fs.String("namespace", "default", "namespace the set is written to")
	outDir := fs.String("o", "", "write applicationset.yaml, secrets/*.yaml and report.yaml here; stdout when empty")
	provider := fs.String("secretProvider", "", "where the credentials the tool creates come from: generate | env | file:<dir>")

	var secrets multiFlag
	fs.Var(&secrets, "secret", "a password leaf to provision from the provider, as a values pointer (/kafka/jaas/password); repeatable")
	var skip multiFlag
	fs.Var(&skip, "skip", "exclude a key from the install, even if the values document or the whole-bundle fallback would otherwise include it; refused if another installed key still requires it; repeatable")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if len(*packDir) == 0 || len(*name) == 0 {
		return errors.New("template: --pack and --name are required")
	}

	b, err := openPack(*packDir, *plainHTTP)
	if err != nil {
		return err
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

	ctx := render.Context{Installation: *name, Namespace: *namespace}
	credentials, err := render.Provision(b, &v, ctx, render.Provider(*provider), secrets)
	if err != nil {
		return err
	}

	out, err := render.Render(b, v, ctx)
	if err != nil {
		return err
	}
	out.Secrets = append(credentials, out.Secrets...)

	setYAML, err := yaml.Marshal(out.Set)
	if err != nil {
		return err
	}

	if len(*outDir) == 0 {
		os.Stdout.Write(setYAML)
		for _, s := range out.Secrets {
			b, _ := yaml.Marshal(s)
			fmt.Println("---")
			os.Stdout.Write(b)
		}
		return nil
	}

	if err := os.MkdirAll(filepath.Join(*outDir, "secrets"), 0o755); err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(*outDir, "applicationset.yaml"), setYAML, 0o644); err != nil {
		return err
	}
	for _, s := range out.Secrets {
		b, _ := yaml.Marshal(s)
		if err := os.WriteFile(filepath.Join(*outDir, "secrets", s.Metadata.Name+".yaml"), b, 0o600); err != nil {
			return err
		}
	}
	rep, _ := yaml.Marshal(out.Report)
	if err := os.WriteFile(filepath.Join(*outDir, "report.yaml"), rep, 0o644); err != nil {
		return err
	}

	fmt.Printf("wrote %s: applicationset.yaml, %d secret(s), report.yaml\n", *outDir, len(out.Secrets))
	return nil
}
