#include "audiocpp.h"

#define WIN32_LEAN_AND_MEAN
#define NOMINMAX
#include <windows.h>
#include <fcntl.h>
#include <io.h>

#include <algorithm>
#include <array>
#include <charconv>
#include <chrono>
#include <cmath>
#include <cstdint>
#include <cstdio>
#include <filesystem>
#include <memory>
#include <numbers>
#include <stdexcept>
#include <string_view>
#include <vector>

namespace {
constexpr int model_rate = 44100;
constexpr int bed_channels = 18;
constexpr double pi = std::numbers::pi;

bool valid_rate(uint32_t rate) { return rate >= 32000 && rate <= 192000; }

void check(audiocpp_status status) {
    if (status != AUDIOCPP_OK) {
        throw std::runtime_error(audiocpp_last_error());
    }
}

std::vector<float> resample(const std::vector<float>& pcm, int channels, int from, int to) {
    if (from == to) {
        return pcm;
    }
    // Band-limit before changing rates. Zero padding matches the block edges
    // of the previous resampler, which playback already overlaps.
    constexpr int phases = 512;
    const double cutoff = 0.99 * std::min(1.0, static_cast<double>(to) / from);
    const int width = static_cast<int>(std::ceil(6 / cutoff));
    const int taps = 2 * width + 1;
    std::vector<float> kernel(phases * taps);
    for (int phase = 0; phase < phases; ++phase) {
        for (int tap = 0; tap < taps; ++tap) {
            const double t = std::clamp((tap - width - static_cast<double>(phase) / phases) * cutoff, -6.0, 6.0);
            const double window = std::cos(pi * t / 12);
            const double sinc = t == 0 ? 1 : std::sin(pi * t) / (pi * t);
            kernel[phase * taps + tap] = static_cast<float>(cutoff * window * window * sinc);
        }
    }
    const int frames = static_cast<int>(pcm.size() / channels);
    const auto out_frames = (int64_t{frames} * to + from - 1) / from;
    std::vector<float> output(out_frames * channels);
    for (int64_t i = 0, time = 0; i < out_frames; ++i, time += from) {
        const int start = static_cast<int>(time / to) - width;
        const auto* weights = kernel.data() + (time % to * phases / to) * taps;
        auto* dst = output.data() + i * channels;
        for (int tap = std::max(0, -start); tap < std::min(taps, frames - start); ++tap) {
            const auto* src = pcm.data() + (start + tap) * channels;
            for (int ch = 0; ch < channels; ++ch) {
                dst[ch] += src[ch] * weights[tap];
            }
        }
    }
    return output;
}

std::vector<float> high_pass(const std::vector<float>& stereo, double cutoff) {
    const double delta = 1 - std::cos(2 * pi * cutoff / model_rate);
    const float pole = static_cast<float>(1 / (1 + delta + std::sqrt(delta * (delta + 2))));
    const float gain = 1 - pole;
    const int frames = static_cast<int>(stereo.size() / 2);
    const float cycle = static_cast<float>(1 - std::pow(pole, frames));
    auto output = stereo;
    // Subtract a forward/backward low-pass to move ambience without phase
    // shift. Periodic boundary conditions match the original FFT filter.
    const auto smooth = [&](int start, int end, int step) {
        std::array<float, 2> state{};
        for (int i = start; i != end; i += step) {
            for (int ch = 0; ch < 2; ++ch) {
                state[ch] = gain * output[2 * i + ch] + pole * state[ch];
            }
        }
        for (auto& value : state) {
            value /= cycle;
        }
        for (int i = start; i != end; i += step) {
            for (int ch = 0; ch < 2; ++ch) {
                auto& sample = output[2 * i + ch];
                sample = state[ch] = gain * sample + pole * state[ch];
            }
        }
    };
    smooth(0, frames, 1);
    smooth(frames - 1, -1, -1);
    for (size_t i = 0; i < stereo.size(); ++i) {
        output[i] = stereo[i] - output[i];
    }
    return output;
}

std::vector<float> render(audiocpp_session* session, std::vector<float>& stereo, int rate) {
    for (auto& sample : stereo) {
        if (!std::isfinite(sample)) {
            sample = 0;
        }
    }
    const auto audio = resample(stereo, 2, rate, model_rate);
    const size_t frames = audio.size() / 2;
    const std::unique_ptr<audiocpp_request, decltype(&audiocpp_request_free)>
        request(audiocpp_request_create(), audiocpp_request_free);
    if (!request) {
        throw std::runtime_error("could not allocate an inference request");
    }
    check(audiocpp_request_set_audio(request.get(), audio.data(), frames, model_rate, 2));
    audiocpp_result* raw_result = nullptr;
    check(audiocpp_session_run(session, request.get(), &raw_result));
    const std::unique_ptr<audiocpp_result, decltype(&audiocpp_result_free)>
        result(raw_result, audiocpp_result_free);

    constexpr std::array<std::string_view, 4> names{"drums", "bass", "other", "vocals"};
    std::array<const float*, names.size()> stems{};
    for (size_t i = 0; i < audiocpp_result_named_audio_count(result.get()); ++i) {
        const char* name = nullptr;
        const float* samples = nullptr;
        size_t stem_frames = 0;
        int stem_rate = 0, channels = 0;
        check(audiocpp_result_named_audio(result.get(), i, &name, &samples, &stem_frames, &stem_rate, &channels));
        const auto stem = std::find(names.begin(), names.end(), std::string_view(name ? name : ""));
        if (stem == names.end() || !samples || stem_frames != frames || stem_rate != model_rate || channels != 2) {
            throw std::runtime_error("invalid HTDemucs stem");
        }
        stems[stem - names.begin()] = samples;
    }
    if (std::find(stems.begin(), stems.end(), nullptr) != stems.end()) {
        throw std::runtime_error("missing HTDemucs stem");
    }
    const auto [drums, bass, other, vocals] = stems;
    std::vector<float> ambience(audio.size()), percussion(audio.size());
    for (size_t i = 0; i < audio.size(); i += 2) {
        const float other_mono = 0.325f * (other[i] + other[i + 1]);
        const float drums_mono = 0.39f * (drums[i] + drums[i + 1]);
        for (size_t ch = 0; ch < 2; ++ch) {
            ambience[i + ch] = other[i + ch] - other_mono;
            percussion[i + ch] = drums[i + ch] - drums_mono;
        }
    }
    const auto sides = high_pass(ambience, 260);
    const auto ambience_high = high_pass(ambience, 1100);
    const auto drums_high = high_pass(percussion, 1100);
    std::vector<float> bed(frames * bed_channels);
    for (size_t i = 0; i < frames; ++i) {
        const size_t sample = 2 * i;
        auto* out = bed.data() + i * bed_channels;
        out[2] = 0.37f * (vocals[sample] + vocals[sample + 1]);
        for (size_t ch = 0; ch < 2; ++ch) {
            const size_t k = sample + ch;
            out[4 + ch] = 0.12f * percussion[k];
            out[6 + ch] = 0.30f * sides[k];
            out[16 + ch] = 0.13f * drums_high[k];
            out[8 + ch] = out[16 + ch] + 0.07f * ambience_high[k];
            out[ch] = audio[k] - 0.70710678f * (out[2] + out[4 + ch] + out[6 + ch] + out[8 + ch]);
            out[10 + ch] = vocals[k];
            out[12 + ch] = drums[k];
            out[14 + ch] = bass[k];
        }
        for (int ch = 0; ch < bed_channels; ++ch) {
            out[ch] *= 0.94f;
        }
    }
    auto output = resample(bed, bed_channels, model_rate, rate);
    output.resize(stereo.size() / 2 * bed_channels);
    if (!std::all_of(output.begin(), output.end(), [](float sample) { return std::isfinite(sample); })) {
        throw std::runtime_error("non-finite HTDemucs output");
    }
    return output;
}

void warmup(audiocpp_session* session, int rate) {
    const int frames = 2 * rate + rate / 4;
    std::vector<float> stereo(2 * frames);
    for (int i = 0; i < frames; ++i) {
        const double t = static_cast<double>(i) / rate;
        const double envelope = std::exp(-8 * std::fmod(t, 0.25));
        stereo[2 * i] = 0.15f * static_cast<float>(envelope * std::sin(2 * pi * 220 * t));
        stereo[2 * i + 1] = 0.10f * static_cast<float>(envelope * std::sin(2 * pi * 440 * t));
    }
    const auto output = render(session, stereo, rate);
    float stem_peak = 0;
    for (int i = 64; i < frames - 64; ++i) {
        const auto* bed = output.data() + i * bed_channels;
        for (int ch = 0; ch < 2; ++ch) {
            const float fold = bed[ch] + 0.70710678f * (bed[2] + bed[4 + ch] + bed[6 + ch] + bed[8 + ch]);
            if (std::abs(fold - 0.94f * stereo[2 * i + ch]) > 0.002f) {
                throw std::runtime_error("HTDemucs fold-down self-test failed");
            }
            stem_peak = std::max(stem_peak, std::abs(bed[10 + ch]) + std::abs(bed[12 + ch]) + std::abs(bed[14 + ch]));
        }
    }
    if (stem_peak == 0) {
        throw std::runtime_error("HTDemucs returned empty stems");
    }
}
}

