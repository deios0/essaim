<#
.SYNOPSIS
    install.ps1 - the `irm https://get.oikos.sh/install.ps1 | iex` installer for oikos (Windows).

.DESCRIPTION
    The PowerShell analog of scripts/install.sh. Its ONLY job: detect the
    platform, download the right signed static binary, verify it, place
    oikos.exe on a NO-ADMIN PATH dir, and print the next step. It NEVER elevates
    (no admin), NEVER installs a service, and writes NO runtime state (the purity
    invariant - a machine that installs and never configures is filesystem-clean
    except the executable). See docs/specs/2026-06-23-install-simplicity-spec.md sec 4.

    Corporate-proxy aware: honors $env:HTTPS_PROXY / $env:HTTP_PROXY so the
    download works behind a corp egress proxy (30-40 percent of indie devs are on
    Windows, often behind one).

    Offline-testable: set OIKOS_INSTALL_DRYRUN=1 to print every step (detected
    os/arch + resolved URL + target path) and exit 0 WITHOUT any network or
    filesystem write.

    Overridable knobs (env):
      OIKOS_INSTALL_DRYRUN=1     plan only; no download, no write
      OIKOS_VERSION=vX.Y.Z       which release to fetch (default: latest)
      OIKOS_INSTALL_DIR=<dir>    where to place oikos.exe (default: a no-admin
                                 PATH dir, $env:LOCALAPPDATA\Programs\oikos)
      OIKOS_BASE_URL=<url>       release host base (default: the public releases URL)
      OIKOS_OS / OIKOS_ARCH      override detection (testing)
      HTTPS_PROXY / HTTP_PROXY   corporate egress proxy for the download
#>

# Write-Host is the correct surface for an interactive installer: its output is
# user-facing console text, not pipeline data, so writing it to the success
# stream (which `| iex` would capture) would be wrong. The PSAvoidUsingWriteHost
# rule is therefore suppressed here with that justification.
[Diagnostics.CodeAnalysis.SuppressMessageAttribute('PSAvoidUsingWriteHost', '',
    Justification = 'Installer console output is user-facing, not pipeline data.')]
param()

# Strict mode + fail-fast: an unset variable or a non-terminating error that we
# missed must stop the script, not half-install.
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Fail {
    param([Parameter(Mandatory = $true)][string]$Message)
    Write-Error "oikos install error: $Message"
    exit 1
}

# Get-EnvOrDefault returns the named env var's value, or $Default when unset/empty.
function Get-EnvOrDefault {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Default
    )
    $value = [Environment]::GetEnvironmentVariable($Name)
    if ([string]::IsNullOrEmpty($value)) {
        return $Default
    }
    return $value
}

# Resolve-Arch maps the host processor architecture to the release asset suffix.
function Resolve-Arch {
    $override = [Environment]::GetEnvironmentVariable('OIKOS_ARCH')
    if (-not [string]::IsNullOrEmpty($override)) {
        return $override
    }
    # PROCESSOR_ARCHITECTURE is AMD64 / ARM64 / x86 on Windows; on ARM64 hosts a
    # 32-bit shell reports the WOW6432 value, so also consult the W6432 var.
    $procArch = $env:PROCESSOR_ARCHITECTURE
    $procArchW6432 = [Environment]::GetEnvironmentVariable('PROCESSOR_ARCHITEW6432')
    if (-not [string]::IsNullOrEmpty($procArchW6432)) {
        $procArch = $procArchW6432
    }
    switch ($procArch) {
        'AMD64' { return 'amd64' }
        'ARM64' { return 'arm64' }
        default {
            Fail "unsupported architecture: $procArch - download a binary from $script:BaseUrl"
        }
    }
}

