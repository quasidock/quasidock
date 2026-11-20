param(
    [string]$Quasidock = (Join-Path $PSScriptRoot '..\out\bin\quasidock.exe'),
    [string]$SeedDownloads = $env:SEED_DOWNLOADS,
    [ValidateSet('node', 'go', 'python')][string[]]$Language = @('node', 'go', 'python'),
    [switch]$NativeTools,
    [string]$WorkParent = [IO.Path]::GetTempPath()
)

$ErrorActionPreference = 'Stop'
$conversionRepo = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
# Normalize a trailing separator while preserving drive roots for the cleanup check.
$conversionParent = [IO.Path]::GetFullPath((Join-Path $WorkParent '.'))
$conversionWork = Join-Path $conversionParent ('quasidock-dockerfile-' + [guid]::NewGuid().ToString('N'))
$conversionPreviousHome = $env:QUASIDOCK_HOME
$conversionStore = Join-Path $conversionWork 'store'
New-Item -ItemType Directory -Path (Join-Path $conversionStore 'cache\downloads') -Force | Out-Null
$env:QUASIDOCK_HOME = $conversionStore

function Write-ConversionFile([string]$Name, [string]$Content) {
    [IO.File]::WriteAllText($Name, $Content, [Text.UTF8Encoding]::new($false))
}
function Invoke-ConversionCLI([string[]]$CommandArgs) {
    $conversionOldPreference = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    $conversionOutput = & $Quasidock @CommandArgs 2>&1
    $conversionExit = $LASTEXITCODE
    $ErrorActionPreference = $conversionOldPreference
    $conversionText = (($conversionOutput | ForEach-Object { $_.ToString() }) -join [Environment]::NewLine).Trim()
    if ($conversionExit -ne 0) { throw "quasidock $($CommandArgs -join ' ') failed ($conversionExit): $conversionText" }
    return $conversionText
}
function Assert-ConversionFailure([string[]]$CommandArgs, [int]$Code, [string]$Message) {
    $conversionOldPreference = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    $conversionOutput = & $Quasidock @CommandArgs 2>&1
    $conversionExit = $LASTEXITCODE
    $ErrorActionPreference = $conversionOldPreference
    $conversionText = (($conversionOutput | ForEach-Object { $_.ToString() }) -join [Environment]::NewLine)
    if ($conversionExit -ne $Code -or ($Message -and !$conversionText.Contains($Message))) {
        throw "Expected exit $Code containing '$Message', got ${conversionExit}: $conversionText"
    }
}

