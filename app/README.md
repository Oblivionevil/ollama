# Ollama Desktop for Windows

This directory contains the Windows desktop application and its packaging assets.

The app is now Windows-only and Copilot-only. It no longer depends on a bundled `ollama.exe`, local model runtime binaries, or macOS build paths.

## Download

Download the latest `OllamaSetup.exe` from the repository release assets.

## Development

### Run the desktop app

```powershell
go run ./app/cmd/app
```

### Develop the UI with hot reload

Install the frontend dependencies:

```powershell
Set-Location app/ui/app
npm install
```

Start the Vite development server:

```powershell
npm run dev
```

In a second terminal, run the app in development mode:

```powershell
$env:OLLAMA_DEBUG = "1"
go run ./app/cmd/app -dev
```

The `-dev` flag keeps the desktop backend on `http://127.0.0.1:3001` and loads the frontend from the Vite dev server.

## Build

### Desktop app

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\build_windows.ps1 app
```

The staged desktop package is written to `dist/windows-<arch>`.
The build script auto-detects the Visual Studio LLVM toolchain when present and copies the required VC++ runtime DLLs into the package.

### Signed installer and App Installer packages

Requirements:

- Windows SDK with `MakeAppx.exe`
- signing configured through `KEY_CONTAINER` and `ollama_inc.crt`, or through `SIGN_PFX` and `SIGN_PFX_PASSWORD`
- optional `APPINSTALLER_BASE_URI` for hosted `.appinstaller` manifests

```powershell
$env:KEY_CONTAINER = "your-signing-key-container"
$env:APPINSTALLER_BASE_URI = "https://example.com/downloads/ollama"
powershell -ExecutionPolicy Bypass -File .\scripts\build_windows.ps1 app sign installer appinstaller
```

The MSIX/App Installer path is per-user, like the desktop installer, and packages only the desktop app payload.

For local test validation of the MSIX/App Installer path without release signing:

```powershell
$env:OLLAMA_LOCAL_TEST_SIGNING = "1"
powershell -ExecutionPolicy Bypass -File .\scripts\build_windows.ps1 appinstaller
```

This creates a self-signed test certificate under `dist/signing/` and signs the generated MSIX with it.

For GitHub release packaging, `.github/workflows/release.yaml` accepts either the existing KMS signing configuration or a PFX provided through `WINDOWS_SIGNING_PFX_BASE64` and `WINDOWS_SIGNING_PFX_PASSWORD`. The workflow uses `APPINSTALLER_BASE_URI` when configured, and otherwise points the generated `.appinstaller` manifest at the GitHub release asset URL for the current tag.

You can configure the GitHub `release` environment from a workstation with GitHub CLI access by running `scripts/configure_release_environment.ps1`. The script sets the correct environment variables and secrets for either KMS or PFX signing, removes the unused signing mode, and can optionally dispatch the release workflow for a tag.
