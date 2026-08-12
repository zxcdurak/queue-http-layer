# Тесты с детектором гонок в Linux-контейнере — для машин без C-компилятора.

$backend = (Join-Path $PSScriptRoot 'backend') -replace '\\', '/'

docker run --rm `
    -v "${backend}:/src" `
    -v "avito-queue-gomod:/go/pkg/mod" `
    -w /src `
    golang:1.25 `
    go test ./... -race -count=1
