package recorder

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// RecordingConfig represents the recording configuration
type RecordingConfig struct {
	// Path where recordings will be stored
	RecordingsPath string `json:"recordings_path"`
	// FFmpeg executable path (optional)
	FFmpegPath string `json:"ffmpeg_path"`
	// Whether to auto-merge recordings when a room ends
	AutoMerge bool `json:"auto_merge"`
	// Default audio parameters
	DefaultSampleRate   uint32 `json:"default_sample_rate"`
	DefaultChannelCount uint16 `json:"default_channel_count"`
	// S3 upload configuration (optional)
	S3Upload *S3UploadConfig `json:"s3_upload,omitempty"`
}

// ConfigManager manages recording configuration
type ConfigManager struct {
	mu     sync.RWMutex
	config RecordingConfig
}

// DefaultConfig returns the default recording configuration
func DefaultConfig() RecordingConfig {
	return RecordingConfig{
		RecordingsPath:      "recordings",
		FFmpegPath:          "ffmpeg",
		AutoMerge:           true,
		DefaultSampleRate:   48000,
		DefaultChannelCount: 1,
	}
}

// NewConfigManager creates a new config manager with default values
func NewConfigManager() *ConfigManager {
	return &ConfigManager{
		mu:     sync.RWMutex{},
		config: DefaultConfig(),
	}
}

// LoadConfig loads configuration from a JSON file
func (c *ConfigManager) LoadConfig(configPath string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	file, err := os.Open(configPath)
	if err != nil {
		return fmt.Errorf("failed to open config file: %w", err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&c.config); err != nil {
		return fmt.Errorf("failed to decode config: %w", err)
	}

	// Create recordings directory if it doesn't exist
	if err := os.MkdirAll(c.config.RecordingsPath, 0755); err != nil {
		return fmt.Errorf("failed to create recordings directory: %w", err)
	}

	return nil
}

// SaveConfig saves the current configuration to a JSON file
func (c *ConfigManager) SaveConfig(configPath string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Create directory if it doesn't exist
	dir := filepath.Dir(configPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	file, err := os.Create(configPath)
	if err != nil {
		return fmt.Errorf("failed to create config file: %w", err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(c.config); err != nil {
		return fmt.Errorf("failed to encode config: %w", err)
	}

	return nil
}

// GetConfig returns a copy of the current configuration
func (c *ConfigManager) GetConfig() RecordingConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.config
}

// UpdateConfig updates the configuration with new values
func (c *ConfigManager) UpdateConfig(config RecordingConfig) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Create recordings directory if it doesn't exist or has changed
	if c.config.RecordingsPath != config.RecordingsPath {
		if err := os.MkdirAll(config.RecordingsPath, 0755); err != nil {
			return fmt.Errorf("failed to create recordings directory: %w", err)
		}
	}

	c.config = config
	return nil
}
