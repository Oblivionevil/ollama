param(
    [string]$Repository,
    [string]$EnvironmentName = "release",
    [ValidateSet("kms", "pfx")]
    [string]$SigningMode,
    [string]$AppInstallerBaseUri,
    [switch]$UseWorkflowDefaultAppInstallerUri,
    [string]$SignTimestampUrl = "http://timestamp.digicert.com",
    [string]$ReleaseTag,
    [switch]$DispatchWorkflow,
    [string]$KeyContainer,
    [string]$OllamaCertPath,
    [string]$GoogleSigningCredentialsPath,
    [string]$WindowsSigningPfxPath,
    [string]$WindowsSigningPfxPassword
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

function getDefaultRepository {
    $remoteUrl = git config --get remote.origin.url
    if (-not $remoteUrl) {
        throw "Unable to determine the repository from remote.origin.url. Pass -Repository owner/repo."
    }

    if ($remoteUrl -match 'github\.com[:/](?<repo>[^/]+/[^/.]+)(?:\.git)?$') {
        return $matches['repo']
    }

    throw "Unable to parse a GitHub repository from remote.origin.url: $remoteUrl"
}

function invokeGh {
    param(
        [string[]]$Arguments,
        [string]$InputText = $null,
        [switch]$IgnoreErrors
    )

    if ($null -ne $InputText) {
        $InputText | & gh @Arguments
    } else {
        & gh @Arguments
    }

    if (-not $IgnoreErrors -and $LASTEXITCODE -ne 0) {
        throw "gh $($Arguments -join ' ') failed with exit code $LASTEXITCODE"
    }
}

function requireGh {
    if (-not (Get-Command gh -ErrorAction SilentlyContinue)) {
        throw "GitHub CLI is not installed. Install it with winget install --id GitHub.cli -e"
    }

    & gh auth status *> $null
    if ($LASTEXITCODE -ne 0) {
        throw "GitHub CLI is not authenticated. Run 'gh auth login' and re-run this script."
    }
}

function ensureEnvironment {
    invokeGh @(
        "api",
        "--method", "PUT",
        "-H", "Accept: application/vnd.github+json",
        "repos/$script:TargetRepository/environments/$script:TargetEnvironment"
    ) | Out-Null
}

function setEnvironmentVariable {
    param(
        [string]$Name,
        [string]$Value
    )

    if ([string]::IsNullOrWhiteSpace($Value)) {
        return
    }

    invokeGh @(
        "variable", "set", $Name,
        "--repo", $script:TargetRepository,
        "--env", $script:TargetEnvironment,
        "--body", $Value
    ) | Out-Null
}

function deleteEnvironmentVariable {
    param(
        [string]$Name
    )

    invokeGh @(
        "variable", "delete", $Name,
        "--repo", $script:TargetRepository,
        "--env", $script:TargetEnvironment
    ) -IgnoreErrors | Out-Null
}

function setEnvironmentSecret {
    param(
        [string]$Name,
        [string]$Value
    )

    if ([string]::IsNullOrWhiteSpace($Value)) {
        return
    }

    invokeGh @(
        "secret", "set", $Name,
        "--repo", $script:TargetRepository,
        "--env", $script:TargetEnvironment
    ) -InputText $Value | Out-Null
}

function deleteEnvironmentSecret {
    param(
        [string]$Name
    )

    invokeGh @(
        "secret", "delete", $Name,
        "--repo", $script:TargetRepository,
        "--env", $script:TargetEnvironment
    ) -IgnoreErrors | Out-Null
}

function requireParameterValue {
    param(
        [string]$Value,
        [string]$Message
    )

    if ([string]::IsNullOrWhiteSpace($Value)) {
        throw $Message
    }
}

$script:TargetRepository = if ($Repository) { $Repository } else { getDefaultRepository }
$script:TargetEnvironment = $EnvironmentName

requireGh
ensureEnvironment

setEnvironmentVariable "SIGN_TIMESTAMP_URL" $SignTimestampUrl

if ($UseWorkflowDefaultAppInstallerUri) {
    deleteEnvironmentVariable "APPINSTALLER_BASE_URI"
} else {
    setEnvironmentVariable "APPINSTALLER_BASE_URI" $AppInstallerBaseUri
}

switch ($SigningMode) {
    "kms" {
        requireParameterValue $KeyContainer "-KeyContainer is required for KMS signing."
        requireParameterValue $OllamaCertPath "-OllamaCertPath is required for KMS signing."
        requireParameterValue $GoogleSigningCredentialsPath "-GoogleSigningCredentialsPath is required for KMS signing."
        if (-not (Test-Path $OllamaCertPath -PathType Leaf)) {
            throw "OLLAMA certificate file not found: $OllamaCertPath"
        }
        if (-not (Test-Path $GoogleSigningCredentialsPath -PathType Leaf)) {
            throw "Google signing credentials file not found: $GoogleSigningCredentialsPath"
        }

        $certificateText = Get-Content -Path $OllamaCertPath -Raw
        $credentialsText = Get-Content -Path $GoogleSigningCredentialsPath -Raw

        setEnvironmentVariable "KEY_CONTAINER" $KeyContainer
        setEnvironmentVariable "OLLAMA_CERT" $certificateText
        setEnvironmentSecret "GOOGLE_SIGNING_CREDENTIALS" $credentialsText

        deleteEnvironmentSecret "WINDOWS_SIGNING_PFX_BASE64"
        deleteEnvironmentSecret "WINDOWS_SIGNING_PFX_PASSWORD"
    }
    "pfx" {
        requireParameterValue $WindowsSigningPfxPath "-WindowsSigningPfxPath is required for PFX signing."
        requireParameterValue $WindowsSigningPfxPassword "-WindowsSigningPfxPassword is required for PFX signing."
        if (-not (Test-Path $WindowsSigningPfxPath -PathType Leaf)) {
            throw "PFX file not found: $WindowsSigningPfxPath"
        }

        $pfxBase64 = [Convert]::ToBase64String([System.IO.File]::ReadAllBytes((Resolve-Path $WindowsSigningPfxPath)))

        setEnvironmentSecret "WINDOWS_SIGNING_PFX_BASE64" $pfxBase64
        setEnvironmentSecret "WINDOWS_SIGNING_PFX_PASSWORD" $WindowsSigningPfxPassword

        deleteEnvironmentVariable "KEY_CONTAINER"
        deleteEnvironmentVariable "OLLAMA_CERT"
        deleteEnvironmentSecret "GOOGLE_SIGNING_CREDENTIALS"
    }
}

if ($DispatchWorkflow) {
    requireParameterValue $ReleaseTag "-ReleaseTag is required when -DispatchWorkflow is used."
    invokeGh @(
        "workflow", "run", "release.yaml",
        "--repo", $script:TargetRepository,
        "-f", "tag=$ReleaseTag"
    ) | Out-Null
}

Write-Output "Configured GitHub environment '$script:TargetEnvironment' for $script:TargetRepository using signing mode '$SigningMode'."
if ($UseWorkflowDefaultAppInstallerUri) {
    Write-Output "APPINSTALLER_BASE_URI was removed so the workflow fallback URL will be used."
} elseif ($AppInstallerBaseUri) {
    Write-Output "APPINSTALLER_BASE_URI set to $AppInstallerBaseUri"
}
if ($DispatchWorkflow) {
    Write-Output "Triggered release workflow for tag $ReleaseTag"
}