package docker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/ks-tool/pack/bundle"
	"github.com/ks-tool/pack/environment"
	"github.com/ks-tool/pack/render"
	"github.com/ks-tool/pack/target"
)

// adh renders the adh example into one of its Docker environments.
func adh(t *testing.T, envFile string) (*Installation, *render.Release) {
	t.Helper()
	root := filepath.Join("..", "..", "examples", "adh")
	env, err := environment.Load(context.Background(), filepath.Join(root, "envs", envFile), nil)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := Target{}.Configure("prod", target.Settings(env.Settings))
	if err != nil {
		t.Fatal(err)
	}
	b, err := bundle.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "testdata", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var v render.Values
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	rel, err := render.Render(b, v, render.Context{Installation: "prod", Target: inst})
	if err != nil {
		t.Fatal(err)
	}
	return inst.(*Installation), rel
}

// The adh composition in its Docker environments is this target's golden
// output: on one engine (envs/docker.yaml) under testdata/adh, across three
// hosts (envs/docker-hosts.yaml) under testdata/adh-hosts, compose files
// included. UPDATE_GOLDEN=1 rewrites them.
func TestConvertADH_Golden(t *testing.T) {
	for env, dir := range map[string]string{"docker.yaml": "adh", "docker-hosts.yaml": "adh-hosts"} {
		t.Run(dir, func(t *testing.T) {
			inst, rel := adh(t, env)
			inst.settings["compose"] = "true"
			out, err := inst.Convert(rel, nil)
			if err != nil {
				t.Fatal(err)
			}
			expected := filepath.Join("testdata", dir, "expected")
			if os.Getenv("UPDATE_GOLDEN") != "" {
				_ = os.RemoveAll(expected)
				for _, f := range out.Files {
					p := filepath.Join(expected, filepath.FromSlash(f.Path))
					_ = os.MkdirAll(filepath.Dir(p), 0o755)
					if err := os.WriteFile(p, f.Data, 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, f := range out.Files {
				want, err := os.ReadFile(filepath.Join(expected, filepath.FromSlash(f.Path)))
				if err != nil {
					t.Fatalf("%s: %v — UPDATE_GOLDEN=1 to write it", f.Path, err)
				}
				if string(want) != string(f.Data) {
					t.Errorf("%s differs from golden:\n%s", f.Path, f.Data)
				}
			}
		})
	}
}

// What one engine runs, stated: a container per replica answering to the
// key's name, a per-instance file as each container's own, a library as an
// image mount, a placeholder as it was rendered.
func TestConvertADH_Properties(t *testing.T) {
	inst, rel := adh(t, "docker.yaml")
	out, err := inst.Convert(rel, nil)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, f := range out.Files {
		files[f.Path] = string(f.Data)
	}
	spec := files["containers.yaml"]
	for _, want := range []string{"name: prod-zookeeper-2", "- prod-zookeeper", "network: prod",
		"image: images/jdk:21.0.4", "file: files/prod-zookeeper-1/var/lib/zookeeper/myid"} {
		if !strings.Contains(spec, want) {
			t.Errorf("containers.yaml lacks %q", want)
		}
	}
	for i, id := range []string{"1", "2", "3"} {
		if got := strings.TrimSpace(files[fmt.Sprintf("files/prod-zookeeper-%d/var/lib/zookeeper/myid", i)]); got != id {
			t.Errorf("myid of zookeeper-%d: %q", i, got)
		}
	}
	if !strings.Contains(files["files/prod-kafka-0/opt/kafka/config/kafka_jaas.conf"], "${secret:prod-kafka-sasl/password}") {
		t.Error("a placeholder stays in what is rendered for reading")
	}
	if _, ok := files["compose.yaml"]; ok {
		t.Error("compose only when asked")
	}
}

// Across hosts: an instance at its host's address and the component's port,
// published as itself, a compose file per host.
func TestConvertADH_Hosts(t *testing.T) {
	inst, rel := adh(t, "docker-hosts.yaml")
	inst.settings["compose"] = "true"
	out, err := inst.Convert(rel, nil)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, f := range out.Files {
		files[f.Path] = string(f.Data)
	}
	if got := files["files/prod-kafka-2/opt/kafka/config/server.properties"]; !strings.Contains(got,
		"zookeeper.connect=10.0.0.11:2181,10.0.0.12:2181,node-c.dc1.example:2181/prod") {
		t.Errorf("kafka reaches zookeeper at host:port:\n%s", got)
	}
	a, c := files["compose.a.yaml"], files["compose.c.yaml"]
	for _, want := range []string{"container_name: prod-zookeeper-0", "container_name: prod-kafka-0", "container_name: prod-hive-db", "- 2181:2181", "- 5432:5432"} {
		if !strings.Contains(a, want) {
			t.Errorf("host a lacks %q", want)
		}
	}
	if strings.Contains(c, "5432") || !strings.Contains(c, "container_name: prod-zookeeper-2") {
		t.Errorf("host c:\n%s", c)
	}
}

func TestHosts(t *testing.T) {
	inst, err := Target{}.Configure("dev", target.Settings{
		"hosts_a_address": "10.0.0.1", "hosts_b_address": "b.lan", "hosts_b_host": "ssh://b",
		"platform_zk_hosts": "a,b"})
	if err != nil {
		t.Fatal(err)
	}
	in := inst.(*Installation)
	res, err := in.Resolve("zk", "replicas", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Instances) != 2 || res.Instances[0].Address != "10.0.0.1" || res.Instances[1].Address != "b.lan" || res.Platform["replicas"] != int64(2) {
		t.Errorf("%+v", res)
	}
	for name, platform := range map[string]map[string]any{
		"no hosts":        {},
		"unknown host":    {"hosts": []any{"x"}},
		"one host twice":  {"hosts": []any{"a", "a"}},
		"replicas differ": {"hosts": []any{"a", "b"}, "replicas": 3},
		"publish":         {"hosts": []any{"a"}, "publish": map[string]any{"pg": 1}},
	} {
		if _, err := in.Resolve("pg", "replicas", platform, false); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := in.Resolve("pg", "single", map[string]any{"hosts": []any{"a", "b"}}, false); err == nil {
		t.Error("a key that does not scale on two hosts")
	}
	if _, err := (Target{}).Configure("dev", target.Settings{"hosts_a_host": "ssh://a"}); err == nil {
		t.Error("a host without an address")
	}
}

// Two containers that publish one port on one host are refused.
func TestPortsFree(t *testing.T) {
	a, b := &engine{name: "a"}, &engine{name: "b"}
	pg := []render.Port{{Name: "pg", Port: 5432}}
	c := func(name string, e *engine) container {
		return container{Name: name, Engine: e, Ports: pg, Publish: map[string]int{"pg": 5432}}
	}
	if err := portsFree([]container{c("x", a), c("y", b)}); err != nil {
		t.Error(err)
	}
	if err := portsFree([]container{c("x", a), c("y", a)}); err == nil {
		t.Error("5432 twice on a")
	}
}

func TestPublish(t *testing.T) {
	inst := &Installation{installation: "dev", network: "dev", settings: target.Settings{"platform_pg_publish_pg": "15432"}}
	res, err := inst.Resolve("pg", "single", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Address != "dev-pg" || res.Platform["publish"].(map[string]any)["pg"] != int64(15432) {
		t.Errorf("%+v", res)
	}
	if _, err := inst.Resolve("pg", "replicas", map[string]any{"replicas": 2}, false); err == nil {
		t.Error("two replicas cannot publish one host port")
	}
}

func TestQuantity(t *testing.T) {
	for s, want := range map[string]float64{"500m": 0.5, "2": 2, "512Mi": 512 << 20, "1G": 1e9, "1.5Gi": 1.5 * (1 << 30)} {
		if got, err := quantity(s); err != nil || got != want {
			t.Errorf("%s: %v %v", s, got, err)
		}
	}
	if _, err := quantity("lots"); err == nil {
		t.Error("lots is no quantity")
	}
}

func TestSubstitute(t *testing.T) {
	s, err := substitute("password=${secret:pg/password}\n", func(ref string) (string, error) { return "s3cret", nil })
	if err != nil || s != "password=s3cret\n" {
		t.Errorf("%q %v", s, err)
	}
}

// A state file keeps each installation's record and credentials apart, and
// the engine is never asked.
func TestFileState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "docker.json")
	open := func(name string) *Installation {
		inst, err := Target{}.Configure(name, target.Settings{"state": path, "docker": "/nonexistent/docker"})
		if err != nil {
			t.Fatal(err)
		}
		return inst.(*Installation)
	}
	prod, dev := open("prod"), open("dev")
	if _, exists, err := prod.ReadInstalled(); err != nil || exists {
		t.Fatalf("nothing installed yet: %v %v", exists, err)
	}
	if err := prod.WriteInstalled([]byte(`{"name":"prod","state":"installed"}`)); err != nil {
		t.Fatal(err)
	}
	if err := dev.WriteInstalled([]byte(`{"name":"dev"}`)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := prod.keep(ctx, []render.Credential{{Name: "prod-pg", Leaf: "password", Value: "s3cret"}}); err != nil {
		t.Fatal(err)
	}
	rec, exists, err := prod.ReadInstalled()
	if err != nil || !exists || string(rec) != `{"name":"prod","state":"installed"}` {
		t.Errorf("prod: %s %v %v", rec, exists, err)
	}
	if v, err := prod.resolver(ctx, nil)("prod-pg/password"); err != nil || v != "s3cret" {
		t.Errorf("kept credential: %q %v", v, err)
	}
	if _, ok, _ := dev.state.secret(ctx, "prod-pg", "password"); ok {
		t.Error("dev sees prod's credential")
	}
	if err := prod.WriteInstalled([]byte("not json")); err == nil {
		t.Error("a record that is not JSON is refused")
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("state file mode: %v %v", fi, err)
	}
}

// The installed version is rendered as it was installed: the hosts the
// record kept, not the ones the environment names now — kafka's neighbours
// where zookeeper ran.
func TestRenderInstalled(t *testing.T) {
	root := filepath.Join("..", "..", "examples", "adh")
	inst, rel := adh(t, "docker-hosts.yaml")
	b, err := bundle.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "testdata", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var v render.Values
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	installed := rel.Platforms
	installed["zookeeper"] = map[string]any{"hosts": []any{"c", "b", "a"}, "replicas": 3}
	again, err := render.Render(b, v, render.Context{Installation: "prod", Target: inst, Installed: installed})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, u := range again.Units {
		if u.Key != "kafka" {
			continue
		}
		for _, f := range u.Files {
			found = found || strings.HasSuffix(f.Path, "server.properties")
			if strings.HasSuffix(f.Path, "server.properties") && !strings.Contains(string(f.Content),
				"zookeeper.connect=node-c.dc1.example:2181,10.0.0.12:2181,10.0.0.11:2181/prod") {
				t.Errorf("kafka:\n%s", f.Content)
			}
		}
	}
	if !found {
		t.Error("kafka has no server.properties")
	}
	if fmt.Sprint(again.Platforms["zookeeper"]["hosts"]) != "[c b a]" {
		t.Errorf("platforms: %v", again.Platforms["zookeeper"])
	}
}
