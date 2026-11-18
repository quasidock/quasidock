package dockerfile

import (
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"
)

func TestConvertTargetSelectsRuntimeAndDependencies(t *testing.T) {
	data := []byte(`FROM node:22 AS source
ENV MODE=source
WORKDIR /source
CMD ["node","source.js"]
FROM source AS preview
ENV MODE=preview
CMD ["node","preview.js"]
FROM alpine AS unused
RUN apt-get update
FROM python:3.14 AS production
CMD ["python","main.py"]`)
	for _, tc := range []struct {
		target, family, argument, workdir, mode string
		calls                                   []string
		units                                   int
	}{
		{"PREVIEW", "node", "preview.js", "source", "preview", []string{"node:22:", "busybox::"}, 1},
		{"source", "node", "source.js", "source", "source", []string{"node:22:", "busybox::"}, 0},
		{"", "python", "main.py", ".", "", []string{"python-sdk:3.14:", "busybox::"}, 0},
	} {
		t.Run(tc.target, func(t *testing.T) {
			opts := options(t)
			opts.Target = tc.target
			resolver := &fakeResolver{}
			result, err := Convert(data, opts, resolver)
			if err != nil {
				t.Fatalf("%v %v", err, result.Diagnostics)
			}
			r := result.Recipe
			if len(r.From) != 1 || r.From[tc.family] == "" || len(r.Builds) != tc.units || !reflect.DeepEqual(r.Cmd, []string{tc.argument}) || r.Workdir != tc.workdir || r.Env["MODE"] != tc.mode || !reflect.DeepEqual(resolver.calls, tc.calls) {
				t.Fatalf("wrong selected runtime: %+v; lookups: %v", r, resolver.calls)
			}
			if !strings.Contains(fmt.Sprint(result.Diagnostics), "selected conversion target") || !strings.Contains(fmt.Sprint(result.Diagnostics), "stage unused (2): unused stage skipped") {
				t.Fatal(result.Diagnostics)
			}
		})
	}
}

func TestConvertTargetErrorsBeforeResolution(t *testing.T) {
	for _, tc := range []struct{ name, target, text, message string }{
		{"unknown", "missing", "FROM node AS app", "unknown target stage"},
		{"numeric", "0", "FROM node AS app", "unknown target stage"},
		{"artifact target", "build", "FROM golang AS build\nRUN go build -o /app .\nFROM scratch\nCOPY --from=build /app /app\nCMD [\"/app\"]", "CMD"},
		{"invalid unused reference", "app", "FROM node AS app\nFROM scratch\nCOPY --from=missing /x /x", "unknown COPY --from stage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := options(t)
			opts.Target = tc.target
			resolver := &fakeResolver{}
			result, err := Convert([]byte(tc.text), opts, resolver)
			if err == nil || result.Recipe != nil || len(resolver.calls) != 0 || !strings.Contains(fmt.Sprint(result.Diagnostics), tc.message) {
				t.Fatalf("expected %q before lookup: %v %v %v", tc.message, err, result.Diagnostics, resolver.calls)
			}
		})
	}
}

func TestConvertStageBaseOverrides(t *testing.T) {
	data := []byte("FROM node:custom@sha256:" + strings.Repeat("b", 64) + ` AS build
COPY app.js /src/app.js
FROM build AS preview
CMD ["node","src/app.js"]
FROM node:22 AS runtime
COPY --from=preview /src/ /app/
CMD ["node","app/app.js"]`)
	for _, tc := range []struct {
		name, target, base string
		overrides          map[string]string
		calls              []string
		units              int
	}{
		{"names", "", "", map[string]string{"BUILD": "node:24", "runtime": "node:26"}, []string{"node::node:24", "node:22:node:26"}, 2},
		{"indexes", "", "", map[string]string{"0": "node:24", "2": "node:26"}, []string{"node::node:24", "node:22:node:26"}, 2},
		{"inherited target", "preview", "", map[string]string{"build": "node:24", "runtime": "node:26"}, []string{"node::node:24"}, 1},
		{"base selects earlier target", "build", "node:24", map[string]string{"runtime": "node:26"}, []string{"node::node:24"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := options(t)
			opts.Target, opts.Base, opts.StageBases = tc.target, tc.base, tc.overrides
			before := maps.Clone(opts.StageBases)
			resolver := &fakeResolver{}
			result, err := Convert(data, opts, resolver)
			if err != nil || !reflect.DeepEqual(resolver.calls, tc.calls) {
				t.Fatalf("%v %v %v", err, result.Diagnostics, resolver.calls)
			}
			if len(result.Recipe.Builds) != tc.units || len(result.Recipe.From) != 1 || result.Recipe.From["node"] == "" {
				t.Fatalf("wrong dependency closure: %+v", result.Recipe)
			}
			if !maps.Equal(opts.StageBases, before) {
				t.Fatal("conversion changed caller overrides")
			}
			if tc.target != "" && !strings.Contains(fmt.Sprint(result.Diagnostics), "unused stage and its native base override skipped") {
				t.Fatal(result.Diagnostics)
			}
		})
	}
}

