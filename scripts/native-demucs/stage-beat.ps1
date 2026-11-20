param(
    [Parameter(Mandatory)][string]$Destination,
    [Parameter(Mandatory)][string]$Work,
    [string]$ArchiveDirectory
)

$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false

function BeatAsset($Name, $Hash, $Urls) {
    $path = if ($ArchiveDirectory) { Join-Path $ArchiveDirectory $Name } else { Join-Path $Work $Name }
    if (!$ArchiveDirectory) {
        foreach ($url in $Urls) {
            curl.exe -fsSL --retry 1 --connect-timeout 10 --max-time 180 -o $path $url
            if (!$LASTEXITCODE) { break }
        }
        if ($LASTEXITCODE) { throw "Could not download $Name." }
    }
    if ((Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash -ne $Hash) {
        throw "SHA-256 verification failed for $Name."
    }
    $path
}

function CopyZipEntry($Zip, $Entry, $Name) {
    $file = $Zip.GetEntry($Entry)
    if (!$file) { throw "Missing $Entry in native runtime." }
    [IO.Compression.ZipFileExtensions]::ExtractToFile($file, (Join-Path $Destination $Name), $true)
}

$ort = BeatAsset 'ort-gpu.nupkg' '20DA5E7A22409E77BB8F7796920FE0931DA16741A61037F0138EC812072E71F8' @(
    'https://api.nuget.org/v3-flatcontainer/microsoft.ml.onnxruntime.gpu.windows/1.27.0/microsoft.ml.onnxruntime.gpu.windows.1.27.0.nupkg'
)
$dnn = BeatAsset 'nvidia_cudnn_cu13-9.28.1.4-py3-none-win_amd64.whl' '73D874C6BD8A1C75CA040228F77C4E163EFE9BAB3B65BD85914E2B7334AC68EE' @(
    'https://files.pythonhosted.org/packages/48/9c/26e6bdd31c97c389aa565b49c916be0dec17dd61dad6cd33fee286d1b946/nvidia_cudnn_cu13-9.28.1.4-py3-none-win_amd64.whl'
)
$export = 'tuneperfect/beat-this-onnx/resolve/93b53ffd41d7c69044b658a298b12278318c614d'
$hashes = @{
    'logmel.onnx' = '161BF43D3C258A3F5E98C609721DCA6D259B8C75404A56C4C135DDD36DCDD8AD'
    'beat_this.onnx' = '926847099887DE0D577509824846C89797B02FB28E35FC004C3ADD149C465891'
    'LICENSE' = '909AB6549794A18E9BB243AACFADDA4A5F308436FC2846F350755C53C4F06AE1'
}
foreach ($name in $hashes.Keys) {
    $path = BeatAsset $name $hashes[$name] @(
        "https://huggingface.co/$export/${name}?download=true",
        "https://hf-mirror.com/$export/${name}?download=true"
    )
    $target = if ($name -eq 'LICENSE') { 'LICENSE.beat-this' } else { $name }
    Copy-Item -LiteralPath $path -Destination (Join-Path $Destination $target)
}
Add-Type -AssemblyName System.IO.Compression.FileSystem
$zip = [IO.Compression.ZipFile]::OpenRead($ort)
try {
    foreach ($name in 'onnxruntime.dll', 'onnxruntime_providers_shared.dll', 'onnxruntime_providers_cuda.dll') {
        CopyZipEntry $zip "runtimes/win-x64/native/$name" $name
    }
    CopyZipEntry $zip 'LICENSE' 'LICENSE.onnxruntime'
    CopyZipEntry $zip 'ThirdPartyNotices.txt' 'THIRD-PARTY.onnxruntime.txt'
} finally { $zip.Dispose() }
$zip = [IO.Compression.ZipFile]::OpenRead($dnn)
try {
    # Beat This uses the graph engines and tensor ops from the pinned cuDNN build.
    foreach ($name in 'cudnn64_9.dll', 'cudnn_graph64_9.dll', 'cudnn_ops64_9.dll', 'cudnn_heuristic64_9.dll', 'cudnn_engines_precompiled64_9.dll', 'cudnn_engines_runtime_compiled64_9.dll', 'cudnn_engines_tensor_ir64_9.dll') {
        CopyZipEntry $zip "nvidia/cudnn/bin/$name" $name
    }
    CopyZipEntry $zip 'nvidia_cudnn_cu13-9.28.1.4.dist-info/licenses/LICENSE' 'LICENSE.cudnn'
} finally { $zip.Dispose() }
