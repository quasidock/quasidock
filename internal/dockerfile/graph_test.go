package dockerfile

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/moby/buildkit/frontend/dockerfile/parser"
)

func graphFromText(t *testing.T, text string, opts Options) (*stageGraph, []Diagnostic) {
	t.Helper()
	parsed, err := parser.Parse(bytes.NewBufferString(text))
	if err != nil {
		t.Fatal(err)
	}
	return parseStageGraph(parsed, opts)
}

func graphOK(t *testing.T, text string, opts Options) *stageGraph {
	t.Helper()
	g, diagnostics := graphFromText(t, text, opts)
	if len(diagnostics) != 0 {
		t.Fatalf("graph diagnostics: %v", diagnostics)
	}
	return g
}

func stageIndexes(stages []*dockerStage) []int {
	var indexes []int
	for _, stage := range stages {
		indexes = append(indexes, stage.index)
	}
	return indexes
}

func TestStageGraphSelection(t *testing.T) {
	opts := options(t)
	opts.BuildArgs = map[string]string{"VERSION": "24"}
	g := graphOK(t, `ARG VERSION=22
FROM node:${VERSION}-alpine AS Base
WORKDIR /app
FROM base AS build
RUN npm ci
FROM ubuntu AS unused
RUN apt-get update
FROM node:${VERSION} AS runtime
COPY --from=BUILD /app/dist /app/dist
COPY --from=1 /app/package.json /app/package.json
CMD ["node", "/app/dist/server.js"]`, opts)
	if g.stages[0].base != "node:24-alpine" || g.stages[1].parent != 0 || g.stages[3].parent != -1 {
		t.Fatalf("incorrect base expansion or inheritance: %+v %+v %+v", g.stages[0], g.stages[1], g.stages[3])
	}
	for _, tc := range []struct {
		name             string
		target           int
		ordered, skipped []int
	}{
		{"", 3, []int{0, 1, 3}, []int{2}},
		{"BUILD", 1, []int{0, 1}, []int{2, 3}},
		{"base", 0, []int{0}, []int{1, 2, 3}},
	} {
		t.Run("target="+tc.name, func(t *testing.T) {
			selection, err := g.selectTarget(tc.name)
			if err != nil {
				t.Fatal(err)
			}
			if selection.target.index != tc.target || !reflect.DeepEqual(stageIndexes(selection.ordered), tc.ordered) || !reflect.DeepEqual(stageIndexes(selection.skipped), tc.skipped) {
				t.Fatalf("target %d, ordered %v, skipped %v", selection.target.index, stageIndexes(selection.ordered), stageIndexes(selection.skipped))
			}
		})
	}
	if _, err := g.selectTarget("missing"); err == nil || !strings.Contains(err.Error(), "unknown target") {
		t.Fatalf("missing target: %v", err)
	}
	if _, err := g.selectTarget("1"); err == nil {
		t.Fatal("target indexes are not part of the target-name interface")
	}
}

func TestStageGraphInvalidReferences(t *testing.T) {
	for _, tc := range []struct {
		name, text, message string
		line                int
	}{
		{"duplicate names", "FROM node AS Build\nFROM node AS build", "duplicate stage name", 2},
		{"unknown copy", "FROM node\nCOPY --from=missing /x /x", "unknown COPY --from stage", 2},
		{"external image", "FROM node\nCOPY --from=nginx:latest /x /x", "external images", 2},
		{"negative index", "FROM node\nCOPY --from=-1 /x /x", "out of range", 2},
		{"large index", "FROM node\nCOPY --from=9 /x /x", "out of range", 2},
		{"overflow index", "FROM node\nCOPY --from=9999999999999999999999999999 /x /x", "out of range", 2},
		{"self by name", "FROM node AS build\nCOPY --from=build /x /x", "cannot reference itself", 2},
		{"self by index", "FROM node\nCOPY --from=0 /x /x", "cannot reference itself", 2},
		{"forward copy name", "FROM node\nCOPY --from=later /x /x\nFROM node AS later", "forward reference", 2},
		{"forward copy index", "FROM node\nCOPY --from=1 /x /x\nFROM node", "forward reference", 2},
		{"forward base", "FROM later AS first\nFROM node AS later", "forward reference", 1},
		{"self base", "FROM build AS build", "cannot reference itself", 1},
		{"unresolved base", "FROM node:${VERSION}", "unresolved variable", 1},
		{"local arg in base", "FROM node AS first\nARG BASE=node\nFROM ${BASE}", "unresolved variable", 3},
		{"copy selector expansion", "ARG SOURCE=build\nFROM node AS build\nFROM node\nCOPY --from=${SOURCE} /x /x", "unknown COPY --from stage", 4},
		{"instruction before from", "ENV MODE=wrong\nFROM node", "only ARG", 1},
		{"missing from", "ARG BASE=node", "base image is required", 1},
		{"unused syntax error", "FROM ubuntu AS unused\nCOPY one\nFROM node", "COPY requires at least two arguments", 2},
		{"unused graph error", "FROM ubuntu AS unused\nCOPY --from=missing /x /x\nFROM node", "unknown COPY --from stage", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, diagnostics := graphFromText(t, tc.text, options(t))
			if len(diagnostics) == 0 || diagnostics[0].Line != tc.line || !strings.Contains(diagnostics[0].Message, tc.message) {
				t.Fatalf("want line %d containing %q, got %v", tc.line, tc.message, diagnostics)
			}
		})
	}
}

