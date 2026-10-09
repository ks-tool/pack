package io.horchestra.pack.sdk.example;

import io.horchestra.pack.sdk.Claim;
import io.horchestra.pack.sdk.Component;
import io.horchestra.pack.sdk.File;
import io.horchestra.pack.sdk.Meta;
import io.horchestra.pack.sdk.Pack;
import io.horchestra.pack.sdk.Platform;
import io.horchestra.pack.sdk.Root;
import io.horchestra.pack.sdk.Scale;
import io.horchestra.pack.sdk.Secret;

import java.util.List;
import java.util.Map;

/** examples/postgres described in Java: the same schema.json the Go struct yields. */
public final class Postgres implements Component {
    public Platform platform = Platform.sized("500m", "512Mi");
    public Ports ports = new Ports();
    public Claims claims = new Claims();
    @Pack(required = true, title = "База данных") public String database;
    public Superuser superuser = new Superuser();
    @File(path = "/etc/postgresql/postgresql.conf", format = "properties") public Conf conf = new Conf();

    public static final class Ports {
        @Pack(kind = Pack.Kind.CONST) public int pg = 5432;
    }

    public static final class Claims {
        public Claim data = Claim.at("/var/lib/postgresql/data", "10Gi");
    }

    public static final class Superuser {
        @Pack(kind = Pack.Kind.DEFAULT, env = "POSTGRES_USER") public String name = "postgres";
        @Pack(required = true, title = "Пароль суперпользователя", env = "POSTGRES_PASSWORD") public Secret password;
    }

    public static final class Conf {
        @Pack(kind = Pack.Kind.DEFAULT, json = "max_connections", minimum = "1") public int maxConnections = 100;
        @Pack(json = "shared_buffers", description = "Четверть лимита памяти.") public String sharedBuffers;
        @Pack(kind = Pack.Kind.CONST, json = "listen_addresses") public String listenAddresses = "*";
        @Pack(kind = Pack.Kind.CONST) public int port = 5432;
    }

    @Override
    public Meta meta() {
        return new Meta("postgres", "16.3")
                .image("oci://localhost:5001/images/postgres:16.3")
                .artifact("oci://localhost:5001/components/postgres");
    }

    @Override
    public Root root() {
        return new Root()
                .scale(Scale.SINGLE)                                           // what sdk.Workload lifts in Go
                .readiness(Root.Readiness.tcp("pg"))
                .upgrade(">=15")
                .hooks(List.of(
                        new Root.Hook("init-extensions", "postInstall", List.of("/opt/pack/init-extensions.sh"))
                                .image("new").idempotent().timeout("5m"),
                        new Root.Hook("pg-upgrade", "preUpgrade", List.of("/usr/lib/postgresql/16/bin/pg_upgrade",
                                "--old-bindir=/usr/lib/postgresql/15/bin", "--new-bindir=/usr/lib/postgresql/16/bin",
                                "--old-datadir=/var/lib/postgresql/data", "--new-datadir=/var/lib/postgresql/data.16"))
                                .when("<16", ">=16").image("new").exclusive().timeout("30m")
                                .compensate("old", List.of("/opt/pack/restore-15.sh")),
                        new Root.Hook("analyze", "postUpgrade", List.of("vacuumdb", "--all", "--analyze-in-stages"))
                                .when(null, ">=16").image("new").idempotent()));
    }

    @Override
    public Map<String, String> compute() {
        return Map.of("/conf/shared_buffers",
                "has(limits.memory) ? string(limits.memory / 1048576 / 4) + 'MB' : '128MB'");
    }
}
