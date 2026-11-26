# Run from a regular PowerShell session as a Windows user who can consent to UAC.
param([string]$BuildRoot = 'out/hcn-smoke')

$ErrorActionPreference = 'Stop'
if ($env:QUASIDOCK_TEST_HCN -ne '1') {
    throw 'Set QUASIDOCK_TEST_HCN=1 to run the live HCN tests.'
}
$admin = [Security.Principal.WindowsPrincipal]::new([Security.Principal.WindowsIdentity]::GetCurrent())
if ($admin.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run this test from a regular PowerShell session so the payloads stay unelevated.'
}

$repo = Split-Path $PSScriptRoot
Push-Location $repo
try {
    go run ./scripts/build.go --prefix $BuildRoot
    if ($LASTEXITCODE -ne 0) { throw 'Quasidock build failed.' }
    go run ./scripts/build.go --prefix $BuildRoot cli-fixture
    if ($LASTEXITCODE -ne 0) { throw 'CLI fixture build failed.' }
    $binary = (Resolve-Path (Join-Path $BuildRoot 'bin/quasidock.exe')).Path
    $fixture = (Resolve-Path (Join-Path $BuildRoot 'test/quasidock-cli-fixture.exe')).Path
} finally {
    Pop-Location
}

$suffix = [Guid]::NewGuid().ToString('N').Substring(0, 12)
$root = Join-Path ([IO.Path]::GetTempPath()) "quasidock-hcn-$suffix"
$store = Join-Path $root 'store'
$networkFile = Join-Path $root 'networks.txt'
$adminLog = Join-Path $root 'admin.log'
$networkName = "quasidock-hcn-$suffix"
$envBefore = $env:QUASIDOCK_HOME
New-Item -ItemType Directory -Path $root | Out-Null

function Invoke-Quasidock([string[]]$Arguments) {
    $result = & $binary @Arguments
    if ($LASTEXITCODE -ne 0) { throw "quasidock $($Arguments -join ' ') exited $LASTEXITCODE" }
    return $result
}

function Quote-Literal([string]$Value) {
    return "'" + $Value.Replace("'", "''") + "'"
}

function Invoke-Admin([string]$Script) {
    $header = "`$ErrorActionPreference = 'Stop'; `$env:QUASIDOCK_HOME = $(Quote-Literal $store); "
    $cmd = $header + '& {' + $Script + '} *> ' + (Quote-Literal $adminLog) + '; exit $LASTEXITCODE'
    $encoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($cmd))
    $powershell = Join-Path $env:windir 'System32/WindowsPowerShell/v1.0/powershell.exe'
    $child = Start-Process -FilePath $powershell -Verb RunAs -WindowStyle Hidden -Wait -PassThru -ArgumentList '-NoProfile', '-NonInteractive', '-EncodedCommand', $encoded
    if ($child.ExitCode -ne 0) { throw "Elevated HCN operation failed.`n$(Get-Content -LiteralPath $adminLog -Raw)" }
}

function Read-Container([string]$ContainerName) {
    return ((Invoke-Quasidock @('container', 'inspect', $ContainerName)) -join "`n" | ConvertFrom-Json)
}

function Wait-Peer([string]$Endpoint) {
    $limit = [DateTime]::UtcNow.AddSeconds(15)
    do {
        $response = & $binary exec peer quasidock-cli-fixture.exe tcp-ping $Endpoint 2>$null
        if ($LASTEXITCODE -eq 0 -and $response -eq 'echo') { return }
        Start-Sleep -Milliseconds 200
    } while ([DateTime]::UtcNow -lt $limit)
    throw "Timed out reaching $Endpoint from the peer."
}

