package dockerfile

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestUbuntuFlaskConversion(t *testing.T) {
	resolver := &fakeResolver{}
	result, err := Convert([]byte(`FROM ubuntu:24.04
EXPOSE 8501
RUN apt-get update && apt-get install -y python3 python3-pip
RUN pip install flask==3.0.*
COPY hello.py /
ENV FLASK_APP=hello
CMD ["flask", "run", "--host", "0.0.0.0", "--port", "8501"]`), options(t), resolver)
	if err != nil {
		t.Fatalf("%v: %v", err, result.Diagnostics)
	}
	r := result.Recipe
	if !reflect.DeepEqual(resolver.calls, []string{"python-sdk::", "busybox::"}) || len(r.From) != 1 || r.From["python"] == "" || r.Env["PYTHONPATH"] != "${app}/.python" || r.Env["FLASK_APP"] != "hello" || r.Entrypoint[3] != "flask" {
		t.Fatalf("calls %v, recipe %+v", resolver.calls, r)
	}
	if !strings.Contains(fmt.Sprint(result.Diagnostics), "source distro metadata") || !strings.Contains(fmt.Sprint(result.Diagnostics), "apt package python3-pip") {
		t.Fatal(result.Diagnostics)
	}
	for _, stage := range r.Stages {
		for _, command := range stage.Build {
			if strings.Contains(command, "apt-get") {
				t.Fatal(command)
			}
		}
	}
}

func TestAptPackageCatalog(t *testing.T) {
	for _, tc := range []struct{ pkg, alias, image string }{
		{"python3", "python", "python-sdk"}, {"python3-pip", "python", "python-sdk"}, {"python3-dev", "python", "python-sdk"},
		{"nodejs", "node", "node"}, {"npm", "node", "node"}, {"golang", "go", "go"}, {"golang-go", "go", "go"},
		{"git", "git", "mingit"}, {"make", "make", "make"}, {"cmake", "cmake", "cmake"}, {"ninja-build", "cmake", "cmake"},
		{"clang", "compiler", "llvm-mingw"}, {"gcc", "compiler", "llvm-mingw"}, {"g++", "compiler", "llvm-mingw"},
	} {
		t.Run(tc.pkg, func(t *testing.T) {
			r := convertOK(t, "FROM debian:bookworm-slim\nRUN apt-get -qq install --no-install-recommends -y "+tc.pkg+"\nCMD [\"/app.exe\"]")
			if len(r.From) != 1 || !strings.Contains(r.From[tc.alias], "/"+tc.image+"@sha256:") {
				t.Fatalf("wrong dependencies: %v", r.From)
			}
		})
	}
	r := convertOK(t, "FROM ubuntu\nRUN apt install build-essential gcc g++ make\nCMD [\"/app.exe\"]")
	if len(r.From) != 2 || !strings.Contains(r.From["compiler"], "/llvm-mingw@") || !strings.Contains(r.From["make"], "/make@") {
		t.Fatal(r.From)
	}
}

func TestDistroNamesAndAptForms(t *testing.T) {
	for _, base := range []string{"ubuntu", "ubuntu:24.04", "docker.io/library/ubuntu:noble", "library/debian:trixie-slim", "docker.io/debian:latest", "node:24-bookworm"} {
		t.Run(base, func(t *testing.T) {
			r := convertOK(t, "FROM "+base+` 
ARG PACKAGE=python3
RUN DEBIAN_FRONTEND=noninteractive /usr/bin/apt-get update && 'apt-get' install --assume-yes "$PACKAGE" python3-pip
RUN ["apt", "install", "--yes", "python3-dev"]
RUN apt clean; apt-get autoclean
RUN rm -rf /var/lib/apt/lists/*
CMD ["/usr/bin/python3", "main.py"]`)
			if r.Entrypoint[0] != "${python}/python.exe" || r.From["python"] == "" {
				t.Fatalf("%+v", r)
			}
		})
	}
}

