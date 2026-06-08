# roborev installer for Windows
# Usage: powershell -ExecutionPolicy ByPass -c "irm https://roborev.io/install.ps1 | iex"

$ErrorActionPreference = 'Stop'

$repo = 'roborev-dev/roborev'
$binaryName = 'roborev.exe'

function Write-Info($msg) { Write-Host $msg -ForegroundColor Green }
function Write-Warn($msg) { Write-Host $msg -ForegroundColor Yellow }
function Write-Err($msg) { Write-Host $msg -ForegroundColor Red }

function Get-Architecture {
    # Check if running on ARM64
    if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') {
        return 'arm64'
    }

    # Check using .NET for more reliable detection
    try {
        $arch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture
        switch ($arch.ToString()) {
            'X64' { return 'amd64' }
            'X86' { return '386' }
            'Arm64' { return 'arm64' }
            default { return 'amd64' }
        }
    } catch {
        # Fallback for older PowerShell
        if ([System.Environment]::Is64BitOperatingSystem) {
            return 'amd64'
        } else {
            return '386'
        }
    }
}

function Invoke-WebRequestCompat {
    # -UseBasicParsing is required in PowerShell 5.x but removed in 6+
    param([string]$Uri, [string]$OutFile)

    $params = @{ Uri = $Uri }
    if ($OutFile) { $params.OutFile = $OutFile }

    # Only add -UseBasicParsing for PowerShell 5.x (it's removed in 6+)
    if ($PSVersionTable.PSVersion.Major -lt 6) {
        $params.UseBasicParsing = $true
    }

    if ($OutFile) {
        Invoke-WebRequest @params
    } else {
        Invoke-RestMethod @params
    }
}

function Get-FinalUrl {
    # Returns the URL that ultimately responded to a request, after any
    # redirects were followed. The property differs by PowerShell edition:
    # Windows PowerShell 5.x exposes HttpWebResponse.ResponseUri, while
    # PowerShell 7+ exposes HttpResponseMessage.RequestMessage.RequestUri.
    param($Response)
    try {
        if ($Response.BaseResponse.ResponseUri) {
            return $Response.BaseResponse.ResponseUri.AbsoluteUri
        }
    } catch {}
    try {
        if ($Response.BaseResponse.RequestMessage.RequestUri) {
            return $Response.BaseResponse.RequestMessage.RequestUri.AbsoluteUri
        }
    } catch {}
    return $null
}

function Get-LatestVersion {
    # Resolve the latest release tag by following the /releases/latest
    # 302 redirect to /releases/tag/<version> and reading the final URL.
    # Using the HTML endpoint (not api.github.com) avoids the 60 req/hr
    # per-IP rate limit, so users behind shared NAT / VPN don't get 403.
    #
    # We let Invoke-WebRequest follow the redirect rather than inspecting
    # the Location header with MaximumRedirection=0: in Windows PowerShell
    # 5.x that throws a System.InvalidOperationException (not a WebException
    # with a usable .Response), which broke the install. A HEAD request
    # follows the redirect without downloading the release page body.
    $url = "https://github.com/$repo/releases/latest"

    if ($PSVersionTable.PSVersion.Major -lt 6) {
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    }

    $params = @{
        Uri = $url
        Method = 'Head'
        ErrorAction = 'Stop'
    }
    if ($PSVersionTable.PSVersion.Major -lt 6) {
        $params.UseBasicParsing = $true
    }

    $finalUrl = $null
    try {
        $response = Invoke-WebRequest @params
        $finalUrl = Get-FinalUrl $response
    } catch {
        throw "Failed to fetch latest version: $_"
    }

    if (-not $finalUrl) {
        throw "Failed to fetch latest version: could not resolve release URL from $url"
    }
    if ($finalUrl -notmatch '/releases/tag/([^/]+)/?$') {
        throw "Failed to fetch latest version: unexpected release URL $finalUrl"
    }
    return $Matches[1]
}

function Get-InstallDir {
    # Use ROBOREV_INSTALL_DIR if set, otherwise default to ~/.roborev/bin
    if ($env:ROBOREV_INSTALL_DIR) {
        return $env:ROBOREV_INSTALL_DIR
    }
    return Join-Path $env:USERPROFILE '.roborev\bin'
}

