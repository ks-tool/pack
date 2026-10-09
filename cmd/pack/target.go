package main

import (
	"flag"
	"fmt"
	"strings"

	"github.com/ks-tool/pack/provider"
	"github.com/ks-tool/pack/target"
)

// targetFlags are the flags every command that renders shares: which target,
// and its settings. --namespace is the shorthand for --set namespace=….
type targetFlags struct {
	name      *string
	namespace *string
	settings  multiFlag
}

func addTargetFlags(fs *flag.FlagSet) *targetFlags {
	tf := &targetFlags{
		name:      fs.String("target", "horchestra", "the system to render for: its provider pack-provider-<target> is run ("+provider.EnvPath+", beside pack, PATH)"),
		namespace: fs.String("namespace", "", "shorthand for --set namespace=<ns>"),
	}
	fs.Var(&tf.settings, "set", "a setting of the target, key=value; repeatable")
	return tf
}

// configure starts the selected target's provider and binds it to an
// installation; close stops the provider.
func (tf *targetFlags) configure(installation string) (inst target.Installation, close func(), err error) {
	settings := target.Settings{}
	for _, kv := range tf.settings {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || len(k) == 0 {
			return nil, nil, fmt.Errorf("--set %q: key=value", kv)
		}
		settings[k] = v
	}
	if len(*tf.namespace) > 0 {
		if _, dup := settings["namespace"]; dup {
			return nil, nil, fmt.Errorf("--namespace and --set namespace=… both given")
		}
		settings["namespace"] = *tf.namespace
	}
	p, err := provider.Launch(*tf.name)
	if err != nil {
		return nil, nil, err
	}
	if inst, err = p.Configure(installation, settings); err != nil {
		p.Close()
		return nil, nil, err
	}
	return inst, p.Close, nil
}
