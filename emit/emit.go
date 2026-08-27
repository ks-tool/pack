// Package emit turns a values subtree into the bytes of a configuration file.
// Every format but template is serialisation — it cannot produce a broken
// file; template can, so its output is parsed back as the format it claims.
// Output depends on the values and the format only: keys are sorted, nothing
// reads a clock.
package emit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"sigs.k8s.io/yaml"
)

// Emit renders v in the given format. For template, tmpl is the template
// text and data what it sees; parseAs names the format the result must be.
func Emit(format string, v any) ([]byte, error) {
	switch format {
	case "json":
		return emitJSON(v)
	case "yaml":
		return yaml.Marshal(v)
	case "properties":
		return emitProperties(v)
	case "env":
		return emitEnv(v, "")
	case "shell":
		return emitEnv(v, "export ")
	case "ini":
		return emitINI(v)
	case "flagfile":
		return emitFlagfile(v)
	case "lines":
		return emitLines(v)
	case "hadoop-xml":
		return emitHadoopXML(v)
	case "text":
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("text: the node is not a string (%T)", v)
		}
		if !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
		return []byte(s), nil
	}
	return nil, fmt.Errorf("format %q: no emitter", format)
}

// Template renders tmpl with data and checks the result parses as parseAs.
func Template(name string, tmpl []byte, data any, parseAs string) ([]byte, error) {
	t, err := template.New(name).Option("missingkey=error").Parse(string(tmpl))
	if err != nil {
		return nil, fmt.Errorf("template %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("template %s: %w", name, err)
	}
	if err := Parse(parseAs, buf.Bytes()); err != nil {
		return nil, fmt.Errorf("template %s: the result is not %s: %w", name, parseAs, err)
	}
	return buf.Bytes(), nil
}

// Parse checks that data is a well-formed file of the format — the read-back
// a template's output must survive.
func Parse(format string, data []byte) error {
	switch format {
	case "json":
		var v any
		return json.Unmarshal(data, &v)
	case "yaml":
		var v any
		return yaml.Unmarshal(data, &v)
	case "properties", "env", "shell", "flagfile", "ini":
		for i, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if len(line) == 0 || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") || strings.HasPrefix(line, ";") {
				continue
			}
			if format == "ini" && strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
				continue
			}
			if format == "shell" {
				line = strings.TrimPrefix(line, "export ")
			}
			if format == "flagfile" && !strings.HasPrefix(line, "-") {
				return fmt.Errorf("line %d: a flagfile line starts with -", i+1)
			}
			if !strings.ContainsAny(line, "=:") && format != "properties" {
				return fmt.Errorf("line %d: no key=value", i+1)
			}
		}
		return nil
	case "lines":
		return nil
	case "hadoop-xml":
		if !bytes.Contains(data, []byte("<configuration>")) {
			return errors.New("no <configuration> element")
		}
		return nil
	case "text":
		return nil
	}
	return fmt.Errorf("format %q: cannot parse", format)
}

func emitJSON(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// flatten walks nested objects into dotted keys; arrays become comma lists.
func flatten(prefix string, v any, out map[string]string, sep string) error {
	switch x := v.(type) {
	case map[string]any:
		for _, k := range sortedKeys(x) {
			key := k
			if len(prefix) > 0 {
				key = prefix + sep + k
			}
			if err := flatten(key, x[k], out, sep); err != nil {
				return err
			}
		}
		return nil
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			s, err := scalar(e)
			if err != nil {
				return fmt.Errorf("%s: %w", prefix, err)
			}
			parts = append(parts, s)
		}
		out[prefix] = strings.Join(parts, ",")
		return nil
	case nil:
		return nil
	}
	s, err := scalar(v)
	if err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	out[prefix] = s
	return nil
}

func scalar(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case bool:
		return strconv.FormatBool(x), nil
	case int:
		return strconv.Itoa(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), nil
	case json.Number:
		return x.String(), nil
	}
	return "", fmt.Errorf("not a scalar (%T)", v)
}

func emitProperties(v any) ([]byte, error) {
	flat := map[string]string{}
	if err := flatten("", v, flat, "."); err != nil {
		return nil, err
	}
	var b strings.Builder
	for _, k := range sortedKeysS(flat) {
		b.WriteString(escapeProp(k, true))
		b.WriteString("=")
		b.WriteString(escapeProp(flat[k], false))
		b.WriteString("\n")
	}
	return []byte(b.String()), nil
}

