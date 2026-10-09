package compute

import (
	"reflect"
	"testing"
)

func TestReads(t *testing.T) {
	env, err := Env()
	if err != nil {
		t.Fatal(err)
	}
	a, err := Compile(env, `has(limits.memory) ? '-Xmx' + string(values.jdk.heapMB) + peers.zk.instances.map(h, h + ':' + string(peers.zk.ports.client)).join(',') : common.realm`)
	if err != nil {
		t.Fatal(err)
	}
	got := Reads(a)
	want := []string{"limits.memory", "values.jdk.heapMB", "peers.zk.instances", "peers.zk.ports.client", "common.realm"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reads: got %v want %v", got, want)
	}
}

func TestEvalAndFunctions(t *testing.T) {
	env, _ := Env()
	a, err := Compile(env, `indexMap(lists.range(me.replicas).map(i, me.service + '-' + string(i)), 1)`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Eval(env, a, map[string]any{"me": map[string]any{"replicas": 3, "service": "zk"}})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := out.(map[string]any)
	if !ok || len(m) != 3 || m["1"] != "zk-0" || m["3"] != "zk-2" {
		t.Fatalf("indexMap: %#v", out)
	}
	q, _ := Compile(env, `quantity("32Gi") / 1000 / 1048576 * 3 / 4`)
	v, err := Eval(env, q, nil)
	if err != nil || v.(int64) != 24576 {
		t.Fatalf("quantity: %v %v", v, err)
	}
	n, _ := Compile(env, `has(limits.x) ? 1 : null`)
	v, err = Eval(env, n, map[string]any{"limits": map[string]any{}})
	if err != nil || v != nil {
		t.Fatalf("null: %v %v", v, err)
	}
}

func TestQuantity(t *testing.T) {
	for in, want := range map[string]int64{"500m": 500, "2": 2000, "1.5G": 1_500_000_000_000, "32Gi": 32 * (1 << 30) * 1000} {
		got, err := Quantity(in)
		if err != nil || got != want {
			t.Errorf("%s: got %d want %d (%v)", in, got, want, err)
		}
	}
}

func TestResultType(t *testing.T) {
	env, err := Env()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		`string(horchestra.ordinal)`: "string",
		`peers.zk.instances.map(h, h + ':' + string(peers.zk.ports.client)).join(',')`: "string",
		`limits.memory / 2`:         "integer",
		`values.a + 1`:              "integer",
		`values.replicas * 2 > 3`:   "boolean",
		`[1, 2]`:                    "array",
		`{'a': 1}`:                  "object",
		`values.x`:                  "",
		`null`:                      "",
		`values.x > 1 ? 3 : null`:   "integer",
		`values.x > 1 ? null : 'a'`: "string",
		`has(limits.memory) ? string(limits.memory / 4) + 'MB' : null`: "string",
		`values.a ? (values.b ? 1 : null) : null`:                      "integer",
		`[values.a ? 1 : null]`:                                        "array",
	}
	for expr, want := range cases {
		got, err := ResultType(env, expr)
		if err != nil {
			t.Errorf("%s: %v", expr, err)
			continue
		}
		if got != want {
			t.Errorf("%s: got %q, want %q", expr, got, want)
		}
	}
	if _, err := ResultType(env, `1 + "a"`); err == nil {
		t.Error("a genuine type error must be reported")
	}
	// the rewrite works on its own parse: the formula still evaluates with the null branch
	a, _ := Compile(env, `values.x > 1 ? 3 : null`)
	if _, err := ResultType(env, `values.x > 1 ? 3 : null`); err != nil {
		t.Fatal(err)
	}
	out, err := Eval(env, a, map[string]any{"values": map[string]any{"x": 0}})
	if err != nil || out != nil {
		t.Errorf("null branch must survive: %v %v", out, err)
	}
}