# Join-Win joins path parts with a single Windows backslash, trimming a trailing
# separator so we never produce a doubled '\\'. Used instead of Join-Path because
# Join-Path validates the drive (it rejects 'C:\...' on a non-Windows host, which
# breaks the offline cross-platform dry-run); a plain string join is purely
# lexical and host-independent.
function Join-Win {
    param([Parameter(Mandatory = $true)][string[]]$Parts)
    $out = $Parts[0].TrimEnd('\')
    foreach ($p in $Parts[1..($Parts.Length - 1)]) {
        $out = $out + '\' + $p.Trim('\')
    }
    return $out
}

# Resolve-InstallDir picks a per-user, NO-ADMIN PATH dir, defaulting to
# $env:LOCALAPPDATA\Programs\oikos (the standard no-admin install location).
# IsDryRun relaxes the 'no LOCALAPPDATA' failure to a clearly-marked placeholder
# so an offline dry run on any host still prints a representative target.
function Resolve-InstallDir {
    param([bool]$IsDryRun = $false)
    $override = [Environment]::GetEnvironmentVariable('OIKOS_INSTALL_DIR')
    if (-not [string]::IsNullOrEmpty($override)) {
        return $override
    }
    $localAppData = $env:LOCALAPPDATA
    if ([string]::IsNullOrEmpty($localAppData)) {
        # Fall back to %APPDATA% if LOCALAPPDATA is somehow unset (rare).
        $localAppData = $env:APPDATA
    }
    if ([string]::IsNullOrEmpty($localAppData)) {
        if ($IsDryRun) {
            # Offline dry run on a host with no Windows app-data vars: show the
            # canonical placeholder so the planned layout is still visible.
            $localAppData = '%LOCALAPPDATA%'
        } else {
            Fail 'neither LOCALAPPDATA nor APPDATA is set - pass OIKOS_INSTALL_DIR explicitly'
        }
    }
    return (Join-Win -Parts @($localAppData, 'Programs', 'oikos'))
}

# Get-ProxyParam returns the Invoke-WebRequest splat for the corp proxy, honoring
# $env:HTTPS_PROXY then $env:HTTP_PROXY. Empty hashtable when no proxy is set.
function Get-ProxyParam {
    $proxy = Get-EnvOrDefault -Name 'HTTPS_PROXY' -Default (Get-EnvOrDefault -Name 'HTTP_PROXY' -Default '')
    if ([string]::IsNullOrEmpty($proxy)) {
        return @{}
    }
    return @{ Proxy = $proxy; ProxyUseDefaultCredentials = $true }
}

# Get-RemoteText downloads a small text resource (e.g. SHA256SUMS), returning ''
# on any failure (the caller decides whether absence is fatal). Proxy-aware.
function Get-RemoteText {
    param([Parameter(Mandatory = $true)][string]$Url)
    try {
        $proxyParam = Get-ProxyParam
        $resp = Invoke-WebRequest -Uri $Url -UseBasicParsing @proxyParam
        return [string]$resp.Content
    } catch {
        return ''
    }
}

# Test-Checksum verifies the downloaded file against a published SHA256SUMS, when
# present. Returns $true on a verified match, $false when verification could not
# be performed (sums absent / asset unlisted) - the caller tolerates $false but a
# real MISMATCH is fatal (we never silently install a tampered binary).
function Test-Checksum {
    param(
        [Parameter(Mandatory = $true)][string]$FilePath,
        [Parameter(Mandatory = $true)][string]$Asset,
        [Parameter(Mandatory = $true)][string]$Url
    )
    $sumsUrl = ($Url -replace '/[^/]+$', '/SHA256SUMS')
    $sums = Get-RemoteText -Url $sumsUrl
    if ([string]::IsNullOrEmpty($sums)) {
        Write-Host 'oikos: SHA256SUMS not published for this release; skipping checksum verification'
        return $false
    }
    $want = ''
    foreach ($line in ($sums -split "`n")) {
        $line = $line.Trim()
        if ($line -match "\s\*?$([regex]::Escape($Asset))$") {
            $want = ($line -split '\s+')[0]
            break
        }
    }
    if ([string]::IsNullOrEmpty($want)) {
        Write-Host "oikos: $Asset not listed in SHA256SUMS; skipping checksum verification"
        return $false
    }
    $got = (Get-FileHash -Path $FilePath -Algorithm SHA256).Hash.ToLower()
    if ($want.ToLower() -ne $got) {
        Fail "checksum mismatch for $Asset (expected $want, got $got) - refusing to install"
    }
    Write-Host 'oikos: checksum verified'
    return $true
}

# Test-IsFileLocked reports whether Path is a file currently held open by another
# process (e.g. a running oikos.exe). It probes by trying to open the file for
# write with NO sharing; a sharing violation means it is in use. Any file that
# does not exist, or opens cleanly, is NOT locked. This is how we distinguish the
# "oikos is running" upgrade case from an ordinary move failure WITHOUT depending
# on PowerShell's exception-unwrapping under -ErrorActionPreference Stop.
function Test-IsFileLocked {
    param([Parameter(Mandatory = $true)][string]$Path)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        return $false
    }
    $stream = $null
    try {
        # Open for write with FileShare::None: succeeds only if nothing else holds it.
        $stream = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open,
            [System.IO.FileAccess]::ReadWrite, [System.IO.FileShare]::None)
        return $false
    } catch [System.IO.IOException] {
        # A sharing violation (or general IO error opening it) -> in use / locked.
        return $true
    } catch [System.UnauthorizedAccessException] {
        # Locked or access-guarded -> treat as in-use (the user must stop/close it).
        return $true
    } finally {
        if ($null -ne $stream) { $stream.Dispose() }
    }
}

