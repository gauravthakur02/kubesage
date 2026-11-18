# Install kubeweave on Windows from GitHub releases.
#
#   irm https://raw.githubusercontent.com/gauravthakur02/kubeweave/main/install.ps1 | iex
#
# Options (environment variables):
#   $env:KUBEWEAVE_VERSION = "0.2.0"        a specific version (default: the latest release)
#   $env:KUBEWEAVE_INSTALL_DIR = "C:\tools"  where to put kubeweave.exe (default: %LOCALAPPDATA%\Programs\kubeweave)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$repo = 'gauravthakur02/kubeweave'
$headers = @{ 'User-Agent' = 'kubeweave-installer' }

$cpu = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
switch ($cpu) {
    'AMD64' { $arch = 'amd64' }
    'ARM64' { $arch = 'arm64' }
    default { throw "kubeweave install: unsupported CPU $cpu" }
}

$version = $env:KUBEWEAVE_VERSION
if (-not $version) {
    $latest = Invoke-RestMethod -Uri "https://api.github.com/repos/$repo/releases/latest" -Headers $headers
    $version = $latest.tag_name
}
$version = $version.TrimStart('v')

$archive = "kubeweave_${version}_windows_${arch}.zip"
$base = "https://github.com/$repo/releases/download/v$version"
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("kubeweave-" + [Guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    Write-Host "Downloading kubeweave $version for windows/$arch..."
    Invoke-WebRequest -Uri "$base/$archive" -OutFile (Join-Path $tmp $archive) -Headers $headers -UseBasicParsing
    Invoke-WebRequest -Uri "$base/checksums.txt" -OutFile (Join-Path $tmp 'checksums.txt') -Headers $headers -UseBasicParsing

    $line = Get-Content (Join-Path $tmp 'checksums.txt') | Where-Object { $_ -match "\s$([Regex]::Escape($archive))$" } | Select-Object -First 1
    if (-not $line) { throw "kubeweave install: $archive is not listed in checksums.txt" }
    $want = ($line -split '\s+')[0].ToLower()
    $got = (Get-FileHash -Algorithm SHA256 (Join-Path $tmp $archive)).Hash.ToLower()
    if ($want -ne $got) { throw "kubeweave install: checksum mismatch for $archive" }

    Expand-Archive -Path (Join-Path $tmp $archive) -DestinationPath (Join-Path $tmp 'x') -Force

    $dir = $env:KUBEWEAVE_INSTALL_DIR
    if (-not $dir) { $dir = Join-Path $env:LOCALAPPDATA 'Programs\kubeweave' }
    New-Item -ItemType Directory -Path $dir -Force | Out-Null
    Copy-Item (Join-Path $tmp 'x\kubeweave.exe') (Join-Path $dir 'kubeweave.exe') -Force
    Write-Host "OK installed $dir\kubeweave.exe"

    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (-not (($userPath -split ';') -contains $dir)) {
        [Environment]::SetEnvironmentVariable('Path', ($userPath.TrimEnd(';') + ";$dir").TrimStart(';'), 'User')
        $env:Path = "$env:Path;$dir"
        Write-Host "   added $dir to your user PATH (open a new terminal to use it everywhere)"
    }
}
finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
