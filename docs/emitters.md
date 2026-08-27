# Emitters

An emitter turns a values subtree into the bytes of a file. Every format
but `template` is serialisation — it cannot produce a malformed file; keys
are sorted; nothing depends on the clock. Rendering the same subtree twice
gives the same bytes, which is what lets a Secret be named by its content.

| format | rule | example |
|---|---|---|
| `json` | two-space indent, sorted keys | — |
| `yaml` | block style, sorted keys (ClickHouse accepts YAML configs natively; attributes are `@replace` keys) | — |
| `properties` | nested objects flattened with dots; lists joined by commas; **keys** escape `=`, `:`, space, `#`, `!`; **values** escape only `\`, line breaks and a leading space, so `host:port` stays readable | `server.1=h1:2888:3888` |
| `env` | flattened with `_`, upper-cased, `-` and `.` → `_`; quoted when the value has spaces or quotes | `LOG_DIRS=/var/lib/kafka` |
| `shell` | `env` with `export ` in front and single-quoting for anything the shell would interpret — a file that is *sourced* (`hadoop-env.sh`, `java.env`) | `export HADOOP_OPTS='-Djava.library.path=… $HADOOP_OPTS'` |
| `ini` | objects are sections; an object inside a section is a nested section with one more pair of brackets, the way configobj (`hue.ini`) reads it; scalars before the first section are global | `[desktop]\n[[database]]\nengine=…` |
| `flagfile` | `--<key>=<value>`, one leaf per line, key verbatim (gflags names are the application's vocabulary); `true`/`false`, decimal numbers, comma-joined lists; an absent value — no line; nesting is refused; a value with `\n`, `\r` or NUL is refused — one assignment per line, a line break would append flags | `--mem_limit=52428m` |
| `lines` | a list of scalars, one per line (a scalar alone is one line); an item with a line break is refused | trino `jvm.config`: `-server\n-Xmx1G` |
| `hadoop-xml` | `<configuration><property><name/><value/></property>…`, one level, XML-escaped | HDFS, Hive, HBase, Ranger |
| `text` | a string leaf as is, newline-terminated | `myid`, a `VERSION` file |
| `template` | Go `text/template` with `missingkey=error`, no functions beyond the built-ins, no environment, files or network; the result is parsed back as `parseAs` | JAAS, log4j |

Not an emitter, but the same annotation: **`format: secret`** on a
`format: password` leaf mounts the referenced Secret's key at the path,
bytes untouched — a keytab, a keystore, a certificate. Nothing is rendered
and no placeholder is involved; `perInstance` makes the key
`<key>.${horchestra.index}` (a keytab per host). See [output.md](output.md).

## Template data

For a file whose node is an object, the object's fields are the top-level
data; for a scalar node, it is under `value`. In both cases `values` (the
whole component subtree) and `horchestra` (the context) are available:

```
username="{{ .username }}"                       a field of the subtree
{{ .values.conf.dataDir }}                       any value of the component
{{ .horchestra.installation }}                   the context
{{ .horchestra.ordinal }}                        only in a perInstance file
```

## Which format for which application

| application | format |
|---|---|
| Kafka, ZooKeeper, Java `.properties` | `properties` |
| Postgres `postgresql.conf` | `properties` (`key=value`, `*` and paths unescaped) |
| Impala, gflags binaries | `flagfile` |
| Hadoop, Hive, HBase, Ranger `*-site.xml` | `hadoop-xml` |
| ClickHouse | `yaml` |
| systemd-style `EnvironmentFile` | `env` |
| sourced shell files (`hadoop-env.sh`, `spark-env.sh`, `java.env`) | `shell` |
| `.ini` (MySQL, PHP, Python tools), configobj (`hue.ini`) | `ini` |
| one flag per line (`jvm.config`) | `lines` |
| one value in a file | `text` |
| keytabs, keystores, certificates | `secret` (a mount, not a rendering) |
| anything else | `template` + `parseAs` |
