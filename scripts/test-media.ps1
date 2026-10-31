param(
    [string]$FFmpegBin,
    [string]$GoExecutable = 'go'
)

$ErrorActionPreference = 'Stop'
$previousPath = $env:PATH
Push-Location (Split-Path $PSScriptRoot -Parent)
try {
    if ($FFmpegBin) {
        $resolvedBin = (Resolve-Path -LiteralPath $FFmpegBin).Path
        $env:PATH = $resolvedBin + [IO.Path]::PathSeparator + $env:PATH
    }
    foreach ($tool in @('ffmpeg', 'ffprobe')) {
        $command = Get-Command $tool -CommandType Application -ErrorAction Stop
        $versionOutput = & $command.Source -version
        if ($LASTEXITCODE -ne 0) { throw "$tool cannot run" }
        $versionOutput | Select-Object -First 1
    }
    & $GoExecutable test -tags dev,lite -count=1 -timeout 5m ./internal/handlers ./internal/services ./internal/testutil/executable
    if ($LASTEXITCODE -ne 0) { throw 'Media tests failed; see individual test diagnostics above.' }
}
finally {
    $env:PATH = $previousPath
    Pop-Location
}
