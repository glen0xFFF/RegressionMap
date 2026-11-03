# Production build: web/dist + a single Go binary that serves the API and UI.
#
#   powershell -ExecutionPolicy Bypass -File scripts\build.ps1
$ErrorActionPreference = 'Stop'

$Repo = Split-Path -Parent $PSScriptRoot
$Backend = Join-Path $Repo 'backend'
$Web = Join-Path $Repo 'web'
$Bin = Join-Path $Backend 'bin\regressionmap.exe'

Write-Host '== web: npm ci && npm run build'
Push-Location $Web
try {
    npm ci
    if ($LASTEXITCODE -ne 0) { throw "npm ci failed ($LASTEXITCODE)" }
    npm run build
    if ($LASTEXITCODE -ne 0) { throw "npm run build failed ($LASTEXITCODE)" }
} finally { Pop-Location }

Write-Host '== backend: go build'
Push-Location $Backend
try {
    go build -trimpath -o $Bin ./cmd/server
    if ($LASTEXITCODE -ne 0) { throw "go build failed ($LASTEXITCODE)" }
} finally { Pop-Location }

$Dist = Join-Path $Web 'dist'
$Demo = Join-Path $Repo 'demo-workspace'
Write-Host @"

Built:
  $Dist
  $Bin

Run it (UI and API on one port, http://127.0.0.1:8787):
  `$env:WORKSPACE_ROOT = '$Demo'
  `$env:WEB_DIR = '$Dist'
  & '$Bin'

Set `$env:MODEL_API_KEY or `$env:MODEL_API_KEY_FILE to enable the model step.
Optional: LOG_FORMAT=json, ADDR=127.0.0.1:PORT, DATA_DIR=...
"@