func TestDistroNativeCommandsAndPaths(t *testing.T) {
	r := convertOK(t, `FROM debian:trixie
WORKDIR /src
RUN apt update && apt install -y python3 nodejs npm golang-go git build-essential cmake ninja-build
RUN git -C /src --git-dir=/src/.git status
RUN make -C /src -f /src/Makefile -I/src/include
RUN cmake -S /src -B /build && cmake --build /build
RUN ninja -C /build -f /build/build.ninja
RUN gcc -I/src/include -isystem /src/system -c /src/main.c -o /out/main.o
RUN g++ /out/main.o -o/out/server
RUN /out/server
CMD ["/out/server"]`)
	if len(r.From) != 7 || r.Entrypoint[0] != "${app}/out/server.exe" || !reflect.DeepEqual(r.Executables, []string{"out/server"}) {
		t.Fatalf("%+v", r)
	}
	b, _ := json.Marshal(r)
	for _, text := range []string{"git.exe", "make.exe", "cmake.exe", "ninja.exe", "cc.exe", "c++.exe", "out/server.exe", "out/main.o"} {
		if !strings.Contains(string(b), text) {
			t.Fatalf("missing %s: %s", text, b)
		}
	}
	if strings.Contains(string(b), "main.o.exe") {
		t.Fatal(string(b))
	}
}

func TestDistroInheritanceAndToolIsolation(t *testing.T) {
	data := []byte(`FROM ubuntu AS base
RUN apt install python3
CMD ["python3"]
FROM base AS child
RUN apt install git python3
CMD ["git", "--version"]
FROM scratch AS runtime
COPY --from=child /out/app.exe /app.exe
CMD ["/app.exe"]`)
	for _, target := range []string{"base", "child", "runtime"} {
		t.Run(target, func(t *testing.T) {
			opts := options(t)
			opts.Target = target
			result, err := Convert(data, opts, &fakeResolver{})
			if err != nil {
				t.Fatalf("%v %v", err, result.Diagnostics)
			}
			r := result.Recipe
			switch target {
			case "base":
				if len(r.From) != 1 || len(r.Builds) != 0 {
					t.Fatalf("%+v", r)
				}
			case "child":
				if len(r.From) != 2 || len(r.Builds) != 1 || len(r.Builds[0].From) != 1 || r.Builds[0].From["git"] != "" {
					t.Fatalf("%+v", r)
				}
			case "runtime":
				if len(r.From) != 0 || len(r.Builds) != 2 || len(r.Builds[1].From) != 2 {
					t.Fatalf("%+v", r)
				}
			}
		})
	}
}

func TestDistroCompilerStageCopy(t *testing.T) {
	r := convertOK(t, `FROM debian AS build
RUN apt install gcc
COPY main.c /src/main.c
RUN gcc /src/main.c -o /out/server
FROM scratch
COPY --from=build /out/server /app/server
CMD ["/app/server"]`)
	if len(r.From) != 0 || r.Builds[0].From["compiler"] == "" || r.Entrypoint[0] != "${app}/app/server.exe" {
		t.Fatalf("%+v", r)
	}
	if !reflect.DeepEqual(r.Executables, []string{"app/server"}) {
		t.Fatal(r.Executables)
	}
}

func TestAptAndCompilerRejections(t *testing.T) {
	for _, tc := range []struct{ line, want string }{
		{"RUN apt install curl", "package \"curl\""},
		{"RUN apt install python3=3.12", "version constraints"},
		{"RUN apt install ./app.deb", ".deb files"},
		{"RUN apt install -t stable python3", "unsupported apt option"},
		{"RUN apt upgrade", "operation \"upgrade\""},
		{"RUN apt remove python3", "operation \"remove\""},
		{"RUN apt install", "at least one"},
		{"RUN apt update python3", "does not accept operands"},
		{"RUN apt install $UNKNOWN", "must resolve"},
		{"RUN apt install python*", "must resolve"},
		{"RUN apt install python3 || true", "|| or pipelines"},
		{"RUN true || apt install python3", "|| or pipelines"},
		{"RUN apt install python3 | cat", "|| or pipelines"},
		{"RUN false && apt install python3; python3 main.py", "gated by other commands"},
		{"RUN 'apt' install python3 > log", "apt redirection"},
		{"RUN python3 main.py", "not installed"},
		{"RUN apt install python3 && git --version", "not installed"},
		{"RUN apt install gcc && gcc main.c", "explicit -o"},
		{"RUN apt install gcc && gcc -shared main.c -o lib.so", "not shared"},
		{"RUN apt install gcc && gcc --target=x86_64-linux-gnu main.c -o app", "target and sysroot"},
		{"RUN apt install gcc && gcc --sysroot=/usr main.c -o app", "target and sysroot"},
		{"RUN apt install gcc && gcc -MT opaque-target main.c -o app", "operand translation"},
		{"RUN apt install git && git clone https://example.test/repo /src", "absolute Linux path"},
	} {
		t.Run(tc.line, func(t *testing.T) {
			resolver := &fakeResolver{}
			result, err := Convert([]byte("FROM ubuntu\n"+tc.line+"\nCMD [\"/app.exe\"]"), options(t), resolver)
			if err == nil || result.Recipe != nil || len(resolver.calls) != 0 || !strings.Contains(fmt.Sprint(result.Diagnostics), tc.want) || !strings.Contains(fmt.Sprint(result.Diagnostics), "Dockerfile:2: RUN") {
				t.Fatalf("expected %q before resolution: %v %v %v", tc.want, err, result.Diagnostics, resolver.calls)
			}
		})
	}
	for _, text := range []string{"FROM ubuntu", "FROM ubuntu:", "FROM debian:bad/tag", "FROM ubuntu\nRUN apt install python3", "FROM node:24-alpine\nRUN apt install git", "FROM scratch\nRUN apt install git", "FROM ubuntu\nCMD [\"apt\",\"install\",\"git\"]"} {
		result, err := Convert([]byte(text), options(t), &fakeResolver{})
		if err == nil || result.Recipe != nil {
			t.Fatalf("unexpected success: %s", text)
		}
	}
}

