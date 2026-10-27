package docker

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ks-tool/pack/render"
	"github.com/ks-tool/pack/target"
)

const (
	defaultTimeout = 10 * time.Minute
	stopGrace      = "30"
	healthInterval = "5s"
	healthTimeout  = "3s"
	healthRetries  = 3
)

// docker runs the docker CLI against the installation's engine; stdin, when
// set, is what - reads, env is added to the CLI's environment. Output goes to
// events line by line.
func (in *Installation) docker(ctx context.Context, stdin io.Reader, env []string, events func(string), args ...string) ([]byte, error) {
	cmd := in.command(ctx, args...)
	cmd.Stdin = stdin
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
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
		return stdout.Bytes(), fmt.Errorf("docker %s: %s", args[0], msg)
	}
	return stdout.Bytes(), nil
}

// command is the docker CLI with the engine's host and context.
func (in *Installation) command(ctx context.Context, args ...string) *exec.Cmd {
	var base []string
	if in.host != "" {
		base = append(base, "--host", in.host)
	}
	if in.context != "" {
		base = append(base, "--context", in.context)
	}
	return exec.CommandContext(ctx, in.settings.Get("docker", "docker"), append(base, args...)...)
}

// lines runs docker and returns its output's non-empty lines.
func (in *Installation) lines(ctx context.Context, args ...string) ([]string, error) {
	out, err := in.docker(ctx, nil, nil, nil, args...)
	if err != nil {
		return nil, err
	}
	var ls []string
	for l := range strings.SplitSeq(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			ls = append(ls, l)
		}
	}
	return ls, nil
}

func (in *Installation) filters(labels ...string) []string {
	args := []string{"--filter", "label=" + labelInstallation + "=" + in.installation}
	for _, l := range labels {
		args = append(args, "--filter", "label="+l)
	}
	return args
}

// ReadInstalled reads the record from the installation's state.
func (in *Installation) ReadInstalled() ([]byte, bool, error) {
	return in.state.read(context.Background())
}

// WriteInstalled keeps the record in the installation's state.
func (in *Installation) WriteInstalled(record []byte) error {
	return in.state.write(context.Background(), record)
}

// Execute performs one step of the saga on the engine.
func (in *Installation) Execute(ctx context.Context, st target.Step, events func(string)) error {
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
		return in.apply(ctx, *st.Unit, st.Credentials, events)
	case "wait":
		if st.Unit == nil {
			return fmt.Errorf("wait %s: no unit", st.Key)
		}
		cs, err := in.containers(*st.Unit)
		if err != nil {
			return err
		}
		var names []string
		for _, c := range cs {
			names = append(names, c.Name)
		}
		return in.wait(ctx, names, st.Unit.RestartPolicy == "Never", timeout, events)
	case "stop":
		names, err := in.lines(ctx, append([]string{"ps", "--format", "{{.Names}}"}, in.filters(labelKey+"="+st.Key, labelPart+"=workload")...)...)
		if err != nil || len(names) == 0 {
			return err
		}
		_, err = in.docker(ctx, nil, nil, events, append([]string{"stop", "--time", stopGrace}, names...)...)
		return err
	case "remove":
		names, err := in.lines(ctx, append([]string{"ps", "-a", "--format", "{{.Names}}"}, in.filters(labelKey+"="+st.Key)...)...)
		if err != nil || len(names) == 0 {
			return err
		}
		_, err = in.docker(ctx, nil, nil, events, append([]string{"rm", "--force"}, names...)...)
		return err
	case "hook":
		return in.hook(ctx, st, timeout, events)
	}
	return fmt.Errorf("step %d: %q is no step kind", st.N, st.Kind)
}

