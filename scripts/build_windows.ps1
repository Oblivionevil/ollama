#!powershell
#
# powershell -ExecutionPolicy Bypass -File .\scripts\build_windows.ps1
#
# gcloud auth application-default login

# Use "Continue" so that stderr output from native commands (e.g. CGo warnings)
# is not promoted to a terminating exception by the try/catch block.
# All native commands already check $LASTEXITCODE explicitly.
$ErrorActionPreference = "Continue"

mkdir -Force -path .\dist | Out-Null

$script:MSIX_PACKAGE_NAME = "Ollama.Ollama"
$script:MSIX_DISPLAY_NAME = "Ollama"
$script:MSIX_DESCRIPTION = "Ollama desktop app"
$script:MSIX_BACKGROUND = "#111827"
$script:SigningMode = "none"

function findVisualStudioDeveloperCommand {
    $patterns = @(
        "C:\Program Files\Microsoft Visual Studio\*\*\Common7\Tools\VsDevCmd.bat",
        "C:\Program Files (x86)\Microsoft Visual Studio\*\*\Common7\Tools\VsDevCmd.bat"
    )

    foreach ($pattern in $patterns) {
        $match = Get-ChildItem -Path $pattern -File -ErrorAction SilentlyContinue | Sort-Object FullName -Descending | Select-Object -First 1
        if ($match) {
            return $match.FullName
        }
    }

    return $null
}

function findVisualStudioLlvmBinDir {
    $patterns = @(
        "C:\Program Files\Microsoft Visual Studio\*\*\VC\Tools\Llvm\x64\bin",
        "C:\Program Files (x86)\Microsoft Visual Studio\*\*\VC\Tools\Llvm\x64\bin"
    )

    foreach ($pattern in $patterns) {
        $match = Get-ChildItem -Path $pattern -Directory -ErrorAction SilentlyContinue | Sort-Object FullName -Descending | Select-Object -First 1
        if ($match) {
            return $match.FullName
        }
    }

    return $null
}

function importCmdEnvironment {
    param(
        [string]$CommandPath,
        [string[]]$Arguments = @()
    )

    if (-not $CommandPath) {
        return $false
    }

    $argumentString = ($Arguments | Where-Object { $_ }) -join ' '
    $commandLine = if ($argumentString) {
        "`"$CommandPath`" $argumentString >nul && set"
    } else {
        "`"$CommandPath`" >nul && set"
    }

    $environmentLines = & cmd.exe /d /s /c $commandLine
    if ($LASTEXITCODE -ne 0) {
        return $false
    }

    foreach ($line in $environmentLines) {
        if ($line -match '^(.*?)=(.*)$') {
            [System.Environment]::SetEnvironmentVariable($matches[1], $matches[2], 'Process')
        }
    }

    return $true
}

function enableLocalTestSigning {
    if ($env:OLLAMA_LOCAL_TEST_SIGNING -ne "1") {
        return
    }

    $signingDir = Join-Path $script:SRC_DIR "dist\signing"
    $pfxPath = Join-Path $signingDir "ollama-local-test.pfx"
    $cerPath = Join-Path $signingDir "ollama-local-test.cer"
    $passwordPath = Join-Path $signingDir "ollama-local-test.password.txt"
    $subject = if ($env:OLLAMA_LOCAL_TEST_CERT_SUBJECT) { $env:OLLAMA_LOCAL_TEST_CERT_SUBJECT } else { "CN=Ollama Local Test" }

    mkdir -Force -path $signingDir | Out-Null

    $password = if ($env:OLLAMA_LOCAL_TEST_CERT_PASSWORD) {
        $env:OLLAMA_LOCAL_TEST_CERT_PASSWORD
    } elseif (Test-Path $passwordPath) {
        (Get-Content -Path $passwordPath -Raw).Trim()
    } else {
        [guid]::NewGuid().ToString('N')
    }

    if (-not (Test-Path $pfxPath) -or -not (Test-Path $cerPath)) {
        $securePassword = ConvertTo-SecureString -String $password -AsPlainText -Force
        $cert = New-SelfSignedCertificate -Type CodeSigningCert -Subject $subject -CertStoreLocation "Cert:\CurrentUser\My" -KeyExportPolicy Exportable -HashAlgorithm SHA256 -KeyAlgorithm RSA -KeyLength 2048 -NotAfter (Get-Date).AddYears(2)
        Export-PfxCertificate -Cert $cert -FilePath $pfxPath -Password $securePassword | Out-Null
        Export-Certificate -Cert $cert -FilePath $cerPath | Out-Null
        Set-Content -Path $passwordPath -Value $password -Encoding ascii
        Write-Output "Generated local test signing certificate at $pfxPath"
        Write-Output "Exported public certificate at $cerPath"
    }

    $env:SIGN_PFX = $pfxPath
    $env:SIGN_CERT = $cerPath
    $env:SIGN_PFX_PASSWORD = $password
    if (-not $env:APPINSTALLER_BASE_URI) {
        $env:APPINSTALLER_BASE_URI = "https://example.invalid/ollama-local"
    }
}

function configureWindowsCgoToolchain {
    if ($env:CC) {
        return
    }

    $vsDevCmd = findVisualStudioDeveloperCommand
    $llvmBinDir = findVisualStudioLlvmBinDir
    if ($vsDevCmd -and $llvmBinDir) {
        $vsArch = switch ($script:TARGET_ARCH) {
            "amd64" { "x64" }
            "arm64" { "arm64" }
            default { $null }
        }

        if ($vsArch -and (importCmdEnvironment $vsDevCmd @("-arch=$vsArch"))) {
            if ($env:PATH -notmatch [regex]::Escape($llvmBinDir)) {
                $env:PATH = "$llvmBinDir;$env:PATH"
            }
            $env:CC = "clang"
            $env:CXX = "clang++"
            $env:OLLAMA_LLVM_BIN = $llvmBinDir
            if (-not $env:OLLAMA_EXTLD) {
                $env:OLLAMA_EXTLD = Join-Path $script:SRC_DIR "scripts\clang-extld.cmd"
            }
            Write-Output "Using Visual Studio LLVM toolchain for CGO"
            return
        }
    }

    if (Get-Command zig -ErrorAction SilentlyContinue) {
        $env:CC = "zig cc"
        $env:CXX = "zig c++"
        Write-Output "Using zig as the C/C++ compiler for CGO"
    }
}

function loadSigningCertificate {
    if ($script:OLLAMA_PFX -and (Test-Path $script:OLLAMA_PFX)) {
        return New-Object System.Security.Cryptography.X509Certificates.X509Certificate2($script:OLLAMA_PFX, $script:OLLAMA_PFX_PASSWORD)
    }

    if ($script:OLLAMA_CERT -and (Test-Path $script:OLLAMA_CERT)) {
        return [System.Security.Cryptography.X509Certificates.X509Certificate2]::CreateFromCertFile($script:OLLAMA_CERT)
    }

    return $null
}