// escapeProp follows java.util.Properties: in a KEY, '=', ':', spaces, '#'
// and '!' are escaped, because they would end or comment the key; in a VALUE
// only what a reader would misread — a backslash, a line break, a leading
// space — so that URLs and host:port lists stay readable.
func escapeProp(s string, key bool) string {
	var b strings.Builder
	for i, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '=', ':', '#', '!':
			if key {
				b.WriteRune('\\')
			}
			b.WriteRune(r)
		case ' ':
			if key || i == 0 {
				b.WriteString(`\ `)
			} else {
				b.WriteRune(r)
			}
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// emitEnv writes NAME=value lines; prefix "export " makes it a shell file
// that is sourced (hadoop-env.sh, java.env). A value with shell-significant
// characters is single-quoted: sourced files are code, and $ or spaces would
// be interpreted.
func emitEnv(v any, prefix string) ([]byte, error) {
	flat := map[string]string{}
	if err := flatten("", v, flat, "_"); err != nil {
		return nil, err
	}
	var b strings.Builder
	for _, k := range sortedKeysS(flat) {
		name := strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(k))
		val := flat[k]
		if strings.ContainsAny(val, " \t\"'$`\\#;&|<>(){}*?!~") {
			if len(prefix) > 0 {
				val = "'" + strings.ReplaceAll(val, "'", `'\''`) + "'"
			} else {
				val = strconv.Quote(val)
			}
		}
		b.WriteString(prefix + name + "=" + val + "\n")
	}
	return []byte(b.String()), nil
}

// emitINI writes sections for objects; an object inside a section is a
// nested section written with one more pair of brackets ([[sub]]), the way
// configobj (hue.ini) reads it. Keys before the first section have none.
func emitINI(v any) ([]byte, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("ini: the node is not an object")
	}
	var b strings.Builder
	if err := iniSection(&b, m, "", 0); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func iniSection(b *strings.Builder, m map[string]any, path string, depth int) error {
	var sections []string
	for _, k := range sortedKeys(m) {
		if _, isSection := m[k].(map[string]any); isSection {
			sections = append(sections, k)
			continue
		}
		s, err := scalarOrList(m[k])
		if err != nil {
			return fmt.Errorf("ini: %s%s: %w", path, k, err)
		}
		b.WriteString(k + "=" + s + "\n")
	}
	for _, sec := range sections {
		if depth == 0 {
			b.WriteString("\n")
		}
		b.WriteString(strings.Repeat("[", depth+1) + sec + strings.Repeat("]", depth+1) + "\n")
		if err := iniSection(b, m[sec].(map[string]any), path+sec+".", depth+1); err != nil {
			return err
		}
	}
	return nil
}

// emitLines writes one scalar per line — a list of JVM flags (trino's
// jvm.config), a list of hosts. A scalar alone is one line.
func emitLines(v any) ([]byte, error) {
	var items []any
	switch x := v.(type) {
	case []any:
		items = x
	case map[string]any:
		return nil, errors.New("lines: the node is an object; lines wants a list of scalars")
	default:
		items = []any{v}
	}
	var b strings.Builder
	for i, e := range items {
		s, err := scalar(e)
		if err != nil {
			return nil, fmt.Errorf("lines: item %d: %w", i, err)
		}
		if strings.ContainsAny(s, "\n\r") {
			return nil, fmt.Errorf("lines: item %d carries a line break", i)
		}
		b.WriteString(s + "\n")
	}
	return []byte(b.String()), nil
}

func scalarOrList(v any) (string, error) {
	if l, ok := v.([]any); ok {
		parts := make([]string, 0, len(l))
		for _, e := range l {
			s, err := scalar(e)
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, ","), nil
	}
	return scalar(v)
}

func emitFlagfile(v any) ([]byte, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("flagfile: the node is not an object")
	}
	var b strings.Builder
	for _, k := range sortedKeys(m) {
		if m[k] == nil {
			continue
		}
		if _, deeper := m[k].(map[string]any); deeper {
			return nil, fmt.Errorf("flagfile: %s: deeper than one level", k)
		}
		s, err := scalarOrList(m[k])
		if err != nil {
			return nil, fmt.Errorf("flagfile: %s: %w", k, err)
		}
		if strings.ContainsAny(s, "\n\r\x00") {
			return nil, fmt.Errorf("flagfile: %s: the value carries a line break — one assignment per line", k)
		}
		b.WriteString("--" + k + "=" + s + "\n")
	}
	return []byte(b.String()), nil
}

func emitHadoopXML(v any) ([]byte, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("hadoop-xml: the node is not an object")
	}
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<configuration>\n")
	for _, k := range sortedKeys(m) {
		if _, deeper := m[k].(map[string]any); deeper {
			return nil, fmt.Errorf("hadoop-xml: %s: deeper than one level", k)
		}
		s, err := scalarOrList(m[k])
		if err != nil {
			return nil, fmt.Errorf("hadoop-xml: %s: %w", k, err)
		}
		b.WriteString("  <property>\n    <name>" + xmlEscape(k) + "</name>\n    <value>" + xmlEscape(s) + "</value>\n  </property>\n")
	}
	b.WriteString("</configuration>\n")
	return []byte(b.String()), nil
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeysS(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
