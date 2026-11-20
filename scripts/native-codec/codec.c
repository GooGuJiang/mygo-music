#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <errno.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/mem.h>
#include <libswresample/swresample.h>

enum { rate = 48000, wav_header = 44 };

static int canceled(void *flag) {
    return InterlockedCompareExchange((volatile LONG *)flag, 0, 0) != 0;
}

typedef struct {
    FILE *file;
    int64_t skip, remaining;
    int channels;
    AVFrame *format;
} PCMOutput;

static int write_pcm(PCMOutput *out, const float *data, int frames) {
    int skip = out->skip < frames ? (int)out->skip : frames;
    out->skip -= skip;
    data += skip * out->channels;
    frames -= skip;
    if (out->remaining < frames) frames = (int)out->remaining;
    if (fwrite(data, sizeof(float) * out->channels, frames, out->file) != (size_t)frames)
        return AVERROR(EIO);
    out->remaining -= frames;
    return 0;
}

static int resample(SwrContext *swr, AVFrame *frame, PCMOutput *out) {
    const int frames = frame ? frame->nb_samples : 0;
    int capacity = swr_get_out_samples(swr, frames);
    if (capacity <= 0) return capacity;
    float *data = av_malloc_array(capacity, sizeof(float) * out->channels);
    if (!data) return AVERROR(ENOMEM);
    uint8_t *planes[] = { (uint8_t *)data };
    int got = swr_convert(swr, planes, capacity,
                          frame ? (const uint8_t **)frame->extended_data : NULL, frames);
    int err = got < 0 ? got : write_pcm(out, data, got);
    av_free(data);
    return err < 0 ? err : got;
}

static int decode_packet(AVCodecContext *dec, AVPacket *packet, AVFrame *frame,
                         SwrContext **swr, PCMOutput *out) {
    int err = avcodec_send_packet(dec, packet);
    if (err < 0) return err;
    while ((err = avcodec_receive_frame(dec, frame)) >= 0) {
        if (!*swr) {
            AVChannelLayout stereo = AV_CHANNEL_LAYOUT_STEREO;
            AVChannelLayout surround = AV_CHANNEL_LAYOUT_5POINT1_BACK;
            AVChannelLayout *layout = &stereo;
            if (frame->ch_layout.nb_channels > 2) layout = &surround;
            out->channels = layout->nb_channels;
            out->format->sample_rate = frame->sample_rate;
            out->format->format = frame->format;
            err = av_channel_layout_copy(&out->format->ch_layout, &frame->ch_layout);
            if (err < 0) return err;
            err = swr_alloc_set_opts2(swr, layout, AV_SAMPLE_FMT_FLT, rate,
                &frame->ch_layout, frame->format, frame->sample_rate, 0, NULL);
            if (err >= 0) err = swr_init(*swr);
            if (err < 0) return err;
        } else if (frame->sample_rate != out->format->sample_rate || frame->format != out->format->format ||
                   av_channel_layout_compare(&frame->ch_layout, &out->format->ch_layout) != 0) {
            return AVERROR_INVALIDDATA;
        }
        err = resample(*swr, frame, out);
        av_frame_unref(frame);
        if (err < 0 || out->remaining == 0) return err < 0 ? err : 0;
    }
    return err == AVERROR(EAGAIN) || err == AVERROR_EOF ? 0 : err;
}

__declspec(dllexport) int decode_pcm_v3(const char *input, const char *demuxer,
    const wchar_t *output, int64_t skip, int64_t limit, int *pcm_channels,
    volatile LONG *cancel) {
    int err = AVERROR(ENOMEM);
    AVFormatContext *fmt = avformat_alloc_context();
    AVCodecContext *dec = NULL;
    SwrContext *swr = NULL;
    AVPacket *packet = av_packet_alloc();
    AVFrame *frame = av_frame_alloc();
    AVDictionary *options = NULL;
    PCMOutput out = { NULL, skip, limit < 0 ? INT64_MAX : limit, 2, av_frame_alloc() };
    if (!fmt || !packet || !frame || !out.format) goto done;
    fmt->interrupt_callback = (AVIOInterruptCB){ canceled, (void *)cancel };
    const AVInputFormat *type = av_find_input_format(demuxer);
    if (!type) { err = AVERROR_DEMUXER_NOT_FOUND; goto done; }
    err = av_dict_set(&options, "protocol_whitelist", "file", 0);
    if (err < 0) goto done;
    err = avformat_open_input(&fmt, input, type, &options);
    if (err < 0) goto done;
    // The raw E-AC-3 decoder gets its format from the frames. Playback counts PCM samples for duration.
    if (strcmp(demuxer, "eac3") != 0) {
        err = avformat_find_stream_info(fmt, NULL);
        if (err < 0) goto done;
    }
    int track = -1;
    for (unsigned int i = 0; i < fmt->nb_streams; i++) {
        if (fmt->streams[i]->codecpar->codec_type == AVMEDIA_TYPE_AUDIO) { track = (int)i; break; }
    }
    if (track < 0) { err = track; goto done; }
    AVCodecParameters *params = fmt->streams[track]->codecpar;
    const AVCodec *codec = avcodec_find_decoder(params->codec_id);
    if (!codec) { err = AVERROR_DECODER_NOT_FOUND; goto done; }
    dec = avcodec_alloc_context3(codec);
    if (!dec) { err = AVERROR(ENOMEM); goto done; }
    dec->pkt_timebase = fmt->streams[track]->time_base;
    err = avcodec_parameters_to_context(dec, params);
    if (err >= 0) err = avcodec_open2(dec, codec, NULL);
    if (err < 0) goto done;
    out.file = _wfopen(output, L"wb");
    if (!out.file) { err = AVERROR(errno); goto done; }
    const char header[wav_header] = {0};
    if (fwrite(header, 1, sizeof(header), out.file) != sizeof(header)) {
        err = AVERROR(EIO); goto done;
    }
    while (!canceled((void *)cancel) && out.remaining) {
        err = av_read_frame(fmt, packet);
        if (err < 0) break;
        if (packet->stream_index == track) err = decode_packet(dec, packet, frame, &swr, &out);
        av_packet_unref(packet);
        if (err < 0) goto done;
    }
    if (canceled((void *)cancel)) { err = AVERROR_EXIT; goto done; }
    if (err == AVERROR_EOF) err = decode_packet(dec, NULL, frame, &swr, &out);
    if (err < 0) goto done;
    while (swr && out.remaining && (err = resample(swr, NULL, &out)) > 0) {}
    if (err >= 0 && limit >= 0 && out.remaining) err = AVERROR_INVALIDDATA;
    *pcm_channels = out.channels;
done:
    if (out.file && fclose(out.file) && err >= 0) err = AVERROR(EIO);
    av_dict_free(&options);
    avformat_close_input(&fmt);
    avcodec_free_context(&dec);
    av_packet_free(&packet);
    av_frame_free(&frame);
    av_frame_free(&out.format);
    swr_free(&swr);
    return err < 0 ? err : 0;
}
