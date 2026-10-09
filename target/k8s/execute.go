package k8s

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/ks-tool/pack/target"
)

const defaultTimeout = 10 * time.Minute

// kubectl runs kubectl against the installation's cluster and namespace;
// stdin, when set, is what -f - reads. Output goes to events line by line.
func (in *Installation) kubectl(ctx context.Context, stdin []byte, events func(string), args ...string) ([]byte, error) {
	var base []string
	if in.kubeconfig != "" {
		base = append(base, "--kubeconfig", in.kubeconfig)
	}
	if in.context != "" {
		base = append(base, "--context", in.context)
	}
	base = append(base, "--namespace", in.namespace)
	cmd := exec.CommandContext(ctx, target.Settings(in.settings).Get("kubectl", "kubectl"), append(base, args...)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if events != nil {
		sc := bufio.NewScanner(bytes.NewReader(stdout.Bytes()))
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				events(line)
			}
		}
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.Bytes(), fmt.Errorf("kubectl %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.Bytes(), nil
}

func (in *Installation) apply(ctx context.Context, objs []obj, events func(string)) error {
	if len(objs) == 0 {
		return nil
	}
	b, err := stream(objs)
	if err != nil {
		return err
	}
	_, err = in.kubectl(ctx, b, events, "apply", "--server-side", "--force-conflicts", "--field-manager=pack", "-f", "-")
	return err
}

func (in *Installation) recordName() string { return "pack-" + in.installation }

// ReadInstalled reads the record pack keeps in the namespace: the ConfigMap
// pack-<installation>.
func (in *Installation) ReadInstalled() ([]byte, bool, error) {
	out, err := in.kubectl(context.Background(), nil, nil, "get", "configmap", in.recordName(), "--ignore-not-found", "-o", "json")
	if err != nil {
		return nil, false, err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, false, nil
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(out, &cm); err != nil {
		return nil, false, err
	}
	rec, ok := cm.Data["record.json"]
	return []byte(rec), ok, nil
}

// WriteInstalled keeps the record in the ConfigMap pack-<installation>.
func (in *Installation) WriteInstalled(record []byte) error {
	cm := obj{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": obj{"name": in.recordName(), "namespace": in.namespace,
			"labels": obj{labelInstance: in.installation, labelManagedBy: "pack", labelPart: "record"}},
		"data": obj{"record.json": string(record)}}
	return in.apply(context.Background(), []obj{cm}, nil)
}

// Execute performs one step of the saga on the cluster.
func (in *Installation) Execute(ctx context.Context, st target.Step, events func(string)) error {
	svc := in.service(st.Key)
	timeout := defaultTimeout
	if st.Timeout != "" {
		if d, err := time.ParseDuration(st.Timeout); err == nil {
			timeout = d
		}
	}
	switch st.Kind {
	case "apply":
		if st.Unit == nil {
			return fmt.Errorf("apply %s: no unit", st.Key)
		}
		objs, err := in.objects(*st.Unit, st.Credentials, in.resolver(ctx))
		if err != nil {
			return err
		}
		return in.apply(ctx, objs, events)
	case "wait":
		if st.Unit != nil && st.Unit.RestartPolicy == "Never" {
			return in.waitJob(ctx, svc, timeout, events)
		}
		_, err := in.kubectl(ctx, nil, events, "rollout", "status", "statefulset/"+svc, "--timeout="+timeout.String())
		return err
	case "stop":
		if _, err := in.kubectl(ctx, nil, events, "scale", "statefulset/"+svc, "--replicas=0"); err != nil {
			return err
		}
		_, err := in.kubectl(ctx, nil, events, "wait", "--for=delete", "pod", "-l", selector(in.labels(st.Key)), "--timeout="+timeout.String())
		return err
	case "remove":
		_, err := in.kubectl(ctx, nil, events, "delete", "statefulset,job,service,secret",
			"-l", selector(in.labels(st.Key))+",pack.horchestra.io/part in (workload,config,hook)", "--ignore-not-found", "--wait=true")
		return err
	case "hook":
		return in.hook(ctx, st, timeout, events)
	}
	return fmt.Errorf("step %d: %q is no step kind", st.N, st.Kind)
}

// hook runs a hook as a Job: the key's pod — its files, env and libraries —
// with the hook's image and command; an exclusive hook gets the volumes of
// replica 0. The key's credentials and config Secrets are applied first, a
// hook may come before the key's first apply.
func (in *Installation) hook(ctx context.Context, st target.Step, timeout time.Duration, events func(string)) error {
	if st.Unit == nil {
		return fmt.Errorf("hook %s: no unit", st.Name)
	}
	u := *st.Unit
	svc := in.service(st.Key)
	objs, err := in.objects(u, st.Credentials, in.resolver(ctx))
	if err != nil {
		return err
	}
	var pre []obj
	for _, o := range objs {
		if o["kind"] == "Secret" {
			pre = append(pre, o)
		}
	}
	if err := in.apply(ctx, pre, events); err != nil {
		return err
	}
	pod, _, _, err := in.podSpec(u, nil)
	if err != nil {
		return err
	}
	c := pod["containers"].([]any)[0].(obj)
	c["image"], c["command"] = st.Image, toList(st.Command)
	delete(c, "args")
	delete(c, "readinessProbe")
	delete(c, "ports")
	pod["restartPolicy"] = "Never"
	if st.Exclusive {
		for _, d := range u.Data {
			addVolume(pod, obj{"name": d.Name, "persistentVolumeClaim": obj{"claimName": d.Name + "-" + svc + "-0"}})
		}
	} else {
		mounts, _ := c["volumeMounts"].([]any)
		var kept []any
		data := map[string]bool{}
		for _, d := range u.Data {
			data[d.Name] = true
		}
		for _, m := range mounts {
			if !data[m.(obj)["name"].(string)] {
				kept = append(kept, m)
			}
		}
		c["volumeMounts"] = kept
	}
	name := dnsLabel(fmt.Sprintf("%s-%s-%d", svc, st.Name, st.N))
	if st.Rollback {
		name = dnsLabel(fmt.Sprintf("%s-undo-%s-%d", svc, st.Name, st.N))
	}
	job := obj{"apiVersion": "batch/v1", "kind": "Job", "metadata": in.meta(name, st.Key, "hook"),
		"spec": obj{"backoffLimit": 0, "template": obj{"metadata": obj{"labels": in.labels(st.Key)}, "spec": pod}}}
	_, _ = in.kubectl(ctx, nil, nil, "delete", "job/"+name, "--ignore-not-found", "--wait=true")
	if err := in.apply(ctx, []obj{job}, events); err != nil {
		return err
	}
	if err := in.waitJob(ctx, name, timeout, events); err != nil {
		return fmt.Errorf("hook %s: %w", st.Name, err)
	}
	_, _ = in.kubectl(ctx, nil, nil, "delete", "job/"+name, "--ignore-not-found")
	return nil
}

// waitJob waits until a Job succeeds or fails — kubectl wait sees only one
// condition, and a failed Job never completes — and tells its log either way.
func (in *Installation) waitJob(ctx context.Context, name string, timeout time.Duration, events func(string)) error {
	deadline := time.Now().Add(timeout)
	var verdict error
	for {
		out, err := in.kubectl(ctx, nil, nil, "get", "job/"+name, "-o", "jsonpath={.status.succeeded},{.status.failed}")
		if err == nil {
			ok, failed, _ := strings.Cut(strings.TrimSpace(string(out)), ",")
			if ok != "" && ok != "0" {
				break
			}
			if failed != "" && failed != "0" {
				verdict = fmt.Errorf("job %s failed", name)
				break
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			verdict = fmt.Errorf("job %s did not finish within %s", name, timeout)
			break
		}
		time.Sleep(2 * time.Second)
	}
	// the log, after the verdict and on a context of its own: a step that ran
	// out of time still says what its pod did
	lctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	logs, _ := in.kubectl(lctx, nil, nil, "logs", "job/"+name, "--tail=50")
	for l := range strings.SplitSeq(strings.TrimSpace(string(logs)), "\n") {
		if l != "" {
			events(name + ": " + l)
		}
	}
	return verdict
}

// resolver reads ${secret:<name>/<key>} from the cluster's Secrets.
func (in *Installation) resolver(ctx context.Context) func(string) (string, error) {
	cache := map[string]map[string]string{}
	return func(ref string) (string, error) {
		name, key, ok := strings.Cut(ref, "/")
		if !ok {
			return "", fmt.Errorf("${secret:%s}: <name>/<key>", ref)
		}
		data, ok := cache[name]
		if !ok {
			out, err := in.kubectl(ctx, nil, nil, "get", "secret", name, "-o", "json")
			if err != nil {
				return "", err
			}
			var s struct {
				Data map[string]string `json:"data"`
			}
			if err := json.Unmarshal(out, &s); err != nil {
				return "", err
			}
			data = s.Data
			cache[name] = data
		}
		enc, ok := data[key]
		if !ok {
			return "", fmt.Errorf("secret %s has no key %s", name, key)
		}
		b, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
}

func selector(labels map[string]any) string {
	var parts []string
	for _, k := range []string{labelName, labelInstance, labelManagedBy} {
		parts = append(parts, k+"="+labels[k].(string))
	}
	return strings.Join(parts, ",")
}

// dnsLabel is s as a Kubernetes name: lower case, [a-z0-9-], at most 63.
func dnsLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		}
		return '-'
	}, s)
	if len(s) > 63 {
		s = s[:63]
	}
	return strings.Trim(s, "-")
}

var (
	_ target.Target       = Target{}
	_ target.Capable      = Target{}
	_ target.Installation = (*Installation)(nil)
	_ target.Executor     = (*Installation)(nil)
)
