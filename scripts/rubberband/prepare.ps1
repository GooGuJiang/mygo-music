$ErrorActionPreference = 'Stop'
$version = '4.0.0'
$archiveName = "rubberband-$version.tar.bz2"
$hash = 'AF050313EE63BC18B35B2E064E5DCE05B276AAF6D1AA2B8A82CED1FE2F8028E9'
$work = Join-Path ([IO.Path]::GetTempPath()) ('mygo-rubberband-build-' + [guid]::NewGuid())
$destination = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../resources/windows-amd64/rubberband'))
$vswhere = Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio/Installer/vswhere.exe'
$vs = & $vswhere -latest -products '*' -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath
if (!$vs) { throw 'MSVC x64 Build Tools are required to build Rubber Band.' }
$vars = Join-Path $vs 'VC/Auxiliary/Build/vcvars64.bat'

New-Item -ItemType Directory -Path $work | Out-Null
try {
    $archive = Join-Path $work $archiveName
    curl.exe -fsSL --retry 2 --connect-timeout 15 -o $archive "https://breakfastquay.com/files/releases/$archiveName"
    if ($LASTEXITCODE -or (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash -ne $hash) {
        throw 'Could not verify the Rubber Band source archive.'
    }
    tar.exe -xf $archive -C $work
    if ($LASTEXITCODE) { throw 'Could not extract Rubber Band.' }
    $source = "rubberband-$version"
    Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'stretch.cpp') -Destination $work
    @"
@echo off
call "$vars" >NUL
if errorlevel 1 exit /b %errorlevel%
cl /nologo /std:c++17 /EHsc /O2 /MT /LD /DNOMINMAX /DRUBBERBAND_STATIC /I "$source" stretch.cpp "$source\single\RubberBandSingle.cpp" /link /OUT:mygo-rubberband.dll
"@ | Set-Content -LiteralPath (Join-Path $work 'build.cmd') -Encoding ascii
    Push-Location $work
    try {
        & cmd.exe /d /c build.cmd
        if ($LASTEXITCODE) { throw 'Rubber Band failed to build.' }
    } finally { Pop-Location }

    New-Item -ItemType Directory -Path $destination -Force | Out-Null
    Copy-Item -LiteralPath (Join-Path $work 'mygo-rubberband.dll'), $archive, $PSCommandPath, (Join-Path $PSScriptRoot 'stretch.cpp') -Destination $destination
    Copy-Item -LiteralPath (Join-Path $work "$source/COPYING") -Destination (Join-Path $destination 'COPYING')
} finally {
    $resolved = (Resolve-Path -LiteralPath $work).Path
    if ([string]::Equals($resolved, $work, [StringComparison]::OrdinalIgnoreCase)) {
        Remove-Item -LiteralPath $resolved -Recurse -Force
    }
}
