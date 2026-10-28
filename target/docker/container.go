package docker

import (
	"fmt"
	"io/fs"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/ks-tool/pack/render"
)

const (
	labelInstallation = "pack.horchestra.io/installation"
	labelKey          = "pack.horchestra.io/key"
	labelPart         = "pack.horchestra.io/part" // workload | hook | data | files | publish | record | secret
	labelInstance     = "pack.horchestra.io/instance"
	labelHash         = "pack.horchestra.io/hash"
	labelContainer    = "pack.horchestra.io/container"
)

// container is one container of a key as its engine runs it — the one model
// both `pack template` and the steps (docker create) are made from.
type container struct {
	Engine   *engine
	Name     string
	Key      string
	Instance int
	Part     string
	Image    string
	// Entrypoint and Cmd: the unit's command and args.
	Entrypoint, Cmd []string
	Env             []render.EnvVar
	Ports           []render.Port
	Publish         map[string]int // port name → host port
	// Aliases are its other names on the network: on one engine, the key's.
	Aliases        []string
	Restart        string // unless-stopped | no
	CPUs           string
	CPUShares      int64
	Memory         int64
	MemReservation int64
	Health         []string // a shell command
	Labels         map[string]string
	Volumes        []mount // claims: volume → path
	// the runtime half (runtime.go)
	Hostname, Domain string
	IP, User         string
	DNS, GroupAdd    []string
	SecurityOpt      []string
	Sysctls          map[string]string
	Shared           []shared
	Binds            []bind
	// Completes: it runs to completion, whatever its restart policy
	Completes bool
	Libraries []mount // image → path
	// Files are the instance's config files, ${secret:…} unresolved, kept in
	// the container's files volume on its host and mounted file by file.
	Files       []file
	SecretFiles []secretFile
}

type mount struct{ Source, Target string }

type file struct {
	Path string
	Mode *fs.FileMode
	Data []byte
}

type secretFile struct {
	Path string
	Mode *fs.FileMode
	Ref  render.SecretRef
}

