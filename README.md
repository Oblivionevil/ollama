# Ollama Desktop for Windows

This repository now contains only the Windows desktop application.

The retained product is a GitHub Copilot-powered desktop app built from the code under `app/` plus the small shared Go packages it depends on. Legacy local-model runtime code, CLI/server entrypoints, Docker paths, and macOS/Linux build targets were intentionally removed.

## What is in this repository

- Windows desktop app entrypoint: `go run ./app/cmd/app`
- Desktop backend and UI: `app/ui`
- Windows packaging: `scripts/build_windows.ps1`, `app/ollama.iss`, `app/msix`
- Windows release workflow: `.github/workflows/release.yaml`

## Development

### Prerequisites

- Windows
- Go
- Node.js and npm
- Native Windows toolchain required by `app/webview`
- Optional: `zig` for CGO builds when no C compiler is configured

### Run the UI in development mode

```powershell
Set-Location app/ui/app
npm install
npm run dev
```

In a second terminal:

```powershell
$env:OLLAMA_DEBUG = "1"
go run ./app/cmd/app -dev
```

### Build the production UI

```powershell
Set-Location app/ui/app
npm install
npm run build
```

### Build the desktop app

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\build_windows.ps1 app
```

The build script stages the packaged desktop app under `dist/windows-<arch>`.
On Windows it now prefers the Visual Studio LLVM toolchain automatically when available and bundles the VC++ runtime DLLs into the staged package.

## Packaging

### Build installer artifacts

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\build_windows.ps1 app sign installer appinstaller
```

This emits the Windows desktop binaries plus signed installer artifacts when signing is configured.

For local MSIX and `.appinstaller` validation without the release signing setup:

```powershell
$env:OLLAMA_LOCAL_TEST_SIGNING = "1"
powershell -ExecutionPolicy Bypass -File .\scripts\build_windows.ps1 appinstaller
```

This generates a local self-signed test certificate in `dist/signing/` and uses a placeholder `APPINSTALLER_BASE_URI` unless one is already set.

### Signing inputs

- `KEY_CONTAINER`
- `ollama_inc.crt`
- Optional: `SIGN_PFX` and `SIGN_PFX_PASSWORD` for PFX-based signing
- Optional: `OLLAMA_LOCAL_TEST_SIGNING=1` to generate a local self-signed test certificate for MSIX/AppInstaller validation
- Optional: `APPINSTALLER_BASE_URI` for `.appinstaller` publishing

### Release workflow

The GitHub release workflow in `.github/workflows/release.yaml` now supports two real signing modes for Windows release packaging:

- Google Cloud KMS: `GOOGLE_SIGNING_CREDENTIALS` secret plus `KEY_CONTAINER` and `OLLAMA_CERT` repository variables
- PFX signing: `WINDOWS_SIGNING_PFX_BASE64` secret plus `WINDOWS_SIGNING_PFX_PASSWORD` secret

The workflow publishes `.appinstaller` files against `APPINSTALLER_BASE_URI` when that repository variable is set. If it is not set, the workflow defaults to the current GitHub release asset URL for the active tag.

## Validation

Useful checks for the reduced repository:

```powershell
go list ./...
go test ./api ./app/ui ./auth ./envconfig ./format ./internal/cloud ./internal/orderedmap ./types/model ./version
```
