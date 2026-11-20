#define ORT_API_MANUAL_INIT
#include <onnxruntime_cxx_api.h>

class BeatTracker {
    Ort::Env env{ORT_LOGGING_LEVEL_ERROR, "beat-this"};
    Ort::Session frontend{nullptr}, model{nullptr};

    static std::vector<int32_t> peaks(const float* values, int frames) {
        std::vector<int32_t> result;
        for (int i = 0; i < frames; ++i) {
            if (!(values[i] > 0) ||
                values[i] < *std::max_element(values + std::max(0, i - 3), values + std::min(frames, i + 4))) {
                continue;
            }
            if (!result.empty() && i == result.back() + 1) {
                continue;
            }
            result.push_back(i);
        }
        return result;
    }

public:
    static std::unique_ptr<BeatTracker> open(const std::filesystem::path& dir) {
        const auto library = LoadLibraryExW((dir / L"onnxruntime.dll").c_str(), nullptr,
            LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR | LOAD_LIBRARY_SEARCH_SYSTEM32);
        if (!library) throw std::runtime_error("could not load ONNX Runtime");
        using GetApiBase = const OrtApiBase* (ORT_API_CALL*)();
        const auto get_api = reinterpret_cast<GetApiBase>(GetProcAddress(library, "OrtGetApiBase"));
        const auto api = get_api ? get_api()->GetApi(ORT_API_VERSION) : nullptr;
        if (!api) throw std::runtime_error("incompatible ONNX Runtime");
        Ort::InitApi(api);
        return std::make_unique<BeatTracker>(dir);
    }

    explicit BeatTracker(const std::filesystem::path& dir) {
        Ort::SessionOptions options;
        options.SetIntraOpNumThreads(2);
        options.AddConfigEntry("session.record_ep_graph_assignment_info", "1");
        OrtCUDAProviderOptions cuda;
        cuda.cudnn_conv_algo_search = OrtCudnnConvAlgoSearchHeuristic;
        options.AppendExecutionProvider_CUDA(cuda);
        frontend = Ort::Session(env, (dir / L"logmel.onnx").c_str(), options);
        model = Ort::Session(env, (dir / L"beat_this.onnx").c_str(), options);
        const auto graphs = model.GetEpGraphAssignmentInfo();
        if (std::none_of(graphs.begin(), graphs.end(), [](const auto& graph) {
            return graph.GetEpName() == "CUDAExecutionProvider" && !graph.GetNodes().empty();
        })) throw std::runtime_error("Beat This model was not assigned to CUDA");
    }

    std::vector<int32_t> detect(const std::vector<float>& stereo, int rate) {
        std::vector<float> mono(stereo.size() / 2);
        for (size_t i = 0; i < mono.size(); ++i) {
            mono[i] = 0.5f * (stereo[2*i] + stereo[2*i + 1]);
        }
        auto audio = resample(mono, 1, rate, 22050);
        const auto memory = Ort::MemoryInfo::CreateCpu(OrtArenaAllocator, OrtMemTypeDefault);
        const int64_t samples = static_cast<int64_t>(audio.size());
        auto waveform = Ort::Value::CreateTensor<float>(memory, audio.data(), audio.size(), &samples, 1);
        const char* audio_name = "audio";
        const char* spect_name = "spect";
        auto features = frontend.Run(Ort::RunOptions{nullptr}, &audio_name, &waveform, 1, &spect_name, 1);
        const auto shape = features[0].GetTensorTypeAndShapeInfo().GetShape();
        if (shape.size() != 2 || shape[1] != 128 || shape[0] < 1 || shape[0] > 1500) {
            throw std::runtime_error("unexpected Beat This mel spectrogram");
        }
        const int64_t batch_shape[]{1, shape[0], shape[1]};
        auto spect = Ort::Value::CreateTensor<float>(memory, features[0].GetTensorMutableData<float>(),
            static_cast<size_t>(shape[0] * shape[1]), batch_shape, 3);
        const char* output_names[]{"beat", "downbeat"};
        const auto output = model.Run(Ort::RunOptions{nullptr}, &spect_name, &spect, 1, output_names, 2);
        if (output[0].GetTensorTypeAndShapeInfo().GetElementCount() != static_cast<size_t>(shape[0]) ||
            output[1].GetTensorTypeAndShapeInfo().GetElementCount() != static_cast<size_t>(shape[0])) {
            throw std::runtime_error("unexpected Beat This logits");
        }
        auto beats = peaks(output[0].GetTensorData<float>(), static_cast<int>(shape[0]));
        std::vector<int32_t> downbeats;
        for (const auto bar : peaks(output[1].GetTensorData<float>(), static_cast<int>(shape[0]))) {
            auto hit = std::lower_bound(beats.begin(), beats.end(), bar);
            if (hit != beats.begin() && (hit == beats.end() || bar - hit[-1] <= *hit - bar)) --hit;
            if (hit != beats.end() && std::abs(*hit - bar) <= 7) downbeats.push_back(*hit);
        }
        for (auto& beat : beats) {
            const bool down = std::binary_search(downbeats.begin(), downbeats.end(), beat);
            beat = down ? -(beat + 1) : beat + 1;
        }
        return beats;
    }
};
