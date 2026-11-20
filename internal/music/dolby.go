package music

import "bytes"

const trueHDMajorSync = "\xf8\x72\x6f\xba"

// DolbyAudioFormat identifies raw Dolby audio by its synchronization word.
func DolbyAudioFormat(head []byte) string {
	if len(head) >= 6 && head[0] == 0x0b && head[1] == 0x77 {
		if head[5]>>3 <= 10 {
			return "ac3"
		}
		if head[5]>>3 <= 16 {
			return "eac3"
		}
	}
	if len(head) >= 8 && string(head[4:8]) == trueHDMajorSync {
		return "truehd"
	}
	return ""
}

// ContainsTrueHDSync detects a major sync after a partial or minor-sync frame.
func ContainsTrueHDSync(head []byte) bool {
	return len(head) >= 8 && bytes.Contains(head[4:], []byte(trueHDMajorSync))
}

// IsDolbyMP4 reports whether an MP4 sample entry needs a Dolby audio decoder.
func IsDolbyMP4(codec string) bool { return codec == "ac-3" || codec == "ec-3" || codec == "mlpa" }
