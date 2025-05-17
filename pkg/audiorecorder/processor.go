package audiorecorder

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/pion/logging"
)

// ProcessorConfig contains configuration for audio processing
type ProcessorConfig struct {
	FFmpegPath string
	Logger     logging.LeveledLogger
}

// Processor handles merging and uploading of recordings
type Processor struct {
	config ProcessorConfig
	logger logging.LeveledLogger
}

// NewProcessor creates a new audio processor
func NewProcessor(config ProcessorConfig) *Processor {
	logger := config.Logger
	if logger == nil {
		logger = logging.NewDefaultLoggerFactory().NewLogger("audiorecorder")
	}

	return &Processor{
		config: config,
		logger: logger,
	}
}

// processRecordings merges individual recordings and uploads to S3
func (r *Recorder) processRecordings() error {
	processor := NewProcessor(ProcessorConfig{
		FFmpegPath: "ffmpeg", // Default path, could be configurable
		Logger:     nil,      // Use default logger
	})

	// Get all tracks by channel
	channelOne := make([]*Track, 0)
	channelTwo := make([]*Track, 0)

	for _, track := range r.tracks {
		switch track.ChannelType {
		case ChannelOne:
			channelOne = append(channelOne, track)
		case ChannelTwo:
			channelTwo = append(channelTwo, track)
		}
	}

	recordingPath := filepath.Join(r.basePath, r.recordingID)

	// Create output directory for merged files
	mergedPath := filepath.Join(recordingPath, "merged")
	if err := os.MkdirAll(mergedPath, 0755); err != nil {
		return fmt.Errorf("failed to create merged directory: %w", err)
	}

	// Process files for each channel
	wg := sync.WaitGroup{}
	errChan := make(chan error, 2)

	if len(channelOne) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mergedFile := filepath.Join(mergedPath, "channel_one.ogg")
			if err := processor.mergeFiles(channelOne, mergedFile); err != nil {
				errChan <- fmt.Errorf("failed to merge channel one: %w", err)
			}
		}()
	}

	if len(channelTwo) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mergedFile := filepath.Join(mergedPath, "channel_two.ogg")
			if err := processor.mergeFiles(channelTwo, mergedFile); err != nil {
				errChan <- fmt.Errorf("failed to merge channel two: %w", err)
			}
		}()
	}

	wg.Wait()
	close(errChan)

	// Check for errors
	for err := range errChan {
		if err != nil {
			return err
		}
	}

	// If we have both channels, create a stereo file
	finalOutput := filepath.Join(recordingPath, fmt.Sprintf("%s.ogg", r.recordingID))
	channelOnePath := filepath.Join(mergedPath, "channel_one.ogg")
	channelTwoPath := filepath.Join(mergedPath, "channel_two.ogg")

	if len(channelOne) > 0 && len(channelTwo) > 0 {
		// Merge into stereo
		if err := processor.createStereoFile(channelOnePath, channelTwoPath, finalOutput); err != nil {
			return fmt.Errorf("failed to create stereo file: %w", err)
		}
	} else if len(channelOne) > 0 {
		// Only channel one exists, use it as final output
		if err := os.Rename(channelOnePath, finalOutput); err != nil {
			return fmt.Errorf("failed to rename channel one to final output: %w", err)
		}
	} else if len(channelTwo) > 0 {
		// Only channel two exists, use it as final output
		if err := os.Rename(channelTwoPath, finalOutput); err != nil {
			return fmt.Errorf("failed to rename channel two to final output: %w", err)
		}
	} else {
		// No tracks were recorded
		return nil
	}

	// Upload to S3 if configured
	if r.s3Config != nil {
		if err := processor.uploadToS3(finalOutput, r.s3Config, r.recordingID); err != nil {
			return fmt.Errorf("failed to upload to S3: %w", err)
		}
	}

	return nil
}

// mergeFiles combines multiple audio files into a single file
func (p *Processor) mergeFiles(tracks []*Track, outputFile string) error {
	if len(tracks) == 0 {
		return fmt.Errorf("no tracks to merge")
	}

	if len(tracks) == 1 {
		// Just copy the single file
		if err := os.Link(tracks[0].FilePath, outputFile); err != nil {
			// If hard link fails, try copy
			input, err := os.ReadFile(tracks[0].FilePath)
			if err != nil {
				return fmt.Errorf("failed to read source file: %w", err)
			}
			if err := os.WriteFile(outputFile, input, 0644); err != nil {
				return fmt.Errorf("failed to write destination file: %w", err)
			}
		}
		return nil
	}

	// Create a concat file for ffmpeg
	concatFile := outputFile + ".txt"
	concatContent := ""
	for _, track := range tracks {
		concatContent += fmt.Sprintf("file '%s'\n", track.FilePath)
	}

	if err := os.WriteFile(concatFile, []byte(concatContent), 0644); err != nil {
		return fmt.Errorf("failed to write concat file: %w", err)
	}
	defer os.Remove(concatFile)

	// Run ffmpeg command to concatenate files
	cmd := exec.Command(
		p.config.FFmpegPath,
		"-f", "concat",
		"-safe", "0",
		"-i", concatFile,
		"-c", "copy",
		outputFile,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg error: %w, output: %s", err, string(output))
	}

	return nil
}

// createStereoFile combines two mono files into a stereo file
func (p *Processor) createStereoFile(leftChannel, rightChannel, outputFile string) error {
	cmd := exec.Command(
		p.config.FFmpegPath,
		"-i", leftChannel,
		"-i", rightChannel,
		"-filter_complex", "amerge=inputs=2",
		"-ac", "2",
		"-c:a", "libopus",
		outputFile,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg error: %w, output: %s", err, string(output))
	}

	return nil
}

// uploadToS3 uploads a file to an S3 bucket
func (p *Processor) uploadToS3(filePath string, config *S3Config, recordingID string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file for upload: %w", err)
	}
	defer file.Close()

	// Create S3 service client
	s3Config := aws.NewConfig().
		WithCredentials(credentials.NewStaticCredentials(
			config.AccessKey,
			config.SecretKey,
			"",
		)).
		WithEndpoint(config.Endpoint).
		WithS3ForcePathStyle(true)

	if !config.Secure {
		s3Config = s3Config.WithDisableSSL(true)
	}

	sess, err := session.NewSession(s3Config)
	if err != nil {
		return fmt.Errorf("failed to create S3 session: %w", err)
	}

	// Create an uploader with the session and default options
	uploader := s3manager.NewUploader(sess)

	// Determine the S3 key (destination path)
	key := recordingID + ".ogg"
	if config.FilePrefix != "" {
		// Remove leading slash if present to avoid double slashes
		prefix := strings.TrimPrefix(config.FilePrefix, "/")
		if prefix != "" {
			// Ensure there's a trailing slash
			if !strings.HasSuffix(prefix, "/") {
				prefix += "/"
			}
			key = prefix + key
		}
	}

	// Upload the file to S3
	_, err = uploader.Upload(&s3manager.UploadInput{
		Bucket: aws.String(config.Bucket),
		Key:    aws.String(key),
		Body:   file,
	})
	if err != nil {
		return fmt.Errorf("failed to upload file to S3: %w", err)
	}

	// Verify the upload was successful
	svc := s3.New(sess)
	_, err = svc.HeadObject(&s3.HeadObjectInput{
		Bucket: aws.String(config.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("failed to verify S3 upload: %w", err)
	}

	p.logger.Infof("Successfully uploaded %s to s3://%s/%s", filePath, config.Bucket, key)
	return nil
}
