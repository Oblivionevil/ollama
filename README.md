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

## Packaging

### Build installer artifacts

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\build_windows.ps1 app sign installer appinstaller
```

This emits the Windows desktop binaries plus signed installer artifacts when signing is configured.

### Signing inputs

- `KEY_CONTAINER`
- `ollama_inc.crt`
- Optional: `APPINSTALLER_BASE_URI` for `.appinstaller` publishing

## Validation

Useful checks for the reduced repository:

```powershell
go list ./...
go test ./api ./app/ui ./auth ./envconfig ./format ./internal/cloud ./internal/orderedmap ./types/model ./version
```
