"""examples/postgres, described in Python: the same schema.json the Go
struct yields. `python example/postgres.py generate ./out` writes it."""

from dataclasses import dataclass

from packsdk import (Claim, Component, Hook, Meta, Platform, Readiness, Root, Scale, Secret, Step, Upgrade, When,
                     const, default, default_factory, file, operator, secret, main)


@dataclass
class Ports:
    pg: int = const(5432)


@dataclass
class Claims:
    data: Claim = default_factory(Claim.at("/var/lib/postgresql/data", "10Gi"))


@dataclass
class Superuser:
    name: str = default("postgres", env="POSTGRES_USER")
    password: Secret = secret(title="Пароль суперпользователя", env="POSTGRES_PASSWORD")


@dataclass
class Conf:
    max_connections: int = default(100, minimum=1)
    shared_buffers: str = operator(description="Четверть лимита памяти.")
    listen_addresses: str = const("*")
    port: int = const(5432)


@dataclass
class Postgres(Component):
    scale: str = const(Scale.SINGLE)                                           # what sdk.Workload embeds in Go
    platform: Platform = default_factory(Platform.sized("500m", "512Mi"))
    ports: Ports = default_factory(Ports)
    claims: Claims = default_factory(Claims)
    database: str = operator(required=True, title="База данных")
    superuser: Superuser = default_factory(Superuser)
    conf: Conf = file(Conf(), path="/etc/postgresql/postgresql.conf", format="properties")

    meta = Meta(name="postgres", version="16.3", image="cr://localhost:5001/images/postgres:16.3",
                artifact="cr://localhost:5001/components/postgres")

    def root(self) -> Root:
        return Root(
            readiness=Readiness(tcp="pg"),
            upgrade=Upgrade(from_=">=15"),
            hooks=[
                Hook(name="init-extensions", phase="postInstall", image="new",
                     command=["/opt/pack/init-extensions.sh"], idempotent=True, timeout="5m"),
                Hook(name="pg-upgrade", phase="preUpgrade", when=When(from_="<16", to=">=16"), image="new",
                     command=["/usr/lib/postgresql/16/bin/pg_upgrade", "--old-bindir=/usr/lib/postgresql/15/bin",
                              "--new-bindir=/usr/lib/postgresql/16/bin", "--old-datadir=/var/lib/postgresql/data",
                              "--new-datadir=/var/lib/postgresql/data.16"],
                     exclusive=True, timeout="30m",
                     compensate=Step(image="old", command=["/opt/pack/restore-15.sh"])),
                Hook(name="analyze", phase="postUpgrade", when=When(to=">=16"), image="new",
                     command=["vacuumdb", "--all", "--analyze-in-stages"], idempotent=True),
            ],
        )

    def compute(self) -> dict[str, str]:
        return {"/conf/shared_buffers":
                "has(limits.memory) ? string(limits.memory / 1048576 / 4) + 'MB' : '128MB'"}


if __name__ == "__main__":
    main(Postgres())
