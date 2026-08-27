// Package adh is a COMPOSITION: the keys of the system, each a ref to a
// component by name, and what no component can know about the system —
// who is whose neighbour when the type is ambiguous, ordering beyond the peers
// graph, spread, savepoints of the upgrade saga.
package adh

import (
	"github.com/ks-tool/pack/examples/kafka"
	"github.com/ks-tool/pack/examples/postgres"
	"github.com/ks-tool/pack/examples/zookeeper"
	"github.com/ks-tool/pack/sdk/packschema"
)

type ADH struct {
	Common    Common              `json:"common"`
	Zookeeper zookeeper.ZooKeeper `json:"zookeeper" pack_ref:"zookeeper" pack:"spread"`
	Kafka     kafka.Kafka         `json:"kafka" pack_ref:"kafka" pack:"spread,minimum=memory:4Gi"`
	RangerDB  postgres.Postgres   `json:"ranger-db" pack_ref:"postgres" pack:"default"`
	HiveDB    postgres.Postgres   `json:"hive-db" pack_ref:"postgres" pack:"default"`
}

type Common struct {
	Realm    string `json:"realm" jsonschema:"title=Kerberos realm"`
	Timezone string `json:"timezone" pack:"default"`
}

// Defaults of a composition are the system-level ones: common, and per-key
// defaults layered over the component's own (a $ref node's default merges).
func Defaults() ADH {
	return ADH{
		Common:   Common{Timezone: "UTC"},
		RangerDB: postgres.Postgres{Database: "ranger"},
		HiveDB:   postgres.Postgres{Database: "metastore"},
	}
}

func (ADH) PackRoot() packschema.Root {
	return packschema.Root{
		Savepoints: []string{"zookeeper", "kafka"},
		Rollout:    &packschema.Rollout{MaxUnavailable: 1},
	}
}
