#include "rubberband/RubberBandStretcher.h"

#include <algorithm>
#include <array>

using RubberBand::RubberBandStretcher;

namespace {
constexpr int block = 512;

struct Stretch {
    RubberBandStretcher engine;
    std::array<float, block> left{}, right{};
    size_t delay;

    explicit Stretch(unsigned rate) :
        engine(rate, 2, RubberBandStretcher::OptionProcessRealTime |
                        RubberBandStretcher::OptionEngineFiner |
                        RubberBandStretcher::OptionChannelsTogether),
        delay(engine.getStartDelay()) {
        engine.setMaxProcessSize(block);
        const float * silence[] = {left.data(), right.data()};
        for (size_t pad = engine.getPreferredStartPad(); pad > 0;) {
            size_t count = std::min<size_t>(pad, block);
            engine.process(silence, count, false);
            pad -= count;
        }
    }
};
}

extern "C" __declspec(dllexport) Stretch *mygo_stretch_open(unsigned rate) {
    try { return new Stretch(rate); }
    catch (...) { return nullptr; }
}

extern "C" __declspec(dllexport) void mygo_stretch_close(Stretch *s) {
    delete s;
}

extern "C" __declspec(dllexport) void mygo_stretch_push(Stretch *s, const float *pcm, int frames, double ratio, int final) {
    for (int i = 0; i < frames; ++i) {
        s->left[i] = pcm[2 * i];
        s->right[i] = pcm[2 * i + 1];
    }
    const float *input[] = {s->left.data(), s->right.data()};
    s->engine.setTimeRatio(ratio);
    s->engine.process(input, frames, final != 0);
}

extern "C" __declspec(dllexport) int mygo_stretch_pull(Stretch *s, float *pcm, int frames) {
    float *output[] = {s->left.data(), s->right.data()};
    int available = s->engine.available();
    while (s->delay > 0 && available > 0) {
        size_t count = std::min<size_t>({s->delay, block, static_cast<size_t>(available)});
        s->delay -= s->engine.retrieve(output, count);
        available = s->engine.available();
    }
    if (available <= 0) return available;
    int count = static_cast<int>(s->engine.retrieve(output, std::min({frames, block, available})));
    for (int i = 0; i < count; ++i) {
        pcm[2 * i] = s->left[i];
        pcm[2 * i + 1] = s->right[i];
    }
    return count;
}
