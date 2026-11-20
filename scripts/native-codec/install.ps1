param(
    [Parameter(Mandatory)][string]$AafApo,
    [string]$Archive,
    [string]$Proxy,
    [string]$RendererArchive,
    [string]$ObjectDecoderArchive
)

$ErrorActionPreference = 'Stop'
$asset = 'ffmpeg-n8.1.3-39-g660b5aa7db-win64-lgpl-shared-8.1'
$url = "https://github.com/FFmpeg-Builds/FFmpeg-Builds/releases/download/autobuild-2026-11-20-12-40/$asset.zip"
$sha = 'BF6109642E7DA71905CADCB0B8DBAB8C2A0A72F8A1A4C88C2DBCDA771AA358CD'
$work = Join-Path ([IO.Path]::GetTempPath()) ('mygo-codec-build-' + [guid]::NewGuid())
$destination = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../resources/windows-amd64/native-codecs'))
if (!(Test-Path -LiteralPath $aafApo -PathType Leaf)) { throw "AAF Dolby APO not found at $aafApo" }

function Expand-VerifiedArchive($Path, $Url, $Hash, $Directory) {
    if (!$Path) {
        $Path = Join-Path $work ([IO.Path]::GetFileName($Url))
        $request = @{Uri=$Url; OutFile=$Path}
        if ($Proxy) { $request.Proxy = $Proxy }
        Invoke-WebRequest @request
    }
    if ((Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash -ne $Hash) {
        throw "SHA-256 verification failed for $Path"
    }
    Expand-Archive -LiteralPath $Path -DestinationPath $Directory
}

New-Item -Type Directory -Path $work | Out-Null
try {
    Expand-VerifiedArchive $Archive $url $sha $work
    $sdk = Join-Path $work $asset
    $renderer = Join-Path $work 'renderer'
    $objectDecoder = Join-Path $work 'object-decoder'
    Expand-VerifiedArchive $RendererArchive 'https://github.com/mgth/Omniphony/releases/download/v0.7.0/liborender-v0.7.0-windows-x86_64.zip' 'F28D2DEB8A4A9B6F8C84DDE66AE96A770AAA222E1ABF49313DA8B0180C60B928' $renderer
    Expand-VerifiedArchive $ObjectDecoderArchive 'https://github.com/harletty/harletty-bridge/releases/download/v0.9.0/harletty-bridge-v0.9.0-windows-x86_64.zip' '97D01AAA324BFD24D24608764A4FA1DC3ECB5725EC1C755093E33DE7E5030291' $objectDecoder
    $vswhere = Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio/Installer/vswhere.exe'
    $vs = & $vswhere -latest -products '*' -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath
    if (!$vs) { throw 'MSVC x64 Build Tools are required to build the native codec bridge.' }
    $vars = Join-Path $vs 'VC/Auxiliary/Build/vcvars64.bat'
    Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'codec.c'), (Join-Path $PSScriptRoot 'atmos.c') -Destination $work
    @"
@echo off
call "$vars" >NUL
if errorlevel 1 exit /b %errorlevel%
cl /nologo /TC /std:c17 /utf-8 /O2 /MD /LD /I "$sdk\include" /I "$renderer" codec.c atmos.c /link /LIBPATH:"$sdk\lib" avformat.lib avcodec.lib avutil.lib swresample.lib /OUT:mygo-codec.dll
"@ | Set-Content -LiteralPath (Join-Path $work 'build.cmd') -Encoding ascii
    Push-Location $work
    try {
        & cmd.exe /d /c build.cmd
        if ($LASTEXITCODE -ne 0) { throw 'The native codec bridge failed to build.' }
    } finally { Pop-Location }

    New-Item -Type Directory -Force -Path $destination | Out-Null
    foreach ($dll in 'avformat-62.dll', 'avcodec-62.dll', 'avutil-60.dll', 'swresample-6.dll') {
        Copy-Item -LiteralPath (Join-Path $sdk "bin/$dll") -Destination $destination
    }
    Get-ChildItem -LiteralPath $sdk -File | Where-Object Name -Match 'COPYING|LICENSE|README' | Copy-Item -Destination $destination
    Copy-Item -LiteralPath (Join-Path $renderer 'orender.dll'), (Join-Path $objectDecoder 'harletty_dolby_bridge.dll'), (Join-Path $PSScriptRoot 'atmos-7.1.2.yaml'), $aafApo -Destination $destination
    "render:`n  osc: false" | Set-Content -LiteralPath (Join-Path $destination 'atmos-render.yaml') -Encoding ascii
    Copy-Item -LiteralPath (Join-Path $work 'mygo-codec.dll') -Destination $destination
    Write-Output "Installed native codec DLLs in $destination"
} finally {
    $resolved = (Resolve-Path -LiteralPath $work).Path
    if ([string]::Equals($resolved, $work, [StringComparison]::OrdinalIgnoreCase)) {
        Remove-Item -LiteralPath $resolved -Recurse -Force
    }
}
