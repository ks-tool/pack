# The SDK: describe a component in code, get the artifact

An author describes a component as a typed value — a Go struct, a Python
dataclass, a Java class — whose **defaults are the values** and whose fields
say what they are (a fact, a setting, the operator's) and where they go (a
file, an argument, an environment variable). The SDK turns that into the
artifact `oci-packer` packs and the pack tool reads: `schema.json`,
`pack.yaml`, `templates/`. All three languages yield the **same schema** for
the same description — the tests prove it on `examples/postgres`.

| language | where | the author writes | generates |
|---|---|---|---|
| Go | `pack/sdk` | a struct with `json`/`pack` tags, embedding `sdk.Workload`; `Meta()`; optional `PackRoot()`, `PackCompute()` | `sdk.Generate`, `sdk.Verify`, `sdk.Run`; `sdk.PackFile` is oci-packer's own `Pack` type |
| Python | `pack/sdk/python` (`packsdk`) | a dataclass with `const()`, `default()`, `operator()`, `secret()`, `file()`, `pack_ref()`; `meta`; optional `root()`, `compute()` | `packsdk.generate`, `packsdk.main` |
| Java | `pack/sdk/java` (`io.horchestra.pack.sdk`) | a class with `@Pack`, `@File`, `@PackRef`; `meta()`; optional `root()`, `compute()` | `PackSdk.generate`, `PackSdk.main` |

## What every component has

| piece | Go | Python | Java |
|---|---|---|---|
| the scale fact and the platform half (horchestra's schema, `$ref` + default) | embed `sdk.Workload`; `Workload: sdk.Single("500m", "512Mi")` or `sdk.Replicated(3, "1", "2Gi")`, `.Limits("1Gi")`, `.Placement(…)` | `scale: str = const(Scale.SINGLE)`, `platform: Platform = default_factory(Platform.sized("500m", "512Mi"))` | `@Pack(kind=CONST) String scale = Scale.SINGLE`, `Platform platform = Platform.sized("500m", "512Mi")` |
| a volume | `sdk.Claim` / `sdk.ClaimAt(path, size)` | `Claim.at(path, size)` | `Claim.at(path, size)` |
| a credential | `sdk.Secret` (+ `pack:"env=…"` or under a file) | `secret(env=…)` | `Secret` field (+ `@Pack(env=…)`) |
| a fact / a setting / the operator's | `pack:"const"` / `pack:"default"` / no tag | `const(v)` / `default(v)` / `operator()` | `@Pack(kind=CONST)` / `DEFAULT` / none |
| a file | `pack:"file=<path>,format=<f>[,template=,parseAs=,mode=,perInstance]"` | `file(value, path=, format=, …)` | `@File(path=, format=, …)` |
| argv, env | `pack:"arg=--flag[,prefix=][,each][,omitEmpty]"`, `pack:"env=NAME"` | `arg=`, `env=` keywords | `@Pack(arg=, argPrefix=, env=)` |
| a library, a composition key | `pack_ref:"jdk" pack:"mount=/opt/java"`, `pack_ref:"kafka" pack:"spread"` | `pack_ref("jdk", mount=…)`, `pack_ref("kafka", spread=True)` (`key(kafka, …)` — by the component's own name) | `@PackRef("jdk") @Pack(mount=…)`, `@PackRef("kafka") @Pack(spread=true)` |
| schema keywords | `jsonschema:"required,title=…,minimum=1,enum=a,enum=b"` | `required=, title=, minimum=, enum=` | `@Pack(required=, title=, minimum="1", enumValues={…})` |
| the root (`x-pack`) | `PackRoot() packschema.Root` | `root() -> Root` | `root() -> Root` |
| formulas | `PackCompute() map[string]string` | `compute() -> dict` | `compute() -> Map` |

The identity — name, version, the image it runs, where the artifact is
published, the templates it ships — is `Meta`, the one thing every component
must provide.

## Go

```go
type PgInit struct {
    sdk.Workload
    DB   DB   `json:"db"`
    Init Init `json:"init" pack:"file=/opt/pack/pg-init.sh,format=template,template=pg-init.sh.tmpl,parseAs=text,mode=0500"`
}

//go:embed templates
var templates embed.FS

func Defaults() PgInit { return PgInit{Workload: sdk.Single("100m", "128Mi"), DB: DB{User: "hive"}} }
func (PgInit) Meta() sdk.Meta {
    return sdk.Meta{Name: "pg-init", Version: "4.3.0", Image: "cr://10.92.16.25:5000/ee/adh/aio:4.3.0",
        Artifact: "cr://10.92.16.25:5000/ee/adh/pg-init", Templates: templates, TemplatesDir: "templates"}
}
func (PgInit) PackRoot() packschema.Root { return packschema.Root{RestartPolicy: "Never", Peers: …} }
```

```go
func main() {
    if err := sdk.Run(pginit.Defaults(), postgres.Defaults(), adhshow.Defaults()); err != nil {   // generate [-o dir] | check | schema <name>
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }
}
```

In a test: `sdk.Verify(pginit.Defaults())` runs the author checks (spec
§2.6) on the generated schema and templates; `sdk.Schema(c)` is the bytes.
`sdk/example` is `examples/postgres` written this way — byte-identical output.

## Python

```python
@dataclass
class Postgres(Component):
    scale: str = const(Scale.SINGLE)
    platform: Platform = default_factory(Platform.sized("500m", "512Mi"))
    ports: Ports = default_factory(Ports)
    superuser: Superuser = default_factory(Superuser)
    conf: Conf = file(Conf(), path="/etc/postgresql/postgresql.conf", format="properties")

    meta = Meta(name="postgres", version="16.3", image="cr://…/postgres:16.3", artifact="cr://…/components/postgres", templates="templates")

    def root(self): return Root(readiness=Readiness(tcp="pg"), upgrade=Upgrade(from_=">=15"))
    def compute(self): return {"/conf/shared_buffers": "…"}

if __name__ == "__main__":
    main(Postgres())        # python postgres.py generate ./out
```

Nested objects are dataclasses given through `default_factory(...)`;
names with dots or reserved words take `json="log.dirs"`. Requires Python
3.11+, no dependencies. Test: `python tests/test_postgres.py` in
`sdk/python` (equality with the Go schema).

## Java

```java
public final class Postgres implements Component {
    @Pack(kind = Pack.Kind.CONST) public String scale = Scale.SINGLE;
    public Platform platform = Platform.sized("500m", "512Mi");
    @File(path = "/etc/postgresql/postgresql.conf", format = "properties") public Conf conf = new Conf();

    public Meta meta() { return new Meta("postgres", "16.3").image("cr://…/postgres:16.3").artifact("cr://…/components/postgres"); }
    public Root root() { return new Root().readiness(Root.Readiness.tcp("pg")).upgrade(">=15"); }
}
```

```java
public static void main(String[] args) throws IOException { PackSdk.main(args, new Postgres()); }
```

Public fields in declaration order are the properties (HotSpot returns
`getDeclaredFields()` in that order); nested classes are objects; `Secret`,
`Platform`, `Claim` are the shared pieces. Maven project, Jackson for JSON;
`mvn test` runs the equality test against the Go schema. **Not yet run on
this machine — no JDK here**; the Go and Python SDKs are.

## The contract

Whatever the language, the output is one `schema.json` per artifact with the
`x-pack-*` vocabulary of `pack schema vocabulary`, and `pack check` is the
judge: the checks of spec §2.6 run on the schema, not on the source. The Go
SDK is the reference (it is `packschema.Reflect`); the others are held to it
by the tests.
