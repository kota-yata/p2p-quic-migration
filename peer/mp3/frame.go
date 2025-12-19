package mp3

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sync"
)

type MP3Frame struct {
	// version (1), protection (1), bitrate index(4), sample rate index(2),
	// version=0 indicates MPEG2, version=1 indicates MPEG1
	flag1      byte
	flag1Mutex sync.Mutex
	// unused(5), padding (1), channel mode (2)
	flag2      byte
	flag2Mutex sync.Mutex
	// CRC16 value read from the frame (if present)
	crcValue      [2]byte
	crcValueMutex sync.Mutex
	// CRC16 target bytes that is used to calculate and compare with crcValue
	crcTarget      []byte
	crcTargetMutex sync.Mutex

	headerRead   bool
	headerMutex  sync.Mutex
	payloadRead  bool
	payloadMutex sync.Mutex
}

func OpenMP3File(path string) (io.ReadCloser, error) {
	ext := ".mp3"
	if len(path) < len(ext) || path[len(path)-len(ext):] != ext {
		return nil, fmt.Errorf("unsupported file format: %s", path)
	}
	return os.Open(path)
}

func (h *MP3Frame) ReadHeader(reader *bufio.Reader) error {
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
		// sync bit is 11 bits, check the last 3 bits to confirm
		if b&0xE0 != 0xE0 {
			continue
		}
		// version check
		switch (b >> 3) & 0x03 {
		case 0x10: // MPEG Version 2.0
		case 0x11: // MPEG Version 1.0
			h.flag1 |= 1 << 7 // set msb to indicate MPEG1
		default: // no support for MPEG Version 2.5 at this time
			return fmt.Errorf("unsupported MPEG version %02x", (b>>3)&0x03)
		}
		// at this time we only support Layer III
		if (b >> 1 & 0x03) != 0x01 {
			return fmt.Errorf("unsupported layer, only Layer III (MP3) is supported")
		}
		protectionBit := b & 0x01
		hasCRC := protectionBit == 0
		h.flag1Mutex.Lock()
		h.flag1 |= protectionBit << 6 // set protection bit
		h.flag1Mutex.Unlock()

		b, err = reader.ReadByte()
		if err != nil {
			return err
		}
		if hasCRC {
			h.crcTargetMutex.Lock()
			h.crcTarget = make([]byte, 2)
			h.crcTarget[0] = b
			h.crcTargetMutex.Unlock()
		}

		bitrateIndex := (b >> 4) & 0x0F
		if bitrateIndex == 0x0F {
			return fmt.Errorf("invalid bitrate index: bad")
		}
		h.flag1Mutex.Lock()
		h.flag1 |= bitrateIndex << 2 // set bitrate index
		h.flag1Mutex.Unlock()

		sampleRateIndex := (b >> 2) & 0x03
		if sampleRateIndex == 0x03 {
			return fmt.Errorf("invalid sample rate index: reserved")
		}
		h.flag1Mutex.Lock()
		h.flag1 |= sampleRateIndex // set sample rate index
		h.flag1Mutex.Unlock()

		h.flag2Mutex.Lock()
		h.flag2 |= ((b >> 1) & 0x01) << 1 // set padding bit
		h.flag2Mutex.Unlock()

		b, err = reader.ReadByte()
		if err != nil {
			return err
		}
		if hasCRC {
			h.crcTargetMutex.Lock()
			h.crcTarget[1] = b
			h.crcTargetMutex.Unlock()
		}
		h.flag2Mutex.Lock()
		h.flag2 |= (b >> 6) & 0x03 // set channel mode
		h.flag2Mutex.Unlock()

		if hasCRC {
			h.crcValueMutex.Lock()
			_, err := io.ReadFull(reader, h.crcValue[:])
			h.crcValueMutex.Unlock()
			if err != nil {
				return err
			}
		}

		return nil
	}
}

func (h *MP3Frame) ReadPayload(reader *bufio.Reader, frameLength int) ([]byte, error) {

}

// Layer III frame length = (144 * Bitrate / SampleRate) + Padding
func (h *MP3Frame) GetFrameLength() (int, error) {
	bitrateKbps, isFree := h.GetBitrateKbps()
	if isFree {
		return 0, fmt.Errorf("free bitrate frames are not supported")
	}
	sampleRate := h.GetSampleRate()
	padding := h.GetPadding()

	frameLength := (144 * int(bitrateKbps*1000) / int(sampleRate)) + int(padding)
	return frameLength, nil
}

func (h *MP3Frame) ValidateCRC(reader *bufio.Reader) bool {
	if !h.hasCRC() {
		return true // no CRC to validate
	}
	return true // TODO: implement CRC16 validation
}

func (h *MP3Frame) hasCRC() bool {
	h.flag1Mutex.Lock()
	defer h.flag1Mutex.Unlock()
	return (h.flag1 & (1 << 6)) == 0
}

// IsStereo does not distinguish between joint stereo and dual channel modes
func (h *MP3Frame) isStereo() bool {
	h.flag2Mutex.Lock()
	defer h.flag2Mutex.Unlock()
	return (h.flag2 & 0x03) != 0x03
}

// GetBitrate returns the bitrate in bps. If the frame uses free bitrate, returns (0, true)
func (h *MP3Frame) GetBitrateKbps() (uint16, bool) {
	h.flag1Mutex.Lock()
	defer h.flag1Mutex.Unlock()
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
	h.flag1Mutex.Lock()
	defer h.flag1Mutex.Unlock()
	sampleRateIndex := h.flag1 & 0x03
	version := (h.flag1 >> 7) & 0x01
	if version == 1 {
		return V1_SAMPLE_RATE_TABLE[sampleRateIndex]
	} else {
		return V2_SAMPLE_RATE_TABLE[sampleRateIndex]
	}
}

func (h *MP3Frame) GetPadding() uint8 {
	h.flag2Mutex.Lock()
	defer h.flag2Mutex.Unlock()
	return (h.flag2 >> 1) & 0x01
}