func TestStageGraphBaseNamesAndScratch(t *testing.T) {
	g := graphOK(t, "FROM node AS node\nFROM node AS build\nFROM scratch AS runtime\nCOPY --from=build /out/server /server", options(t))
	if g.stages[0].parent != -1 || g.stages[1].parent != 0 || g.stages[2].base != "scratch" || g.stages[2].parent != -1 {
		t.Fatalf("incorrect external/parent bases: %+v %+v %+v", g.stages[0], g.stages[1], g.stages[2])
	}
	g = graphOK(t, "FROM node AS build\nFROM node AS node", options(t))
	if g.stages[0].parent != -1 || g.stages[1].parent != -1 {
		t.Fatal("a later alias shadowed an external base")
	}
}

func TestStageGraphCycleDetection(t *testing.T) {
	// Parsed graphs require backward references, which already excludes cycles.
	// The traversal also rejects cycles if future graph transformations add one.
	g := &stageGraph{stages: []*dockerStage{
		{index: 0, name: "first", dependencies: []int{1}},
		{index: 1, name: "second", dependencies: []int{0}},
	}}
	if _, err := g.selectTarget(""); err == nil || !strings.Contains(err.Error(), "circular dependency") {
		t.Fatalf("cycle was accepted: %v", err)
	}
}

func TestStageSelectionSkipsNativeCompatibilityInUnusedBranches(t *testing.T) {
	opts := options(t)
	g := graphOK(t, "FROM ubuntu AS unused\nRUN apt-get update\nFROM node:24 AS runtime\nCMD [\"node\"]", opts)
	selection, err := g.selectTarget("")
	if err != nil {
		t.Fatal(err)
	}
	if got := stageIndexes(selection.ordered); !reflect.DeepEqual(got, []int{1}) {
		t.Fatal(got)
	}
	s := g.translateStage(selection.target, nil, opts, nil)
	if s.failed() || s.family != "node" || s.requested != "24" {
		t.Fatalf("unused branch affected translation: %v", s.result.Diagnostics)
	}
	unused := g.translateStage(g.stages[0], nil, opts, nil)
	if !unused.failed() {
		t.Fatal("the unused Linux base was unexpectedly compatible")
	}
}

