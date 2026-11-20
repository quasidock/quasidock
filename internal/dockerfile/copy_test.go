package dockerfile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/recipe"
)

func TestMultiStageGoCopyPaths(t *testing.T) {
	for _, tc := range []struct {
		name, copy, startup, logical, native string
	}{
		{"named file", "COPY --from=build /out/server ./server", "./server", "app/server", "app/server.exe"},
		{"numeric rename", "COPY --from=0 /out/server /tools/renamed", "/tools/renamed", "tools/renamed", "tools/renamed.exe"},
		{"directory contents", "COPY --from=build /out/ .", "./server", "app/server", "app/server.exe"},
		{"glob", "COPY --from=build /out/serv* ./", "./server", "app/server", "app/server.exe"},
		{"source root ignores workdir", "COPY --from=build out/server .", "./server", "app/server", "app/server.exe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := "FROM golang:1.26 AS build\nWORKDIR /src\nCOPY . .\nRUN go build -o /out/server .\nFROM scratch AS runtime\nWORKDIR /app\n" + tc.copy + "\nCMD [" + fmt.Sprintf("%q", tc.startup) + "]"
			resolver := &fakeResolver{}
			result, err := Convert([]byte(input), options(t), resolver)
			if err != nil {
				t.Fatalf("%v: %v", err, result.Diagnostics)
			}
			r := result.Recipe
			if len(r.Builds) != 1 || r.Builds[0].From["go"] == "" || len(r.From) != 0 || r.Entrypoint[0] != "${app}/"+tc.native {
				t.Fatalf("incorrect Go stage emission: %+v", r)
			}
			if !reflect.DeepEqual(r.Builds[0].Executables, []string{"out/server"}) || !reflect.DeepEqual(r.Executables, []string{tc.logical}) {
				t.Fatalf("artifact mappings were not propagated: %v %v", r.Builds[0].Executables, r.Executables)
			}
			if result.SourceLines["docker-stage-0/step-002"] != 3 || result.SourceLines["step-002"] != 7 {
				t.Fatalf("COPY source locations lost: %v", result.SourceLines)
			}
			if !strings.Contains(fmt.Sprint(result.Diagnostics), "mapped executable output out/server to out/server.exe") || !strings.Contains(fmt.Sprint(result.Diagnostics), "mapped executable "+tc.logical) {
				t.Fatal("native substitutions were not reported:", result.Diagnostics)
			}
			encoded, _ := json.Marshal(r)
			if _, err := recipe.Parse("converted.json", encoded); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMultiStageExecutableGlobAndIntermediateCopy(t *testing.T) {
	r := convertOK(t, `FROM golang AS build
RUN go build -o /out/serv1 . && go build -o /out/serv2 .
FROM scratch AS middle
COPY --from=build /out/serv? /pkg/
FROM scratch
COPY --from=middle /pkg/serv1 /bin/renamed
CMD ["/bin/renamed"]`)
	if len(r.Builds) != 2 || !reflect.DeepEqual(r.Builds[1].Executables, []string{"pkg/serv1", "pkg/serv2"}) || r.Entrypoint[0] != "${app}/bin/renamed.exe" {
		t.Fatalf("glob or intermediate mappings lost: %+v", r)
	}
	if paths := r.Builds[1].Stages[0].Sources[0].Build.Paths; !reflect.DeepEqual(paths, []string{"out/serv?"}) {
		t.Fatal("logical selectors were replaced by physical filenames:", paths)
	}
}

func TestMultiStageNativeSuffixAndWholeOutput(t *testing.T) {
	r := convertOK(t, `FROM golang AS build
RUN go build -o /out/server.exe .
FROM scratch
COPY --from=build / /package/
COPY --from=build /out/server.exe /renamed
CMD ["/renamed"]`)
	if !reflect.DeepEqual(r.Executables, []string{"package/out/server.exe", "renamed"}) || r.Entrypoint[0] != "${app}/renamed.exe" {
		t.Fatalf("native-named executable rename lost: %+v", r)
	}
}

func TestMultiStageCopyIntoCreatedDirectoryAndArgumentMapping(t *testing.T) {
	r := convertOK(t, `FROM golang AS build
RUN go build -o /out/server .
FROM scratch
RUN mkdir -p /tools
COPY --from=build /out/server /tools
CMD ["/tools/server"]`)
	if r.Entrypoint[0] != "${app}/tools/server.exe" {
		t.Fatal(r.Entrypoint)
	}
	r = convertOK(t, `FROM golang AS build
RUN go build -o /server .
FROM node
COPY --from=build /server /app/worker
ENTRYPOINT ["node", "launcher.js"]
CMD ["/app/worker"]`)
	if !reflect.DeepEqual(r.Cmd, []string{"${app}/app/worker.exe"}) {
		t.Fatal(r.Cmd)
	}
}

func TestMultiStageCopyIntoLiteralContextDirectory(t *testing.T) {
	opts := options(t)
	if err := os.Mkdir(filepath.Join(opts.Context, "assets"), 0755); err != nil {
		t.Fatal(err)
	}
	input := "FROM golang AS build\nRUN go build -o /out/server .\nFROM scratch\nCOPY assets /app\nCOPY --from=build /out/server /app\nCMD [\"/app/server\"]"
	result, err := Convert([]byte(input), opts, &fakeResolver{})
	if err != nil || result.Recipe.Entrypoint[0] != "${app}/app/server.exe" {
		t.Fatalf("context directory was treated as a file rename: %v %v", err, result.Diagnostics)
	}
}

func TestMultiStageInheritedFilesystemAndRuntime(t *testing.T) {
	r := convertOK(t, `FROM golang:1.26 AS build
ENV MODE=base
WORKDIR /app
COPY . .
RUN go build -o server .
ENTRYPOINT ["/app/server"]
CMD ["--old"]
FROM build AS runtime
ENV MODE=runtime
CMD ["--new"]`)
	if len(r.Builds) != 1 || r.From["go"] != r.Builds[0].From["go"] || r.Env["MODE"] != "runtime" || r.Workdir != "app" || r.Entrypoint[0] != "${app}/app/server.exe" || !reflect.DeepEqual(r.Cmd, []string{"--new"}) {
		t.Fatalf("inherited stage configuration: %+v", r)
	}
	if len(r.Stages) != 1 || r.Stages[0].Sources[0].Build.Name != "docker-stage-0" || !reflect.DeepEqual(r.Stages[0].Sources[0].Build.Paths, []string{"."}) {
		t.Fatal("parent instructions repeated instead of copying its filesystem:", r.Stages)
	}
}

func TestMultiStageLanguageCopiesKeepFinalConfiguration(t *testing.T) {
	for _, tc := range []struct{ family, input, executable string }{
		{"node", `FROM node:22 AS builder
ENV BUILD_ONLY=yes
COPY . /src/
RUN npm ci
FROM node:24
COPY --from=builder /src/dist /app/
ENV MODE=production
CMD ["node", "/app/server.js"]`, "${node}/node.exe"},
		{"python", `FROM python:3.14 AS deps
COPY requirements.txt /requirements.txt
RUN pip install -r /requirements.txt
FROM python:3.14
COPY --from=deps /.python /.python/
COPY main.py /main.py
CMD ["python", "/main.py"]`, "${python}/python.exe"},
	} {
		t.Run(tc.family, func(t *testing.T) {
			r := convertOK(t, tc.input)
			if len(r.Builds) != 1 || r.From[tc.family] == "" || r.Entrypoint[0] != tc.executable || r.Env["BUILD_ONLY"] != "" {
				t.Fatalf("builder state leaked into final runtime: %+v", r)
			}
			if tc.family == "python" && r.Env["PYTHONPATH"] != "${app}/.python" {
				t.Fatal(r.Env)
			}
		})
	}
}

func TestMultiStageBuildOnlyStartupAndUnusedBranches(t *testing.T) {
	input := `FROM ubuntu AS unused
RUN apt-get update
FROM node AS builder
ENTRYPOINT ["/bin/bash"]
COPY . /app/
FROM scratch
COPY --from=builder /app/app.exe /app.exe
CMD ["./app.exe"]`
	resolver := &fakeResolver{}
	result, err := Convert([]byte(input), options(t), resolver)
	if err != nil || len(result.Recipe.Builds) != 1 || len(result.Recipe.From) != 0 {
		t.Fatalf("unused stage or builder startup prevented conversion: %v %v", err, result.Diagnostics)
	}
	if !reflect.DeepEqual(resolver.calls, []string{"node::"}) || !strings.Contains(fmt.Sprint(result.Diagnostics), "unused stage skipped") {
		t.Fatalf("unused tools were resolved: %v %v", resolver.calls, result.Diagnostics)
	}
}

func TestMultiStageShellStartupKeepsOnlyRequiredRuntime(t *testing.T) {
	r := convertOK(t, "FROM golang AS build\nRUN go build -o /server .\nFROM scratch\nCOPY --from=build /server /server\nCMD /server --port \"$PORT\"")
	if len(r.From) != 1 || r.From["busybox"] == "" || r.SDK["busybox"] != "" || !strings.Contains(r.Entrypoint[3], "server.exe") {
		t.Fatalf("shell/runtime dependencies incorrect: %+v", r)
	}
}

func TestMultiStageCopyAndArtifactErrors(t *testing.T) {
	for _, tc := range []struct{ name, input, message string }{
		{"copy ownership", "FROM node AS build\nFROM node\nCOPY --from=build --chown=1000 /app /app", "COPY flag --chown"},
		{"copy mount", "FROM node AS build\nFROM node\nCOPY --from=build --link /app /app", "COPY flag --link"},
		{"source escape", "FROM node AS build\nFROM node\nCOPY --from=build /../outside /app", "source root"},
		{"multiple destinations", "FROM node AS build\nFROM node\nCOPY --from=build /a /b /app", "multiple sources"},
		{"ambiguous output", "FROM golang AS build\nRUN go build -o /server . && go build -o /server.exe .\nFROM scratch\nCOPY --from=build /server /server\nCMD [\"/server\"]", "ambiguous executable"},
		{"scratch startup", "FROM node AS build\nFROM scratch\nCOPY --from=build /app /app", "scratch stages require"},
		{"unsupported reachable base", "FROM fedora AS build\nFROM scratch\nCOPY --from=build /app /app\nCMD [\"/app.exe\"]", "base \"fedora\" is unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolver := &fakeResolver{}
			result, err := Convert([]byte(tc.input), options(t), resolver)
			if err == nil || result.Recipe != nil || !strings.Contains(fmt.Sprint(result.Diagnostics), tc.message) || len(resolver.calls) != 0 {
				t.Fatalf("expected %q before metadata resolution: %v %v %v", tc.message, err, result.Diagnostics, resolver.calls)
			}
		})
	}
}

func TestMultiStageFinalBaseOverride(t *testing.T) {
	opts := options(t)
	opts.Base = "node:24"
	resolver := &fakeResolver{}
	result, err := Convert([]byte("FROM golang:1.26 AS build\nRUN go build -o /app .\nFROM node:22\nCOPY --from=build /app /app\nCMD [\"/app\"]"), opts, resolver)
	if err != nil || !reflect.DeepEqual(resolver.calls, []string{"go:1.26:", "busybox::", "node:22:node:24"}) || result.Recipe.From["node"] == "" {
		t.Fatalf("override leaked into a builder: %v %v %v", err, result.Diagnostics, resolver.calls)
	}
	for _, final := range []string{"FROM scratch", "FROM build"} {
		resolver := &fakeResolver{}
		result, err := Convert([]byte("FROM node AS build\n"+final+"\nCMD [\"/app.exe\"]"), opts, resolver)
		if err == nil || len(resolver.calls) != 0 || !strings.Contains(fmt.Sprint(result.Diagnostics), "--base requires") {
			t.Fatalf("ambiguous final override was accepted: %v %v", err, result.Diagnostics)
		}
	}
}

func TestScratchBaseMustBeUnqualifiedAndUntagged(t *testing.T) {
	for _, base := range []string{"scratch:latest", "scratch:1", "docker.io/library/scratch"} {
		resolver := &fakeResolver{}
		result, err := Convert([]byte("FROM "+base+"\nCOPY app.exe /app.exe\nCMD [\"/app.exe\"]"), options(t), resolver)
		if err == nil || !strings.Contains(fmt.Sprint(result.Diagnostics), "untagged, unqualified") || len(resolver.calls) != 0 {
			t.Fatalf("invalid scratch base was accepted: %s %v %v", base, err, result.Diagnostics)
		}
	}
}
