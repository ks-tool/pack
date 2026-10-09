package provider

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"strconv"

	"github.com/ks-tool/pack/provider/providerpb"
	"github.com/ks-tool/pack/render"
	"github.com/ks-tool/pack/sdk/packschema"
)

// encode writes a free-form document as JSON; nil stays empty.
func encode(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	if m, ok := v.(map[string]any); ok && m == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

// decodeMap reads a JSON object back with its numbers as they were meant:
// an integer as int64, anything else as float64 — what a formula's
// arithmetic depends on.
func decodeMap(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v map[string]any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	return numbers(v).(map[string]any), nil
}

func numbers(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = numbers(e)
		}
	case []any:
		for i, e := range x {
			x[i] = numbers(e)
		}
	case json.Number:
		if i, err := strconv.ParseInt(string(x), 10, 64); err == nil {
			return i
		}
		f, _ := x.Float64()
		return f
	}
	return v
}

func modeTo(m *fs.FileMode) *uint32 {
	if m == nil {
		return nil
	}
	v := uint32(*m)
	return &v
}

func modeFrom(m *uint32) *fs.FileMode {
	if m == nil {
		return nil
	}
	v := fs.FileMode(*m)
	return &v
}

func secretTo(s *render.SecretRef) *providerpb.SecretRef {
	if s == nil {
		return nil
	}
	return &providerpb.SecretRef{Name: s.Name, Key: s.Key}
}

func secretFrom(s *providerpb.SecretRef) *render.SecretRef {
	if s == nil {
		return nil
	}
	return &render.SecretRef{Name: s.Name, Key: s.Key}
}

// releaseTo is render.Release on the wire.
func releaseTo(rel *render.Release) (*providerpb.Release, error) {
	out := &providerpb.Release{Installation: rel.Installation, Pack: rel.Pack, Version: rel.Version}
	if rel.Rollout != nil {
		out.Rollout = &providerpb.Rollout{MaxUnavailable: int64(rel.Rollout.MaxUnavailable)}
	}
	for _, u := range rel.Units {
		pu, err := unitTo(&u)
		if err != nil {
			return nil, err
		}
		out.Units = append(out.Units, pu)
	}
	rep := rel.Report
	out.Report = &providerpb.Report{Installation: rep.Installation, Namespace: rep.Namespace, Pack: rep.Pack, Order: rep.Order}
	if rep.Peers != nil {
		out.Report.Peers = map[string]*providerpb.Roles{}
		for k, roles := range rep.Peers {
			out.Report.Peers[k] = &providerpb.Roles{Roles: roles}
		}
	}
	var err error
	if out.Report.ComputedJson, err = encode(rep.Computed); err != nil {
		return nil, err
	}
	if rep.Secrets != nil {
		out.Report.Secrets = map[string]*providerpb.Names{}
		for k, names := range rep.Secrets {
			out.Report.Secrets[k] = &providerpb.Names{Names: names}
		}
	}
	return out, nil
}

// releaseFrom is the wire back to render.Release.
func releaseFrom(in *providerpb.Release) (*render.Release, error) {
	rel := &render.Release{Installation: in.Installation, Pack: in.Pack, Version: in.Version}
	if in.Rollout != nil {
		rel.Rollout = &packschema.Rollout{MaxUnavailable: int(in.Rollout.MaxUnavailable)}
	}
	for _, pu := range in.Units {
		u, err := unitFrom(pu)
		if err != nil {
			return nil, err
		}
		rel.Units = append(rel.Units, *u)
	}
	if r := in.Report; r != nil {
		rel.Report = render.Report{Installation: r.Installation, Namespace: r.Namespace, Pack: r.Pack, Order: r.Order}
		if r.Peers != nil {
			rel.Report.Peers = map[string]map[string]string{}
			for k, roles := range r.Peers {
				m := roles.Roles
				if m == nil { // a key with no roles bound is {} — proto cannot tell it from absent
					m = map[string]string{}
				}
				rel.Report.Peers[k] = m
			}
		}
		var err error
		if rel.Report.Computed, err = decodeMap(r.ComputedJson); err != nil {
			return nil, err
		}
		if r.Secrets != nil {
			rel.Report.Secrets = map[string][]string{}
			for k, names := range r.Secrets {
				rel.Report.Secrets[k] = names.Names
			}
		}
	}
	return rel, nil
}

