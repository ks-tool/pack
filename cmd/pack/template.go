package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/ks-tool/pack/render"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }
func (m *multiFlag) Type() string       { return "stringArray" }

func newTemplateCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "template", Short: "render for a target — horchestra: the ApplicationSet and its config Secrets; no cluster needed", Args: cobra.NoArgs}
	fs := cmd.Flags()
	packDir := fs.String("pack", "", "composition or component: a directory, or a registry reference (oci://host/repo:tag)")
	plainHTTP := fs.Bool("plain-http", false, "the registry speaks plain HTTP")
	valuesFile := fs.String("values", "", "values document (YAML)")
	name := fs.String("name", "", "installation name — the prefix of everything it creates")
	tf := addTargetFlags(fs)
	outDir := fs.StringP("output", "o", "", "write the target's files here (horchestra: applicationset.yaml, secrets/*.yaml, report.yaml); its objects to stdout when empty")
	provider := fs.String("secretProvider", "", "where the credentials the tool creates come from: generate | env | file:<dir>")

	var secrets multiFlag
	fs.Var(&secrets, "secret", "a password leaf to provision from the provider, as a values pointer (/kafka/jaas/password); repeatable")
	var skip multiFlag
	fs.Var(&skip, "skip", "exclude a key from the install, even if the values document or the whole-bundle fallback would otherwise include it; refused if another installed key still requires it; repeatable")

	cmd.RunE = func(*cobra.Command, []string) error {
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

		inst, closeTarget, err := tf.configure(*name)
		if err != nil {
			return err
		}
		defer closeTarget()
		credentials, err := render.Provision(b, &v, *name, render.Provider(*provider), secrets)
		if err != nil {
			return err
		}
		rel, err := render.Render(b, v, render.Context{Installation: *name, Target: inst})
		if err != nil {
			return err
		}
		out, err := inst.Convert(rel, credentials)
		if err != nil {
			return err
		}

		if len(*outDir) == 0 {
			first := true
			for _, f := range out.Files {
				if !f.Stream {
					continue
				}
				if !first {
					fmt.Println("---")
				}
				first = false
				os.Stdout.Write(f.Data)
			}
			return nil
		}
		for _, f := range out.Files {
			p := filepath.Join(*outDir, filepath.FromSlash(f.Path))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(p, f.Data, f.Mode); err != nil {
				return err
			}
		}
		fmt.Printf("wrote %s: %d file(s) for %s\n", *outDir, len(out.Files), tf.target())
		return nil
	}
	return cmd
}
