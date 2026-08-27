// Package compute is the CEL environment of x-pack-compute: the variables a
// formula may read, the two functions beyond the standard ones, and what a
// compiled formula reads — so that formulas can be ordered and forbidden
// reads (a password leaf) refused before anything runs.
package compute

import (
	"fmt"
	"math/big"
	"slices"
	"strings"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
	"cel.dev/cel-go/ext"
)

// Vars are the names a formula may read. All are dynamic maps.
var Vars = []string{"values", "platform", "common", "me", "peers", "horchestra"}

const CostLimit = 1_000_000

// Env builds the environment; it is deterministic by construction — no clock,
// no randomness, no I/O are declared.
func Env() (*cel.Env, error) {
	opts := []cel.EnvOption{ext.Strings(), ext.Lists(), ext.Math()}
	for _, v := range Vars {
		opts = append(opts, cel.Variable(v, cel.DynType))
	}
	opts = append(opts,
		cel.Function("quantity",
			cel.Overload("quantity_string", []*cel.Type{cel.StringType}, cel.IntType,
				cel.UnaryBinding(func(v ref.Val) ref.Val {
					s, ok := v.Value().(string)
					if !ok {
						return types.NewErr("quantity: not a string")
					}
					n, err := Quantity(s)
					if err != nil {
						return types.NewErr("%s", err)
					}
					return types.Int(n)
				}))),
		cel.Function("indexMap",
			cel.Overload("indexMap_list_int", []*cel.Type{cel.ListType(cel.DynType), cel.IntType}, cel.MapType(cel.StringType, cel.DynType),
				cel.BinaryBinding(func(l, from ref.Val) ref.Val {
					lister, ok := l.(traits.Lister)
					if !ok {
						return types.NewErr("indexMap: not a list")
					}
					start, ok := from.Value().(int64)
					if !ok {
						return types.NewErr("indexMap: offset is not an int")
					}
					out := map[ref.Val]ref.Val{}
					it := lister.Iterator()
					for i := start; it.HasNext() == types.True; i++ {
						out[types.String(fmt.Sprint(i))] = it.Next()
					}
					return types.NewRefValMap(types.DefaultTypeAdapter, out)
				}))),
	)
	return cel.NewEnv(opts...)
}

// Compile parses a formula. Parse-only on purpose: `cond ? value : null` —
// the spec's "I don't know" — does not pass CEL's static type check. The
// static check is ResultType, on a copy with the null branches dropped.
func Compile(env *cel.Env, expr string) (*cel.Ast, error) {
	a, iss := env.Parse(expr)
	if iss != nil && iss.Err() != nil {
		return nil, iss.Err()
	}
	return a, nil
}

// Eval runs a compiled formula with the given variables, under the cost limit.
func Eval(env *cel.Env, a *cel.Ast, vars map[string]any) (any, error) {
	prg, err := env.Program(a, cel.CostLimit(CostLimit))
	if err != nil {
		return nil, err
	}
	if vars == nil {
		vars = map[string]any{}
	}
	for _, v := range Vars {
		if _, ok := vars[v]; !ok {
			vars[v] = map[string]any{}
		}
	}
	out, _, err := prg.Eval(vars)
	if err != nil {
		return nil, err
	}
	return native(out), nil
}

// native converts a CEL value into plain Go: nil, bool, int64, float64,
// string, []byte, []any, map[string]any.
func native(v ref.Val) any {
	if v == nil {
		return nil
	}
	switch v.Type() {
	case types.NullType:
		return nil
	case types.ListType:
		l := v.(traits.Lister)
		var out []any
		for it := l.Iterator(); it.HasNext() == types.True; {
			out = append(out, native(it.Next()))
		}
		if out == nil {
			out = []any{}
		}
		return out
	case types.MapType:
		m := v.(traits.Mapper)
		out := map[string]any{}
		for it := m.Iterator(); it.HasNext() == types.True; {
			k := it.Next()
			out[fmt.Sprint(native(k))] = native(m.Get(k))
		}
		return out
	case types.UintType:
		return int64(v.Value().(uint64))
	}
	return v.Value()
}