func TestUnusedToolOverridesAndLanguageBaseOverride(t *testing.T) {
	opts := options(t)
	opts.Tools = map[string]string{"git": "unavailable:1", "node": "node:24"}
	opts.StageTools = map[string]map[string]string{"unused": {"compiler": "unavailable:1"}}
	resolver := &fakeResolver{}
	result, err := Convert([]byte("FROM fedora AS unused\nRUN unsupported\nFROM node:custom AS app\nCMD [\"node\"]"), opts, resolver)
	if err != nil || !reflect.DeepEqual(resolver.calls, []string{"node::node:24", "busybox::"}) || !strings.Contains(fmt.Sprint(result.Diagnostics), "unused override for tool git skipped") || !strings.Contains(fmt.Sprint(result.Diagnostics), "unused stage and its native tool overrides skipped") {
		t.Fatalf("%v %v %v", err, result.Diagnostics, resolver.calls)
	}
}

func TestToolOverrides(t *testing.T) {
	opts := options(t)
	opts.Tools = map[string]string{"python": "python-sdk:3.14", "git": "mingit:2"}
	opts.StageTools = map[string]map[string]string{"BUILD": {"python": "python-sdk:3.13"}}
	resolver := &fakeResolver{}
	result, err := Convert([]byte(`FROM ubuntu AS build
RUN apt install python3
CMD ["python3"]
FROM build AS child
RUN apt install git
CMD ["git", "--version"]`), opts, resolver)
	if err != nil || !reflect.DeepEqual(resolver.calls, []string{"python-sdk::python-sdk:3.13", "busybox::", "mingit::mingit:2"}) {
		t.Fatalf("%v %v %v", err, result.Diagnostics, resolver.calls)
	}
	if len(result.Recipe.From) != 2 || result.Recipe.Env["PYTHONPATH"] != "${app}/.python" {
		t.Fatal(result.Recipe)
	}
}

func TestToolOverrideErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tools  map[string]string
		stages map[string]map[string]string
		base   string
	}{
		{"unknown tool", map[string]string{"curl": "curl"}, nil, ""},
		{"empty image", map[string]string{"python": ""}, nil, ""},
		{"unknown stage", nil, map[string]map[string]string{"missing": {"python": "python-sdk"}}, ""},
		{"duplicate aliases", nil, map[string]map[string]string{"base": {"python": "python-sdk"}, "0": {"python": "python-sdk"}}, ""},
		{"inherited tool", nil, map[string]map[string]string{"child": {"python": "python-sdk"}}, ""},
		{"absent tool", nil, map[string]map[string]string{"base": {"git": "mingit"}}, ""},
		{"legacy conflict", map[string]string{"node": "node:24"}, nil, "node:22"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := options(t)
			opts.Tools, opts.StageTools, opts.Base = tc.tools, tc.stages, tc.base
			text := "FROM ubuntu AS base\nRUN apt install python3\nCMD [\"python3\"]\nFROM base AS child"
			if tc.base != "" {
				text = "FROM node\nCMD [\"node\"]"
			}
			resolver := &fakeResolver{}
			result, err := Convert([]byte(text), opts, resolver)
			if err == nil || result.Recipe != nil || len(resolver.calls) != 0 {
				t.Fatalf("%v %v %v", err, result.Diagnostics, resolver.calls)
			}
		})
	}
}
