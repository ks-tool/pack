package main

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/ks-tool/pack/environment"
	"github.com/ks-tool/pack/provider"
	"github.com/ks-tool/pack/target"
)

// defaultTarget is the target when neither the environment nor the command
// line names one.
const defaultTarget = "horchestra"

// targetFlags are the flags every command that renders shares: the
// environment's file, and what the command line says over it.
type targetFlags struct {
	config    *string
	name      *string
	provider  *string
	namespace *string
	settings  multiFlag
	chosen    string // the target configure settled on
}

// target is the target configure settled on.
func (tf *targetFlags) target() string { return tf.chosen }

func addTargetFlags(fs *flag.FlagSet) *targetFlags {
	tf := &targetFlags{
		config:    fs.String("config", "", "the environment: a .yaml or .json file — target, provider, vault.addr and vault.paths (read with "+environment.EnvVaultToken+"), and any setting of the target; all of it, Vault's KV included, goes to the provider"),
		name:      fs.String("target", "", "the system to render for, over the environment's (default "+defaultTarget+"); its provider pack-provider-<target> is run ("+provider.EnvPath+", beside pack, PATH)"),
		provider:  fs.String("provider", "", "the target's provider, over the environment's: a binary's path, or a container image as oci://<host>/<repo>:<tag> run with podman or docker ("+provider.EnvRuntime+")"),
		namespace: fs.String("namespace", "", "shorthand for --set namespace=<ns>"),
	}
	fs.Var(&tf.settings, "set", "a setting, key=value, over the environment's file and Vault; repeatable")
	return tf
}

// configure loads the environment, starts its target's provider and binds it
// to an installation with the environment's whole set; close stops the provider.
func (tf *targetFlags) configure(installation string) (inst target.Installation, close func(), err error) {
	over := map[string]string{}
	for _, kv := range tf.settings {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || len(k) == 0 {
			return nil, nil, fmt.Errorf("--set %q: key=value", kv)
		}
		over[strings.ToLower(k)] = v
	}
	for key, flagValue := range map[string]string{"namespace": *tf.namespace, "target": *tf.name, "provider": *tf.provider} {
		if len(flagValue) == 0 {
			continue
		}
		if _, dup := over[key]; dup {
			return nil, nil, fmt.Errorf("--%s and --set %s=… both given", key, key)
		}
		over[key] = flagValue
	}
	env, err := environment.Load(context.Background(), *tf.config, over)
	if err != nil {
		return nil, nil, err
	}
	name := env.Target
	if len(name) == 0 {
		name = defaultTarget
		env.Settings["target"] = name
	}
	tf.chosen = name
	p, err := provider.Open(name, env.Provider)
	if err != nil {
		return nil, nil, err
	}
	if inst, err = p.Configure(installation, target.Settings(env.Settings)); err != nil {
		p.Close()
		return nil, nil, err
	}
	return inst, p.Close, nil
}
