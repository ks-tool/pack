package semver

import "testing"

func TestConstraint(t *testing.T) {
	cases := []struct {
		c, v string
		ok   bool
	}{{">=15", "16.3", true}, {"<16", "16.3.0", false}, {">=21, <22", "21.0.4", true}, {">=21,<22", "22.0.0", false}, {"", "1", true}, {"=3.9.2", "3.9.2", true}, {">15", "15.0.0-rc1", false}}
	for _, c := range cases {
		cons, err := ParseConstraint(c.c)
		if err != nil {
			t.Fatal(err)
		}
		v, err := Parse(c.v)
		if err != nil {
			t.Fatal(err)
		}
		if cons.Check(v) != c.ok {
			t.Errorf("%q against %q: want %v", c.v, c.c, c.ok)
		}
	}
}
