package target

import (
	"strings"
	"testing"
)

// A target knows its settings: pack's own pass, names and prefixes pass,
// anything else is refused, all of it named at once.
func TestSettingsCheck(t *testing.T) {
	names, prefixes := []string{"namespace"}, []string{"platform_"}
	ok := Settings{"target": "k8s", "provider": "x", "vault_addr": "a", "vault_paths": "p", "namespace": "data", "platform_kafka_replicas": "3"}
	if err := ok.Check("k8s", names, prefixes); err != nil {
		t.Error(err)
	}
	bad := Settings{"namespace": "data", "namspace": "data", "network": "x"}
	err := bad.Check("k8s", names, prefixes)
	if err == nil || !strings.Contains(err.Error(), "no setting namspace, network; it has namespace, platform_…") {
		t.Errorf("%v", err)
	}
}
