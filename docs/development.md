# Development

This repository contains only the Windows desktop app.

## Prerequisites

- [Go](https://go.dev/doc/install)
- [Node.js and npm](https://nodejs.org/)
- A working C/C++ toolchain for CGO builds used by `app/webview`

Optional dependencies for local packaging or GPU-specific work:

- [Visual Studio 2022](https://visualstudio.microsoft.com/downloads/) with the Native Desktop Workload
- [Inno Setup](https://jrsoftware.org/isdl.php) for `OllamaSetup.exe`
- [Windows SDK](https://developer.microsoft.com/windows/downloads/windows-sdk/) for MSIX/App Installer packaging
- ROCm, CUDA, cuDNN, or Vulkan SDK if you are validating those acceleration paths locally

If no working C compiler is configured, Zig is a practical fallback for Go builds:

```powershell
$env:CC = "zig cc"
$env:CXX = "zig c++"
```

## Run the UI in development mode

In one terminal:

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

The `-dev` flag keeps the backend on `http://127.0.0.1:3001` and loads the frontend from the Vite dev server.

## Build the desktop app

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\build_windows.ps1 app
```

This stages the packaged desktop app under `dist/windows-<arch>`.

## Build installer artifacts

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\build_windows.ps1 app sign installer appinstaller
```

For local MSIX and `.appinstaller` validation without release signing:

```powershell
$env:OLLAMA_LOCAL_TEST_SIGNING = "1"
powershell -ExecutionPolicy Bypass -File .\scripts\build_windows.ps1 appinstaller
```

## Running tests

Useful checks for the reduced repository:

```powershell
go list ./...
go test ./api ./app/ui ./auth ./envconfig ./format ./internal/cloud ./internal/orderedmap ./types/model ./version
```
