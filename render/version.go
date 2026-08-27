package render

import "github.com/ks-tool/pack/sdk/semver"

// versionFits reports whether a member's version satisfies a constraint; an
// unknown version (no pack.yaml) fits anything — there is nothing to compare.
func versionFits(version, constraint string) bool {
	if len(version) == 0 {
		return true
	}
	c, err := semver.ParseConstraint(constraint)
	if err != nil {
		return false
	}
	v, err := semver.Parse(version)
	if err != nil {
		return false
	}
	return c.Check(v)
}
