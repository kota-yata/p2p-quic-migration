package mp3

import (
	"bufio"
	"fmt"
	"io"
	"os"
)

var (
	V1L3_BITRATE_TABLE = [15]uint16{
		0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320,
	}
	V2L3_BITRATE_TABLE = [15]uint16{
		0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160,
	}
	V1_SAMPLE_RATE_TABLE = [3]uint16{
		44100, 48000, 32000,
	}
	V2_SAMPLE_RATE_TABLE = [3]uint16{
		22050, 24000, 16000,
	}
)

type MP3Frame struct {
	// version (1), protection (1), bitrate index(4), sample rate index(2),
	// version=0 indicates MPEG2, version=1 indicates MPEG1
	flag1 byte
	// unused(5), padding (1), channel mode (2)
	flag2 byte
}

func openMP3File(path string) (io.ReadCloser, error) {
	ext := ".mp3"
	if len(path) < len(ext) || path[len(path)-len(ext):] != ext {
		return nil, fmt.Errorf("unsupported file format: %s", path)
	}
	return os.Open(path)
}

func (h *MP3Frame) readHeader(reader *bufio.Reader) error {
	var b byte
	var err error
	for {
		b, err = reader.ReadByte()
		if err != nil {
			return err
		}
		if b != 0xFF {
			// possibly ID3 tag, keep searching
			continue
		}

		b, err = reader.ReadByte()
		if err != nil {
			return err
		}
		// syncword is 11 bits, check the last 3 bits to confirm
		if b&0xE0 != 0xE0 {
			continue
		}
		// version check
		switch (b >> 3) & 0x03 {
		case 0x10: // MPEG Version 2.0
		case 0x11: // MPEG Version 1.0
			h.flag1 |= 1 << 7 // set msb to indicate MPEG1
		default:
			return fmt.Errorf("unsupported MPEG version %02x", (b>>3)&0x03)
		}
		// at this point we only support Layer III (MP3)
		if (b >> 1 & 0x03) != 0x01 {
			return fmt.Errorf("unsupported layer, only Layer III (MP3) is supported")
		}
		h.flag1 |= (b & 0x01) << 6 // set protection bit

		b, err = reader.ReadByte()
		if err != nil {
			return err
		}

		bitrateIndex := (b >> 4) & 0x0F
		if bitrateIndex == 0x0F {
			return fmt.Errorf("invalid bitrate index: bad")
		}
		h.flag1 |= bitrateIndex << 2 // set bitrate index

		sampleRateIndex := (b >> 2) & 0x03
		if sampleRateIndex == 0x03 {
			return fmt.Errorf("invalid sample rate index: reserved")
		}
		h.flag1 |= sampleRateIndex // set sample rate index

		h.flag2 |= ((b >> 1) & 0x01) << 1 // set padding bit

		b, err = reader.ReadByte()
		if err != nil {
			return err
		}
		h.flag2 |= (b >> 6) & 0x03 // set channel mode

		return nil
	}
}

// TODO: check CRC
// TODO: calculate frame length

// IsStereo does not distinguish between joint stereo and dual channel modes
func (h *MP3Frame) IsStereo() bool {
	return (h.flag2 & 0x03) != 0x03
}

func (h *MP3Frame) GetBitrateKbps() (uint16, bool) {
	bitrateIndex := (h.flag1 >> 2) & 0x0F
	if bitrateIndex == 0 {
		return 0, true // free bitrate
	}
	version := (h.flag1 >> 7) & 0x01
	if version == 1 {
		return V1L3_BITRATE_TABLE[bitrateIndex], false
	} else {
		return V2L3_BITRATE_TABLE[bitrateIndex], false
	}
}

func (h *MP3Frame) GetSampleRate() uint16 {
	sampleRateIndex := h.flag1 & 0x03
	version := (h.flag1 >> 7) & 0x01
	if version == 1 {
		return V1_SAMPLE_RATE_TABLE[sampleRateIndex]
	} else {
		return V2_SAMPLE_RATE_TABLE[sampleRateIndex]
	}
}