#include "beat.h"

int main(int argc, char** argv) try {
    std::string_view device = "cpu";
    int rate = 48000;
    bool self_test = false;
    for (int i = 1; i < argc; ++i) {
        const std::string_view arg = argv[i];
        if (arg == "--device" && i + 1 < argc) {
            device = argv[++i];
        } else if (arg == "--rate" && i + 1 < argc) {
            const std::string_view value = argv[++i];
            const auto end = value.data() + value.size();
            const auto parsed = std::from_chars(value.data(), end, rate);
            if (parsed.ec != std::errc{} || parsed.ptr != end) {
                throw std::runtime_error("invalid sample rate");
            }
        } else if (arg == "--check") {
            self_test = true;
        } else {
            throw std::runtime_error("unsupported argument");
        }
    }
    if (!valid_rate(rate) || (device != "cpu" && device != "cuda")) {
        throw std::runtime_error("unsupported device or sample rate");
    }
    if (_setmode(_fileno(stdin), _O_BINARY) == -1 || _setmode(_fileno(stdout), _O_BINARY) == -1) {
        throw std::runtime_error("could not open binary PCM pipes");
    }
    const auto runtime_dir = std::filesystem::current_path();
    if (!SetDefaultDllDirectories(LOAD_LIBRARY_SEARCH_SYSTEM32 | LOAD_LIBRARY_SEARCH_USER_DIRS) ||
        !AddDllDirectory(runtime_dir.c_str()) ||
        !LoadLibraryExW((runtime_dir / "audiocpp.dll").c_str(), nullptr,
                        LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR | LOAD_LIBRARY_SEARCH_SYSTEM32)) {
        throw std::runtime_error("could not load the Demucs runtime (Windows error " + std::to_string(GetLastError()) + ")");
    }
    constexpr auto abi_version = (AUDIOCPP_ABI_VERSION_MAJOR << 16) | (AUDIOCPP_ABI_VERSION_MINOR << 8) | AUDIOCPP_ABI_VERSION_PATCH;
    if (audiocpp_abi_version() != abi_version) {
        throw std::runtime_error("incompatible audio.cpp ABI");
    }
    audiocpp_registry* raw_registry = nullptr;
    check(audiocpp_registry_create(nullptr, &raw_registry));
    const std::unique_ptr<audiocpp_registry, decltype(&audiocpp_registry_free)>
        registry(raw_registry, audiocpp_registry_free);
    const audiocpp_model_config config{"htdemucs"};
    audiocpp_model* raw_model = nullptr;
    check(audiocpp_model_load(registry.get(), "../htdemucs-f16.gguf", &config, nullptr, &raw_model));
    const std::unique_ptr<audiocpp_model, decltype(&audiocpp_model_free)> model(raw_model, audiocpp_model_free);
    const audiocpp_backend_config backend{device.data(), 0, 6};
    audiocpp_session* raw_session = nullptr;
    check(audiocpp_session_create(model.get(), "sep", "offline", &backend, nullptr, &raw_session));
    const std::unique_ptr<audiocpp_session, decltype(&audiocpp_session_free)> session(raw_session, audiocpp_session_free);

    const auto start = std::chrono::steady_clock::now();
    warmup(session.get(), rate);
    if (self_test) {
        const double seconds = std::chrono::duration<double>(std::chrono::steady_clock::now() - start).count();
        std::printf("Demucs ready: %s (2.25 s of PCM in %.3f s)\n", device.data(), seconds);
        return 0;
    }
    const bool beat_supported = device == "cuda" && std::filesystem::exists(runtime_dir / L"beat_this.onnx");
    std::printf("READY %s %s\n", beat_supported ? "mix-v3" : "mix-v2", device.data());
    if (std::fflush(stdout) != 0) {
        throw std::runtime_error("could not write Demucs handshake");
    }
    std::array<uint32_t, 2> header{};
    std::unique_ptr<BeatTracker> tracker;
    bool beat_failed = false;
    for (;;) {
        const size_t bytes = std::fread(header.data(), 1, sizeof(header), stdin);
        if (bytes == 0 && std::feof(stdin)) {
            return 0;
        }
        const bool beat = (header[0] & 0x80000000u) != 0;
        const auto frames = header[0] & 0x7fffffffu;
        const auto input_rate = header[1];
        if (bytes != sizeof(header) || !valid_rate(input_rate) || frames == 0 || frames > (beat ? 24u : 3u) * input_rate) {
            throw std::runtime_error("invalid PCM block header");
        }
        std::vector<float> stereo(frames * 2);
        if (std::fread(stereo.data(), sizeof(float), stereo.size(), stdin) != stereo.size()) {
            throw std::runtime_error("incomplete PCM block");
        }
        if (beat) {
            int32_t count = -1;
            std::vector<int32_t> events;
            if (beat_supported && !beat_failed) {
                try {
                    if (!tracker) tracker = BeatTracker::open(runtime_dir);
                    events = tracker->detect(stereo, input_rate);
                    count = static_cast<int32_t>(events.size());
                } catch (const std::exception& error) {
                    beat_failed = true;
                    tracker.reset();
                    std::fprintf(stderr, "Beat This: %s\n", error.what());
                }
            }
            if (std::fwrite(&count, sizeof(count), 1, stdout) != 1 ||
                std::fwrite(events.data(), sizeof(int32_t), events.size(), stdout) != events.size() ||
                std::fflush(stdout) != 0) {
                throw std::runtime_error("could not write Beat This result");
            }
            continue;
        }
        const auto bed = render(session.get(), stereo, input_rate);
        if (std::fwrite(bed.data(), sizeof(float), bed.size(), stdout) != bed.size() || std::fflush(stdout) != 0) {
            throw std::runtime_error("could not write Demucs PCM");
        }
    }
} catch (const std::exception& error) {
    std::fprintf(stderr, "Demucs: %s\n", error.what());
    return 1;
}
