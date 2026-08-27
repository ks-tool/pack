package emit

import "testing"

func TestEmitters(t *testing.T) {
	doc := map[string]any{"log.dirs": "/var/lib/kafka", "server": map[string]any{"1": "h1:2888:3888", "2": "h2:2888:3888"}, "num": 3, "flag": true, "list": []any{"a", "b"}}
	cases := map[string]string{
		"properties": "flag=true\nlist=a,b\nlog.dirs=/var/lib/kafka\nnum=3\nserver.1=h1:2888:3888\nserver.2=h2:2888:3888\n",
		"env":        "FLAG=true\nLIST=a,b\nLOG_DIRS=/var/lib/kafka\nNUM=3\nSERVER_1=h1:2888:3888\nSERVER_2=h2:2888:3888\n",
		"ini":        "flag=true\nlist=a,b\nlog.dirs=/var/lib/kafka\nnum=3\n\n[server]\n1=h1:2888:3888\n2=h2:2888:3888\n",
	}
	for format, want := range cases {
		got, err := Emit(format, doc)
		if err != nil {
			t.Fatal(format, err)
		}
		if string(got) != want {
			t.Errorf("%s:\n got %q\nwant %q", format, got, want)
		}
	}
	if _, err := Emit("flagfile", doc); err == nil {
		t.Error("flagfile must refuse a nested object")
	}
	flat := map[string]any{"mem_limit": "52428m", "enable_ldap_auth": true, "opts": []any{"a", "b"}}
	got, _ := Emit("flagfile", flat)
	if string(got) != "--enable_ldap_auth=true\n--mem_limit=52428m\n--opts=a,b\n" {
		t.Errorf("flagfile: %q", got)
	}
	if _, err := Emit("flagfile", map[string]any{"x": "a\nb"}); err == nil {
		t.Error("flagfile must refuse a line break in a value")
	}
	xml, _ := Emit("hadoop-xml", map[string]any{"fs.defaultFS": "hdfs://nn:8020", "a<b": "x&y"})
	if string(xml) != "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<configuration>\n  <property>\n    <name>a&lt;b</name>\n    <value>x&amp;y</value>\n  </property>\n  <property>\n    <name>fs.defaultFS</name>\n    <value>hdfs://nn:8020</value>\n  </property>\n</configuration>\n" {
		t.Errorf("hadoop-xml: %q", xml)
	}
	out, err := Template("t", []byte("a={{ .a }}\n"), map[string]any{"a": "1"}, "properties")
	if err != nil || string(out) != "a=1\n" {
		t.Errorf("template: %q %v", out, err)
	}
	if _, err := Template("t", []byte("{{ .missing }}"), map[string]any{}, "properties"); err == nil {
		t.Error("a missing key must fail")
	}
	if _, err := Template("t", []byte("not json"), map[string]any{}, "json"); err == nil {
		t.Error("parseAs must refuse garbage")
	}
}

func TestPropertiesEscaping(t *testing.T) {
	got, _ := Emit("properties", map[string]any{"k y=1": " v:al=ue\nx"})
	if string(got) != "k\\ y\\=1=\\ v:al=ue\\nx\n" {
		t.Errorf("%q", got)
	}
}

func TestShellLinesAndNestedINI(t *testing.T) {
	got, err := Emit("shell", map[string]any{"hadoop_home": "/usr/lib/hadoop", "hadoop_opts": "-Djava.library.path=/usr/lib/hadoop/lib/native $HADOOP_OPTS", "heap": 1024})
	if err != nil {
		t.Fatal(err)
	}
	want := "export HADOOP_HOME=/usr/lib/hadoop\nexport HADOOP_OPTS='-Djava.library.path=/usr/lib/hadoop/lib/native $HADOOP_OPTS'\nexport HEAP=1024\n"
	if string(got) != want {
		t.Errorf("shell:\n%s", got)
	}
	if err := Parse("shell", got); err != nil {
		t.Errorf("shell parses back: %v", err)
	}

	got, err = Emit("lines", []any{"-server", "-Xmx1G", "-XX:+UseG1GC"})
	if err != nil || string(got) != "-server\n-Xmx1G\n-XX:+UseG1GC\n" {
		t.Errorf("lines: %q %v", got, err)
	}
	if _, err := Emit("lines", []any{"a\nb"}); err == nil {
		t.Error("lines: a line break inside an item must be refused")
	}
	if _, err := Emit("lines", map[string]any{"a": 1}); err == nil {
		t.Error("lines: an object must be refused")
	}

	got, err = Emit("ini", map[string]any{
		"desktop": map[string]any{"http_port": 8888, "session": map[string]any{"trusted_origins": "x:8446"},
			"database": map[string]any{"engine": "postgresql_psycopg2"}},
		"hadoop": map[string]any{"hdfs_clusters": map[string]any{"default": map[string]any{"fs_defaultfs": "hdfs://nn:8020"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want = "\n[desktop]\nhttp_port=8888\n[[database]]\nengine=postgresql_psycopg2\n[[session]]\ntrusted_origins=x:8446\n\n[hadoop]\n[[hdfs_clusters]]\n[[[default]]]\nfs_defaultfs=hdfs://nn:8020\n"
	if string(got) != want {
		t.Errorf("ini:\n%s", got)
	}
}