function getSignToolArguments {
    param(
        [switch]$ForMsix
    )

    if ($script:SigningMode -eq "kms") {
        $args = @("sign", "/v", "/fd", "sha256")
        if ($ForMsix) {
            $args += @("/td", "sha256", "/tr", "http://timestamp.digicert.com")
        } else {
            $args += @("/t", "http://timestamp.digicert.com")
        }
        $args += @("/f", "${script:OLLAMA_CERT}", "/csp", "Google Cloud KMS Provider", "/kc", "${env:KEY_CONTAINER}")
        return $args
    }

    if ($script:SigningMode -eq "pfx") {
        $args = @("sign", "/v", "/fd", "sha256", "/f", "${script:OLLAMA_PFX}")
        if ($script:OLLAMA_PFX_PASSWORD) {
            $args += @("/p", "${script:OLLAMA_PFX_PASSWORD}")
        }
        if ($ForMsix) {
            $args += @("/td", "sha256")
        }
        if ($env:SIGN_TIMESTAMP_URL) {
            if ($ForMsix) {
                $args += @("/tr", "${env:SIGN_TIMESTAMP_URL}")
            } else {
                $args += @("/t", "${env:SIGN_TIMESTAMP_URL}")
            }
        }
        return $args
    }

    return @()
}

function findWindowsSdkIncludeDir {
    param(
        [string]$Subdirectory,
        [string]$RequiredFile = $null
    )

    $patterns = @(
        "C:\Program Files (x86)\Windows Kits\10\Include\*\${Subdirectory}",
        "C:\Program Files\Windows Kits\10\Include\*\${Subdirectory}"
    )

    foreach ($pattern in $patterns) {
        $matches = Get-ChildItem -Path $pattern -Directory -ErrorAction SilentlyContinue | Sort-Object FullName -Descending
        foreach ($match in $matches) {
            if (-not $RequiredFile -or (Test-Path (Join-Path $match.FullName $RequiredFile))) {
                return $match.FullName
            }
        }
    }

    return $null
}

function getCompilerFriendlyPath {
    param(
        [string]$Path
    )

    if (-not $Path -or $Path -notmatch '\s') {
        return $Path
    }

    try {
        $fileSystem = New-Object -ComObject Scripting.FileSystemObject
        if (Test-Path $Path -PathType Container) {
            $shortPath = $fileSystem.GetFolder($Path).ShortPath
        } else {
            $shortPath = $fileSystem.GetFile($Path).ShortPath
        }
        if ($shortPath) {
            return $shortPath
        }
    } catch {
    }

    return $Path
}