func TestStageConfigurationInheritanceAndIsolation(t *testing.T) {
	opts := options(t)
	g := graphOK(t, `ARG VERSION=1.26
ARG SHARED=global
FROM golang:${VERSION} AS base
ARG SHARED=parent
ARG LOCAL=local
ENV MODE=base ROOT=/app
WORKDIR /app
RUN go build -o server .
LABEL org.opencontainers.image.title=Base
ENTRYPOINT ["/app/server", "--base"]
CMD ["--default"]
FROM base AS child
ARG LOCAL
ENV MODE=child CHILD_ONLY=yes
WORKDIR child
RUN go build -o child .
CMD ["--child"]
FROM base AS sibling
ENTRYPOINT ["/app/server"]
FROM golang:${VERSION} AS unrelated
ARG SHARED
ENV MODE=fresh`, opts)
	base := g.translateStage(g.stages[0], nil, opts, nil)
	child := g.translateStage(g.stages[1], base, opts, nil)
	sibling := g.translateStage(g.stages[2], base, opts, nil)
	unrelated := g.translateStage(g.stages[3], nil, opts, nil)
	for _, s := range []*state{base, child, sibling, unrelated} {
		if s.failed() {
			t.Fatal(s.result.Diagnostics)
		}
	}
	if child.args["SHARED"] != "parent" || child.args["LOCAL"] != "local" || child.env["MODE"] != "child" || child.workdir != "app/child" || child.family != "go" || child.requested != "1.26" {
		t.Fatalf("child configuration: %+v", child.stageConfig)
	}
	if child.outputs["app/server"] != "app/server.exe" || child.outputs["app/child/child"] != "app/child/child.exe" {
		t.Fatal(child.outputs)
	}
	if len(child.r.Stages) != 3 || child.r.Stages[2].Env["SHARED"] != "parent" || child.r.Stages[2].Env["MODE"] != "child" {
		t.Fatalf("child steps should contain only its own commands with inherited scope: %+v", child.r.Stages)
	}
	if child.r.Name == nil || *child.r.Name != "Base" || child.entry.CmdLine[0] != "/app/server" || child.cmd.CmdLine[0] != "--child" {
		t.Fatalf("startup and metadata were not inherited: %+v", child)
	}
	if sibling.cmd != nil || sibling.entry.CmdLine[0] != "/app/server" {
		t.Fatal("ENTRYPOINT did not reset inherited CMD")
	}
	if err := child.startup(); err != nil || child.r.Entrypoint[0] != "${app}/app/server.exe" || !reflect.DeepEqual(child.r.Cmd, []string{"--child"}) {
		t.Fatalf("inherited executable mapping/startup: %v %+v", err, child.r)
	}
	if unrelated.args["SHARED"] != "global" || len(unrelated.args) != 1 || len(unrelated.env) != 1 || unrelated.workdir != "." || len(unrelated.outputs) != 0 || unrelated.r.Name != nil || unrelated.entry != nil || unrelated.cmd != nil {
		t.Fatalf("state leaked into unrelated FROM: %+v", unrelated)
	}
	if _, exists := child.args["VERSION"]; exists {
		t.Fatal("global ARG was inherited without being consumed")
	}
	child.args["LOCAL"] = "changed"
	child.env["MODE"] = "changed"
	child.outputs["app/server"] = "changed"
	child.entry.CmdLine[0] = "changed"
	*child.r.Name = "Changed"
	child.globals["VERSION"] = "changed"
	if base.args["LOCAL"] != "local" || base.env["MODE"] != "base" || base.outputs["app/server"] != "app/server.exe" || base.entry.CmdLine[0] != "/app/server" || *base.r.Name != "Base" || sibling.env["MODE"] != "base" || g.globals["VERSION"] != "1.26" || child.r.Stages[2].Env["MODE"] != "child" {
		t.Fatal("child mutation affected parent, sibling, globals, or an instruction snapshot")
	}
}

func TestInheritedStageRejectsPlatformOverride(t *testing.T) {
	opts := options(t)
	g := graphOK(t, "FROM node AS base\nFROM --platform=linux/amd64 base AS child", opts)
	base := g.translateStage(g.stages[0], nil, opts, nil)
	child := g.translateStage(g.stages[1], base, opts, nil)
	if !child.failed() || child.result.Diagnostics[0].Line != 2 || !strings.Contains(child.result.Diagnostics[0].Message, "--platform") {
		t.Fatal(child.result.Diagnostics)
	}
	child = g.translateStage(g.stages[1], nil, opts, nil)
	if !child.failed() || !strings.Contains(child.result.Diagnostics[0].Message, "parent stage") {
		t.Fatal(child.result.Diagnostics)
	}
}

func TestMultiStageEmissionUsesBuildUnits(t *testing.T) {
	resolver := &fakeResolver{}
	result, err := Convert([]byte("FROM node AS build\nRUN npm ci\nFROM node AS runtime\nCOPY --from=build /app /app\nCMD [\"node\"]"), options(t), resolver)
	if err != nil || result.Recipe == nil || len(result.Recipe.Builds) != 1 {
		t.Fatalf("multi-stage emission failed: %v %+v", err, result.Diagnostics)
	}
	if result.Recipe.Stages[0].Sources[0].Build.Name != "docker-stage-0" || result.SourceLines["step-001"] != 4 {
		t.Fatal(result)
	}
	if strings.Contains(fmt.Sprint(result.Diagnostics), "unsupported") {
		t.Fatal(result.Diagnostics)
	}
}