function Add-ToPath($dir) {
    $currentPath = [Environment]::GetEnvironmentVariable('Path', 'User')

    if ($currentPath -split ';' | Where-Object { $_ -eq $dir }) {
        Write-Info "Directory already in PATH"
        return $false
    }

    $newPath = "$currentPath;$dir"
    [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')

    # Also update current session
    $env:Path = "$env:Path;$dir"

    return $true
}

function Install-Roborev {
    Write-Info "Installing roborev..."
    Write-Host ""

    $arch = Get-Architecture
    Write-Info "Platform: windows/$arch"

    # Handle unsupported architectures
    if ($arch -eq '386') {
        Write-Err "Error: 32-bit Windows is not supported."
        Write-Err "roborev requires 64-bit Windows (amd64 or arm64)."
        exit 1
    }

    $version = Get-LatestVersion
    Write-Info "Latest version: $version"

    $versionNum = $version.TrimStart('v')
    $archiveName = "roborev_${versionNum}_windows_${arch}.zip"
    $downloadUrl = "https://github.com/$repo/releases/download/$version/$archiveName"

    $installDir = Get-InstallDir
    Write-Info "Install directory: $installDir"
    Write-Host ""

    # Create install directory
    if (-not (Test-Path $installDir)) {
        New-Item -ItemType Directory -Path $installDir -Force | Out-Null
    }

    # Create temp directory
    $tmpDir = Join-Path $env:TEMP "roborev-install-$(Get-Random)"
    New-Item -ItemType Directory -Path $tmpDir -Force | Out-Null

    try {
        $archivePath = Join-Path $tmpDir $archiveName

        Write-Info "Downloading $archiveName..."
        Invoke-WebRequestCompat -Uri $downloadUrl -OutFile $archivePath

        # Verify checksum (fail-closed: errors abort installation unless ROBOREV_SKIP_CHECKSUM env var is set)
        $checksumUrl = "https://github.com/$repo/releases/download/$version/SHA256SUMS"
        $checksumFile = Join-Path $tmpDir "SHA256SUMS"

        if ($env:ROBOREV_SKIP_CHECKSUM) {
            Write-Warn "Warning: Skipping checksum verification (ROBOREV_SKIP_CHECKSUM is set)"
        } else {
            Write-Info "Verifying checksum..."
            try {
                Invoke-WebRequestCompat -Uri $checksumUrl -OutFile $checksumFile
            } catch {
                Write-Err "Error: Could not download checksums file: $_"
                Write-Err "Set ROBOREV_SKIP_CHECKSUM=1 to bypass verification (not recommended)"
                exit 1
            }

            # Parse checksum file and find matching entry
            # Handle SHA256SUMS formats: "hash  filename", "hash *filename" (binary), "hash  ./filename"
            $matchingLines = @()
            foreach ($line in Get-Content $checksumFile) {
                if ($line -match '^\s*$') { continue }  # Skip empty lines
                $parts = $line -split '\s+', 2
                if ($parts.Count -lt 2) { continue }
                $hash = $parts[0]
                $filename = $parts[1]
                # Normalize: strip leading * (binary mode) or ./ (relative path)
                $filename = $filename -replace '^[\*]', ''
                $filename = $filename -replace '^\.\/', ''
                $filename = $filename -replace '^\.\\', ''  # Windows-style
                if ($filename -eq $archiveName) {
                    $matchingLines += $hash
                }
            }

            if ($matchingLines.Count -eq 0) {
                Write-Err "Error: Could not find checksum for $archiveName in SHA256SUMS"
                Write-Err "Set ROBOREV_SKIP_CHECKSUM=1 to bypass verification (not recommended)"
                exit 1
            }

            if ($matchingLines.Count -gt 1) {
                Write-Err "Error: Multiple checksum entries found for $archiveName"
                exit 1
            }

            $expectedHash = $matchingLines[0]
            $actualHash = (Get-FileHash -Path $archivePath -Algorithm SHA256).Hash.ToLower()

            if ($actualHash -ne $expectedHash.ToLower()) {
                Write-Err "Error: Checksum verification failed!"
                Write-Err "Expected: $expectedHash"
                Write-Err "Got:      $actualHash"
                exit 1
            }
            Write-Info "Checksum verified."
        }

        Write-Info "Extracting..."
        Expand-Archive -LiteralPath $archivePath -DestinationPath $tmpDir -Force

        # Move binary to install dir
        $binaryPath = Join-Path $tmpDir $binaryName
        $destPath = Join-Path $installDir $binaryName

        # Remove existing binary if present
        if (Test-Path $destPath) {
            Remove-Item $destPath -Force
        }

        Move-Item $binaryPath $destPath -Force

        Write-Host ""
        Write-Info "Installation complete!"
        Write-Host ""

        # Add to PATH
        if (-not $env:ROBOREV_NO_MODIFY_PATH) {
            $pathUpdated = Add-ToPath $installDir
            if ($pathUpdated) {
                Write-Info "Added $installDir to PATH"
                Write-Warn "Restart your terminal for PATH changes to take effect."
                Write-Host ""
            }
        }

        # Verify installation
        Write-Host "Get started:"
        Write-Host "  cd your-repo"
        Write-Host "  roborev init"
        Write-Host ""
        Write-Host "For AI agent skills (Claude Code, Codex):"
        Write-Host "  roborev skills install"

    } finally {
        # Cleanup temp directory
        if (Test-Path $tmpDir) {
            Remove-Item $tmpDir -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
}

# Run installer
Install-Roborev
