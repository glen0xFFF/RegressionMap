# Development mode: Go API on 127.0.0.1:8787 (go run) + Vite on :5173.
#
#   powershell -ExecutionPolicy Bypass -File scripts\dev.ps1
#
# WORKSPACE_ROOT defaults to .\demo-workspace; the demo fixture is created
# there if it is missing. The model key is read by the backend only from
# MODEL_API_KEY or MODEL_API_KEY_FILE in YOUR environment; this script never
# reads, prints or stores it. Without a key the model step shows "disabled".
$ErrorActionPreference = 'Stop'

$Repo = Split-Path -Parent $PSScriptRoot
$Backend = Join-Path $Repo 'backend'
$Web = Join-Path $Repo 'web'

if (-not $env:WORKSPACE_ROOT) {
    $env:WORKSPACE_ROOT = Join-Path $Repo 'demo-workspace'
    if (-not (Test-Path (Join-Path $env:WORKSPACE_ROOT 'pagination-demo'))) {
        Write-Host "creating demo fixture in $($env:WORKSPACE_ROOT)"
        Push-Location $Backend
        try {
            go run ./cmd/mkfixture $env:WORKSPACE_ROOT
            if ($LASTEXITCODE -ne 0) { throw "mkfixture failed ($LASTEXITCODE)" }
        } finally { Pop-Location }
    }
}

if ($env:MODEL_API_KEY -or $env:MODEL_API_KEY_FILE) {
    Write-Host 'model key: provided via environment'
} else {
    Write-Host 'model key: not set ($env:MODEL_API_KEY or $env:MODEL_API_KEY_FILE enables hypotheses)'
}

if (-not (Test-Path (Join-Path $Web 'node_modules'))) {
    Push-Location $Web
    try {
        npm ci
        if ($LASTEXITCODE -ne 0) { throw "npm ci failed ($LASTEXITCODE)" }
    } finally { Pop-Location }
}

# The child inherits this process's environment (WORKSPACE_ROOT, key vars).
$backendProc = Start-Process -FilePath 'go' -ArgumentList 'run', './cmd/server' `
    -WorkingDirectory $Backend -NoNewWindow -PassThru

Write-Host "API  http://127.0.0.1:8787   workspace=$($env:WORKSPACE_ROOT)"
Write-Host 'UI   http://localhost:5173'
Push-Location $Web
try {
    npm run dev
} finally {
    Pop-Location
    # `go run` starts the server as a child process: stop the whole tree.
    if (-not $backendProc.HasExited) {
        taskkill /PID $backendProc.Id /T /F | Out-Null
    }
}
