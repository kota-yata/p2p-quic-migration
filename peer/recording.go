package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// AudioRecorder persists incoming audio so we can measure interruptions later.
// It is safe for concurrent writes across multiple streams.
type AudioRecorder struct {
	mu   sync.Mutex
	file *os.File
	path string
}

func NewAudioRecorder(path string) (*AudioRecorder, error) {
	if path == "" {
		return nil, fmt.Errorf("recording path must not be empty")
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("failed to create recording directory: %w", err)
	}

	file, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("failed to create recording file: %w", err)
	}

	return &AudioRecorder{
		file: file,
		path: path,
	}, nil
}

func (ar *AudioRecorder) Write(data []byte) error {
	if len(data) == 0 {
		return nil
	}

	ar.mu.Lock()
	defer ar.mu.Unlock()

	if ar.file == nil {
		return fmt.Errorf("recorder is closed")
	}

	_, err := ar.file.Write(data)
	return err
}

func (ar *AudioRecorder) Close() error {
	ar.mu.Lock()
	defer ar.mu.Unlock()

	if ar.file == nil {
		return nil
	}

	err := ar.file.Sync()
	if errClose := ar.file.Close(); err == nil {
		err = errClose
	}
	ar.file = nil
	return err
}

func (ar *AudioRecorder) Path() string {
	return ar.path
}
