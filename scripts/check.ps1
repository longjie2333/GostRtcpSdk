param(
    [string]$GostBinary = $env:GOST_V3_BINARY,
    [switch]$UnitOnly
)
$ErrorActionPreference = 'Stop'
$taskRoot = Split-Path -Parent $PSScriptRoot
$taskPreviousBinary = $env:GOST_V3_BINARY
function Invoke-GoCheck([string[]]$GoArguments) {
    & go @GoArguments
    if ($LASTEXITCODE -ne 0) { throw "go $($GoArguments -join ' ') failed" }
}
Push-Location $taskRoot
try {
    if ($UnitOnly) {
        Remove-Item Env:GOST_V3_BINARY -ErrorAction SilentlyContinue
        Write-Warning 'Unit-only mode excludes official gost interoperability; not a complete merge/release gate.'
    } else {
        if (-not $GostBinary) { throw 'Provide -GostBinary <official gost v3.3.0 binary>, or explicitly select -UnitOnly.' }
        $env:GOST_V3_BINARY = (Resolve-Path -LiteralPath $GostBinary).Path
        $taskVersion = (& $env:GOST_V3_BINARY -V 2>&1 | Out-String).Trim()
        if ($LASTEXITCODE -ne 0 -or $taskVersion -notmatch '^gost v3\.3\.0\b') { throw "Expected official gost v3.3.0, got: $taskVersion" }
    }
    $taskUnformatted = & gofmt -l .
    if ($LASTEXITCODE -ne 0 -or $taskUnformatted) { throw "Run gofmt before checking: $taskUnformatted" }
    $taskBefore = @((Get-FileHash go.mod).Hash, (Get-FileHash go.sum).Hash)
    Invoke-GoCheck @('mod','tidy')
    $taskAfter = @((Get-FileHash go.mod).Hash, (Get-FileHash go.sum).Hash)
    if (Compare-Object $taskBefore $taskAfter) { throw 'go mod tidy changed go.mod/go.sum; review and include these changes.' }
    Invoke-GoCheck @('build','./...')
    Invoke-GoCheck @('vet','./...')
    New-Item -ItemType Directory -Force .tools | Out-Null
    & go test -json -race -count=1 -timeout=90s ./... | Tee-Object -FilePath .tools/checks.jsonl
    if ($LASTEXITCODE -ne 0) { throw 'Go tests failed' }
    $taskSkipped = Get-Content .tools/checks.jsonl | ForEach-Object { $_ | ConvertFrom-Json } | Where-Object { $_.Action -eq 'skip' -and $_.Test }
    if (-not $UnitOnly -and $taskSkipped) { throw 'Complete checks must not skip tests' }
    Write-Output $(if ($UnitOnly) { 'PASS: unit checks (official integration excluded)' } else { 'PASS: complete SDK checks, no skipped tests' })
} finally {
    $env:GOST_V3_BINARY = $taskPreviousBinary
    Pop-Location
}
