<#
.SYNOPSIS
    Install gta on Windows from a GitHub release of cmyolo441-coder/go-terminal-agent.

.DESCRIPTION
    Downloads the release zip for this machine (amd64 or arm64), verifies its
    SHA-256 against the release's checksums.txt, and installs gta.exe. Nothing
    is installed when the checksum does not match.

    irm https://raw.githubusercontent.com/cmyolo441-coder/go-terminal-agent/main/scripts/install.ps1 | iex
    & ([scriptblock]::Create((irm .../install.ps1))) -Version vX.Y.Z

.PARAMETER Version
    Release to install, as vX.Y.Z or X.Y.Z. Default: $env:GTA_VERSION, else the latest release.

.PARAMETER InstallDir
    Where to put gta.exe. Default: $env:GTA_INSTALL_DIR, else %LOCALAPPDATA%\Programs\gta.

.PARAMETER NoModifyPath
    Do not add the install directory to the user's PATH.
#>
[CmdletBinding()]
param(
    [string]$Version = $env:GTA_VERSION,
    [string]$InstallDir = $env:GTA_INSTALL_DIR,
    [switch]$NoModifyPath
)

Set-StrictMode -Version 3.0
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$Repo = 'cmyolo441-coder/go-terminal-agent'

function Fail([string]$Message) {
    Write-Host "gta-install: error: $Message" -ForegroundColor Red
    throw "gta-install: $Message"
}

function Say([string]$Message) {
    Write-Host "gta-install: $Message"
}

# Windows PowerShell 5.1 may default to TLS 1.0; GitHub needs 1.2 or newer.
if ($PSVersionTable.PSVersion.Major -lt 6) {
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
}

# ---- platform ----------------------------------------------------------------

$cpu = $env:PROCESSOR_ARCHITEW6432
if (-not $cpu) { $cpu = $env:PROCESSOR_ARCHITECTURE }
switch ($cpu) {
    'AMD64' { $arch = 'amd64' }
    'ARM64' { $arch = 'arm64' }
    default { Fail "unsupported CPU architecture: $cpu (release builds exist for amd64 and arm64)" }
}

if (-not $InstallDir) {
    if (-not $env:LOCALAPPDATA) { Fail 'LOCALAPPDATA is not set; pass -InstallDir' }
    $InstallDir = Join-Path $env:LOCALAPPDATA 'Programs\gta'
}

# ---- version -----------------------------------------------------------------

if (-not $Version -or $Version -eq 'latest') {
    try {
        $release = Invoke-RestMethod -UseBasicParsing -Uri "https://api.github.com/repos/$Repo/releases/latest" -Headers @{ 'User-Agent' = 'gta-install' }
        $Version = [string]$release.tag_name
    } catch {
        $err = $_.Exception.Message
        $none = $false
        try {
            $list = @(Invoke-RestMethod -UseBasicParsing -Uri "https://api.github.com/repos/$Repo/releases?per_page=1" -Headers @{ 'User-Agent' = 'gta-install' })
            $none = ($list.Count -eq 0)
        } catch { }
        if ($none) {
            Fail "no release of $Repo has been published yet. Install from source instead (needs Go 1.26+): go install github.com/$Repo/cmd/gta@latest"
        }
        Fail "could not find the latest release of $Repo (pin one: -Version vX.Y.Z): $err"
    }
}
$Version = $Version -replace '^v', ''
if ($Version -notmatch '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$') {
    Fail "not a release version: $Version (want X.Y.Z or vX.Y.Z)"
}

$archive = "gta_${Version}_windows_${arch}.zip"
$base = "https://github.com/$Repo/releases/download/v$Version"

$tmp = Join-Path ([IO.Path]::GetTempPath()) ("gta-install-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null

try {
    # ---- download ------------------------------------------------------------

    Say "downloading gta v$Version for windows/$arch"
    $zipPath = Join-Path $tmp $archive
    $sumsPath = Join-Path $tmp 'checksums.txt'
    try {
        Invoke-WebRequest -UseBasicParsing -Uri "$base/$archive" -OutFile $zipPath
    } catch {
        Fail "download failed: $base/$archive (does release v$Version exist?): $($_.Exception.Message)"
    }
    try {
        Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt" -OutFile $sumsPath
    } catch {
        Fail "download failed: $base/checksums.txt: $($_.Exception.Message)"
    }

    # ---- verify --------------------------------------------------------------

    $entries = @(Get-Content -LiteralPath $sumsPath | ForEach-Object {
        $fields = $_.Trim() -split '\s+'
        if ($fields.Count -eq 2 -and $fields[1].TrimStart('*') -eq $archive) { $fields[0].ToLowerInvariant() }
    })
    if ($entries.Count -eq 0) { Fail "checksums.txt has no entry for $archive" }
    if ($entries.Count -gt 1) { Fail "checksums.txt lists $archive more than once" }
    $expected = $entries[0]
    if ($expected -notmatch '^[0-9a-f]{64}$') { Fail "checksums.txt has a malformed entry for $archive" }

    $actual = (Get-FileHash -LiteralPath $zipPath -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actual -ne $expected) {
        Fail "checksum mismatch for $archive (expected $expected, got $actual); nothing was installed"
    }
    Say "checksum verified ($expected)"

    # ---- install -------------------------------------------------------------

    $unpacked = Join-Path $tmp 'x'
    Expand-Archive -LiteralPath $zipPath -DestinationPath $unpacked
    $exe = Join-Path $unpacked 'gta.exe'
    if (-not (Test-Path -LiteralPath $exe -PathType Leaf)) { Fail "$archive does not contain gta.exe" }

    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    $target = Join-Path $InstallDir 'gta.exe'
    # Stage next to the target, then rename, so a failed copy never leaves a
    # half-written gta.exe behind.
    $staged = Join-Path $InstallDir ('.gta.install.' + $PID + '.exe')
    Copy-Item -LiteralPath $exe -Destination $staged -Force
    try {
        Move-Item -LiteralPath $staged -Destination $target -Force
    } catch {
        Remove-Item -LiteralPath $staged -Force -ErrorAction SilentlyContinue
        Fail "could not replace $target (is gta running?): $($_.Exception.Message)"
    }
    Say "installed $target"
    try { & $target --version } catch { Say "warning: $target did not run" }
} finally {
    Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
}

# ---- PATH --------------------------------------------------------------------

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
$onPath = @(($userPath, $env:Path) -join ';' -split ';' | Where-Object { $_ -and ($_.TrimEnd('\') -ieq $InstallDir.TrimEnd('\')) }).Count -gt 0
if (-not $onPath) {
    if ($NoModifyPath) {
        Say "note: $InstallDir is not on your PATH"
    } else {
        $newPath = if ($userPath) { "$userPath;$InstallDir" } else { $InstallDir }
        [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
        $env:Path = "$env:Path;$InstallDir"
        Say "added $InstallDir to your user PATH (open a new terminal to pick it up)"
    }
}
