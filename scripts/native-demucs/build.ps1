param([string]$Archive, [string]$OrtSDK)

$ErrorActionPreference = 'Stop'
$asset = 'audio-v0.9.5-lib-windows-x64-cpu-portable.zip'
$sha256 = '0CCA326B8A58A81D4646531D7086C34C8E2E829246EF6015602156588D13857D'
$vswhere = Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio/Installer/vswhere.exe'
$vs = & $vswhere -latest -products '*' -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath
if (!$vs) { throw 'MSVC x64 Build Tools are required to build the Demucs worker.' }
$vars = Join-Path $vs 'VC/Auxiliary/Build/vcvars64.bat'
$destination = Join-Path $PSScriptRoot '../../resources/windows-amd64/demucs'
$work = Join-Path ([IO.Path]::GetTempPath()) ('mygo-demucs-build-' + [guid]::NewGuid())
New-Item -ItemType Directory -Path $work | Out-Null
try {
    if (!$Archive) {
        $Archive = Join-Path $work $asset
        curl.exe -fsSL --retry 2 --connect-timeout 15 -o $Archive "https://github.com/0xShug0/audio.cpp/releases/download/v0.9.5/$asset"
        if ($LASTEXITCODE) { throw 'Could not download the audio.cpp SDK.' }
    }
    if ((Get-FileHash -LiteralPath $Archive -Algorithm SHA256).Hash -ne $sha256) {
        throw 'The audio.cpp SDK failed SHA-256 verification.'
    }
    tar.exe -xf $Archive -C $work include/audiocpp.h libs/audiocpp.lib
    if ($LASTEXITCODE) { throw 'Could not extract the audio.cpp SDK.' }
    if (!$OrtSDK) {
        $OrtSDK = Join-Path $work 'ort-sdk.nupkg'
        curl.exe -fsSL --retry 2 --connect-timeout 15 -o $OrtSDK 'https://api.nuget.org/v3-flatcontainer/microsoft.ml.onnxruntime/1.27.0/microsoft.ml.onnxruntime.1.27.0.nupkg'
        if ($LASTEXITCODE) { throw 'Could not download the ONNX Runtime SDK.' }
    }
    if ((Get-FileHash -LiteralPath $OrtSDK -Algorithm SHA256).Hash -ne 'C725E8CEFDFE4DE08BEFB9F79B3F218C162439CDCDB97CBBB5192888D58EA669') {
        throw 'The ONNX Runtime SDK failed SHA-256 verification.'
    }
    tar.exe -xf $OrtSDK -C $work build/native/include
    if ($LASTEXITCODE) { throw 'Could not extract the ONNX Runtime headers.' }
    Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'worker.cpp'), (Join-Path $PSScriptRoot 'beat.h'), (Join-Path $PSScriptRoot 'worker.manifest') -Destination $work
    @"
@echo off
call "$vars" >NUL
if errorlevel 1 exit /b %errorlevel%
cl /nologo /std:c++20 /utf-8 /EHsc /O2 /MT /W4 /DAUDIOCPP_USE_DLL /I include /I build/native/include worker.cpp /Fe:mygo-demucs.exe /link libs/audiocpp.lib delayimp.lib /DELAYLOAD:audiocpp.dll /MANIFEST:EMBED /MANIFESTINPUT:worker.manifest
"@ | Set-Content -LiteralPath (Join-Path $work 'build.cmd') -Encoding ascii
    Push-Location $work
    try {
        cmd.exe /d /c build.cmd
        if ($LASTEXITCODE) { throw 'Could not build the Demucs worker.' }
    } finally { Pop-Location }
    New-Item -ItemType Directory -Path $destination -Force | Out-Null
    Copy-Item -LiteralPath (Join-Path $work 'mygo-demucs.exe') -Destination $destination
} finally {
    if ((Resolve-Path -LiteralPath $work).Path -eq $work) {
        Remove-Item -LiteralPath $work -Recurse -Force
    }
}
