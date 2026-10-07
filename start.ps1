$ErrorActionPreference = 'Stop'
Set-Location -LiteralPath $PSScriptRoot
if (Test-Path -LiteralPath (Join-Path $PSScriptRoot 'bin\atelier.exe')) {
    & (Join-Path $PSScriptRoot 'bin\atelier.exe')
} else {
    go run .
}
