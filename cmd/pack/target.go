package main

import (
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/ks-tool/pack/target"
	"github.com/ks-tool/pack/target/horchestra"
)

// targets are the systems the CLI can render for, by --target.
var targets = map[string]target.Target{
	horchestra.Target{}.Name(): horchestra.Target{},
}

// targetFlags are the flags every command that renders shares: which target,
// and its settings. --namespace is the shorthand for --set namespace=….
type targetFlags struct {
	name      *string
	namespace *string
	settings  multiFlag
}

func addTargetFlags(fs *flag.FlagSet) *targetFlags {
	tf := &targetFlags{
		name:      fs.String("target", "horchestra", "the system to render for: "+strings.Join(targetNames(), " | ")),
		namespace: fs.String("namespace", "", "shorthand for --set namespace=<ns>"),
	}
	fs.Var(&tf.settings, "set", "a setting of the target, key=value; repeatable")
	return tf
}

// configure binds the selected target to an installation.
func (tf *targetFlags) configure(installation string) (target.Installation, error) {
	t, ok := targets[*tf.name]
	if !ok {
		return nil, fmt.Errorf("--target %q: one of %s", *tf.name, strings.Join(targetNames(), ", "))
	}
	settings := target.Settings{}
	for _, kv := range tf.settings {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || len(k) == 0 {
			return nil, fmt.Errorf("--set %q: key=value", kv)
		}
		settings[k] = v
	}
	if len(*tf.namespace) > 0 {
		if _, dup := settings["namespace"]; dup {
			return nil, fmt.Errorf("--namespace and --set namespace=… both given")
		}
		settings["namespace"] = *tf.namespace
	}
	return t.Configure(installation, settings)
}

func targetNames() []string {
	out := make([]string, 0, len(targets))
	for n := range targets {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