function checkEnv {
    if ($null -ne $env:ARCH ) {
        $script:ARCH = $env:ARCH
    } else {
        $arch=([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture)
        if ($null -ne $arch) {
            $script:ARCH = ($arch.ToString().ToLower()).Replace("x64", "amd64")
        } else {
            Write-Output "WARNING: old powershell detected, assuming amd64 architecture - set `$env:ARCH to override"
            $script:ARCH="amd64"
        }
    }
    $script:TARGET_ARCH=$script:ARCH
    Write-host "Building for ${script:TARGET_ARCH}"
    Write-Output "Locating required tools and paths"
    $script:SRC_DIR=$PWD

    # Locate CUDA versions
    $cudaList=(get-item "C:\Program Files\NVIDIA GPU Computing Toolkit\CUDA\v*\bin\" -ea 'silentlycontinue')
    if ($cudaList.length -eq 0) {
        $d=(get-command -ea 'silentlycontinue' nvcc).path
        if ($null -ne $d) {
            $script:CUDA_DIRS=@($d| split-path -parent)
        }
    } else {
        # Favor newer patch versions if available
        $script:CUDA_DIRS=($cudaList | sort-object -Descending)
    }
    if ($script:CUDA_DIRS.length -gt 0) {
        Write-Output "Available CUDA Versions: $script:CUDA_DIRS"
    } else {
        Write-Output "No CUDA versions detected"
    }

    # Locate ROCm v6
    $rocmDir=(get-item "C:\Program Files\AMD\ROCm\6.*" -ea 'silentlycontinue' | sort-object -Descending | select-object -First 1)
    if ($null -ne $rocmDir) {
        $script:HIP_PATH=$rocmDir.FullName
    } elseif ($null -ne $env:HIP_PATH -and $env:HIP_PATH -match '[/\\]6\.') {
        $script:HIP_PATH=$env:HIP_PATH
    }
    
    $innoSetupRoots = @(
        "C:\Program Files\Inno Setup*",
        "C:\Program Files (x86)\Inno Setup*",
        (Join-Path $env:LOCALAPPDATA "Programs\Inno Setup*")
    )
    foreach ($pattern in $innoSetupRoots) {
        $inoSetup = Get-ChildItem -Path $pattern -Directory -ErrorAction SilentlyContinue | Sort-Object FullName -Descending | Select-Object -First 1
        if ($inoSetup) {
            $script:INNO_SETUP_DIR = $inoSetup.FullName
            break
        }
    }

    $script:DIST_DIR="${script:SRC_DIR}\dist\windows-${script:TARGET_ARCH}"
    $env:CGO_ENABLED="1"
    configureWindowsCgoToolchain
    if (-not $env:CGO_CFLAGS) {
        $env:CGO_CFLAGS = "-O3"
    }
    if (-not $env:CGO_CXXFLAGS) {
        $env:CGO_CXXFLAGS = "-O3"
    }
    $winRtIncludeDir = findWindowsSdkIncludeDir "winrt" "EventToken.h"
    if ($winRtIncludeDir) {
        $compilerWinRtIncludeDir = getCompilerFriendlyPath $winRtIncludeDir
        if ($env:CGO_CFLAGS -notmatch [regex]::Escape($compilerWinRtIncludeDir)) {
            $env:CGO_CFLAGS = "$($env:CGO_CFLAGS) -I$compilerWinRtIncludeDir"
        }
        if ($env:CGO_CXXFLAGS -notmatch [regex]::Escape($compilerWinRtIncludeDir)) {
            $env:CGO_CXXFLAGS = "$($env:CGO_CXXFLAGS) -I$compilerWinRtIncludeDir"
        }
        Write-Output "Using Windows SDK WinRT headers from $winRtIncludeDir"
    } else {
        Write-Output "WARNING: unable to locate Windows SDK WinRT headers; WebView2 builds may fail"
    }
    Write-Output "Checking version"
    if (!$env:VERSION) {
        $data=(git describe --tags --first-parent --abbrev=7 --long --dirty --always)
        $pattern="v(.+)"
        if ($data -match $pattern) {
            $script:VERSION=$matches[1]
        }
    } else {
        $script:VERSION=$env:VERSION
    }
    $pattern = "(\d+[.]\d+[.]\d+).*"
    if ($script:VERSION -match $pattern) {
        $script:PKG_VERSION=$matches[1]
    } else {
        $script:PKG_VERSION="0.0.0"
    }
    Write-Output "Building Ollama $script:VERSION with package version $script:PKG_VERSION"

    enableLocalTestSigning

    if ($null -eq $env:SIGN_TOOL) {
        ${script:SignTool}=findWindowsSdkTool "signtool.exe"
    } else {
        ${script:SignTool}=${env:SIGN_TOOL}
    }
    if (${script:SignTool}) {
        Write-Output "Using SignTool at ${script:SignTool}"
    } else {
        Write-Output "WARNING: unable to locate signtool.exe - signed artifacts cannot be produced"
    }
    $script:SigningMode = "none"
    $script:OLLAMA_CERT = $null
    $script:OLLAMA_PFX = $null
    $script:OLLAMA_PFX_PASSWORD = $null
    if ("${env:KEY_CONTAINER}") {
        if (Test-Path "${script:SRC_DIR}\ollama_inc.crt") {
            ${script:OLLAMA_CERT}=$(resolve-path "${script:SRC_DIR}\ollama_inc.crt")
            $script:SigningMode = "kms"
            Write-host "Code signing enabled via Google Cloud KMS"
        } else {
            Write-Output "WARNING: KEY_CONTAINER is set but ollama_inc.crt not found at ${script:SRC_DIR}\ollama_inc.crt - code signing disabled"
        }
    } elseif ($env:SIGN_PFX) {
        if (Test-Path $env:SIGN_PFX) {
            $script:OLLAMA_PFX = (Resolve-Path $env:SIGN_PFX).Path
            $script:OLLAMA_PFX_PASSWORD = $env:SIGN_PFX_PASSWORD
            if ($env:SIGN_CERT -and (Test-Path $env:SIGN_CERT)) {
                $script:OLLAMA_CERT = (Resolve-Path $env:SIGN_CERT).Path
            }
            $script:SigningMode = "pfx"
            Write-Output "Code signing enabled via PFX certificate"
        } else {
            Write-Output "WARNING: SIGN_PFX is set but the file was not found - code signing disabled"
        }
    } else {
        Write-Output "Code signing disabled - set KEY_CONTAINER with ollama_inc.crt, or SIGN_PFX/SIGN_PFX_PASSWORD, or OLLAMA_LOCAL_TEST_SIGNING=1"
    }
    if ($env:OLLAMA_BUILD_PARALLEL) {
        $script:JOBS=[int]$env:OLLAMA_BUILD_PARALLEL
    } else {
        # Use physical core count rather than logical processors (hyperthreads)
        # to avoid saturating the system during builds
        try {
            $cores = (Get-CimInstance Win32_Processor | Measure-Object -Property NumberOfCores -Sum).Sum
        } catch {
            $cores = 0
        }
        if ($cores -gt 0) {
            $script:JOBS = $cores
        } else {
            $script:JOBS = [Environment]::ProcessorCount
        }
    }
    Write-Output "Build parallelism: $script:JOBS (set OLLAMA_BUILD_PARALLEL to override)"
}

function signingEnabled {
    return ($script:SigningMode -ne "none")
}

function requireCodeSigning {
    param(
        [string]$artifactName
    )

    if (-not (signingEnabled)) {
        Write-Output "ERROR: ${artifactName} requires code signing. Configure KEY_CONTAINER with ollama_inc.crt, or SIGN_PFX/SIGN_PFX_PASSWORD, or set OLLAMA_LOCAL_TEST_SIGNING=1."
        exit 1
    }

    if (-not ${script:SignTool}) {
        Write-Output "ERROR: ${artifactName} requires signtool.exe, but it could not be located. Set SIGN_TOOL to override the path."
        exit 1
    }
}

function invokeSignTool {
    param(
        [string[]]$Paths
    )

    $targets = @($Paths | Where-Object { $_ })
    if ($targets.Count -eq 0) {
        return
    }

    $arguments = getSignToolArguments
    if ($arguments.Count -eq 0) {
        Write-Output "ERROR: signing requested but no signing arguments are available"
        exit 1
    }

    & "${script:SignTool}" @arguments $targets | ForEach-Object { Write-Host $_ }
    if ($LASTEXITCODE -ne 0) { exit($LASTEXITCODE)}
}

function invokeMsixSignTool {
    param(
        [string[]]$Paths
    )

    $targets = @($Paths | Where-Object { $_ })
    if ($targets.Count -eq 0) {
        return
    }

    $arguments = getSignToolArguments -ForMsix
    if ($arguments.Count -eq 0) {
        Write-Output "ERROR: MSIX signing requested but no signing arguments are available"
        exit 1
    }

    & "${script:SignTool}" @arguments $targets | ForEach-Object { Write-Host $_ }
    if ($LASTEXITCODE -ne 0) { exit($LASTEXITCODE)}
}

function findWindowsSdkTool {
    param(
        [string]$ToolName
    )

    $patterns = @(
        "C:\Program Files (x86)\Windows Kits\10\bin\*\x64\${ToolName}",
        "C:\Program Files (x86)\Windows Kits\8.1\bin\x64\${ToolName}"
    )

    foreach ($pattern in $patterns) {
        $match = Get-ChildItem -Path $pattern -ErrorAction SilentlyContinue | Sort-Object FullName -Descending | Select-Object -First 1
        if ($match) {
            return $match.FullName
        }
    }

    return $null
}

function msixPackageVersion {
    if ($env:APPX_PACKAGE_VERSION) {
        return $env:APPX_PACKAGE_VERSION
    }

    $parts = @($script:PKG_VERSION.Split('.'))
    while ($parts.Count -lt 4) {
        $parts += "0"
    }
    if ($parts.Count -gt 4) {
        $parts = $parts[0..3]
    }

    return ($parts -join '.')
}

function msixProcessorArchitecture {
    param(
        [string]$Arch
    )

    switch ($Arch) {
        "amd64" { return "x64" }
        "arm64" { return "arm64" }
        default {
            Write-Output "ERROR: unsupported MSIX architecture ${Arch}"
            exit 1
        }
    }
}

function msixPublisher {
    if ($env:APPX_PUBLISHER) {
        return $env:APPX_PUBLISHER
    }

    $certificate = loadSigningCertificate
    if ($certificate) {
        return $certificate.Subject
    }

    Write-Output "ERROR: unable to determine MSIX publisher. Set APPX_PUBLISHER or enable code signing."
    exit 1
}

function xmlEscape {
    param(
        [string]$Value
    )

    return [System.Security.SecurityElement]::Escape($Value)
}

function appInstallerBaseUri {
    if ($env:APPINSTALLER_BASE_URI) {
        return $env:APPINSTALLER_BASE_URI.TrimEnd('/')
    }

    if ($env:OLLAMA_LOCAL_TEST_SIGNING -eq "1") {
        return "https://example.invalid/ollama-local"
    }

    if ($env:GITHUB_REPOSITORY -and $script:VERSION) {
        return "https://github.com/$($env:GITHUB_REPOSITORY)/releases/download/v$($script:VERSION)"
    }

    return $null
}

function saveMsixPng {
    param(
        [System.Drawing.Image]$Image,
        [int]$Width,
        [int]$Height,
        [string]$Destination,
        [double]$PaddingFactor = 0.78
    )

    $bitmap = New-Object System.Drawing.Bitmap $Width, $Height
    try {
        $graphics = [System.Drawing.Graphics]::FromImage($bitmap)
        try {
            $graphics.Clear([System.Drawing.ColorTranslator]::FromHtml($script:MSIX_BACKGROUND))
            $graphics.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::HighQuality
            $graphics.InterpolationMode = [System.Drawing.Drawing2D.InterpolationMode]::HighQualityBicubic
            $graphics.PixelOffsetMode = [System.Drawing.Drawing2D.PixelOffsetMode]::HighQuality

            $scale = [Math]::Min(([double]$Width / [double]$Image.Width), ([double]$Height / [double]$Image.Height)) * $PaddingFactor
            $drawWidth = [int][Math]::Round($Image.Width * $scale)
            $drawHeight = [int][Math]::Round($Image.Height * $scale)
            $offsetX = [int][Math]::Round(($Width - $drawWidth) / 2)
            $offsetY = [int][Math]::Round(($Height - $drawHeight) / 2)
            $graphics.DrawImage($Image, $offsetX, $offsetY, $drawWidth, $drawHeight)
        } finally {
            $graphics.Dispose()
        }

        $bitmap.Save($Destination, [System.Drawing.Imaging.ImageFormat]::Png)
    } finally {
        $bitmap.Dispose()
    }
}

function writeMsixAssets {
    param(
        [string]$AssetDir
    )

    Add-Type -AssemblyName System.Drawing

    mkdir -Force -path $AssetDir | Out-Null
    $iconPath = Join-Path $script:SRC_DIR "app\assets\app.ico"
    $icon = $null
    $iconStream = $null
    $image = $null
    try {
        $image = [System.Drawing.Image]::FromFile($iconPath)
    } catch {
        try {
            $iconStream = [System.IO.File]::OpenRead($iconPath)
            $icon = New-Object System.Drawing.Icon($iconStream)
            $image = $icon.ToBitmap()
        } catch {
            Write-Output "ERROR: unable to load MSIX icon source from ${iconPath}"
            exit 1
        }
    }
    try {
        saveMsixPng $image 44 44 (Join-Path $AssetDir "Square44x44Logo.png") 0.90
        saveMsixPng $image 50 50 (Join-Path $AssetDir "StoreLogo.png") 0.90
        saveMsixPng $image 71 71 (Join-Path $AssetDir "Square71x71Logo.png") 0.88
        saveMsixPng $image 150 150 (Join-Path $AssetDir "Square150x150Logo.png") 0.82
        saveMsixPng $image 310 150 (Join-Path $AssetDir "Wide310x150Logo.png") 0.62
        saveMsixPng $image 310 310 (Join-Path $AssetDir "Square310x310Logo.png") 0.72
        saveMsixPng $image 620 300 (Join-Path $AssetDir "SplashScreen.png") 0.48
    } finally {
        if ($image) {
            $image.Dispose()
        }
        if ($icon) {
            $icon.Dispose()
        }
        if ($iconStream) {
            $iconStream.Dispose()
        }
    }
}

function copyVcRuntimeLibraries {
    param(
        [string]$Arch,
        [string]$PackageDir
    )

    $msvcArch = msixProcessorArchitecture $Arch
    $patterns = @()
    if ($env:VCToolsRedistDir) {
        $patterns += (Join-Path $env:VCToolsRedistDir "${msvcArch}\Microsoft.VC*.CRT\*.dll")
    }
    $patterns += @(
        "C:\Program Files\Microsoft Visual Studio\*\*\VC\Redist\MSVC\*\${msvcArch}\Microsoft.VC*.CRT\*.dll",
        "C:\Program Files (x86)\Microsoft Visual Studio\*\*\VC\Redist\MSVC\*\${msvcArch}\Microsoft.VC*.CRT\*.dll",
        (Join-Path $env:LOCALAPPDATA "Programs\Microsoft Visual Studio\*\*\VC\Redist\MSVC\*\${msvcArch}\Microsoft.VC*.CRT\*.dll")
    )

    $copied = @{}
    foreach ($pattern in $patterns) {
        $matches = Get-ChildItem -Path $pattern -ErrorAction SilentlyContinue | Sort-Object FullName -Descending
        foreach ($match in $matches) {
            if (-not $copied.ContainsKey($match.Name)) {
                Copy-Item -Path $match.FullName -Destination (Join-Path $PackageDir $match.Name)
                $copied[$match.Name] = $true
            }
        }
    }

    if ($copied.Count -eq 0) {
        Write-Host "WARNING: no VC runtime DLLs found for ${Arch}; the packaged app may require the VC++ runtime to already be installed."
    } else {
        Write-Host "Bundled VC runtime DLLs for ${Arch}: $($copied.Keys -join ', ')"
    }
}

function writeMsixManifest {
    param(
        [string]$Arch,
        [string]$PackageDir,
        [string]$Publisher,
        [string]$PackageVersion
    )

    $templatePath = Join-Path $script:SRC_DIR "app\msix\AppxManifest.xml.in"
    $manifestPath = Join-Path $PackageDir "AppxManifest.xml"
    $displayName = if ($env:APPX_DISPLAY_NAME) { $env:APPX_DISPLAY_NAME } else { $script:MSIX_DISPLAY_NAME }
    $publisherDisplayName = if ($env:APPX_PUBLISHER_DISPLAY_NAME) { $env:APPX_PUBLISHER_DISPLAY_NAME } else { $script:MSIX_DISPLAY_NAME }
    $description = if ($env:APPX_DESCRIPTION) { $env:APPX_DESCRIPTION } else { $script:MSIX_DESCRIPTION }
    $packageName = if ($env:APPX_PACKAGE_NAME) { $env:APPX_PACKAGE_NAME } else { $script:MSIX_PACKAGE_NAME }

    $manifest = Get-Content -Path $templatePath -Raw
    $manifest = $manifest.Replace("__PACKAGE_NAME__", (xmlEscape $packageName))
    $manifest = $manifest.Replace("__PUBLISHER__", (xmlEscape $Publisher))
    $manifest = $manifest.Replace("__PACKAGE_VERSION__", (xmlEscape $PackageVersion))
    $manifest = $manifest.Replace("__ARCH__", (xmlEscape (msixProcessorArchitecture $Arch)))
    $manifest = $manifest.Replace("__DISPLAY_NAME__", (xmlEscape $displayName))
    $manifest = $manifest.Replace("__PUBLISHER_DISPLAY_NAME__", (xmlEscape $publisherDisplayName))
    $manifest = $manifest.Replace("__DESCRIPTION__", (xmlEscape $description))
    $manifest = $manifest.Replace("__BACKGROUND_COLOR__", $script:MSIX_BACKGROUND)

    Set-Content -Path $manifestPath -Value $manifest -Encoding utf8
}

function stageMsixPackage {
    param(
        [string]$Arch,
        [string]$Publisher,
        [string]$PackageVersion,
        [string]$MakeAppxTool
    )

    $packageSource = Join-Path $script:SRC_DIR "dist\windows-${Arch}"
    if (-not (Test-Path (Join-Path $packageSource "ollama app.exe"))) {
        Write-Output "ERROR: missing staged desktop app for ${Arch} at ${packageSource}"
        exit 1
    }

    $packageRoot = Join-Path $script:SRC_DIR "dist\msix\${Arch}\package"
    $assetDir = Join-Path $packageRoot "Assets"
    $msixPath = Join-Path $script:SRC_DIR "dist\Ollama-$((msixProcessorArchitecture $Arch)).msix"

    Remove-Item -ea 0 -Recurse -Force $packageRoot
    mkdir -Force -path $packageRoot | Out-Null

    Copy-Item -Path (Join-Path $packageSource '*') -Destination $packageRoot -Recurse

    copyVcRuntimeLibraries $Arch $packageRoot
    writeMsixAssets $assetDir
    writeMsixManifest $Arch $packageRoot $Publisher $PackageVersion

    Remove-Item -ea 0 -Force $msixPath
    Write-Host "Packing signed MSIX for ${Arch}: ${msixPath}"
    & $MakeAppxTool pack /o /h SHA256 /d $packageRoot /p $msixPath | ForEach-Object { Write-Host $_ }
    if ($LASTEXITCODE -ne 0) { exit($LASTEXITCODE)}

    invokeMsixSignTool @($msixPath)

    return $msixPath
}

function buildMsixBundle {
    param(
        [string[]]$MsixPaths,
        [string]$PackageVersion,
        [string]$MakeAppxTool
    )

    $bundleInputDir = Join-Path $script:SRC_DIR "dist\msix\bundle-input"
    $bundlePath = Join-Path $script:SRC_DIR "dist\Ollama.msixbundle"

    Remove-Item -ea 0 -Recurse -Force $bundleInputDir
    mkdir -Force -path $bundleInputDir | Out-Null
    foreach ($path in $MsixPaths) {
        Copy-Item -Path $path -Destination (Join-Path $bundleInputDir ([IO.Path]::GetFileName($path)))
    }

    Remove-Item -ea 0 -Force $bundlePath
    Write-Host "Packing signed MSIX bundle: ${bundlePath}"
    & $MakeAppxTool bundle /o /bv $PackageVersion /d $bundleInputDir /p $bundlePath | ForEach-Object { Write-Host $_ }
    if ($LASTEXITCODE -ne 0) { exit($LASTEXITCODE)}

    invokeMsixSignTool @($bundlePath)

    return $bundlePath
}

function writeAppInstallerFile {
    param(
        [string]$MainArtifact,
        [bool]$IsBundle,
        [string]$Publisher,
        [string]$PackageVersion,
        [string[]]$Architectures
    )

    $baseUri = appInstallerBaseUri
    if (-not $baseUri) {
        Write-Output "APPINSTALLER_BASE_URI is not set and no GitHub release context is available; skipping .appinstaller generation."
        return
    }

    $packageName = if ($env:APPX_PACKAGE_NAME) { $env:APPX_PACKAGE_NAME } else { $script:MSIX_PACKAGE_NAME }
    $artifactName = [IO.Path]::GetFileName($MainArtifact)
    $artifactUri = "${baseUri}/${artifactName}"
    $appInstallerPath = Join-Path $script:SRC_DIR "dist\Ollama.appinstaller"
    $appInstallerUri = "${baseUri}/Ollama.appinstaller"
    if ($IsBundle) {
        $mainPackageNode = '  <MainBundle Name="{0}" Version="{1}" Publisher="{2}" Uri="{3}" />' -f (xmlEscape $packageName), (xmlEscape $PackageVersion), (xmlEscape $Publisher), (xmlEscape $artifactUri)
    } else {
        $arch = msixProcessorArchitecture $Architectures[0]
        $mainPackageNode = '  <MainPackage Name="{0}" Version="{1}" Publisher="{2}" ProcessorArchitecture="{3}" Uri="{4}" />' -f (xmlEscape $packageName), (xmlEscape $PackageVersion), (xmlEscape $Publisher), (xmlEscape $arch), (xmlEscape $artifactUri)
    }

    $content = @"
<?xml version="1.0" encoding="utf-8"?>
<AppInstaller xmlns="http://schemas.microsoft.com/appx/appinstaller/2018"
  Uri="$(xmlEscape $appInstallerUri)"
  Version="$(xmlEscape $PackageVersion)">
$mainPackageNode
  <UpdateSettings>
    <OnLaunch HoursBetweenUpdateChecks="0" ShowPrompt="true" UpdateBlocksActivation="false" />
    <AutomaticBackgroundTask />
    <ForceUpdateFromAnyVersion>true</ForceUpdateFromAnyVersion>
  </UpdateSettings>
</AppInstaller>
"@

    Set-Content -Path $appInstallerPath -Value $content -Encoding utf8
    Write-Output "Generated App Installer manifest ${appInstallerPath}"
}

function appinstaller {
    requireCodeSigning "App Installer packaging"

    $makeAppxTool = findWindowsSdkTool "MakeAppx.exe"
    if ($null -eq $makeAppxTool) {
        Write-Output "ERROR: missing MakeAppx.exe - install the Windows SDK to build MSIX/App Installer packages"
        exit 1
    }

    $publisher = msixPublisher
    $packageVersion = msixPackageVersion
    $availableArchs = @()
    foreach ($candidate in @("amd64", "arm64")) {
		if (Test-Path (Join-Path $script:SRC_DIR "dist\windows-${candidate}\ollama app.exe")) {
            $availableArchs += $candidate
        }
    }

    if ($availableArchs.Count -eq 0) {
        Write-Output "ERROR: no Windows app artifacts found in dist for MSIX packaging"
        exit 1
    }

    Remove-Item -ea 0 -Force "${script:SRC_DIR}\dist\*.msix"
    Remove-Item -ea 0 -Force "${script:SRC_DIR}\dist\*.msixbundle"
    Remove-Item -ea 0 -Force "${script:SRC_DIR}\dist\*.appinstaller"

    $packages = @()
    foreach ($arch in $availableArchs) {
        $packages += (stageMsixPackage $arch $publisher $packageVersion $makeAppxTool)
    }

    $mainArtifact = if ($packages.Count -gt 1) {
        buildMsixBundle $packages $packageVersion $makeAppxTool
    } else {
        $packages[0]
    }

    writeAppInstallerFile $mainArtifact ($packages.Count -gt 1) $publisher $packageVersion $availableArchs
}


function cpu {
    mkdir -Force -path "${script:DIST_DIR}\" | Out-Null
    if ($script:ARCH -ne "arm64") {
        Remove-Item -ea 0 -recurse -force -path "${script:SRC_DIR}\dist\windows-${script:ARCH}"
        New-Item "${script:SRC_DIR}\dist\windows-${script:ARCH}\lib\ollama\" -ItemType Directory -ea 0

        & cmake -B build\cpu --preset CPU --install-prefix $script:DIST_DIR
        if ($LASTEXITCODE -ne 0) { exit($LASTEXITCODE)}
        & cmake --build build\cpu --target ggml-cpu --config Release --parallel $script:JOBS
        if ($LASTEXITCODE -ne 0) { exit($LASTEXITCODE)}
        & cmake --install build\cpu --component CPU --strip
        if ($LASTEXITCODE -ne 0) { exit($LASTEXITCODE)}
    }
}

function cuda11 {
    # CUDA v11 claims to be compatible with MSVC 2022, but the latest updates are no longer compatible
    # 19.40 is the last compiler version that works, but recent udpates are 19.43
    # So this pins to MSVC 2019 for best compatibility
    mkdir -Force -path "${script:DIST_DIR}\" | Out-Null
    $cudaMajorVer="11"
    if ($script:ARCH -ne "arm64") {
        if ("$script:CUDA_DIRS".Contains("v$cudaMajorVer")) {
            foreach ($d in $Script:CUDA_DIRS){ 
                if ($d.FullName.Contains("v$cudaMajorVer")) {
                    if (test-path -literalpath (join-path -path $d -childpath "nvcc.exe" ) ) {
                        $cuda=($d.FullName|split-path -parent)
                        break
                    }
                }
            }
            Write-Output "Building CUDA v$cudaMajorVer backend libraries $cuda"
            $env:CUDAToolkit_ROOT=$cuda
            & cmake -B build\cuda_v$cudaMajorVer --preset "CUDA $cudaMajorVer" -T cuda="$cuda" -DCMAKE_CUDA_COMPILER="$cuda\bin\nvcc.exe" -G "Visual Studio 16 2019" --install-prefix "$script:DIST_DIR"
            if ($LASTEXITCODE -ne 0) { exit($LASTEXITCODE)}
            & cmake --build build\cuda_v$cudaMajorVer --target ggml-cuda --config Release --parallel $script:JOBS
            if ($LASTEXITCODE -ne 0) { exit($LASTEXITCODE)}
            & cmake --install build\cuda_v$cudaMajorVer --component "CUDA" --strip
            if ($LASTEXITCODE -ne 0) { exit($LASTEXITCODE)}
        } else {
            Write-Output "CUDA v$cudaMajorVer not detected, skipping"
        }
    } else {
        Write-Output "not arch we wanted"
    }
    Write-Output "done"
}

function cudaCommon {
    param (
        [string]$cudaMajorVer
    )
    mkdir -Force -path "${script:DIST_DIR}\" | Out-Null
    if ($script:ARCH -ne "arm64") {
        if ("$script:CUDA_DIRS".Contains("v$cudaMajorVer")) {
            foreach ($d in $Script:CUDA_DIRS){ 
                if ($d.FullName.Contains("v$cudaMajorVer")) {
                    if (test-path -literalpath (join-path -path $d -childpath "nvcc.exe" ) ) {
                        $cuda=($d.FullName|split-path -parent)
                        break
                    }
                }
            }
            Write-Output "Building CUDA v$cudaMajorVer backend libraries $cuda"
            $env:CUDAToolkit_ROOT=$cuda
            & cmake -B build\cuda_v$cudaMajorVer --preset "CUDA $cudaMajorVer" -T cuda="$cuda" --install-prefix "$script:DIST_DIR"
            if ($LASTEXITCODE -ne 0) { exit($LASTEXITCODE)}
            & cmake --build build\cuda_v$cudaMajorVer --target ggml-cuda --config Release --parallel $script:JOBS
            if ($LASTEXITCODE -ne 0) { exit($LASTEXITCODE)}
            & cmake --install build\cuda_v$cudaMajorVer --component "CUDA" --strip
            if ($LASTEXITCODE -ne 0) { exit($LASTEXITCODE)}
        } else {
            Write-Output "CUDA v$cudaMajorVer not detected, skipping"
        }
    }
}

function cuda12 {
    cudaCommon("12")
}

function cuda13 {
    cudaCommon("13")
}

function rocm6 {
    mkdir -Force -path "${script:DIST_DIR}\" | Out-Null
    if ($script:ARCH -ne "arm64") {
        if ($script:HIP_PATH) {
            Write-Output "Building ROCm backend libraries $script:HIP_PATH"
            if (-Not (get-command -ErrorAction silent ninja)) {
                $NINJA_DIR=(gci -path (Get-CimInstance MSFT_VSInstance -Namespace root/cimv2/vs)[0].InstallLocation -r -fi ninja.exe).Directory.FullName
                $env:PATH="$NINJA_DIR;$env:PATH"
            }
            $env:HIPCXX="${script:HIP_PATH}\bin\clang++.exe"
            $env:HIP_PLATFORM="amd"
            $env:CMAKE_PREFIX_PATH="${script:HIP_PATH}"
            # Set CC/CXX via environment instead of -D flags to avoid triggering
            # spurious compiler-change reconfigures that reset CMAKE_INSTALL_PREFIX
            $env:CC="${script:HIP_PATH}\bin\clang.exe"
            $env:CXX="${script:HIP_PATH}\bin\clang++.exe"
            & cmake -B build\rocm --preset "ROCm 6" -G Ninja `
                -DCMAKE_C_FLAGS="-parallel-jobs=4 -Wno-ignored-attributes -Wno-deprecated-pragma" `
                -DCMAKE_CXX_FLAGS="-parallel-jobs=4 -Wno-ignored-attributes -Wno-deprecated-pragma" `
                --install-prefix $script:DIST_DIR
            if ($LASTEXITCODE -ne 0) { exit($LASTEXITCODE)}
            $env:HIPCXX=""
            $env:HIP_PLATFORM=""
            $env:CMAKE_PREFIX_PATH=""
            $env:CC=""
            $env:CXX=""
            & cmake --build build\rocm --target ggml-hip --config Release --parallel $script:JOBS
            if ($LASTEXITCODE -ne 0) { exit($LASTEXITCODE)}
            & cmake --install build\rocm --component "HIP" --strip
            if ($LASTEXITCODE -ne 0) { exit($LASTEXITCODE)}
            Remove-Item -Path $script:DIST_DIR\lib\ollama\rocm\rocblas\library\*gfx906* -ErrorAction SilentlyContinue
        } else {
            Write-Output "ROCm not detected, skipping"
        }
    }
}

function vulkan {
    if ($env:VULKAN_SDK) {
        Write-Output "Building Vulkan backend libraries"
        & cmake -B build\vulkan --preset Vulkan --install-prefix $script:DIST_DIR
        if ($LASTEXITCODE -ne 0) { exit($LASTEXITCODE)}
        & cmake --build build\vulkan --target ggml-vulkan --config Release --parallel $script:JOBS
        if ($LASTEXITCODE -ne 0) { exit($LASTEXITCODE)}
        & cmake --install build\vulkan  --component Vulkan --strip
        if ($LASTEXITCODE -ne 0) { exit($LASTEXITCODE)}
    } else {
        Write-Output "Vulkan not detected, skipping"
    }
}

function mlxCuda13 {
    mkdir -Force -path "${script:DIST_DIR}\" | Out-Null
    $cudaMajorVer="13"
    if ($script:ARCH -ne "arm64") {
        if ("$script:CUDA_DIRS".Contains("v$cudaMajorVer")) {
            foreach ($d in $Script:CUDA_DIRS){
                if ($d.FullName.Contains("v$cudaMajorVer")) {
                    if (test-path -literalpath (join-path -path $d -childpath "nvcc.exe" ) ) {
                        $cuda=($d.FullName|split-path -parent)
                        break
                    }
                }
            }

            # Check for cuDNN - required for MLX CUDA backend
            # Supports two layouts:
            # 1. CI/zip extract: CUDNN\include\cudnn.h, lib\x64\, bin\x64\
            # 2. Official installer: CUDNN\v*\include\{cuda-ver}\cudnn.h, lib\{cuda-ver}\x64\, bin\{cuda-ver}\
            if ($env:CUDNN_INCLUDE_PATH -and $env:CUDNN_LIBRARY_PATH) {
                Write-Output "Using cuDNN from environment: $env:CUDNN_INCLUDE_PATH"
            } elseif (Test-Path "C:\Program Files\NVIDIA\CUDNN\include\cudnn.h") {
                # CI/zip layout (flat)
                $cudnnRoot = "C:\Program Files\NVIDIA\CUDNN"
                $env:CUDNN_ROOT_DIR = $cudnnRoot
                $env:CUDNN_INCLUDE_PATH = "$cudnnRoot\include"
                $env:CUDNN_LIBRARY_PATH = "$cudnnRoot\lib\x64"
                Write-Output "Found cuDNN at $cudnnRoot (flat layout)"
            } else {
                # Official installer layout (versioned)
                $cudnnRoot = $null
                $resolved = Resolve-Path -Path "C:\Program Files\NVIDIA\CUDNN\v*" -ErrorAction SilentlyContinue | Sort-Object -Descending | Select-Object -First 1
                if ($resolved -and (Test-Path "$($resolved.Path)\include\$cudaMajorVer.0\cudnn.h")) {
                    $cudnnRoot = $resolved.Path
                    $env:CUDNN_ROOT_DIR = $cudnnRoot
                    $env:CUDNN_INCLUDE_PATH = "$cudnnRoot\include\$cudaMajorVer.0"
                    $env:CUDNN_LIBRARY_PATH = "$cudnnRoot\lib\$cudaMajorVer.0\x64"
                    Write-Output "Found cuDNN at $cudnnRoot (official installer, CUDA $cudaMajorVer.0)"
                } else {
                    Write-Output "cuDNN not found - set CUDNN_INCLUDE_PATH and CUDNN_LIBRARY_PATH environment variables"
                    Write-Output "Skipping MLX build"
                    return
                }
            }

            Write-Output "Building MLX CUDA v$cudaMajorVer backend libraries $cuda"
            $env:CUDAToolkit_ROOT=$cuda
            & cmake -B build\mlx_cuda_v$cudaMajorVer --preset "MLX CUDA $cudaMajorVer" -T cuda="$cuda" --install-prefix "$script:DIST_DIR"
            if ($LASTEXITCODE -ne 0) { exit($LASTEXITCODE)}
            & cmake --build build\mlx_cuda_v$cudaMajorVer --target mlx --target mlxc --config Release --parallel $script:JOBS -- /nodeReuse:false
            if ($LASTEXITCODE -ne 0) { exit($LASTEXITCODE)}
            & cmake --install build\mlx_cuda_v$cudaMajorVer --component "MLX" --strip
            if ($LASTEXITCODE -ne 0) { exit($LASTEXITCODE)}
        } else {
            Write-Output "CUDA v$cudaMajorVer not detected, skipping MLX build"
        }
    }
}

function ollama {
    mkdir -Force -path "${script:DIST_DIR}\" | Out-Null
    Write-Output "Building ollama CLI"
    & go build -trimpath -ldflags "-s -w -X=github.com/ollama/ollama/version.Version=$script:VERSION -X=github.com/ollama/ollama/server.mode=release" .
    if ($LASTEXITCODE -ne 0) { exit($LASTEXITCODE)}
    cp .\ollama.exe "${script:DIST_DIR}\"
}

function app {
    Write-Output "Building Ollama App $script:VERSION with package version $script:PKG_VERSION"

    $appLdflags = "-s -w -H windowsgui -X=github.com/ollama/ollama/app/version.Version=$script:VERSION"
    if ($env:OLLAMA_EXTLD) {
        $appLdflags += " -extld=$($env:OLLAMA_EXTLD)"
        Write-Output "Using custom external linker for app build: $($env:OLLAMA_EXTLD)"
    }

    if (!(Get-Command npm -ErrorAction SilentlyContinue)) {
        Write-Output "npm is not installed. Please install Node.js and npm first:"
        Write-Output "   Visit: https://nodejs.org/"
        exit 1
    }

    if (!(Get-Command tsc -ErrorAction SilentlyContinue)) {
        Write-Output "Installing TypeScript compiler..."
        npm install -g typescript
    }
    if (!(Get-Command tscriptify -ErrorAction SilentlyContinue)) {
        Write-Output "Installing tscriptify..."
        go install github.com/tkrajina/typescriptify-golang-structs/tscriptify@latest
    }
    if (!(Get-Command tscriptify -ErrorAction SilentlyContinue)) {
        $env:PATH="$env:PATH;$(go env GOPATH)\bin"
    }

    Push-Location app/ui/app
    npm install
    if ($LASTEXITCODE -ne 0) { 
        Write-Output "ERROR: npm install failed with exit code $LASTEXITCODE"
        exit $LASTEXITCODE
    }

    Write-Output "Building React application..."
    npm run build
    if ($LASTEXITCODE -ne 0) { 
        Write-Output "ERROR: npm run build failed with exit code $LASTEXITCODE"
        exit $LASTEXITCODE
    }

    # Check if dist directory exists and has content
    if (!(Test-Path "dist")) {
        Write-Output "ERROR: dist directory was not created by npm run build"
        exit 1
    }

    $distFiles = Get-ChildItem "dist" -Recurse
    if ($distFiles.Count -eq 0) {
        Write-Output "ERROR: dist directory is empty after npm run build"
        exit 1
    }

    Pop-Location

    if ($env:OLLAMA_APP_GENERATE -eq "1") {
        Write-Output "Running go generate for desktop app"
        & go generate ./app/ui
        if ($LASTEXITCODE -ne 0) { exit($LASTEXITCODE)}
    } else {
        Write-Output "Skipping go generate for the desktop app (set OLLAMA_APP_GENERATE=1 to refresh generated files)"
    }
    & go build -trimpath -ldflags $appLdflags -o .\dist\windows-ollama-app-${script:ARCH}.exe ./app/cmd/app/
    if ($LASTEXITCODE -ne 0) { exit($LASTEXITCODE)}

    $packageDir = Join-Path $script:SRC_DIR "dist\windows-${script:ARCH}"
    Remove-Item -ea 0 -Recurse -Force $packageDir
    mkdir -Force -path $packageDir | Out-Null
    Copy-Item -Path ".\dist\windows-ollama-app-${script:ARCH}.exe" -Destination (Join-Path $packageDir "ollama app.exe")
    Copy-Item -Path ".\app\assets\app.ico" -Destination (Join-Path $packageDir "app.ico")
    copyVcRuntimeLibraries $script:ARCH $packageDir
}

function deps {
    Write-Output "Download MSVC Redistributables"
    mkdir -Force -path "${script:SRC_DIR}\dist\\windows-arm64" | Out-Null
    mkdir -Force -path "${script:SRC_DIR}\dist\\windows-amd64" | Out-Null
    invoke-webrequest -Uri "https://aka.ms/vs/17/release/vc_redist.arm64.exe" -OutFile  "${script:SRC_DIR}\dist\windows-arm64\vc_redist.arm64.exe" -ErrorAction Stop
    invoke-webrequest -Uri "https://aka.ms/vs/17/release/vc_redist.x64.exe" -OutFile  "${script:SRC_DIR}\dist\windows-amd64\vc_redist.x64.exe" -ErrorAction Stop
    Write-Output "Done."
}

function sign {
    # Copy install.ps1 to dist for release packaging
    Write-Output "Copying install.ps1 to dist"
    Copy-Item -Path "${script:SRC_DIR}\scripts\install.ps1" -Destination "${script:SRC_DIR}\dist\install.ps1" -ErrorAction Stop

    if (signingEnabled) {
        Write-Output "Signing Ollama executables, scripts and libraries"
        invokeSignTool @((get-childitem -path "${script:SRC_DIR}\dist\windows-*" -r -include @('*.exe', '*.dll') | ForEach-Object { $_.FullName }))

        Write-Output "Signing install.ps1"
        invokeSignTool @("${script:SRC_DIR}\dist\install.ps1")
    } else {
        Write-Output "Signing not enabled"
    }
}

function installer {
    if ($null -eq ${script:INNO_SETUP_DIR}) {
        Write-Output "ERROR: missing Inno Setup installation directory - install from https://jrsoftware.org/isdl.php"
        exit 1
    }
    Write-Output "Building Ollama Installer"
    cd "${script:SRC_DIR}\app"
    $env:PKG_VERSION=$script:PKG_VERSION
    if (signingEnabled) {
        & "${script:INNO_SETUP_DIR}\ISCC.exe" /DARCH=$script:TARGET_ARCH /SMySignTool="${script:SignTool} sign /fd sha256 /t http://timestamp.digicert.com /f ${script:OLLAMA_CERT} /csp `$qGoogle Cloud KMS Provider`$q /kc ${env:KEY_CONTAINER} `$f" .\ollama.iss
    } else {
        & "${script:INNO_SETUP_DIR}\ISCC.exe" /DARCH=$script:TARGET_ARCH .\ollama.iss
    }
    if ($LASTEXITCODE -ne 0) { exit($LASTEXITCODE)}
}

function newZipJob($sourceDir, $destZip) {
    $use7z = [bool](Get-Command 7z -ErrorAction SilentlyContinue)
    Start-Job -ScriptBlock {
        param($src, $dst, $use7z)
        if ($use7z) {
            & 7z a -tzip -mx=9 -mmt=on $dst "${src}\*"
            if ($LASTEXITCODE -ne 0) { throw "7z failed with exit code $LASTEXITCODE" }
        } else {
            Compress-Archive -CompressionLevel Optimal -Path "${src}\*" -DestinationPath $dst -Force
        }
    } -ArgumentList $sourceDir, $destZip, $use7z
}

function stageComponents($mainDir, $stagingDir, $pattern, $readmePrefix) {
    $components = Get-ChildItem -Path "${mainDir}\lib\ollama" -Directory -Filter $pattern -ErrorAction SilentlyContinue
    if ($components) {
        Remove-Item -ea 0 -r $stagingDir
        mkdir -Force -path "${stagingDir}\lib\ollama" | Out-Null
        Write-Output "Extract this ${readmePrefix} zip file to the same location where you extracted ollama-windows-amd64.zip" > "${stagingDir}\README_${readmePrefix}.txt"
        foreach ($dir in $components) {
            Write-Output "  Staging $($dir.Name)"
            Move-Item -path $dir.FullName -destination "${stagingDir}\lib\ollama\$($dir.Name)"
        }
        return $true
    }
    return $false
}

function restoreComponents($mainDir, $stagingDir) {
    if (Test-Path -Path "${stagingDir}\lib\ollama") {
        foreach ($dir in (Get-ChildItem -Path "${stagingDir}\lib\ollama" -Directory)) {
            Move-Item -path $dir.FullName -destination "${mainDir}\lib\ollama\$($dir.Name)"
        }
    }
    Remove-Item -ea 0 -r $stagingDir
}

function zip {
    $jobs = @()
    $distDir = "${script:SRC_DIR}\dist"
    $amd64Dir = "${distDir}\windows-amd64"

    # Remove any stale zip files before starting
    Remove-Item -ea 0 "${distDir}\ollama-windows-*.zip"

    try {
        if (Test-Path -Path $amd64Dir) {
            # Stage ROCm into its own directory for independent compression
            if (stageComponents $amd64Dir "${distDir}\windows-amd64-rocm" "rocm*" "ROCm") {
                Write-Output "Generating ${distDir}\ollama-windows-amd64-rocm.zip"
                $jobs += newZipJob "${distDir}\windows-amd64-rocm" "${distDir}\ollama-windows-amd64-rocm.zip"
            }

            # Stage MLX into its own directory for independent compression
            if (stageComponents $amd64Dir "${distDir}\windows-amd64-mlx" "mlx_*" "MLX") {
                Write-Output "Generating ${distDir}\ollama-windows-amd64-mlx.zip"
                $jobs += newZipJob "${distDir}\windows-amd64-mlx" "${distDir}\ollama-windows-amd64-mlx.zip"
            }

            # Compress the main amd64 zip (without rocm/mlx)
            Write-Output "Generating ${distDir}\ollama-windows-amd64.zip"
            $jobs += newZipJob $amd64Dir "${distDir}\ollama-windows-amd64.zip"
        }

        if (Test-Path -Path "${distDir}\windows-arm64") {
            Write-Output "Generating ${distDir}\ollama-windows-arm64.zip"
            $jobs += newZipJob "${distDir}\windows-arm64" "${distDir}\ollama-windows-arm64.zip"
        }

        if ($jobs.Count -gt 0) {
            Write-Output "Waiting for $($jobs.Count) parallel zip jobs..."
            $jobs | Wait-Job | Out-Null
            $failed = $false
            foreach ($job in $jobs) {
                if ($job.State -eq 'Failed') {
                    Write-Error "Zip job failed: $($job.ChildJobs[0].JobStateInfo.Reason)"
                    $failed = $true
                }
                Receive-Job $job
                Remove-Job $job
            }
            if ($failed) { throw "One or more zip jobs failed" }
        }
    } finally {
        # Always restore staged components back into the main tree
        restoreComponents $amd64Dir "${distDir}\windows-amd64-rocm"
        restoreComponents $amd64Dir "${distDir}\windows-amd64-mlx"
    }
}

function clean {
    Remove-Item -ea 0 -r "${script:SRC_DIR}\dist\"
    Remove-Item -ea 0 -r "${script:SRC_DIR}\build\"
}

checkEnv
try {
    if ($($args.count) -eq 0) {
        app
        sign
        if ($null -ne ${script:INNO_SETUP_DIR}) {
            installer
        } else {
            Write-Output "Skipping Inno Setup packaging because Inno Setup is not installed"
        }
        if (signingEnabled) {
            appinstaller
        } else {
            Write-Output "Skipping App Installer packaging because code signing is not enabled"
        }
    } else {
        for ( $i = 0; $i -lt $args.count; $i++ ) {
            Write-Output "running build step $($args[$i])"
            & $($args[$i])
        } 
    }
} catch {
    Write-Error "Build Failed: $($_.Exception.Message)"
    Write-Error "$($_.ScriptStackTrace)"
} finally {
    set-location $script:SRC_DIR
    $env:PKG_VERSION=""
}