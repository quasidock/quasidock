package mounts

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseAndTargets(t *testing.T) {
	for _, tc := range []struct {
		value                string
		short                bool
		kind, source, target string
	}{
		{`work:${data}`, true, "volume", "work", "${data}"},
		{`C:\my work:${data}\workspace:rw`, true, "bind", `C:\my work`, "${data}/workspace"},
		{`..\work:${data}/src`, true, "bind", `..\work`, "${data}/src"},
		{`type=bind,"source=C:\work, files",target=${data}/src`, false, "bind", `C:\work, files`, "${data}/src"},
	} {
		s, err := Parse(tc.value, tc.short)
		if err != nil {
			t.Fatal(err)
		}
		if s.Type != tc.kind || s.Source != tc.source || s.Target != tc.target {
			t.Fatalf("%s: %+v", tc.value, s)
		}
	}
	for _, tc := range []struct {
		value string
		short bool
	}{
		{`work:${data}:ro`, true}, {`:${data}`, true}, {`work:C:\data`, true},
		{`type=bind,source=C:\work,target=${data}/../outside`, false},
		{`type=bind,source=C:\work,target=${data}/NUL.txt`, false},
		{`type=volume,source=CON,target=${data}`, false},
		{`type=volume,source=work,target=${data}/x:ads`, false},
		{`type=bind,source=x,source=y,target=${data}`, false},
		{`type=bind,source=x,target=${data},readonly`, false},
		{`type=tmpfs,source=x,target=${data}`, false},
	} {
		if _, err := Parse(tc.value, tc.short); err == nil {
			t.Errorf("accepted %s", tc.value)
		}
	}
	for _, pair := range [][2]string{{"${data}", "${data}/x"}, {"${data}/X", "${data}/x"}, {"${data}/x", "${data}/x/y"}} {
		if err := ValidateTargets([]Spec{{Target: pair[0]}, {Target: pair[1]}}); err == nil {
			t.Errorf("accepted overlap %v", pair)
		}
	}
	if err := ValidateTargets([]Spec{{Target: "${data}/x"}, {Target: "${data}/xy"}}); err != nil {
		t.Fatal(err)
	}
}

func TestJunctionCleanupPreservesBackingData(t *testing.T) {
	for _, target := range []string{"${data}", "${data}/workspace"} {
		t.Run(target, func(t *testing.T) {
			base := t.TempDir()
			source := filepath.Join(base, "source")
			container := filepath.Join(base, "container")
			data := filepath.Join(container, "data")
			if err := os.MkdirAll(source, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(data, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, "sentinel"), []byte("keep me"), 0o644); err != nil {
				t.Fatal(err)
			}
			m := Mount{Source: source, Destination: Destination(data, target), RW: true}
			if err := Attach(data, m); err != nil {
				t.Fatal(err)
			}
			if _, err := CanonicalDir(m.Destination); err != nil {
				t.Fatalf("canonical junction path: %v", err)
			}
			if err := os.MkdirAll(filepath.Join(m.Destination, "registry"), 0o755); err != nil {
				t.Fatal(err)
			}
			if resolved, err := CanonicalDir(filepath.Join(m.Destination, "registry")); err != nil || resolved != filepath.Join(source, "registry") {
				t.Fatalf("canonical directory beneath junction: %s %v", resolved, err)
			}
			if b, err := os.ReadFile(filepath.Join(m.Destination, "sentinel")); err != nil || string(b) != "keep me" {
				t.Fatalf("junction read: %s %v", b, err)
			}
			if size := TreeSize(container); size != 0 {
				t.Fatalf("counted mounted bytes: %d", size)
			}
			if err := RemoveTree(container); err != nil {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(filepath.Join(source, "sentinel")); err != nil || string(b) != "keep me" {
				t.Fatalf("source lost: %s %v", b, err)
			}
		})
	}
}

func TestAttachRejectsExistingContentAndReparseAncestors(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "source")
	data := filepath.Join(base, "container", "data")
	_ = os.MkdirAll(source, 0o755)
	_ = os.MkdirAll(data, 0o755)
	_ = os.WriteFile(filepath.Join(data, "sentinel"), []byte("keep"), 0o644)
	if err := Attach(data, Mount{Source: source, Destination: data}); err == nil {
		t.Fatal("overwrote nonempty destination")
	}
	_ = os.Remove(filepath.Join(data, "sentinel"))
	if err := Attach(data, Mount{Source: source, Destination: filepath.Join(data, "link")}); err != nil {
		t.Fatal(err)
	}
	if err := Attach(data, Mount{Source: source, Destination: filepath.Join(data, "link", "nested")}); err == nil {
		t.Fatal("followed destination junction")
	}
	if _, err := os.Stat(filepath.Join(source, "nested")); !os.IsNotExist(err) {
		t.Fatal("wrote through destination junction")
	}
	if err := RemoveTree(filepath.Dir(data)); err != nil {
		t.Fatal(err)
	}
}
