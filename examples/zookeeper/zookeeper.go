// Package zookeeper is a quorum component: replicas, a per-instance myid, a
// server list computed from its own instance names.
package zookeeper

import (
	"github.com/ks-tool/pack/examples/jdk"
	"github.com/ks-tool/pack/sdk"
	"github.com/ks-tool/pack/sdk/packschema"
)

type ZooKeeper struct {
	sdk.Workload

	Ports    Ports   `json:"ports"`
	JDK      jdk.JDK `json:"jdk" pack_ref:"jdk" pack:"mount=/opt/java"`
	Claims   Claims  `json:"claims"`
	Conf     Conf    `json:"conf" pack:"file=/conf/zoo.cfg,format=properties"`
	MyID     string  `json:"myid,omitempty" pack:"file=/var/lib/zookeeper/myid,format=text,perInstance"`
	Log4j    string  `json:"log4j,omitempty" pack:"file=/conf/log4j.properties,format=template,template=log4j.properties.tmpl,parseAs=properties"`
	HeapOpts string  `json:"heapOpts,omitempty" pack:"env=JVMFLAGS"`
}

type Ports struct {
	Client   int `json:"client" pack:"const"`
	Peer     int `json:"peer" pack:"const"`
	Election int `json:"election" pack:"const"`
}

type Claims struct {
	Data packschema.Claim `json:"data"`
}

type Conf struct {
	TickTime   int               `json:"tickTime" jsonschema:"minimum=1" pack:"default"`
	InitLimit  int               `json:"initLimit" pack:"default"`
	SyncLimit  int               `json:"syncLimit" pack:"default"`
	DataDir    string            `json:"dataDir" pack:"const"`
	ClientPort int               `json:"clientPort" pack:"const"`
	Server     map[string]string `json:"server,omitempty" jsonschema:"title=Кворум: server.N=host:peer:election"`
}

func Defaults() ZooKeeper {
	return ZooKeeper{
		Ports:    Ports{Client: 2181, Peer: 2888, Election: 3888},
		Workload: sdk.Replicated("500m", "1Gi"),
		Claims:   Claims{Data: packschema.Claim{MountPath: "/var/lib/zookeeper", Size: "10Gi"}},
		Conf:     Conf{TickTime: 2000, InitLimit: 10, SyncLimit: 5, DataDir: "/var/lib/zookeeper", ClientPort: 2181},
	}
}

func (ZooKeeper) PackRoot() packschema.Root {
	return packschema.Root{
		Readiness: &packschema.Readiness{TCP: "client"},
		Requires:  map[string]string{"jdk": ">=11"},
		Upgrade:   &packschema.Upgrade{From: ">=3.5"},
	}
}

func (ZooKeeper) PackCompute() map[string]string {
	return map[string]string{
		"/conf/server": "indexMap(me.instances.map(h, h + ':' + string(values.ports.peer) + ':' + string(values.ports.election)), 1)",
		"/myid":        "string(pack.ordinal)",
		"/heapOpts":    "values.jdk.heapOpts",
	}
}