$env:QUASIDOCK_HOME = $store
try {
    Copy-Item -LiteralPath $fixture -Destination $root
    Set-Content -LiteralPath (Join-Path $root 'broken.exe') -Value 'invalid executable' -Encoding ASCII
    foreach ($role in @('server', 'peer')) {
        $image = "quasidock.hcn.$role.$suffix"
        $recipe = @{
            id = $image; version = '1'; entrypoint = @('quasidock-cli-fixture.exe')
            stages = @(@{name = 'fixture'; sources = @(@{path = 'quasidock-cli-fixture.exe'}, @{path = 'broken.exe'})})
        } | ConvertTo-Json -Depth 6
        $path = Join-Path $root "$role.json"
        Set-Content -LiteralPath $path -Value $recipe -Encoding ASCII
        Invoke-Quasidock @('build', $path) | Out-Null
        $port = if ($role -eq 'server') { '42000' } else { '42001' }
        $argsForCreate = @('create', '--sandbox=appcontainer', '--share=network', '--restart=always', '--name', $role, '--entrypoint=quasidock-cli-fixture.exe')
        if ($role -eq 'server') { $argsForCreate += @('-p', '127.0.0.1::42000') }
        $argsForCreate += @($image, 'tcp-serve', "0.0.0.0:$port")
        Invoke-Quasidock $argsForCreate | Out-Null
    }
    Invoke-Quasidock @('create', '--sandbox=appcontainer', '--share=network', '--name', 'broken', '--entrypoint=broken.exe', "quasidock.hcn.peer.$suffix") | Out-Null
    Invoke-Admin @"
`$binary = $(Quote-Literal $binary)
`$file = $(Quote-Literal $networkFile)
foreach (`$name in @($(Quote-Literal $networkName), $(Quote-Literal "$networkName-other"))) {
    `$id = & `$binary network create `$name
    if (`$LASTEXITCODE -ne 0) { exit `$LASTEXITCODE }
    Add-Content -LiteralPath `$file -Value `$id
}
foreach (`$container in @('server', 'peer', 'broken')) {
    & `$binary network connect $(Quote-Literal $networkName) `$container
    if (`$LASTEXITCODE -ne 0) { exit `$LASTEXITCODE }
}
"@
    $network, $other = Get-Content -LiteralPath $networkFile
    $saved = Read-Container 'server'
    if ($saved.HostConfig.Networks.$network -ne $networkName -or $saved.NetworkSettings.Networks) { throw 'Unexpected stopped membership.' }

    Invoke-Quasidock @('start', 'server') | Out-Null
    Invoke-Quasidock @('start', 'peer') | Out-Null
    $server = Read-Container 'server'
    $client = Read-Container 'peer'
    $address = $server.NetworkSettings.Networks.$network.IPAddress
    $endpoint = "${address}:42000"
    Wait-Peer $endpoint
    $published = Invoke-Quasidock @('port', 'server', '42000/tcp')
    if ((& $fixture tcp-ping $published) -ne 'echo' -or $LASTEXITCODE -ne 0) { throw 'The published host port did not reach the server.' }

    Invoke-Quasidock @('network', 'connect', $other, 'peer') | Out-Null
    Invoke-Quasidock @('network', 'disconnect', $other, 'peer') | Out-Null
    Invoke-Quasidock @('pause', 'peer') | Out-Null
    Invoke-Quasidock @('network', 'disconnect', ('{' + $network.ToUpper() + '}'), 'peer') | Out-Null
    Invoke-Quasidock @('unpause', 'peer') | Out-Null
    & $binary exec peer quasidock-cli-fixture.exe tcp-ping $endpoint 2>$null | Out-Null
    if ($LASTEXITCODE -eq 0) { throw 'The disconnected peer can still reach the server.' }
    $empty = Read-Container 'peer'
    if ($empty.HostConfig.Networks -or $empty.NetworkSettings.Networks) { throw 'Disconnected membership is still present.' }
    Invoke-Quasidock @('network', 'connect', $networkName.ToUpper(), 'peer') | Out-Null
    Wait-Peer $endpoint
    if ((Read-Container 'peer').State.Pid -ne $client.State.Pid) { throw 'A live change replaced the peer process.' }

    $peerPID = (Read-Container 'peer').State.Pid
    Stop-Process -Id $peerPID
    $limit = [DateTime]::UtcNow.AddSeconds(20)
    do {
        Start-Sleep -Milliseconds 100
        $restarted = Read-Container 'peer'
    } until (($restarted.State.Running -and $restarted.State.Pid -ne $peerPID) -or [DateTime]::UtcNow -ge $limit)
    if ($restarted.State.Pid -eq $peerPID -or !$restarted.NetworkSettings.Networks.$network.IPAddress) {
        throw 'The automatic restart did not rebuild the endpoint.'
    }
    Wait-Peer $endpoint

    Invoke-Quasidock @('stop', 'peer') | Out-Null
    $stopped = Read-Container 'peer'
    if ($stopped.HostConfig.Networks.$network -ne $networkName -or $stopped.NetworkSettings.Networks) { throw 'Unexpected membership after stop.' }
    & $binary start broken 2>$null | Out-Null
    if ($LASTEXITCODE -eq 0) { throw 'The invalid executable started.' }
    $failed = Read-Container 'broken'
    if ($failed.State.Running -or $failed.NetworkSettings.Networks -or $failed.HostConfig.Networks.$network -ne $networkName) {
        throw 'Failed startup did not preserve only the saved membership.'
    }
    Write-Output 'HCN connect, disconnect, reconnect, and restart passed.'
} catch {
    Write-Warning "HCN smoke test failed at $($_.InvocationInfo.ScriptLineNumber): $($_.Exception.Message)"
    throw
} finally {
    foreach ($role in @('broken', 'peer', 'server')) {
        & $binary rm -f $role 2>$null | Out-Null
    }
    $env:QUASIDOCK_HOME = $envBefore
    if (Test-Path -LiteralPath $networkFile) {
        Invoke-Admin @"
foreach (`$networkID in Get-Content -LiteralPath $(Quote-Literal $networkFile)) {
    & "`$env:windir\System32\hnsdiag.exe" delete networks ([Guid]::Parse(`$networkID).ToString())
    if (`$LASTEXITCODE -ne 0) { exit `$LASTEXITCODE }
}
"@
    }
    $absoluteRoot = (Resolve-Path -LiteralPath $root).Path
    $expectedRoot = [IO.Path]::GetFullPath((Join-Path ([IO.Path]::GetTempPath()) "quasidock-hcn-$suffix"))
    if ($absoluteRoot -eq $expectedRoot) {
        & "$env:windir\System32\icacls.exe" $absoluteRoot /reset /T /C /Q | Out-Null
        Remove-Item -LiteralPath $absoluteRoot -Recurse -Force -ErrorAction Continue
    }
}