// containers are a key's containers: <service> for a key that does not
// scale, <service>-<i> for each replica.
func (in *Installation) containers(u render.Unit) ([]container, error) {
	var out []container
	for i := range max(u.Replicas, 1) {
		c, err := in.container(u, i)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (in *Installation) container(u render.Unit, i int) (container, error) {
	svc := in.serviceOf(u.Key, u.Platform)
	e, err := in.engineOf(u, i)
	if err != nil {
		return container{}, err
	}
	c := container{Engine: e, Name: svc, Key: u.Key, Instance: i, Part: "workload", Image: u.Image,
		Entrypoint: u.Command, Cmd: u.Args, Env: u.Env, Ports: u.Ports, Restart: "unless-stopped"}
	if u.Replicas > 0 {
		c.Name = fmt.Sprintf("%s-%d", svc, i)
		if !in.spread() {
			c.Aliases = []string{svc}
		}
	}
	if u.RestartPolicy == "Never" {
		c.Restart, c.Completes = "no", true
	}
	if r := strOf(u.Platform, "restart"); r != "" {
		c.Restart = r
	}
	c.Aliases = append(c.Aliases, listOf(u.Platform, "aliases")...)
	if h := strOf(u.Platform, "hostname"); h != "" && u.Replicas == 0 {
		c.Hostname, c.Domain = splitHost(h)
	} else if !in.spread() {
		c.Hostname, c.Domain = c.Name, in.network
	} else {
		c.Hostname = c.Name
	}
	c.IP, c.User = strOf(u.Platform, "ip"), strOf(u.Platform, "user")
	c.DNS, c.GroupAdd, c.SecurityOpt = listOf(u.Platform, "dns"), listOf(u.Platform, "groupAdd"), listOf(u.Platform, "securityOpt")
	if c.Sysctls = mapOf(u.Platform, "sysctls"); len(c.Sysctls) == 0 {
		c.Sysctls = nil
	}
	if c.Shared, c.Binds, err = in.parseMounts(u.Key, listOf(u.Platform, "mounts")); err != nil {
		return c, err
	}
	if in.spread() {
		// a neighbour reaches the instance at <host address>:<port>
		c.Publish = map[string]int{}
		for _, p := range u.Ports {
			c.Publish[p.Name] = p.Port
		}
	} else if pub, ok := u.Platform["publish"].(map[string]any); ok {
		c.Publish = map[string]int{}
		for name, v := range pub {
			if !slices.ContainsFunc(u.Ports, func(p render.Port) bool { return p.Name == name }) {
				return c, fmt.Errorf("publish: %s is no port of %s", name, u.Key)
			}
			port, _ := toInt(v)
			c.Publish[name] = port
		}
	}
	if r := u.Resources; r != nil {
		var err error
		if q, ok := r.Limits["cpu"]; ok {
			cpu, e := quantity(q)
			c.CPUs, err = strconv.FormatFloat(cpu, 'f', -1, 64), e
		}
		if q, ok := r.Requests["cpu"]; ok && err == nil {
			cpu, e := quantity(q)
			c.CPUShares, err = int64(math.Round(cpu*1024)), e
		}
		if q, ok := r.Limits["memory"]; ok && err == nil {
			m, e := quantity(q)
			c.Memory, err = int64(m), e
		}
		if q, ok := r.Requests["memory"]; ok && err == nil {
			m, e := quantity(q)
			c.MemReservation, err = int64(m), e
		}
		if err != nil {
			return c, fmt.Errorf("resources: %w", err)
		}
	}
	if rd := u.Readiness; rd != nil {
		// probed at the container's own name, not 127.0.0.1: a service bound
		// to the address its name resolves to (hdfs://adh-namenode:8020 binds
		// that one) answers there, and so does one bound to every address
		host := c.Name
		switch {
		case rd.TCP != nil:
			p := strconv.Itoa(rd.TCP.Port)
			c.Health = []string{"CMD-SHELL", "nc -z " + host + " " + p + " || bash -c '</dev/tcp/" + host + "/" + p + "'"}
		case rd.HTTP != nil:
			url := fmt.Sprintf("http://%s:%d%s", host, rd.HTTP.Port, rd.HTTP.Path)
			c.Health = []string{"CMD-SHELL", "curl -fsS -o /dev/null " + url + " || wget -q -O /dev/null " + url}
		case len(rd.Exec) > 0:
			c.Health = append([]string{"CMD"}, rd.Exec...)
		}
	}
	c.Labels = map[string]string{}
	maps.Copy(c.Labels, u.Annotations)
	maps.Copy(c.Labels, u.Labels)
	c.Labels[labelInstallation] = in.installation
	c.Labels[labelKey] = u.Key
	c.Labels[labelPart] = c.Part
	c.Labels[labelInstance] = strconv.Itoa(i)

	for _, d := range u.Data {
		c.Volumes = append(c.Volumes, mount{d.Name + "-" + c.Name, d.Path})
	}
	for _, l := range u.Libraries {
		c.Libraries = append(c.Libraries, mount{l.Image, l.Path})
	}
	for _, f := range u.Files {
		data := f.Content
		if f.PerInstance() {
			if i >= len(f.Instances) || f.Instances[i] == nil {
				continue // the instance's section was left empty: no file
			}
			data = f.Instances[i]
		}
		c.Files = append(c.Files, file{f.Path, f.Mode, data})
	}
	for _, f := range u.SecretFiles {
		ref := f.Secret
		if f.PerInstance {
			ref.Key = fmt.Sprintf("%s.%d", ref.Key, i)
		}
		c.SecretFiles = append(c.SecretFiles, secretFile{f.Path, f.Mode, ref})
	}
	return c, nil
}

// quantity reads a Kubernetes-style quantity: 500m, 2, 512Mi, 1G.
func quantity(s string) (float64, error) {
	units := []struct {
		suffix string
		mul    float64
	}{
		{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
		{"k", 1e3}, {"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}, {"m", 1e-3},
	}
	mul := 1.0
	num := s
	for _, u := range units {
		if rest, ok := strings.CutSuffix(s, u.suffix); ok {
			num, mul = rest, u.mul
			break
		}
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is no quantity", s)
	}
	return v * mul, nil
}
