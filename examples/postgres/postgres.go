// Package postgres is a single-instance component with an install hook (the
// extensions every database gets, after the first start) and a major-version
// upgrade hook: the saga runs pg_upgrade with the component stopped.
package postgres

import (
	"github.com/ks-tool/pack/sdk"
	"github.com/ks-tool/pack/sdk/packschema"
)

type Postgres struct {
	sdk.Workload

	Ports     Ports     `json:"ports"`
	Claims    Claims    `json:"claims"`
	Database  string    `json:"database" jsonschema:"required,title=База данных"`
	Superuser Superuser `json:"superuser"`
	Conf      Conf      `json:"conf" pack:"file=/etc/postgresql/postgresql.conf,format=properties"`
}

type Ports struct {
	PG int `json:"pg" pack:"const"`
}

type Claims struct {
	Data packschema.Claim `json:"data"`
}

type Superuser struct {
	Name     string            `json:"name" pack:"default,env=POSTGRES_USER"`
	Password packschema.Secret `json:"password" jsonschema:"required,title=Пароль суперпользователя" pack:"env=POSTGRES_PASSWORD"`
}

type Conf struct {
	MaxConnections int    `json:"max_connections" jsonschema:"minimum=1" pack:"default"`
	SharedBuffers  string `json:"shared_buffers,omitempty" jsonschema:"description=Четверть лимита памяти."`
	ListenAddress  string `json:"listen_addresses" pack:"const"`
	Port           int    `json:"port" pack:"const"`
}

func Defaults() Postgres {
	return Postgres{
		Ports:     Ports{PG: 5432},
		Workload:  sdk.Single("500m", "512Mi"),
		Claims:    Claims{Data: packschema.Claim{MountPath: "/var/lib/postgresql/data", Size: "10Gi"}},
		Superuser: Superuser{Name: "postgres"},
		Conf:      Conf{MaxConnections: 100, ListenAddress: "*", Port: 5432},
	}
}

func (Postgres) PackRoot() packschema.Root {
	return packschema.Root{
		Readiness: &packschema.Readiness{TCP: "pg"},
		Upgrade:   &packschema.Upgrade{From: ">=15"},
		Hooks: []packschema.Hook{{
			Name: "init-extensions", Phase: "postInstall", Image: "new",
			Command: []string{"/opt/pack/init-extensions.sh"}, Idempotent: true, Timeout: "5m",
		}, {
			Name: "pg-upgrade", Phase: "preUpgrade",
			When:      &packschema.When{From: "<16", To: ">=16"},
			Image:     "new",
			Command:   []string{"/usr/lib/postgresql/16/bin/pg_upgrade", "--old-bindir=/usr/lib/postgresql/15/bin", "--new-bindir=/usr/lib/postgresql/16/bin", "--old-datadir=/var/lib/postgresql/data", "--new-datadir=/var/lib/postgresql/data.16"},
			Exclusive: true, Timeout: "30m",
			Compensate: &packschema.Step{Image: "old", Command: []string{"/opt/pack/restore-15.sh"}},
		}, {
			Name: "analyze", Phase: "postUpgrade", When: &packschema.When{To: ">=16"}, Image: "new",
			Command: []string{"vacuumdb", "--all", "--analyze-in-stages"}, Idempotent: true,
		}},
	}
}

func (Postgres) PackCompute() map[string]string {
	return map[string]string{
		"/conf/shared_buffers": "has(limits.memory) ? string(limits.memory / 1048576 / 4) + 'MB' : '128MB'",
	}
}
