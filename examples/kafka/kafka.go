// Package kafka needs a ZooKeeper neighbour (the role zk) and the jdk library.
package kafka

import (
	"github.com/ks-tool/pack/examples/jdk"
	"github.com/ks-tool/pack/sdk"
	"github.com/ks-tool/pack/sdk/packschema"
)

type Kafka struct {
	sdk.Workload

	Ports    Ports   `json:"ports"`
	JDK      jdk.JDK `json:"jdk" pack_ref:"jdk" pack:"mount=/opt/java"`
	Claims   Claims  `json:"claims"`
	Conf     Conf    `json:"conf" pack:"file=/opt/kafka/config/server.properties,format=properties"`
	Jaas     Jaas    `json:"jaas" pack:"file=/opt/kafka/config/kafka_jaas.conf,format=template,template=jaas.tmpl,parseAs=properties,mode=0400"`
	TLS      TLS     `json:"tls"`
	HeapOpts string  `json:"heapOpts,omitempty" pack:"env=KAFKA_HEAP_OPTS"`
}

type Ports struct {
	Broker   int `json:"broker" pack:"const"`
	Internal int `json:"internal" pack:"const"`
}

type Claims struct {
	Data packschema.Claim `json:"data"`
}

type Conf struct {
	LogDirs           string `json:"log.dirs" pack:"const"`
	BrokerIDGen       bool   `json:"broker.id.generation.enable" pack:"const"`
	MinInsyncReplicas int    `json:"min.insync.replicas" jsonschema:"minimum=1" pack:"default,arg=--override,prefix=min.insync.replicas="`
	NumPartitions     int    `json:"num.partitions" jsonschema:"minimum=1" pack:"default"`
	ZookeeperConnect  string `json:"zookeeper.connect,omitempty" jsonschema:"title=Строка подключения к ZooKeeper,description=Собирается из имён экземпляров соседа zk."`
	Listeners         string `json:"listeners,omitempty"`
}

// TLS is a secret FILE: the keystore is stored as a Secret and mounted at the
// path as it is — no rendering, no placeholder.
type TLS struct {
	Keystore packschema.Secret `json:"keystore,omitzero" jsonschema:"title=Keystore брокера (JKS)" pack:"file=/opt/kafka/config/keystore.jks,format=secret,mode=0400"`
}

type Jaas struct {
	Mechanism string            `json:"mechanism" jsonschema:"enum=SCRAM-SHA-512,enum=PLAIN" pack:"default"`
	Username  string            `json:"username" pack:"default"`
	Password  packschema.Secret `json:"password" jsonschema:"required,title=Пароль брокера"`
}

func Defaults() Kafka {
	return Kafka{
		Ports:    Ports{Broker: 9092, Internal: 9093},
		Workload: sdk.Replicated(3, "1", "2Gi"),
		Claims:   Claims{Data: packschema.Claim{MountPath: "/var/lib/kafka/data", Size: "10Gi"}},
		Conf:     Conf{LogDirs: "/var/lib/kafka/data", BrokerIDGen: true, MinInsyncReplicas: 1, NumPartitions: 3},
		Jaas:     Jaas{Mechanism: "SCRAM-SHA-512", Username: "kafka"},
	}
}

func (Kafka) PackRoot() packschema.Root {
	return packschema.Root{
		Readiness: &packschema.Readiness{TCP: "broker"},
		Peers:     map[string]packschema.Peer{"zk": {Schema: "zookeeper", Required: true}},
		Requires:  map[string]string{"jdk": ">=17"},
		Upgrade:   &packschema.Upgrade{From: ">=3.0"},
	}
}

func (Kafka) PackCompute() map[string]string {
	return map[string]string{
		"/conf/zookeeper.connect": "peers.zk.instances.map(h, h + ':' + string(peers.zk.ports.client)).join(',') + '/' + horchestra.installation",
		"/conf/listeners":         "'PLAINTEXT://:' + string(values.ports.broker) + ',INTERNAL://:' + string(values.ports.internal)",
		"/heapOpts":               "values.jdk.heapOpts",
	}
}