// apply brings the key's containers to what the unit says: a container whose
// hash is the unit's stays (started, if it was stopped), any other is
// recreated, and a replica beyond the unit's count is removed.
func (in *Installation) apply(ctx context.Context, u render.Unit, creds []render.Credential, events func(string)) error {
	if err := in.keep(ctx, creds); err != nil {
		return err
	}
	if err := in.ensureNetwork(ctx); err != nil {
		return err
	}
	cs, err := in.containers(u)
	if err != nil {
		return err
	}
	resolve := in.resolver(ctx, creds)
	var want []string
	for _, c := range cs {
		want = append(want, c.Name)
		m, err := in.materialize(c, resolve)
		if err != nil {
			return fmt.Errorf("%s: %w", c.Name, err)
		}
		rows, err := in.lines(ctx, "ps", "-a", "--filter", "name=^/"+c.Name+"$", "--format", `{{.Label "`+labelHash+`"}} {{.State}}`)
		if err != nil {
			return err
		}
		if len(rows) == 1 {
			hash, state, _ := strings.Cut(rows[0], " ")
			if hash == m.hash {
				if state != "running" && c.Restart != "no" {
					_, err = in.docker(ctx, nil, nil, events, "start", c.Name)
				}
				if err != nil {
					return err
				}
				continue
			}
			if _, err := in.docker(ctx, nil, nil, nil, "rm", "--force", c.Name); err != nil {
				return err
			}
		}
		if err := in.run(ctx, m, events); err != nil {
			return err
		}
	}
	have, err := in.lines(ctx, append([]string{"ps", "-a", "--format", "{{.Names}}"}, in.filters(labelKey+"="+u.Key, labelPart+"=workload")...)...)
	if err != nil {
		return err
	}
	var extra []string
	for _, n := range have {
		if !slices.Contains(want, n) {
			extra = append(extra, n)
		}
	}
	if len(extra) > 0 {
		_, err = in.docker(ctx, nil, nil, events, append([]string{"rm", "--force"}, extra...)...)
	}
	return err
}

func (in *Installation) ensureNetwork(ctx context.Context) error {
	if _, err := in.docker(ctx, nil, nil, nil, "network", "inspect", in.network); err == nil {
		return nil
	}
	_, err := in.docker(ctx, nil, nil, nil, "network", "create", "--label", labelInstallation+"="+in.installation, in.network)
	return err
}

// materialized is a container ready to create: docker create's arguments,
// the variables its secret env takes from the CLI's environment, the files
// copied in before it starts, and a hash of all of it.
type materialized struct {
	name, key string
	volumes   []string
	args      []string
	env       []string
	files     []byte // a tar
	hash      string
}

