function findLldLink {
    if ($env:OLLAMA_LLVM_BIN) {
        $candidate = Join-Path $env:OLLAMA_LLVM_BIN 'lld-link.exe'
        if (Test-Path $candidate) {
            return $candidate
        }
    }

    $command = Get-Command 'lld-link.exe' -ErrorAction SilentlyContinue
    if ($command) {
        return $command.Source
    }

    $patterns = @(
        'C:\Program Files\Microsoft Visual Studio\*\*\VC\Tools\Llvm\x64\bin\lld-link.exe',
        'C:\Program Files (x86)\Microsoft Visual Studio\*\*\VC\Tools\Llvm\x64\bin\lld-link.exe'
    )

    foreach ($pattern in $patterns) {
        $match = Get-ChildItem -Path $pattern -File -ErrorAction SilentlyContinue | Sort-Object FullName -Descending | Select-Object -First 1
        if ($match) {
            return $match.FullName
        }
    }

    return $null
}

$lldLink = findLldLink
if (-not $lldLink) {
    Write-Output 'ERROR: unable to locate lld-link.exe. Set OLLAMA_LLVM_BIN to override the LLVM toolchain directory.'
    exit 1
}

$linkArgs = @('/nologo')
$subsystem = $null
$subsystemVersionMajor = $null
$subsystemVersionMinor = $null
$osVersionMajor = $null
$osVersionMinor = $null

for ($index = 0; $index -lt $args.Count; $index++) {
    $argument = $args[$index]

    switch -Regex ($argument) {
        '^-m64$' { continue }
        '^-mconsole$' { $subsystem = 'console'; continue }
        '^-mwindows$' { $subsystem = 'windows'; continue }
        '^-o$' {
            $index++
            $linkArgs += "/out:$($args[$index])"
            continue
        }
        '^-Wl,--tsaware$' { $linkArgs += '/tsaware'; continue }
        '^-Wl,--nxcompat$' { $linkArgs += '/nxcompat'; continue }
        '^-Wl,--dynamicbase$' { $linkArgs += '/dynamicbase'; continue }
        '^-Wl,--high-entropy-va$' { $linkArgs += '/highentropyva'; continue }
        '^-Wl,--major-os-version=(\d+)$' { $osVersionMajor = $Matches[1]; continue }
        '^-Wl,--minor-os-version=(\d+)$' { $osVersionMinor = $Matches[1]; continue }
        '^-Wl,--major-subsystem-version=(\d+)$' { $subsystemVersionMajor = $Matches[1]; continue }
        '^-Wl,--minor-subsystem-version=(\d+)$' { $subsystemVersionMinor = $Matches[1]; continue }
        '^-Wl,--compress-debug-sections=.*$' { continue }
        '^-Wl,-T,.*$' { continue }
        '^-Wl,--start-group$' { continue }
        '^-Wl,--end-group$' { continue }
        '^-Qunused-arguments$' { continue }
        '^-static$' { continue }
        '^-s$' { continue }
        '^-O\d*$' { continue }
        '^-g$' { continue }
        '^-lmingw32$' { continue }
        '^-lmingwex$' { continue }
        '^-l(.+)$' {
            $linkArgs += "$($Matches[1]).lib"
            continue
        }
        '^-Wl,-def:(.+)$' {
            $linkArgs += "/def:$($Matches[1])"
            continue
        }
        default {
            if ($argument -like '*.def') {
                $linkArgs += "/def:$argument"
            } else {
                $linkArgs += $argument
            }
        }
    }
}

if ($subsystem) {
    if ($subsystemVersionMajor -and $subsystemVersionMinor) {
        $linkArgs += "/subsystem:$subsystem,$subsystemVersionMajor.$subsystemVersionMinor"
    } else {
        $linkArgs += "/subsystem:$subsystem"
    }

    if ($subsystem -eq 'windows') {
        $linkArgs += '/entry:mainCRTStartup'
    }
}

if ($osVersionMajor -and $osVersionMinor) {
    $linkArgs += "/osversion:$osVersionMajor.$osVersionMinor"
}

$linkArgs += @(
    'libcpmt.lib',
    'libcmt.lib',
    'libvcruntime.lib',
    'libucrt.lib',
    'legacy_stdio_definitions.lib',
    'uuid.lib'
)

& $lldLink @linkArgs
exit $LASTEXITCODE