// unitTo is render.Unit on the wire; nil stays nil.
func unitTo(u *render.Unit) (*providerpb.Unit, error) {
	if u == nil {
		return nil, nil
	}
	pu := &providerpb.Unit{Key: u.Key, Component: u.Component, Replicas: int64(u.Replicas), Image: u.Image,
		RestartPolicy: u.RestartPolicy, Command: u.Command, Args: u.Args, Spread: u.Spread}
	for _, e := range u.Env {
		pu.Env = append(pu.Env, &providerpb.EnvVar{Name: e.Name, Value: e.Value, Secret: secretTo(e.Secret)})
	}
	for _, p := range u.Ports {
		pu.Ports = append(pu.Ports, &providerpb.Port{Name: p.Name, Port: int64(p.Port)})
	}
	var err error
	if pu.PlatformJson, err = encode(u.Platform); err != nil {
		return nil, err
	}
	if r := u.Readiness; r != nil {
		pu.Readiness = &providerpb.Readiness{Exec: r.Exec}
		if r.TCP != nil {
			pu.Readiness.Tcp = &providerpb.TCPProbe{Port: int64(r.TCP.Port)}
		}
		if r.HTTP != nil {
			pu.Readiness.Http = &providerpb.HTTPProbe{Port: int64(r.HTTP.Port), Path: r.HTTP.Path}
		}
	}
	for _, c := range u.Data {
		pu.Data = append(pu.Data, &providerpb.Claim{Name: c.Name, Path: c.Path, Size: c.Size})
	}
	for _, l := range u.Libraries {
		pu.Libraries = append(pu.Libraries, &providerpb.Library{Name: l.Name, Path: l.Path, Image: l.Image})
	}
	for _, f := range u.Files {
		pf := &providerpb.RenderedFile{Path: f.Path, Mode: modeTo(f.Mode), Content: f.Content, PerInstance: f.PerInstance(), Secrets: f.Secrets}
		for _, b := range f.Instances {
			pf.Instances = append(pf.Instances, &providerpb.InstanceBytes{Present: b != nil, Data: b})
		}
		pu.Files = append(pu.Files, pf)
	}
	for _, s := range u.SecretFiles {
		pu.SecretFiles = append(pu.SecretFiles, &providerpb.SecretFile{Path: s.Path, Mode: modeTo(s.Mode), Secret: secretTo(&s.Secret), PerInstance: s.PerInstance})
	}
	return pu, nil
}

// unitFrom is the wire back to render.Unit; nil stays nil.
func unitFrom(pu *providerpb.Unit) (*render.Unit, error) {
	if pu == nil {
		return nil, nil
	}
	u := render.Unit{Key: pu.Key, Component: pu.Component, Replicas: int(pu.Replicas), Image: pu.Image,
		RestartPolicy: pu.RestartPolicy, Command: pu.Command, Args: pu.Args, Spread: pu.Spread}
	for _, e := range pu.Env {
		u.Env = append(u.Env, render.EnvVar{Name: e.Name, Value: e.Value, Secret: secretFrom(e.Secret)})
	}
	for _, p := range pu.Ports {
		u.Ports = append(u.Ports, render.Port{Name: p.Name, Port: int(p.Port)})
	}
	var err error
	if u.Platform, err = decodeMap(pu.PlatformJson); err != nil {
		return nil, err
	}
	if r := pu.Readiness; r != nil {
		u.Readiness = &render.Readiness{Exec: r.Exec}
		if r.Tcp != nil {
			u.Readiness.TCP = &render.TCPProbe{Port: int(r.Tcp.Port)}
		}
		if r.Http != nil {
			u.Readiness.HTTP = &render.HTTPProbe{Port: int(r.Http.Port), Path: r.Http.Path}
		}
	}
	for _, c := range pu.Data {
		u.Data = append(u.Data, render.Claim{Name: c.Name, Path: c.Path, Size: c.Size})
	}
	for _, l := range pu.Libraries {
		u.Libraries = append(u.Libraries, render.Library{Name: l.Name, Path: l.Path, Image: l.Image})
	}
	for _, pf := range pu.Files {
		f := render.File{Path: pf.Path, Mode: modeFrom(pf.Mode), Secrets: pf.Secrets}
		if pf.PerInstance {
			f.Instances = make([][]byte, len(pf.Instances))
			for i, b := range pf.Instances {
				if b.Present {
					f.Instances[i] = append([]byte{}, b.Data...)
				}
			}
		} else {
			f.Content = append([]byte{}, pf.Content...)
		}
		u.Files = append(u.Files, f)
	}
	for _, s := range pu.SecretFiles {
		sf := render.SecretFile{Path: s.Path, Mode: modeFrom(s.Mode), PerInstance: s.PerInstance}
		if s.Secret != nil {
			sf.Secret = render.SecretRef{Name: s.Secret.Name, Key: s.Secret.Key}
		}
		u.SecretFiles = append(u.SecretFiles, sf)
	}
	return &u, nil
}

func credentialsTo(creds []render.Credential) []*providerpb.Credential {
	var out []*providerpb.Credential
	for _, c := range creds {
		out = append(out, &providerpb.Credential{Name: c.Name, Leaf: c.Leaf, Value: c.Value, Key: c.Key})
	}
	return out
}

func credentialsFrom(in []*providerpb.Credential) []render.Credential {
	var out []render.Credential
	for _, c := range in {
		out = append(out, render.Credential{Name: c.Name, Leaf: c.Leaf, Value: c.Value, Key: c.Key})
	}
	return out
}
