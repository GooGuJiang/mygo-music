param([string]$ArchiveDirectory, [string]$ModelFile, [string]$BeatArchiveDirectory)

$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false
$destination = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../resources/windows-amd64/demucs'))
$release = 'https://github.com/0xShug0/audio.cpp/releases/download/v0.9.5'
$modelPath = '/audio-cpp/audio.cpp-gguf/resolve/33120a4cfdf0d56786db934a03a093d4675de938/HTDemucs-GGUF/htdemucs-f16.gguf'
$modelHash = 'F8C54BA35DF95AAFEA7881EAC214B58EC18A73CBCB6FF5A90F4B762631A286B6'
$playbackHash = '4D285D7A9E98067ECC15DCAAF1646E452FC9574AE4668298AC091CB6D8B5C9EE'

function Confirm-Hash($Path, $Hash) {
    if ((Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash -ne $Hash) {
        throw "SHA-256 verification failed for $Path"
    }
}

function Set-PlaybackSegment($Path) {
    # HTDemucs advances by 75% of a segment. A 3 s segment covers each
    # 2.25 s MyGo block in one inference and leaves padding at both ends.
    # Edit only the two metadata values in the verified GGUF header.
    $file = [IO.File]::Open($Path, 'Open', 'ReadWrite')
    try {
        $header = [byte[]]::new(65536)
        if ($file.Read($header, 0, $header.Length) -ne $header.Length) { throw 'Incomplete GGUF header.' }
        $text = [Text.Encoding]::ASCII.GetString($header)
        $pattern = '"segment": 7.8'
        $offset = 0
        for ($i = 0; $i -lt 2; $i++) {
            $offset = $text.IndexOf($pattern, $offset, [StringComparison]::Ordinal)
            if ($offset -lt 0) { throw 'Missing HTDemucs segment metadata.' }
            $offset += $pattern.Length - 3
            $file.Position = $offset
            $file.Write([Text.Encoding]::ASCII.GetBytes('3.0'), 0, 3)
        }
    } finally { $file.Dispose() }
}

function Get-Archive($Name, $Hash) {
    if ($ArchiveDirectory) {
        $archive = Join-Path $ArchiveDirectory $Name
    } else {
        $archive = Join-Path $work $Name
        curl.exe -fsSL --retry 2 --connect-timeout 15 -o $archive "$release/$Name"
        if ($LASTEXITCODE) { throw "Could not download $Name" }
    }
    Confirm-Hash $archive $Hash
    return $archive
}

function Stage-Backend($Device, $Archive) {
    $package = Join-Path $work $Device
    $target = Join-Path $staged $Device
    New-Item -ItemType Directory -Path $package, $target | Out-Null
    tar.exe -xf $Archive -C $package
    if ($LASTEXITCODE) { throw "Could not extract $Archive" }
    Get-ChildItem -LiteralPath (Join-Path $package 'libs') -Filter '*.dll' -File | Copy-Item -Destination $target
    Copy-Item -LiteralPath (Join-Path $package 'LICENSE') -Destination (Join-Path $target 'LICENSE.audio.cpp')
}

$runtime = Join-Path $destination 'native-v1'
$work = Join-Path $destination ('.runtime/package-' + [guid]::NewGuid())
$staged = Join-Path $work 'native-v1'
$backup = "$work-previous"
New-Item -ItemType Directory -Path $staged -Force | Out-Null
try {
    if (!$ModelFile) {
        $ModelFile = Join-Path $work 'htdemucs.download'
        foreach ($hostName in 'huggingface.co', 'hf-mirror.com') {
            curl.exe -fsSL --connect-timeout 8 --max-time 180 -o $ModelFile "https://$hostName${modelPath}"
            if (!$LASTEXITCODE) { break }
        }
        if ($LASTEXITCODE) { throw 'Could not download the HTDemucs model.' }
    }
    $model = Join-Path $staged 'htdemucs-f16.gguf'
    Copy-Item -LiteralPath $ModelFile -Destination $model
    Confirm-Hash $model $modelHash
    Set-PlaybackSegment $model
    Confirm-Hash $model $playbackHash

    $portable = Get-Archive 'audio-v0.9.5-lib-windows-x64-cpu-portable.zip' '0CCA326B8A58A81D4646531D7086C34C8E2E829246EF6015602156588D13857D'
    $cpu = Get-Archive 'audio-v0.9.5-lib-windows-x64-cpu.zip' 'FEF51C31705F524F51D47C858F93C7B184BD789EECFA916D5E689F139426171B'
    $cuda = Get-Archive 'audio-v0.9.5-lib-windows-x64-cuda13.3.zip' '7B7735581858082523AE216DE12954F7AA4DD55061A3E5006769C87BC4A65CEB'
    $cudart = Get-Archive 'audio-v0.9.5-cudart-windows-x64-cuda13.3.zip' '8614040110CC5852B1DD2192E80344295500F4FA7F3199A51C737C2C7549C3DC'
    Stage-Backend 'cpu-portable' $portable
    Stage-Backend 'cpu' $cpu
    Stage-Backend 'cuda' $cuda
    tar.exe -xf $cudart -C (Join-Path $staged 'cuda') cudart64_13.dll cublas64_13.dll cublasLt64_13.dll cufft64_12.dll
    if ($LASTEXITCODE) { throw 'Could not extract the CUDA runtime DLLs.' }
    & (Join-Path $PSScriptRoot 'stage-beat.ps1') -Destination (Join-Path $staged 'cuda') -Work $work -ArchiveDirectory $BeatArchiveDirectory
    Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'THIRD-PARTY.txt') -Destination $staged
    $ortSDK = if ($BeatArchiveDirectory) { Join-Path $BeatArchiveDirectory 'ort-sdk.nupkg' } else { '' }
    & (Join-Path $PSScriptRoot 'build.ps1') -Archive $portable -OrtSDK $ortSDK

    if (Test-Path -LiteralPath $runtime) {
        if ((Resolve-Path -LiteralPath $runtime).Path -ne $runtime) { throw "Unexpected runtime directory $runtime" }
        [IO.Directory]::Move($runtime, $backup)
    }
    try {
        [IO.Directory]::Move($staged, $runtime)
    } catch {
        if (Test-Path -LiteralPath $backup) { [IO.Directory]::Move($backup, $runtime) }
        throw
    }
    if ((Resolve-Path -LiteralPath $backup -ErrorAction SilentlyContinue).Path -eq $backup) {
        Remove-Item -LiteralPath $backup -Recurse -Force
    }
    Write-Output "Prepared offline Demucs resources in $destination"
} finally {
    if ((Resolve-Path -LiteralPath $work).Path -eq $work) {
        Remove-Item -LiteralPath $work -Recurse -Force
    }
}