// Reads returns every dotted path the formula selects off a variable, such as
// "values.jdk.heapMB" or "peers.zk.instances", in first-seen order. A dynamic
// index (values[x]) is reported as the path up to the index followed by "[]".
func Reads(a *cel.Ast) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	var walk func(e ast.Expr)
	var path func(e ast.Expr) (string, bool)
	path = func(e ast.Expr) (string, bool) {
		switch e.Kind() {
		case ast.IdentKind:
			return e.AsIdent(), true
		case ast.SelectKind:
			sel := e.AsSelect()
			base, ok := path(sel.Operand())
			if !ok {
				return "", false
			}
			return base + "." + sel.FieldName(), true
		case ast.CallKind:
			c := e.AsCall()
			if c.FunctionName() == "_[_]" && len(c.Args()) == 2 {
				base, ok := path(c.Args()[0])
				if !ok {
					return "", false
				}
				if c.Args()[1].Kind() == ast.LiteralKind {
					return base + "." + fmt.Sprint(c.Args()[1].AsLiteral().Value()), true
				}
				return base + "[]", true
			}
		}
		return "", false
	}
	walk = func(e ast.Expr) {
		if e == nil {
			return
		}
		if p, ok := path(e); ok {
			if root, _, _ := strings.Cut(p, "."); isVar(root) {
				add(p)
			}
			// still descend for dynamic index operands
			if e.Kind() == ast.CallKind {
				for _, arg := range e.AsCall().Args() {
					walk(arg)
				}
			}
			return
		}
		switch e.Kind() {
		case ast.CallKind:
			c := e.AsCall()
			if c.IsMemberFunction() {
				walk(c.Target())
			}
			for _, arg := range c.Args() {
				walk(arg)
			}
		case ast.ListKind:
			for _, el := range e.AsList().Elements() {
				walk(el)
			}
		case ast.MapKind:
			for _, en := range e.AsMap().Entries() {
				me := en.AsMapEntry()
				walk(me.Key())
				walk(me.Value())
			}
		case ast.StructKind:
			for _, f := range e.AsStruct().Fields() {
				walk(f.AsStructField().Value())
			}
		case ast.ComprehensionKind:
			c := e.AsComprehension()
			walk(c.IterRange())
			walk(c.AccuInit())
			walk(c.LoopCondition())
			walk(c.LoopStep())
			walk(c.Result())
		}
	}
	walk(a.NativeRep().Expr())
	return out
}

func isVar(name string) bool {
	return slices.Contains(Vars, name)
}

// Quantity parses a resource quantity ("500m", "2", "32Gi", "1.5G") into
// thousandths of a unit, so that milli-cores and bytes share one integer:
// quantity("32Gi") = 32·2³⁰·1000, quantity("500m") = 500.
func Quantity(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if len(s) == 0 {
		return 0, fmt.Errorf("quantity: empty")
	}
	i := len(s)
	for i > 0 && (s[i-1] < '0' || s[i-1] > '9') && s[i-1] != '.' {
		i--
	}
	num, suffix := s[:i], s[i:]
	r, ok := new(big.Rat).SetString(num)
	if !ok {
		return 0, fmt.Errorf("quantity %q: not a number", s)
	}
	mult := big.NewRat(1000, 1)
	switch suffix {
	case "":
	case "m":
		mult = big.NewRat(1, 1)
	case "k":
		mult.Mul(mult, big.NewRat(1000, 1))
	case "M":
		mult.Mul(mult, big.NewRat(1000*1000, 1))
	case "G":
		mult.Mul(mult, big.NewRat(1000*1000*1000, 1))
	case "T":
		mult.Mul(mult, big.NewRat(1000*1000*1000*1000, 1))
	case "Ki":
		mult.Mul(mult, big.NewRat(1<<10, 1))
	case "Mi":
		mult.Mul(mult, big.NewRat(1<<20, 1))
	case "Gi":
		mult.Mul(mult, big.NewRat(1<<30, 1))
	case "Ti":
		mult.Mul(mult, big.NewRat(1<<40, 1))
	case "Pi":
		mult.Mul(mult, big.NewRat(1<<50, 1))
	default:
		return 0, fmt.Errorf("quantity %q: unknown suffix %q", s, suffix)
	}
	r.Mul(r, mult)
	if !r.IsInt() {
		return 0, fmt.Errorf("quantity %q: not a whole number of thousandths", s)
	}
	if !r.Num().IsInt64() {
		return 0, fmt.Errorf("quantity %q: out of range", s)
	}
	return r.Num().Int64(), nil
}