try {
    foreach ($conversionRecipe in @('busybox') + @($Language | ForEach-Object { if ($_ -eq 'python') { 'python-sdk' } else { $_ } })) {
        $conversionRecipeFile = Join-Path $conversionRepo "recipes\$conversionRecipe.json"
        $conversionRecipeData = Get-Content -Raw -LiteralPath $conversionRecipeFile | ConvertFrom-Json
        if ($SeedDownloads) {
            foreach ($conversionSource in $conversionRecipeData.stages.sources) {
                if ($conversionSource.sha256) {
                    $conversionSeed = Join-Path $SeedDownloads $conversionSource.sha256
                    if (Test-Path -LiteralPath $conversionSeed) {
                        Copy-Item -LiteralPath $conversionSeed -Destination (Join-Path $conversionStore "cache\downloads\$($conversionSource.sha256)")
                    }
                }
            }
        }
        Invoke-ConversionCLI @('build', $conversionRecipeFile) | Write-Host
    }

    if ($NativeTools) {
        foreach ($conversionTool in @('mingit', 'llvm-mingw', 'make', 'cmake')) {
            Invoke-ConversionCLI @('pull', $conversionTool) | Write-Host
        }
    }

    foreach ($conversionLanguage in $Language) {
        $conversionProject = Join-Path $conversionWork $conversionLanguage
        New-Item -ItemType Directory -Path $conversionProject | Out-Null
        switch ($conversionLanguage) {
            'node' {
                $conversionVersion = (Get-Content -Raw (Join-Path $conversionRepo 'recipes\node.json') | ConvertFrom-Json).version
                Write-ConversionFile (Join-Path $conversionProject 'Dockerfile') @"
FROM node:$conversionVersion-alpine
WORKDIR /app
COPY package*.json ./
RUN npm ci
COPY . .
RUN npm run build
RUN printf '%s\n' beta alpha alpha | sort | uniq | tr a-z A-Z > /app/pipeline.txt; false || echo append >> /app/pipeline.txt
RUN node -e "process.exit(0)" 2>/app/errors.txt 2>&1
RUN printf '%s' bracket > '/app/a[1].txt' && cat '/app/a['?'].txt' > /app/glob.txt
RUN false | true
CMD ["node", "server.js"]
"@
                Write-ConversionFile (Join-Path $conversionProject 'package.json') '{"name":"conversion-smoke","version":"1.0.0","private":true,"scripts":{"build":"node -e \"require(''fs'').writeFileSync(''built.txt'',''built'')\""}}'
                Write-ConversionFile (Join-Path $conversionProject 'package-lock.json') '{"name":"conversion-smoke","version":"1.0.0","lockfileVersion":3,"packages":{"":{"name":"conversion-smoke","version":"1.0.0"}}}'
                Write-ConversionFile (Join-Path $conversionProject 'server.js') "const fs=require('fs'); if(fs.readFileSync('asset.txt','utf8')!=='live'||fs.readFileSync('built.txt','utf8')!=='built'||fs.readFileSync('pipeline.txt','utf8').replace(/\r/g,'')!=='ALPHA\nBETA\nappend\n'||fs.readFileSync('glob.txt','utf8')!=='bracket'||fs.existsSync('secret.txt'))process.exit(2); console.log('node-ok');"
                Write-ConversionFile (Join-Path $conversionProject 'asset.txt') 'live'
                Write-ConversionFile (Join-Path $conversionProject '.gitignore') 'asset.txt'
                Write-ConversionFile (Join-Path $conversionProject '.dockerignore') 'secret.txt'
                Write-ConversionFile (Join-Path $conversionProject 'secret.txt') 'excluded'
            }
            'go' {
                $conversionVersion = (Get-Content -Raw (Join-Path $conversionRepo 'recipes\go.json') | ConvertFrom-Json).version
                Write-ConversionFile (Join-Path $conversionProject 'Dockerfile') @"
FROM golang:$conversionVersion
WORKDIR /src
COPY . .
RUN go build -o /app/server .
RUN /app/server --build-check > /app/build.txt
RUN cp /app/server /app/copied && mv /app/copied /app/final && rm /app/server
COPY message.txt /app/message.txt
WORKDIR /app
CMD ["./final"]
"@
                Write-ConversionFile (Join-Path $conversionProject 'go.mod') "module example.test/conversion`n`ngo 1.26`n"
                Write-ConversionFile (Join-Path $conversionProject 'main.go') 'package main; import("fmt";"os");func main(){if len(os.Args)>1&&os.Args[1]=="--build-check"{fmt.Println("build-ok");return};b,e:=os.ReadFile("message.txt");if e!=nil||string(b)!="live"{os.Exit(2)};fmt.Println("go-ok")}'
                Write-ConversionFile (Join-Path $conversionProject 'message.txt') 'live'
            }
            'python' {
                $conversionVersion = (Get-Content -Raw (Join-Path $conversionRepo 'recipes\python-sdk.json') | ConvertFrom-Json).version
                Write-ConversionFile (Join-Path $conversionProject 'Dockerfile') @"
FROM python:$conversionVersion-slim
WORKDIR /app
COPY requirements.txt ./
COPY *.whl ./
RUN python -m pip install --no-index -r requirements.txt
RUN conversion-command build 'one two' > /app/console.txt
RUN python -c "from pathlib import Path; assert Path('console.txt').read_text().strip() == 'build-ok'"
RUN ["python", "-c", "import sys; assert sys.argv[1:] == ['\u0024VALUE', 'a*b', 'a; false', '/literal']", "`$VALUE", "a*b", "a; false", "/literal"]
COPY main.py ./
CMD ["conversion-command"]
"@
                Write-ConversionFile (Join-Path $conversionProject 'requirements.txt') './conversion_dependency-1.0-py3-none-any.whl'
                Write-ConversionFile (Join-Path $conversionProject 'main.py') "import conversion_dependency; print(conversion_dependency.message)"
                $conversionWheelRoot = Join-Path $conversionWork 'wheel'
                $conversionDistInfo = Join-Path $conversionWheelRoot 'conversion_dependency-1.0.dist-info'
                New-Item -ItemType Directory -Path $conversionDistInfo -Force | Out-Null
                Write-ConversionFile (Join-Path $conversionWheelRoot 'conversion_dependency.py') @'
import sys
message = 'python-ok'
def main():
    if sys.argv[1:] == ['build', 'one two']:
        print('build-ok')
    elif sys.argv[1:] == ['args', 'one two', '$VALUE', 'a*b', '/literal']:
        print('args-ok')
    elif sys.argv[1:] == ['exit']:
        return 23
    elif sys.argv[1:]:
        raise AssertionError(sys.argv)
    else:
        print(message)
'@
                Write-ConversionFile (Join-Path $conversionDistInfo 'METADATA') "Metadata-Version: 2.1`nName: conversion-dependency`nVersion: 1.0`n"
                Write-ConversionFile (Join-Path $conversionDistInfo 'WHEEL') "Wheel-Version: 1.0`nGenerator: quasidock-smoke`nRoot-Is-Purelib: true`nTag: py3-none-any`n"
                Write-ConversionFile (Join-Path $conversionDistInfo 'RECORD') ''
                Write-ConversionFile (Join-Path $conversionDistInfo 'entry_points.txt') "[console_scripts]`nconversion-command = conversion_dependency:main`n"
                Add-Type -AssemblyName System.IO.Compression.FileSystem
                [IO.Compression.ZipFile]::CreateFromDirectory($conversionWheelRoot, (Join-Path $conversionProject 'conversion_dependency-1.0-py3-none-any.whl'))
            }
        }
        $conversionID = "converted-$conversionLanguage"
        Invoke-ConversionCLI @('convert', $conversionProject, '--id', $conversionID) | Write-Host
        Invoke-ConversionCLI @('build', $conversionProject) | Write-Host
        $conversionResult = Invoke-ConversionCLI @('run', '--rm', $conversionID)
        if ($conversionResult -ne "$conversionLanguage-ok") { throw "Unexpected $conversionLanguage output: $conversionResult" }
        Write-Host "PASS $conversionLanguage conversion, build and run"
        if ($conversionLanguage -eq 'python') {
            $conversionArguments = Invoke-ConversionCLI @('run', '--rm', $conversionID, $conversionID, 'args', 'one two', '$VALUE', 'a*b', '/literal')
            if ($conversionArguments -ne 'args-ok') { throw "Console arguments changed: $conversionArguments" }
            Assert-ConversionFailure @('run', '--rm', $conversionID, $conversionID, 'exit') 23 ''
            $conversionPythonDockerfile = Get-Content -Raw -LiteralPath (Join-Path $conversionProject 'Dockerfile')
            Write-ConversionFile (Join-Path $conversionProject 'Dockerfile') ($conversionPythonDockerfile + "`nCMD [""missing-command""]`n")
            Invoke-ConversionCLI @('convert', $conversionProject, '--id', 'converted-python-missing', '--force') | Out-Null
            Assert-ConversionFailure @('build', $conversionProject) 1 'expected one installed entry point, found 0'

            $conversionDuplicateRoot = Join-Path $conversionWork 'duplicate-wheel'
            $conversionDuplicateInfo = Join-Path $conversionDuplicateRoot 'duplicate_dependency-1.0.dist-info'
            New-Item -ItemType Directory -Path $conversionDuplicateInfo -Force | Out-Null
            Write-ConversionFile (Join-Path $conversionDuplicateInfo 'METADATA') "Metadata-Version: 2.1`nName: duplicate-dependency`nVersion: 1.0`n"
            Write-ConversionFile (Join-Path $conversionDuplicateInfo 'WHEEL') "Wheel-Version: 1.0`nRoot-Is-Purelib: true`nTag: py3-none-any`n"
            Write-ConversionFile (Join-Path $conversionDuplicateInfo 'RECORD') ''
            Write-ConversionFile (Join-Path $conversionDuplicateInfo 'entry_points.txt') "[console_scripts]`nconversion-command = conversion_dependency:main`n"
            [IO.Compression.ZipFile]::CreateFromDirectory($conversionDuplicateRoot, (Join-Path $conversionProject 'duplicate_dependency-1.0-py3-none-any.whl'))
            Write-ConversionFile (Join-Path $conversionProject 'Dockerfile') @"
FROM python:$conversionVersion
WORKDIR /app
COPY *.whl ./
RUN pip install --no-index ./*.whl
CMD ["conversion-command"]
"@
            Invoke-ConversionCLI @('convert', $conversionProject, '--id', 'converted-python-duplicate', '--force') | Out-Null
            Assert-ConversionFailure @('build', $conversionProject) 1 'expected one installed entry point, found 2'
            Write-Host 'PASS Python console arguments, exit codes, missing and duplicate metadata'
        }
    }

    if ($Language -contains 'python') {
        $conversionFlaskProject = Join-Path $conversionWork 'flask'
        New-Item -ItemType Directory -Path $conversionFlaskProject | Out-Null
        $conversionListener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
        $conversionListener.Start()
        $conversionPort = $conversionListener.LocalEndpoint.Port
        $conversionListener.Stop()
        Write-ConversionFile (Join-Path $conversionFlaskProject 'Dockerfile') @"
FROM ubuntu:24.04
EXPOSE 8501
RUN apt-get update && apt-get install -y python3 python3-pip
RUN pip install flask==3.0.*
COPY hello.py /
ENV FLASK_APP=hello
CMD ["flask", "run", "--host", "0.0.0.0", "--port", "$conversionPort"]
"@
        Write-ConversionFile (Join-Path $conversionFlaskProject 'hello.py') @'
from flask import Flask
app = Flask(__name__)
@app.get('/')
def index():
    return 'flask-ok'
'@
        Invoke-ConversionCLI @('convert', $conversionFlaskProject, '--id', 'converted-flask') | Write-Host
        Invoke-ConversionCLI @('build', $conversionFlaskProject) | Write-Host
        $conversionFlaskContainer = Invoke-ConversionCLI @('run', '-d', 'converted-flask')
        try {
            $conversionResponse = $null
            $conversionDeadline = [DateTime]::UtcNow.AddSeconds(20)
            do {
                try { $conversionResponse = Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:$conversionPort/" -TimeoutSec 1 } catch { Start-Sleep -Milliseconds 100 }
            } while (!$conversionResponse -and [DateTime]::UtcNow -lt $conversionDeadline)
            if (!$conversionResponse -or $conversionResponse.Content -ne 'flask-ok') {
                $conversionLogs = Invoke-ConversionCLI @('logs', $conversionFlaskContainer)
                throw "Flask did not serve the expected response: $conversionLogs"
            }
            Write-Host 'PASS Ubuntu apt mapping, Flask wildcard install, relocated console startup and HTTP response'
        } finally {
            Invoke-ConversionCLI @('rm', '--force', $conversionFlaskContainer) | Out-Null
        }
    }

    if ($NativeTools) {
        $conversionNativeProject = Join-Path $conversionWork 'native-tools'
        New-Item -ItemType Directory -Path $conversionNativeProject | Out-Null
        Write-ConversionFile (Join-Path $conversionNativeProject 'Dockerfile') @'
FROM debian:bookworm-slim
RUN apt-get update && apt-get install --no-install-recommends -y git build-essential cmake ninja-build && apt-get clean
RUN rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY . .
RUN git -C /src init && git -C /src status --short
RUN make -C /src -f /src/Makefile
RUN cmake -S /src -B /build && cmake --build /build
CMD ["/build/server.exe"]
'@
        Write-ConversionFile (Join-Path $conversionNativeProject 'main.c') '#include <stdio.h>
int main(void) { puts("native-tools-ok"); return 0; }
'
        Write-ConversionFile (Join-Path $conversionNativeProject 'Makefile') "all:`n`t`$(CC) main.c -o make-server.exe`n"
        Write-ConversionFile (Join-Path $conversionNativeProject 'CMakeLists.txt') "cmake_minimum_required(VERSION 3.20)`nproject(native_tools C)`nadd_executable(server main.c)`n"
        Invoke-ConversionCLI @('convert', $conversionNativeProject, '--id', 'converted-native-tools') | Write-Host
        Invoke-ConversionCLI @('build', $conversionNativeProject) | Write-Host
        $conversionNativeOutput = Invoke-ConversionCLI @('run', '--rm', 'converted-native-tools')
        if ($conversionNativeOutput -notmatch 'native-tools-ok') { throw "Native tools output: $conversionNativeOutput" }
        Write-Host 'PASS Debian Git, GNU Make, CMake/Ninja and native compiler composition'

        $conversionCompilerProject = Join-Path $conversionWork 'compiler-stages'
        New-Item -ItemType Directory -Path $conversionCompilerProject | Out-Null
        Write-ConversionFile (Join-Path $conversionCompilerProject 'Dockerfile') @'
FROM ubuntu:24.04 AS build
RUN apt install -y gcc
WORKDIR /src
COPY main.c .
RUN mkdir -p /out && gcc main.c -o /out/server
FROM scratch
COPY --from=build /out/server /app/server
CMD ["/app/server"]
'@
        Write-ConversionFile (Join-Path $conversionCompilerProject 'main.c') '#include <stdio.h>
int main(void) { puts("compiler-stages-ok"); return 0; }
'
        Invoke-ConversionCLI @('convert', $conversionCompilerProject, '--id', 'converted-compiler-stages') | Write-Host
        $conversionCompilerRecipe = Get-Content -Raw (Join-Path $conversionCompilerProject 'quasidock.json') | ConvertFrom-Json
        if ($conversionCompilerRecipe.from.compiler -or !$conversionCompilerRecipe.builds[0].from.compiler) { throw 'Compiler leaked out of its build unit' }
        Invoke-ConversionCLI @('build', $conversionCompilerProject) | Write-Host
        $conversionCompilerOutput = Invoke-ConversionCLI @('run', '--rm', 'converted-compiler-stages')
        if ($conversionCompilerOutput -notmatch 'compiler-stages-ok') { throw "Compiler stage output: $conversionCompilerOutput" }
        Write-Host 'PASS native compiler output mapping, multi-stage copy and scratch runtime'
    }
} catch {
    Write-Host $_.Exception.Message
    throw
} finally {
    $conversionResolved = [IO.Path]::GetFullPath($conversionWork)
    if ($conversionResolved -ne $conversionWork -or (Split-Path -Parent $conversionResolved) -ne $conversionParent -or (Split-Path -Leaf $conversionResolved) -notlike 'quasidock-dockerfile-*') {
        throw "Refusing to remove unexpected test directory $conversionResolved"
    }
    # Deployments deliberately deny writes. Let Quasidock release their ACLs
    # before deleting only this test's verified directory.
    try {
        Invoke-ConversionCLI @('container', 'prune') | Out-Null
        $conversionImages = Invoke-ConversionCLI @('images', '-q')
        foreach ($conversionImage in ($conversionImages -split '\r?\n')) {
            if ($conversionImage -match '^sha256:[0-9a-f]{64}$') { Invoke-ConversionCLI @('rmi', $conversionImage) | Out-Null }
        }
        $conversionPins = Join-Path $conversionResolved 'store\cache\images'
        if (Test-Path -LiteralPath $conversionPins) { Remove-Item -LiteralPath $conversionPins -Recurse -Force }
        Invoke-ConversionCLI @('prune') | Out-Null
    } catch { Write-Warning "Test-store cleanup: $($_.Exception.Message)" }
    $env:QUASIDOCK_HOME = $conversionPreviousHome
    Remove-Item -LiteralPath $conversionResolved -Recurse -Force -ErrorAction SilentlyContinue
    if (Test-Path -LiteralPath $conversionResolved) { Write-Warning "Test artifacts remain in $conversionResolved" }
}
