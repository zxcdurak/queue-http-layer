# Линтер бэкенда. Правила и обоснования — в .golangci.yaml.

$linter = 'golangci-lint'
if (-not (Get-Command $linter -ErrorAction SilentlyContinue)) {
    # go install кладёт бинарь в GOPATH\bin, которого может не быть в PATH.
    $fallback = Join-Path $env:USERPROFILE 'go\bin\golangci-lint.exe'
    if (Test-Path $fallback) {
        $linter = $fallback
    } else {
        Write-Host "golangci-lint не найден. Установите:" -ForegroundColor Red
        Write-Host "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"
        exit 1
    }
}

Push-Location "$PSScriptRoot\backend"
try {
    & $linter run @args
} finally {
    Pop-Location
}
