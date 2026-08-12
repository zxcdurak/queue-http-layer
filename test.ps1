# Тесты бэкенда. Без gcc детектор гонок недоступен — тогда ./test-race.ps1.

Push-Location "$PSScriptRoot\backend"
try {
    if (Get-Command gcc -ErrorAction SilentlyContinue) {
        $env:CGO_ENABLED = "1"
        go test ./... -race -count=1
    } else {
        Write-Host "gcc не найден: прогоняем без -race." -ForegroundColor Yellow
        Write-Host "Гонки проверяются через ./test-race.ps1" -ForegroundColor Yellow
        go test ./... -count=1
    }
} finally {
    Pop-Location
}