func (in *Installation) materialize(c container, resolve func(string) (string, error)) (*materialized, error) {
	m := &materialized{name: c.Name, key: c.Key}
	a := []string{"--name", c.Name, "--hostname", c.Name, "--network", in.network, "--restart", c.Restart}
	for _, al := range c.Aliases {
		a = append(a, "--network-alias", al)
	}
	sum := sha256.New()
	for _, e := range c.Env {
		if e.Secret == nil {
			a = append(a, "--env", e.Name+"="+e.Value)
			continue
		}
		v, err := resolve(e.Secret.Name + "/" + e.Secret.Key)
		if err != nil {
			return nil, fmt.Errorf("env %s: %w", e.Name, err)
		}
		// the value reaches docker through its environment, never its argv
		a = append(a, "--env", e.Name)
		m.env = append(m.env, e.Name+"="+v)
		fmt.Fprintf(sum, "env %s=%s\n", e.Name, v)
	}
	for _, p := range c.Ports {
		if host, ok := c.Publish[p.Name]; ok {
			a = append(a, "--publish", fmt.Sprintf("%d:%d", host, p.Port))
		}
	}
	for _, v := range c.Volumes {
		a = append(a, "--mount", "type=volume,source="+v.Source+",target="+v.Target)
		m.volumes = append(m.volumes, v.Source)
	}
	for _, l := range c.Libraries {
		a = append(a, "--mount", "type=image,source="+l.Source+",target="+l.Target)
	}
	if c.CPUs != "" {
		a = append(a, "--cpus", c.CPUs)
	}
	if c.CPUShares > 0 {
		a = append(a, "--cpu-shares", strconv.FormatInt(c.CPUShares, 10))
	}
	if c.Memory > 0 {
		a = append(a, "--memory", strconv.FormatInt(c.Memory, 10))
	}
	if c.MemReservation > 0 {
		a = append(a, "--memory-reservation", strconv.FormatInt(c.MemReservation, 10))
	}
	if len(c.Health) > 0 {
		cmd := c.Health[1]
		if c.Health[0] == "CMD" {
			cmd = shellJoin(c.Health[1:])
		}
		a = append(a, "--health-cmd", cmd, "--health-interval", healthInterval,
			"--health-timeout", healthTimeout, "--health-retries", strconv.Itoa(healthRetries))
	}
	for _, k := range sortedKeys(c.Labels) {
		a = append(a, "--label", k+"="+c.Labels[k])
	}
	if len(c.Entrypoint) > 0 {
		a = append(a, "--entrypoint", c.Entrypoint[0])
	}
	tail := []string{c.Image}
	if len(c.Entrypoint) > 1 {
		tail = append(tail, c.Entrypoint[1:]...)
	}
	tail = append(tail, c.Cmd...)

	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	put := func(p string, mode *os.FileMode, data []byte) error {
		perm := int64(0o644)
		if mode != nil {
			perm = int64(mode.Perm())
		}
		fmt.Fprintf(sum, "file %s %o %d\n", p, perm, len(data))
		sum.Write(data)
		if err := tw.WriteHeader(&tar.Header{Name: strings.TrimPrefix(p, "/"), Mode: perm, Size: int64(len(data)),
			Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	for _, f := range c.Files {
		s, err := substitute(string(f.Data), resolve)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		if err := put(f.Path, f.Mode, []byte(s)); err != nil {
			return nil, err
		}
	}
	for _, f := range c.SecretFiles {
		v, err := resolve(f.Ref.Name + "/" + f.Ref.Key)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		if err := put(f.Path, f.Mode, []byte(v)); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if len(c.Files)+len(c.SecretFiles) > 0 {
		m.files = tb.Bytes()
	}
	for _, s := range append(slices.Clone(a), tail...) {
		fmt.Fprintf(sum, "arg %s\n", s)
	}
	m.hash = hex.EncodeToString(sum.Sum(nil))[:16]
	m.args = append(append(a, "--label", labelHash+"="+m.hash), tail...)
	return m, nil
}

// run creates the container — its claims first, volumes of the
// installation that outlive it — copies its files in over the image, starts
// it.
func (in *Installation) run(ctx context.Context, m *materialized, events func(string)) error {
	for _, v := range m.volumes {
		if _, err := in.docker(ctx, nil, nil, nil, "volume", "create", "--label", labelInstallation+"="+in.installation,
			"--label", labelKey+"="+m.key, "--label", labelPart+"=data", v); err != nil {
			return err
		}
	}
	if _, err := in.docker(ctx, nil, m.env, nil, append([]string{"create", "--quiet"}, m.args...)...); err != nil {
		return err
	}
	if m.files != nil {
		if _, err := in.docker(ctx, bytes.NewReader(m.files), nil, nil, "cp", "-", m.name+":/"); err != nil {
			return err
		}
	}
	_, err := in.docker(ctx, nil, nil, nil, "start", m.name)
	if err == nil && events != nil {
		events("started " + m.name)
	}
	return err
}

// wait waits until every container runs and, with a healthcheck, is healthy;
// one that runs to completion until it exits 0. Then, or when it gives up,
// it tells the containers' logs.
func (in *Installation) wait(ctx context.Context, names []string, completes bool, timeout time.Duration, events func(string)) error {
	deadline := time.Now().Add(timeout)
	var verdict error
	for {
		rows, err := in.lines(ctx, append([]string{"inspect", "--format",
			`{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}-{{end}} {{.State.ExitCode}}`}, names...)...)
		if err == nil && len(rows) == len(names) {
			ready, failed := 0, ""
			for i, r := range rows {
				f := strings.Fields(r)
				status, health, code := f[0], f[1], f[2]
				switch {
				case completes && status == "exited" && code == "0":
					ready++
				case completes && (status == "exited" || status == "dead"):
					failed = fmt.Sprintf("%s exited %s", names[i], code)
				case !completes && status == "running" && (health == "-" || health == "healthy"):
					ready++
				}
			}
			if failed != "" {
				verdict = fmt.Errorf("%s", failed)
				break
			}
			if ready == len(names) {
				break
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			verdict = fmt.Errorf("%s not ready within %s", strings.Join(names, ", "), timeout)
			break
		}
		time.Sleep(2 * time.Second)
	}
	if verdict != nil || completes {
		in.logs(names, events)
	}
	return verdict
}

// logs tells the tail of each container's log, on a context of its own: a
// step that ran out of time still says what its containers did.
func (in *Installation) logs(names []string, events func(string)) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, n := range names {
		var b bytes.Buffer
		cmd := in.command(ctx, "logs", "--tail", "50", n)
		cmd.Stdout, cmd.Stderr = &b, &b
		_ = cmd.Run()
		for l := range strings.SplitSeq(strings.TrimSpace(b.String()), "\n") {
			if l != "" {
				events(n + ": " + l)
			}
		}
	}
}

// hook runs a hook as a container of its own: replica 0 of the key — its
// files, env and libraries — with the hook's image and command; an exclusive
// hook gets replica 0's volumes.
func (in *Installation) hook(ctx context.Context, st target.Step, timeout time.Duration, events func(string)) error {
	if st.Unit == nil {
		return fmt.Errorf("hook %s: no unit", st.Name)
	}
	if err := in.keep(ctx, st.Credentials); err != nil {
		return err
	}
	if err := in.ensureNetwork(ctx); err != nil {
		return err
	}
	c, err := in.container(*st.Unit, 0)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%s-%s-%d", in.service(st.Key), st.Name, st.N)
	if st.Rollback {
		name = fmt.Sprintf("%s-undo-%s-%d", in.service(st.Key), st.Name, st.N)
	}
	c.Name, c.Part, c.Image, c.Entrypoint, c.Cmd = containerName(name), "hook", st.Image, st.Command, nil
	c.Aliases, c.Publish, c.Health, c.Restart = nil, nil, nil, "no"
	c.Labels[labelPart] = "hook"
	if !st.Exclusive {
		c.Volumes = nil
	}
	m, err := in.materialize(c, in.resolver(ctx, st.Credentials))
	if err != nil {
		return err
	}
	_, _ = in.docker(ctx, nil, nil, nil, "rm", "--force", c.Name)
	if err := in.run(ctx, m, nil); err != nil {
		return err
	}
	if err := in.wait(ctx, []string{c.Name}, true, timeout, events); err != nil {
		return fmt.Errorf("hook %s: %w", st.Name, err)
	}
	_, _ = in.docker(ctx, nil, nil, nil, "rm", "--force", c.Name)
	return nil
}

// keep stores the step's credentials in the installation's state.
func (in *Installation) keep(ctx context.Context, creds []render.Credential) error {
	for _, c := range creds {
		if err := in.state.keep(ctx, c.Name, c.Leaf, c.Value); err != nil {
			return err
		}
	}
	return nil
}

// resolver reads ${secret:<name>/<key>}: the step's credentials, else the
// ones kept in the state, else the environment's secrets_<name>_<key> (its
// file, its Vault KV).
func (in *Installation) resolver(ctx context.Context, creds []render.Credential) func(string) (string, error) {
	return func(ref string) (string, error) {
		name, key, ok := strings.Cut(ref, "/")
		if !ok {
			return "", fmt.Errorf("${secret:%s}: <name>/<key>", ref)
		}
		for _, c := range creds {
			if c.Name == name && c.Leaf == key {
				return c.Value, nil
			}
		}
		v, ok, err := in.state.secret(ctx, name, key)
		if err != nil || ok {
			return v, err
		}
		setting := strings.ToLower("secrets_" + name + "_" + key)
		if v, ok := in.settings[setting]; ok {
			return v, nil
		}
		return "", fmt.Errorf("secret %s/%s: not kept in the state, and the environment has no %s", name, key, setting)
	}
}

// substitute replaces ${secret:<name>/<key>} with what resolve says.
func substitute(s string, resolve func(string) (string, error)) (string, error) {
	var b strings.Builder
	for {
		i := strings.Index(s, "${secret:")
		if i < 0 {
			b.WriteString(s)
			return b.String(), nil
		}
		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			b.WriteString(s)
			return b.String(), nil
		}
		v, err := resolve(s[i+len("${secret:") : i+j])
		if err != nil {
			return "", err
		}
		b.WriteString(s[:i])
		b.WriteString(v)
		s = s[i+j+1:]
	}
}

// containerName is s as a container or volume name: [a-zA-Z0-9][a-zA-Z0-9_.-]*.
func containerName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			return r
		}
		return '-'
	}, s)
	return strings.TrimLeft(s, "_.-")
}

// shellJoin quotes argv for sh -c.
func shellJoin(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

var (
	_ target.Target       = Target{}
	_ target.Capable      = Target{}
	_ target.Installation = (*Installation)(nil)
	_ target.Executor     = (*Installation)(nil)
)
