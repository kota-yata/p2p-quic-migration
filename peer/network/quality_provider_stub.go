//go:build !linux

package network_monitor

import (
	"fmt"
	"time"
)

type unsupportedQualityProvider struct{}

func NewDefaultQualityProvider() QualityProvider {
	return unsupportedQualityProvider{}
}

func (unsupportedQualityProvider) Snapshot() (QualitySnapshot, error) {
	return QualitySnapshot{Available: false, Time: time.Now()}, fmt.Errorf("wireless quality provider is only implemented on linux/android")
}

func (unsupportedQualityProvider) Close() error {
	return nil
}
