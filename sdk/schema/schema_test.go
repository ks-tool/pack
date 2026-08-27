package schema

import (
	"encoding/json"
	"testing"

	"github.com/ks-tool/pack/sdk/platform"
)

const kafka = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object", "additionalProperties": false,
  "x-pack": {"readiness": {"tcp": "broker"}, "peers": {"zk": {"schema": "zookeeper", "required": true}}, "requires": {"jdk": ">=21"}},
  "properties": {
    "ports": {"type": "object", "properties": {"broker": {"type": "integer", "const": 9092}, "internal": {"type": "integer", "const": 9093}}},
    "scale": {"type": "string", "const": "replicas"},
    "platform": {"$ref": "horchestra://platform/v1#/$defs/component", "default": {"replicas": 3, "resources": {"requests": {"cpu": "1", "memory": "2Gi"}}}},
    "jdk": {"$ref": "pack://jdk#", "x-pack-mount": "/opt/java"},
    "conf": {
      "type": "object", "additionalProperties": false,
      "x-pack-file": {"path": "/opt/kafka/config/server.properties", "format": "properties"},
      "properties": {
        "log.dirs": {"type": "string", "default": "/var/lib/kafka/data"},
        "min.insync.replicas": {"type": "integer", "minimum": 1, "default": 1, "x-pack-arg": {"flag": "--override", "prefix": "min.insync.replicas="}},
        "zookeeper.connect": {"type": "string", "x-pack-compute": "peers.zk.instances.map(h, h + ':2181').join(',')"}
      }
    },
    "jaas": {
      "type": "object", "required": ["password"],
      "x-pack-file": {"path": "/opt/kafka/config/kafka_jaas.conf", "format": "template", "template": "jaas.tmpl", "parseAs": "properties", "mode": "0400"},
      "properties": {
        "mechanism": {"type": "string", "enum": ["SCRAM-SHA-512", "PLAIN"], "default": "SCRAM-SHA-512"},
        "password": {"type": "string", "format": "password"}
      }
    },
    "heapOpts": {"type": "string", "x-pack-env": "KAFKA_HEAP_OPTS", "x-pack-compute": "'-Xmx' + string(values.jdk.heapMB) + 'm'"}
  }
}`

const jdk = `{"type": "object", "additionalProperties": false,
  "properties": {"gc": {"type": "string", "enum": ["G1", "ZGC"], "default": "G1"},
                 "heapMB": {"type": "integer", "minimum": 128, "default": 1024, "x-pack-compute": "has(platform.resources.limits.memory) ? quantity(platform.resources.limits.memory) / 1000 / 1048576 * 3 / 4 : null"}}}`

func parseKafka(t *testing.T) *Schema {
	t.Helper()
	s, err := Parse([]byte(kafka))
	if err != nil {
		t.Fatal(err)
	}
	lib, err := Parse([]byte(jdk))
	if err != nil {
		t.Fatal(err)
	}
	s.Foreign["pack://jdk"] = lib
	if err := s.AttachBuiltins(); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestParse_AnnotationsAndOrder(t *testing.T) {
	s := parseKafka(t)
	if got := s.Root.Order; len(got) != 7 || got[0] != "ports" || got[4] != "conf" || got[6] != "heapOpts" {
		t.Fatalf("document order lost: %v", got)
	}
	conf := s.Root.Properties["conf"]
	if conf.File == nil || conf.File.Path != "/opt/kafka/config/server.properties" || conf.File.Format != "properties" {
		t.Fatalf("x-pack-file not read: %+v", conf.File)
	}
	if a := conf.Properties["min.insync.replicas"].Arg; a == nil || a.Flag != "--override" || a.Prefix != "min.insync.replicas=" {
		t.Fatalf("x-pack-arg not read: %+v", a)
	}
	if s.Root.Properties["heapOpts"].Env != "KAFKA_HEAP_OPTS" || len(s.Root.Properties["heapOpts"].Compute) == 0 {
		t.Fatal("x-pack-env / x-pack-compute not read")
	}
	if s.Root.Properties["jdk"].Mount != "/opt/java" || !s.Root.Properties["jdk"].IsForeign() {
		t.Fatal("library ref not read")
	}
	root, err := s.Root.RootAnnotation()
	if err != nil || root.Readiness.TCP != "broker" || root.Peers["zk"].Schema != "zookeeper" || root.Requires["jdk"] != ">=21" {
		t.Fatalf("x-pack root: %+v %v", root, err)
	}
	if !s.Root.Properties["jaas"].Properties["password"].IsPassword() {
		t.Fatal("password leaf")
	}
}

func TestParse_UnknownAnnotationKeyIsRefused(t *testing.T) {
	_, err := Parse([]byte(`{"type":"object","properties":{"c":{"x-pack-file":{"path":"/a","format":"json","fromat":"x"}}}}`))
	if err == nil {
		t.Fatal("expected refusal of unknown key")
	}
}

func TestDefaults(t *testing.T) {
	s := parseKafka(t)
	d, ok, err := s.Defaults()
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	b, _ := json.Marshal(d)
	want := `{"conf":{"log.dirs":"/var/lib/kafka/data","min.insync.replicas":1},"jaas":{"mechanism":"SCRAM-SHA-512"},"jdk":{"gc":"G1","heapMB":1024},"platform":{"replicas":3,"resources":{"requests":{"cpu":"1","memory":"2Gi"}}},"ports":{"broker":9092,"internal":9093},"scale":"replicas"}`
	if string(b) != want {
		t.Fatalf("defaults:\n got %s\nwant %s", b, want)
	}
}

func TestVisit_FollowsForeignRefsWithInstancePath(t *testing.T) {
	s := parseKafka(t)
	var computes []string
	err := s.Visit(func(ptr string, _ *Schema, n *Node) error {
		if len(n.Compute) > 0 {
			computes = append(computes, ptr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/conf/zookeeper.connect", "/heapOpts", "/jdk/heapMB"}
	if len(computes) != 3 {
		t.Fatalf("computes: %v", computes)
	}
	found := map[string]bool{}
	for _, c := range computes {
		found[c] = true
	}
	for _, w := range want {
		if !found[w] {
			t.Fatalf("missing %s in %v", w, computes)
		}
	}
	_, n, err := s.Lookup("/jdk/heapMB")
	if err != nil || len(n.Compute) == 0 {
		t.Fatalf("lookup through foreign ref: %v %+v", err, n)
	}
}

func TestCompile_PasswordTakesOnlyARef(t *testing.T) {
	s := parseKafka(t)
	c, err := s.Compile("pack://kafka")
	if err != nil {
		t.Fatal(err)
	}
	ok := map[string]any{"scale": "replicas", "platform": map[string]any{"replicas": 3},
		"jdk":  map[string]any{"gc": "G1"},
		"jaas": map[string]any{"password": map[string]any{"secretRef": map[string]any{"name": "s", "key": "k"}}}}
	if v := c.Validate(ok); len(v) != 0 {
		t.Fatalf("valid doc refused: %v", v)
	}
	bad := map[string]any{"jaas": map[string]any{"password": "plaintext"}, "scale": "single", "jdk": map[string]any{"gc": "CMS"}}
	v := c.Validate(bad)
	if len(v) < 3 {
		t.Fatalf("expected refusals for plaintext password, wrong const and bad enum, got %v", v)
	}
	t.Log(v)
	_ = platform.PlatformV1
}