// ResultType type-checks a formula and reports the JSON type of its result:
// "string", "integer", "number", "boolean", "array", "object" — or "" when the
// type is dynamic (every variable is a dynamic map, so `values.x` alone tells
// nothing, while `string(values.x)`, `values.x + 1` or `.join(',')` do).
//
// `cond ? x : null` is the spec's "I don't know": CEL's checker rejects the
// mismatch of the branches, so every conditional with a null branch is typed
// by its other branch. A genuine type error (`1 + "a"`) is returned.
func ResultType(env *cel.Env, expr string) (string, error) {
	a, err := Compile(env, expr) // a fresh parse: the rewrite below mutates it
	if err != nil {
		return "", err
	}
	dropNullBranches(a.NativeRep().Expr())
	checked, iss := env.Check(a)
	if iss != nil && iss.Err() != nil {
		return "", iss.Err()
	}
	return jsonType(checked.OutputType()), nil
}

// dropNullBranches replaces every `c ? x : null` (or `c ? null : x`) in the
// tree by x, in place.
func dropNullBranches(e ast.Expr) {
	if e == nil {
		return
	}
	if e.Kind() == ast.CallKind {
		c := e.AsCall()
		if c.FunctionName() == "_?_:_" && len(c.Args()) == 3 {
			x, y := c.Args()[1], c.Args()[2]
			switch {
			case isNull(y):
				e.SetKindCase(x)
				dropNullBranches(e)
				return
			case isNull(x):
				e.SetKindCase(y)
				dropNullBranches(e)
				return
			}
		}
		if c.IsMemberFunction() {
			dropNullBranches(c.Target())
		}
		for _, arg := range c.Args() {
			dropNullBranches(arg)
		}
		return
	}
	switch e.Kind() {
	case ast.SelectKind:
		dropNullBranches(e.AsSelect().Operand())
	case ast.ListKind:
		for _, el := range e.AsList().Elements() {
			dropNullBranches(el)
		}
	case ast.MapKind:
		for _, en := range e.AsMap().Entries() {
			me := en.AsMapEntry()
			dropNullBranches(me.Key())
			dropNullBranches(me.Value())
		}
	case ast.ComprehensionKind:
		c := e.AsComprehension()
		dropNullBranches(c.IterRange())
		dropNullBranches(c.AccuInit())
		dropNullBranches(c.LoopCondition())
		dropNullBranches(c.LoopStep())
		dropNullBranches(c.Result())
	}
}

func isNull(e ast.Expr) bool {
	return e.Kind() == ast.LiteralKind && e.AsLiteral().Type() == types.NullType
}

func jsonType(t *cel.Type) string {
	if t == nil {
		return ""
	}
	switch t.Kind() {
	case types.StringKind:
		return "string"
	case types.IntKind, types.UintKind:
		return "integer"
	case types.DoubleKind:
		return "number"
	case types.BoolKind:
		return "boolean"
	case types.ListKind:
		return "array"
	case types.MapKind:
		return "object"
	}
	return ""
}
