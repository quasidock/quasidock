<#
.SYNOPSIS
Installs quasidock for the current user, upgrades it, or removes it.

.DESCRIPTION
Downloads a release of quasidock from GitHub, checks it against the release's
SHA256SUMS, puts quasidock.exe, quasidock-relay.exe, and quasidock-host.exe
in %LOCALAPPDATA%\quasidock\bin, and adds that directory to the user's PATH.
No administrator is needed. Run it again to upgrade: programs that are running
are moved aside, and installed apps' state is checked by the new quasidock
(quasidock repair).

    irm https://github.com/quasidock/quasidock/releases/latest/download/install.ps1 | iex

With parameters:

    & ([scriptblock]::Create((irm https://github.com/quasidock/quasidock/releases/latest/download/install.ps1))) -Version 0.2.0

.PARAMETER Version
The release to install, such as 0.2.0. The latest by default. Also
$env:QUASIDOCK_VERSION.

.PARAMETER Dir
Where to put quasidock. %LOCALAPPDATA%\quasidock\bin by default. Also
$env:QUASIDOCK_INSTALL_DIR.

.PARAMETER From
A directory or URL that has quasidock-x86_64-windows.zip and SHA256SUMS, instead
of a release on GitHub. Also $env:QUASIDOCK_INSTALL_FROM.

.PARAMETER NoPath
Leave the user's PATH as it is. Also $env:QUASIDOCK_INSTALL_NO_PATH.

.PARAMETER Uninstall
Remove quasidock and its PATH entry. Apps have to be removed first, with
quasidock rm; the store's other files, such as downloads and apps' data, stay.
#>
param(
    [string]$Version = $env:QUASIDOCK_VERSION,
    [string]$Dir = $env:QUASIDOCK_INSTALL_DIR,
    [string]$From = $env:QUASIDOCK_INSTALL_FROM,
    [switch]$NoPath,
    [switch]$Uninstall
)

# In a scope of its own: piped to iex, the script runs in the caller's session,
# whose preferences are theirs.
& {
    param([string]$Version, [string]$Dir, [string]$From, [bool]$NoPath, [bool]$Uninstall)
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue'

    $programs = 'quasidock.exe', 'quasidock-relay.exe', 'quasidock-host.exe'
    $archive = 'quasidock-x86_64-windows.zip'
    # The user's environment variables. Tests name another key.
    $envKey = if ($env:QUASIDOCK_INSTALL_ENV_KEY) { $env:QUASIDOCK_INSTALL_ENV_KEY } else { 'Environment' }

    $defaultDir = Join-Path $env:LOCALAPPDATA 'quasidock\bin'
    if (-not $Dir) { $Dir = $defaultDir }
    $Dir = [IO.Path]::GetFullPath($Dir).TrimEnd('\')
    $store = if ($env:QUASIDOCK_HOME) { $env:QUASIDOCK_HOME } else { Join-Path $env:LOCALAPPDATA 'quasidock' }

    function Same-Dir([string]$entry) {
        $expanded = [Environment]::ExpandEnvironmentVariables($entry.Trim().Trim('"')).TrimEnd('\')
        return $expanded -ieq $Dir
    }

    # Adds $Dir to the user's PATH, or removes it. The value is read and
    # written as it is stored: it usually refers to other variables
    # (%USERPROFILE%), which [Environment]::SetEnvironmentVariable would
    # replace by their values for good.
    function Set-UserPath([bool]$present) {
        $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey($envKey)
        try {
            $old = [string]$key.GetValue('Path', '', 'DoNotExpandEnvironmentNames')
            $kind = if ($key.GetValueNames() -contains 'Path') { $key.GetValueKind('Path') } else { [Microsoft.Win32.RegistryValueKind]::ExpandString }
            $entries = @($old -split ';' | Where-Object { $_ -ne '' })
            $others = @($entries | Where-Object { -not (Same-Dir $_) })
            if ($present -and $others.Count -ne $entries.Count) { return $false }
            if (-not $present -and $others.Count -eq $entries.Count) { return $false }
            if ($present) { $others += $Dir }
            $key.SetValue('Path', ($others -join ';'), $kind)
        } finally { $key.Close() }
        if ($envKey -eq 'Environment') {
            # Tells running programs, Explorer first, that the environment
            # changed: setting any user variable through .NET broadcasts it.
            [Environment]::SetEnvironmentVariable('QUASIDOCK_INSTALL_NOTIFY', $null, 'User')
        }
        return $true
    }

    # Deletes a program, or moves it aside if it's running: Windows lets a
    # running program be renamed, not deleted. A later install deletes it.
    function Clear-Program([string]$path) {
        if (-not (Test-Path -LiteralPath $path)) { return }
        try { Remove-Item -LiteralPath $path -Force }
        catch { Rename-Item -LiteralPath $path -NewName ((Split-Path -Leaf $path) + '.old-' + [Guid]::NewGuid().ToString('N').Substring(0, 8)) }
    }

    function Remove-Leftovers {
        foreach ($name in $programs) {
            foreach ($filter in "$name.old-*", "$name.new-*") {
                Get-ChildItem -LiteralPath $Dir -Filter $filter -ErrorAction SilentlyContinue |
                    ForEach-Object { Remove-Item -LiteralPath $_.FullName -Force -ErrorAction SilentlyContinue }
            }
        }
    }

    if ($Uninstall) {
        $quasidock = Join-Path $Dir 'quasidock.exe'
        if (Test-Path -LiteralPath $quasidock) {
            $apps = @(& $quasidock images -q | Where-Object { $_ })
            if ($LASTEXITCODE -ne 0) { throw 'Could not read the image store; repair it before uninstalling.' }
            if ($apps.Count -gt 0) {
                throw "These images are still cached: $($apps -join ', '). Remove each with 'quasidock rmi <id>' first: quasidock undoes what it changed for them, which deleting its files wouldn't."
            }
            # What stores since deleted left; later, no quasidock is here to.
            & $quasidock repair | Out-Null
        }
        foreach ($name in $programs) {
            $path = Join-Path $Dir $name
            if (Test-Path -LiteralPath $path) { Remove-Item -LiteralPath $path -Force }
        }
        Remove-Leftovers
        if (-not $NoPath -and (Set-UserPath $false)) { Write-Host "Removed $Dir from your PATH." }
        # The record of what stores changed outside themselves: it goes once it
        # has nothing in it, and until then says what another store still needs.
        $record = 'HKCU:\' + $(if ($env:QUASIDOCK_GRANTS_KEY) { $env:QUASIDOCK_GRANTS_KEY } else { 'Software\Quasidock\Grants' })
        if (Test-Path -LiteralPath $record) {
            $recordKey = Get-Item -LiteralPath $record
            $left = @($recordKey.Property) + @($recordKey.GetSubKeyNames())
            if ($left.Count -eq 0 -and $recordKey.SubKeyCount -eq 0) { Remove-Item -LiteralPath $record -Force }
            else { Write-Warning "quasidock's record still has what other stores changed: $($left -join '; '). A quasidock using them takes it back." }
        }
        Write-Host "quasidock is removed from $Dir. Its store, with downloads and apps' data, is $store; delete that too if you don't want them."
        return
    }

    if (-not [Environment]::Is64BitOperatingSystem) { throw 'quasidock needs 64-bit Windows.' }
    if (-not $From) {
        $From = if ($Version) { "https://github.com/quasidock/quasidock/releases/download/v$($Version.TrimStart('v'))" }
        else { 'https://github.com/quasidock/quasidock/releases/latest/download' }
    }

    $tmp = Join-Path ([IO.Path]::GetTempPath()) ('quasidock-install-' + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        foreach ($name in $archive, 'SHA256SUMS') {
            $dest = Join-Path $tmp $name
            if ($From -match '^https?://') {
                # Windows PowerShell 5.1 doesn't offer TLS 1.2 by itself everywhere.
                [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
                Invoke-WebRequest -UseBasicParsing -Uri "$($From.TrimEnd('/'))/$name" -OutFile $dest
            } else {
                Copy-Item -LiteralPath (Join-Path $From $name) -Destination $dest
            }
        }

        # Before anything is changed.
        $want = Get-Content -LiteralPath (Join-Path $tmp 'SHA256SUMS') |
            Where-Object { $_ -match "^([0-9a-fA-F]{64})\s+\*?$([regex]::Escape($archive))\s*$" } |
            ForEach-Object { $Matches[1].ToLower() } | Select-Object -First 1
        if (-not $want) { throw "SHA256SUMS from $From doesn't list $archive." }
        $got = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $tmp $archive)).Hash.ToLower()
        if ($got -ne $want) { throw "$archive from $From isn't the file its SHA256SUMS lists: expected $want, got $got. Nothing was installed." }

        $unpacked = Join-Path $tmp 'unpacked'
        Expand-Archive -LiteralPath (Join-Path $tmp $archive) -DestinationPath $unpacked
        foreach ($name in $programs) {
            if (-not (Test-Path -LiteralPath (Join-Path $unpacked $name))) { throw "$archive has no $name. Nothing was installed." }
        }

        New-Item -ItemType Directory -Path $Dir -Force | Out-Null
        Remove-Leftovers
        foreach ($name in $programs) {
            Clear-Program (Join-Path $Dir $name)
            Copy-Item -LiteralPath (Join-Path $unpacked $name) -Destination (Join-Path $Dir $name)
        }
    } finally {
        Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }

    $quasidock = Join-Path $Dir 'quasidock.exe'
    $installed = & $quasidock version
    Write-Host "Installed $installed in $Dir."

    if ((Test-Path (Join-Path $store 'images\index.json')) -or (Test-Path (Join-Path $store 'refs\*.json'))) {
        & $quasidock repair
        if ($LASTEXITCODE -ne 0) { Write-Warning "Some apps' grants remain from the earlier quasidock. Close the apps and run 'quasidock repair'." }
    }

    if (-not $NoPath) {
        if (Set-UserPath $true) { Write-Host "Added $Dir to your PATH. Terminals opened from now on have it." }
        if (-not (@($env:Path -split ';') | Where-Object { $_ -ne '' -and (Same-Dir $_) })) { $env:Path = "$($env:Path.TrimEnd(';'));$Dir" }
    }
    Write-Host "Try: quasidock pull node"
} $Version $Dir $From ($NoPath -or [bool]$env:QUASIDOCK_INSTALL_NO_PATH) ([bool]$Uninstall)
