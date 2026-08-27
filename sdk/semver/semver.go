// Package semver is the little of semantic versioning the vocabulary needs:
// compare two versions, and check one against a constraint such as ">=15",
// "<16", ">=21, <22". Pre-release tags are compared as plain strings; build
// metadata is ignored.
package semver

import (
	"fmt"
	"strconv"
	"strings"
)

type Version struct {
	Major, Minor, Patch int
	Pre                 string
}

func Parse(s string) (Version, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var v Version
	if i := strings.IndexByte(s, '-'); i >= 0 {
		v.Pre, s = s[i+1:], s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return v, fmt.Errorf("%q: not a version", s)
	}
	nums := []*int{&v.Major, &v.Minor, &v.Patch}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, fmt.Errorf("%q: not a version", s)
		}
		*nums[i] = n
	}
	return v, nil
}

func (v Version) Compare(o Version) int {
	for _, d := range []int{v.Major - o.Major, v.Minor - o.Minor, v.Patch - o.Patch} {
		if d != 0 {
			if d < 0 {
				return -1
			}
			return 1
		}
	}
	switch {
	case v.Pre == o.Pre:
		return 0
	case len(v.Pre) == 0:
		return 1
	case len(o.Pre) == 0:
		return -1
	}
	return strings.Compare(v.Pre, o.Pre)
}

func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.Pre) > 0 {
		s += "-" + v.Pre
	}
	return s
}

type clause struct {
	op string
	v  Version
}

// Constraint is a conjunction of clauses; "" or "*" matches everything.
type Constraint struct{ clauses []clause }

func ParseConstraint(s string) (Constraint, error) {
	s = strings.TrimSpace(s)
	if len(s) == 0 || s == "*" {
		return Constraint{}, nil
	}
	var c Constraint
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		op := "="
		for _, candidate := range []string{">=", "<=", "!=", "==", ">", "<", "="} {
			if strings.HasPrefix(part, candidate) {
				op, part = candidate, part[len(candidate):]
				break
			}
		}
		if op == "==" {
			op = "="
		}
		v, err := Parse(part)
		if err != nil {
			return Constraint{}, fmt.Errorf("constraint %q: %w", s, err)
		}
		c.clauses = append(c.clauses, clause{op, v})
	}
	return c, nil
}

func (c Constraint) Check(v Version) bool {
	for _, cl := range c.clauses {
		cmp := v.Compare(cl.v)
		ok := false
		switch cl.op {
		case ">=":
			ok = cmp >= 0
		case ">":
			ok = cmp > 0
		case "<=":
			ok = cmp <= 0
		case "<":
			ok = cmp < 0
		case "=":
			ok = cmp == 0
		case "!=":
			ok = cmp != 0
		}
		if !ok {
			return false
		}
	}
	return true
}
