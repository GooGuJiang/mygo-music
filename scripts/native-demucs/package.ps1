param(
    [string]$BuildDirectory = (Join-Path $PSScriptRoot '../../build/windows-amd64'),
    [switch]$CudaOnly
)

$ErrorActionPreference = 'Stop'
$bundle = (Resolve-Path -LiteralPath $BuildDirectory).Path
$source = $bundle
$required = @('demucs/native-v1/cuda/ggml-cuda.dll', 'demucs/native-v1/cuda/beat_this.onnx', 'demucs/native-v1/cuda/logmel.onnx', 'demucs/native-v1/cuda/onnxruntime_providers_cuda.dll', 'demucs/native-v1/cuda/cudnn64_9.dll')
if ($CudaOnly) {
    $source = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot '../../resources/windows-amd64')).Path
    $files = @('demucs/native-v1/cuda')
    $name = 'mygo-music-windows-amd64-cuda.zip'
    $manifest = Get-Content -LiteralPath (Join-Path $PSScriptRoot '../../internal/player/cuda-runtime-v1.txt')
    $cuda = Join-Path $source $files[0]
    if (@(Get-ChildItem -LiteralPath $cuda -Force).Count -ne $manifest.Count) {
        throw 'The CUDA runtime file list does not match the importer.'
    }
    foreach ($line in $manifest) {
        $hash, $size, $file = $line -split ' ', 3
        $asset = Get-Item -LiteralPath (Join-Path $cuda $file)
        if ($asset.Length -ne [long]$size -or (Get-FileHash -LiteralPath $asset.FullName -Algorithm SHA256).Hash -ne $hash) {
            throw "CUDA runtime file failed verification: $file"
        }
    }
} else {
    $required += @('demucs/mygo-demucs.exe', 'demucs/native-v1/htdemucs-f16.gguf', 'demucs/native-v1/cpu/audiocpp.dll', 'demucs/native-v1/cpu-portable/audiocpp.dll')
    $files = @(Get-Content -LiteralPath (Join-Path $bundle '.mygo-files')) + '.mygo-files'
    $name = 'mygo-music-windows-amd64-offline.zip'
}
foreach ($file in $required) {
    if (!(Test-Path -LiteralPath (Join-Path $source $file) -PathType Leaf)) {
        throw "Missing $file. Prepare the Demucs resources and rebuild."
    }
}
$zip = Join-Path (Split-Path $bundle) $name
$temporary = "$zip.$([guid]::NewGuid()).zip"
try {
    tar.exe -a -cf $temporary -C $source @files
    if ($LASTEXITCODE) { throw 'Could not create the Windows ZIP.' }
    Move-Item -LiteralPath $temporary -Destination $zip -Force
    Write-Output $zip
} finally {
    if (Test-Path -LiteralPath $temporary) { Remove-Item -LiteralPath $temporary -Force }
}