function Invoke-Main {
    # --- configuration --------------------------------------------------------
    # Placeholder public release host. The real value is set at release time; the
    # script is the single source of the download URL.
    $script:BaseUrl = Get-EnvOrDefault -Name 'OIKOS_BASE_URL' -Default 'https://github.com/deios0/oikos/releases'
    $version = Get-EnvOrDefault -Name 'OIKOS_VERSION' -Default 'latest'
    $dryRun = (Get-EnvOrDefault -Name 'OIKOS_INSTALL_DRYRUN' -Default '0') -eq '1'

    # --- detect OS / arch -----------------------------------------------------
    # This script only ever runs on Windows; OIKOS_OS exists for the test matrix.
    $os = Get-EnvOrDefault -Name 'OIKOS_OS' -Default 'windows'
    $arch = Resolve-Arch

    # --- resolve artifact + URL ----------------------------------------------
    $ext = ''
    if ($os -eq 'windows') { $ext = '.exe' }
    $asset = "oikos_${os}_${arch}${ext}"

    if ($version -eq 'latest') {
        $url = "$script:BaseUrl/latest/download/$asset"
    } else {
        $url = "$script:BaseUrl/download/$version/$asset"
    }

    # --- resolve install dir (no admin) --------------------------------------
    $binDir = Resolve-InstallDir -IsDryRun:$dryRun
    $target = Join-Win -Parts @($binDir, "oikos$ext")

    # --- dry run: plan only, no side effects ---------------------------------
    if ($dryRun) {
        Write-Host 'oikos installer (dry run - no download, no write)'
        Write-Host "  os:        $os"
        Write-Host "  arch:      $arch"
        Write-Host "  version:   $version"
        Write-Host "  asset:     $asset"
        Write-Host "  url:       $url"
        Write-Host "  target:    $target"
        Write-Host '  next step: oikos init   then oikos emit   (writes your AGENTS.md; no proxy needed)'
        Write-Host '  live mode: oikos serve  then open http://127.0.0.1:4141/setup   (optional)'
        return
    }

    # --- real install ---------------------------------------------------------
    New-Item -ItemType Directory -Path $binDir -Force | Out-Null

    $tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("oikos-" + [System.IO.Path]::GetRandomFileName() + $ext)
    try {
        Write-Host "oikos: downloading $asset ..."
        $proxyParam = Get-ProxyParam
        # -UseBasicParsing keeps this working on minimal/headless hosts; the proxy
        # splat routes through a corp egress proxy when one is configured.
        Invoke-WebRequest -Uri $url -OutFile $tmp -UseBasicParsing @proxyParam

        # Checksum verification (best-effort: only when SHA256SUMS is published).
        # A real mismatch is fatal; absence is reported, never hidden.
        [void](Test-Checksum -FilePath $tmp -Asset $asset -Url $url)

        # Place the new binary. On Windows you CANNOT replace a running .exe: if
        # `oikos serve` is running, oikos.exe is locked and Move-Item over it fails
        # with a raw, confusing sharing-violation error. Guard the move and, when the
        # EXISTING target is the thing that's locked, print a clear, actionable next
        # step (stop the running oikos, then re-run) instead of the raw exception.
        #
        # A broad catch (not a typed `catch [IOException]`) is deliberate: under
        # $ErrorActionPreference='Stop' a cmdlet's terminating error is wrapped, so a
        # typed catch can miss the underlying IOException. We instead disambiguate by
        # STATE - Test-IsFileLocked on the existing target tells us it is in use.
        try {
            Move-Item -Path $tmp -Destination $target -Force
        } catch {
            if ((Test-Path -LiteralPath $target) -and (Test-IsFileLocked -Path $target)) {
                Fail (
                    "cannot replace $target - it is in use (oikos is running). " +
                    "Stop it first (close ``oikos serve`` / end the oikos.exe process), then re-run this installer."
                )
            }
            # Any other move failure (e.g. a genuine permission problem, a full disk)
            # is re-thrown unchanged so it is not masked by the running-binary message.
            throw
        }
    } finally {
        if (Test-Path -LiteralPath $tmp) { Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue }
    }

    Write-Host "oikos: installed to $target"

    # Put $binDir on the PATH if it isn't already. We edit ONLY the per-user PATH
    # (never the machine PATH -> no admin), reading and writing the User scope in
    # isolation. This is deliberately NOT `setx PATH "...;$env:PATH"`: `setx`
    # truncates PATH at 1024 chars, and `$env:PATH` is the COMBINED machine+process
    # PATH, so that idiom flattens machine+process PATH into the User PATH and
    # permanently corrupts it. Instead we prepend $binDir to the existing User
    # PATH value only, via [Environment]::SetEnvironmentVariable(...,'User').
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if ([string]::IsNullOrEmpty($userPath)) { $userPath = '' }
    $binDirTrimmed = $binDir.TrimEnd('\')
    $onUserPath = $false
    foreach ($p in ($userPath -split ';')) {
        if (-not [string]::IsNullOrEmpty($p) -and $p.TrimEnd('\') -ieq $binDirTrimmed) {
            $onUserPath = $true
            break
        }
    }
    if ($onUserPath) {
        Write-Host "oikos: $binDir is already on your user PATH"
    } else {
        # Prepend so oikos wins over any older copy; keep the existing User PATH
        # exactly as-is after it. No machine/process PATH is read or written.
        if ([string]::IsNullOrEmpty($userPath)) {
            $newUserPath = $binDir
        } else {
            $newUserPath = "$binDir;$userPath"
        }
        [Environment]::SetEnvironmentVariable('Path', $newUserPath, 'User')
        # Reflect it in THIS session's process PATH too, so the very next line the
        # user types can find oikos without opening a new terminal.
        $env:PATH = "$binDir;$env:PATH"
        Write-Host "oikos: added $binDir to your user PATH (open a new terminal for other apps to see it)"
    }
    Write-Host ''
    Write-Host 'Next:  oikos init     # seed a vault + a starter rule'
    Write-Host '       oikos emit     # write the ranked block into your AGENTS.md (no proxy needed)'
    Write-Host ''
    Write-Host 'Optional live mode (real-time correction capture via the proxy):'
    Write-Host '       oikos serve    # then open http://127.0.0.1:4141/setup'
}

Invoke-Main
