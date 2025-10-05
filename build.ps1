Param(
  [string[]]$Targets = @("windows/amd64","linux/amd64","windows/arm64","linux/arm64")
)

$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot
New-Item -ItemType Directory -Force -Path dist | Out-Null

$app = "operetta-server"

foreach ($t in $Targets) {
  $parts = $t.Split("/")
  if ($parts.Count -ne 2) { throw "Invalid target '$t'. Use os/arch, e.g. windows/amd64" }
  $goos = $parts[0]
  $goarch = $parts[1]
  $env:CGO_ENABLED = "0"
  $env:GOOS = $goos
  $env:GOARCH = $goarch
  $ext = if ($goos -eq "windows") { ".exe" } else { "" }
  $out = Join-Path dist "$app-$goos-$goarch$ext"
  Write-Host "Building $out"
  go build -trimpath -ldflags "-a -s -w" -o $out ./cmd/operetta
}

Write-Host "Binaries are in .\dist"

