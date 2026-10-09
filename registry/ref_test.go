package registry

import "testing"

// One scheme, oci://, as in oci-packer: a registry's reference parses, a
// local layout's is told apart and refused — pack reads registries and
// directories.
func TestParse_OCIScheme(t *testing.T) {
	for in, want := range map[string]Ref{
		"oci://localhost/my/image:1.2.3":              {Host: "localhost", Repo: "my/image", Tag: "1.2.3"},
		"oci://localho.st/my/image:1.2.3":             {Host: "localho.st", Repo: "my/image", Tag: "1.2.3"},
		"oci://docker.io/library/postgres:18-alpine":  {Host: "docker.io", Repo: "library/postgres", Tag: "18-alpine"},
		"oci://localhost:5000/my/image:1.2.3":         {Host: "localhost:5000", Repo: "my/image", Tag: "1.2.3"},
		"oci://10.92.16.25:5000/ee/bundles/adh:4.3.0": {Host: "10.92.16.25:5000", Repo: "ee/bundles/adh", Tag: "4.3.0"},
	} {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Errorf("%s: %+v %v", in, got, err)
		}
		if IsLayout(in) || !IsRef(in) {
			t.Errorf("%s is a registry's", in)
		}
	}
	for _, in := range []string{
		"oci:///home/user/layout-dir:my/image:1.2.3",
		"oci://layout-dir:my/image:1.2.3",
		"oci://./layout-dir:my/image:1.2.3",
	} {
		if !IsLayout(in) {
			t.Errorf("%s is a layout's", in)
		}
		if _, err := Parse(in); err == nil {
			t.Errorf("%s: pack does not read a layout", in)
		}
	}
	if IsRegistryHost("layout-dir:my") || !IsRegistryHost("registry:5000") {
		t.Error("a colon is a host's only before a port")
	}
}