func TestConvertStageBaseValidation(t *testing.T) {
	data := []byte("FROM node AS build\nFROM build AS inherited\nFROM scratch AS empty\nCMD [\"/app.exe\"]")
	for _, tc := range []struct {
		name, target, base string
		overrides          map[string]string
		message            string
	}{
		{"unknown", "", "", map[string]string{"typo": "node:24"}, "unknown --stage-base stage"},
		{"empty selector", "", "", map[string]string{"": "node:24"}, "unknown --stage-base stage"},
		{"negative", "", "", map[string]string{"-1": "node:24"}, "out of range"},
		{"out of range", "", "", map[string]string{"3": "node:24"}, "out of range"},
		{"overflow", "", "", map[string]string{strings.Repeat("9", 40): "node:24"}, "out of range"},
		{"empty image", "", "", map[string]string{"build": ""}, "nonempty native image"},
		{"whitespace image", "", "", map[string]string{"build": " "}, "nonempty native image"},
		{"duplicate alias", "", "", map[string]string{"build": "node:24", "0": "node:24"}, "duplicate --stage-base"},
		{"duplicate casing", "", "", map[string]string{"build": "node:24", "BUILD": "node:26"}, "duplicate --stage-base"},
		{"duplicate index", "", "", map[string]string{"0": "node:24", "00": "node:26"}, "duplicate --stage-base"},
		{"inherited unused", "build", "", map[string]string{"inherited": "node:24"}, "concrete external base"},
		{"scratch unused", "build", "", map[string]string{"empty": "node:24"}, "concrete external base"},
		{"target conflict", "build", "node:24", map[string]string{"0": "node:24"}, "--base conflicts"},
		{"base on inherited", "inherited", "node:24", nil, "--base requires"},
		{"base on scratch", "", "node:24", nil, "--base requires"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := options(t)
			opts.Target, opts.Base, opts.StageBases = tc.target, tc.base, tc.overrides
			resolver := &fakeResolver{}
			result, err := Convert(data, opts, resolver)
			if err == nil || result.Recipe != nil || len(resolver.calls) != 0 || !strings.Contains(fmt.Sprint(result.Diagnostics), tc.message) {
				t.Fatalf("expected %q before lookup: %v %v %v", tc.message, err, result.Diagnostics, resolver.calls)
			}
		})
	}
}

func TestConvertSingleStageTargetAndOverride(t *testing.T) {
	opts := options(t)
	opts.Target = "APP"
	opts.StageBases = map[string]string{"0": "node:24"}
	resolver := &fakeResolver{}
	result, err := Convert([]byte("FROM node:custom AS app\nCOPY app.js /app.js\nCMD [\"node\",\"app.js\"]"), opts, resolver)
	if err != nil || !reflect.DeepEqual(resolver.calls, []string{"node::node:24"}) {
		t.Fatalf("%v %v %v", err, result.Diagnostics, resolver.calls)
	}
	for name := range result.SourceLines {
		if result.SourceStages[name] != "app (0)" {
			t.Fatal("context diagnostics lost the selected stage")
		}
	}
}
