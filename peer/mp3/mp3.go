package mp3

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sync"
)

type MP3Frame struct {
	mutex sync.Mutex
	// version (1), protection (1), bitrate index(4), sample rate index(2),
	// version=0 indicates MPEG2, version=1 indicates MPEG1
	flag1 byte
	// unused(5), padding (1), channel mode (2)
	flag2 byte
	// CRC16 value read from the frame (if present)
	crcValue [4]byte
	// CRC16 target bytes that is used to calculate and compare with crcValue
	crcTarget []byte
}

func OpenMP3File(path string) (io.ReadCloser, error) {
	ext := ".mp3"
	if len(path) < len(ext) || path[len(path)-len(ext):] != ext {
		return nil, fmt.Errorf("unsupported file format: %s", path)
	}
	return os.Open(path)
}

func (h *MP3Frame) ReadHeader(reader *bufio.Reader) error {
	h.mutex.Lock()
	defer h.mutex.Unlock()
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
		h.flag1 |= protectionBit << 6 // set protection bit

		b, err = reader.ReadByte()
		if err != nil {
			return err
		}
		if hasCRC {
			h.crcTarget = make([]byte, 2)
			h.crcTarget[0] = b
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
		if hasCRC {
			h.crcTarget[1] = b
		}
		h.flag2 |= (b >> 6) & 0x03 // set channel mode

		if hasCRC {
			_, err := io.ReadFull(reader, h.crcValue[:])
			if err != nil {
				return err
			}
		}

		return nil
	}
}

func (h *MP3Frame) ValidateCRC(reader *bufio.Reader) bool {
	if !h.hasCRC() {
		return true // no CRC to validate
	}
	return true // TODO: implement CRC16 validation
}

func (h *MP3Frame) hasCRC() bool {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	return (h.flag1 & (1 << 6)) == 0
}

// TODO: calculate frame length

// IsStereo does not distinguish between joint stereo and dual channel modes
func (h *MP3Frame) isStereo() bool {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	return (h.flag2 & 0x03) != 0x03
}

func (h *MP3Frame) GetBitrateKbps() (uint16, bool) {
	h.mutex.Lock()
	defer h.mutex.Unlock()
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
	h.mutex.Lock()
	defer h.mutex.Unlock()
	sampleRateIndex := h.flag1 & 0x03
	version := (h.flag1 >> 7) & 0x01
	if version == 1 {
		return V1_SAMPLE_RATE_TABLE[sampleRateIndex]
	} else {
		return V2_SAMPLE_RATE_TABLE[sampleRateIndex]
	}
}
