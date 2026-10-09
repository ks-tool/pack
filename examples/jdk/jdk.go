// Package jdk is a LIBRARY component: no image of its own to run, only layers
// (mounted into the consumer at x-pack-mount) and two settings every JVM
// consumer needs. Its formulas run in the consumer's environment: `platform`
// is the consumer's, `values` this subtree.
package jdk

import "github.com/ks-tool/pack/sdk/packschema"

type JDK struct {
	GC       string `json:"gc" jsonschema:"enum=G1,enum=ZGC,title=Сборщик мусора" pack:"default"`
	HeapMB   int    `json:"heapMB,omitempty" jsonschema:"minimum=128,title=Куча JVM в МиБ,description=По умолчанию 3/4 лимита памяти компонента."`
	HeapOpts string `json:"heapOpts,omitempty" jsonschema:"title=Флаги кучи"`
}

func Defaults() JDK { return JDK{GC: "G1"} }

func (JDK) PackRoot() packschema.Root { return packschema.Root{} }

func (JDK) PackCompute() map[string]string {
	return map[string]string{
		"/heapMB":   "has(limits.memory) ? limits.memory / 1048576 * 3 / 4 : 1024",
		"/heapOpts": "'-Xmx' + string(values.heapMB) + 'm -Xms' + string(values.heapMB) + 'm -XX:+Use' + values.gc + 'GC'",
	}
}
