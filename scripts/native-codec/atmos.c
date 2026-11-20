#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <errno.h>
#include <stdio.h>
#include <string.h>
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/mem.h>
#include "orender.h"

enum { sample_rate = 48000, channels = 10, wav_header = 44 };

static int interrupted(void *flag) {
    return InterlockedCompareExchange((volatile LONG *)flag, 0, 0) != 0;
}

// 1 means the output contains a verified object presentation. 0 lets the caller use channel decoding.
__declspec(dllexport) int render_atmos_v1(HMODULE engine, const char *config, const char *layout,
    const char *bridge, const char *input, const char *demuxer, const wchar_t *output, volatile LONG *cancel) {
    uint32_t (*abi_major)(void) = (void *)GetProcAddress(engine, "orender_version_major");
    OrenderRenderer *(*create)(const OrenderConfig *) = (void *)GetProcAddress(engine, "orender_create");
    void (*destroy)(OrenderRenderer *) = (void *)GetProcAddress(engine, "orender_destroy");
    int (*process)(OrenderRenderer *, const uint8_t *, uintptr_t, int64_t, float *, uintptr_t,
        uintptr_t *, uint32_t *, int64_t *) = (void *)GetProcAddress(engine, "orender_process");
    int (*has_objects)(const OrenderRenderer *) = (void *)GetProcAddress(engine, "orender_has_objects");
    uint32_t (*read_layout)(const OrenderRenderer *, uint8_t *, uint32_t) =
        (void *)GetProcAddress(engine, "orender_channel_layout");
    if (!abi_major || !create || !destroy || !process || !has_objects || !read_layout || abi_major() != ORENDER_ABI_MAJOR)
        return AVERROR(ENOSYS);

    AVFormatContext *fmt = avformat_alloc_context();
    AVPacket *packet = av_packet_alloc();
    AVDictionary *options = NULL;
    OrenderRenderer *renderer = NULL;
    FILE *file = NULL;
    float *pcm = NULL;
    int err = AVERROR(ENOMEM);
    if (!fmt || !packet) goto done;
    fmt->interrupt_callback = (AVIOInterruptCB){ interrupted, (void *)cancel };
    const AVInputFormat *type = av_find_input_format(demuxer);
    if (!type) { err = AVERROR_DEMUXER_NOT_FOUND; goto done; }
    err = av_dict_set(&options, "protocol_whitelist", "file", 0);
    if (err < 0) goto done;
    err = avformat_open_input(&fmt, input, type, &options);
    if (err < 0) goto done;

    AVStream *track = NULL;
    for (unsigned int i = 0; i < fmt->nb_streams; i++) {
        if (fmt->streams[i]->codecpar->codec_type == AVMEDIA_TYPE_AUDIO) {
            track = fmt->streams[i];
            break;
        }
    }
    if (!track) goto done;
    enum AVCodecID codec = track->codecpar->codec_id;
    const char *codec_name = codec == AV_CODEC_ID_TRUEHD ? "truehd" : codec == AV_CODEC_ID_EAC3 ? "eac3" : NULL;
    if (!codec_name) goto done;
    OrenderConfig settings = { .sample_rate = sample_rate, .config_yaml_path = config,
        .speaker_layout_path = layout, .bridge_path = bridge, .codec = codec_name };
    renderer = create(&settings);
    if (!renderer) { err = AVERROR_INVALIDDATA; goto done; }
    file = _wfopen(output, L"wb");
    if (!file) { err = AVERROR(errno); goto done; }
    const char header[wav_header] = {0};
    if (fwrite(header, 1, sizeof(header), file) != sizeof(header)) { err = AVERROR(EIO); goto done; }

    uintptr_t capacity = 4096 * channels;
    pcm = av_malloc_array(capacity, sizeof(*pcm));
    if (!pcm) { err = AVERROR(ENOMEM); goto done; }
    uint64_t written = 0;
    int objects = 0;
    const uint8_t expected[] = { OrenderChannelLabel_L, OrenderChannelLabel_R, OrenderChannelLabel_C,
        OrenderChannelLabel_Lfe, OrenderChannelLabel_Lb, OrenderChannelLabel_Rb, OrenderChannelLabel_Ls,
        OrenderChannelLabel_Rs, OrenderChannelLabel_Tfl, OrenderChannelLabel_Tfr };
    while (!interrupted((void *)cancel)) {
        err = av_read_frame(fmt, packet);
        if (err < 0) break;
        if (packet->stream_index == track->index && packet->size) {
            uintptr_t frames = 0;
            uint32_t out_channels = 0;
            int64_t pts = packet->pts == AV_NOPTS_VALUE ? INT64_MIN : av_rescale_q(packet->pts, track->time_base, AV_TIME_BASE_Q);
            for (;;) {
                err = process(renderer, packet->data, packet->size, pts, pcm, capacity, &frames, &out_channels, NULL);
                if (err <= 0) break;
                if (capacity >= 4 * sample_rate * channels) { err = AVERROR_INVALIDDATA; goto done; }
                capacity *= 2;
                float *larger = av_realloc_array(pcm, capacity, sizeof(*pcm));
                if (!larger) { err = AVERROR(ENOMEM); goto done; }
                pcm = larger;
            }
            if (err < 0) goto done;
            if (frames) {
                objects |= has_objects(renderer) > 0;
                if (out_channels != channels || frames > capacity / channels) { err = AVERROR_INVALIDDATA; goto done; }
                if (!written) {
                    uint8_t labels[channels];
                    if (read_layout(renderer, labels, channels) != channels || memcmp(labels, expected, channels)) {
                        err = AVERROR_INVALIDDATA;
                        goto done;
                    }
                }
                if (written + frames > (UINT32_MAX - 36) / (channels * sizeof(*pcm))) { err = AVERROR(EFBIG); goto done; }
                if (fwrite(pcm, channels * sizeof(*pcm), frames, file) != frames) { err = AVERROR(EIO); goto done; }
                written += frames;
                // shortcut: probe for objects in the first two seconds of PCM, until mixed-presentation tracks are supported.
                if (!objects && written >= 2 * sample_rate) break;
            }
        }
        av_packet_unref(packet);
    }
    if (interrupted((void *)cancel)) err = AVERROR_EXIT;
    else if (err == AVERROR_EOF || err == 0) err = objects && written ? 1 : 0;
done:
    av_free(pcm);
    if (file && fclose(file) && err >= 0) err = AVERROR(EIO);
    if (renderer) destroy(renderer);
    av_dict_free(&options);
    av_packet_free(&packet);
    avformat_close_input(&fmt);
    return err;
}
