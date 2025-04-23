package recorder

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Credentials stores the credentials for S3 access
type S3Credentials struct {
	Endpoint        string
	AccessKeyID     string
	SecretAccessKey string
	UseSSL          bool
	BucketName      string
	BucketPrefix    string
}

// S3Uploader handles uploading files to an S3-compatible storage
type S3Uploader struct {
	client       *minio.Client
	credentials  S3Credentials
	bucketName   string
	bucketPrefix string
}

// NewS3Uploader creates a new S3 uploader
func NewS3Uploader(ctx context.Context, creds S3Credentials) (*S3Uploader, error) {
	// Initialize Minio client
	client, err := minio.New(creds.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(creds.AccessKeyID, creds.SecretAccessKey, ""),
		Secure: creds.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize Minio client: %w", err)
	}

	return &S3Uploader{
		client:       client,
		credentials:  creds,
		bucketName:   creds.BucketName,
		bucketPrefix: creds.BucketPrefix,
	}, nil
}

// UploadFile uploads a file to S3
func (u *S3Uploader) UploadFile(ctx context.Context, filePath, objectName string) error {
	// Check if file exists
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("failed to get file info: %w", err)
	}

	// Open the file
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	// Format object name with prefix if provided
	s3ObjectName := objectName
	if u.bucketPrefix != "" {
		s3ObjectName = filepath.Join(u.bucketPrefix, objectName)
	}

	// Upload file to S3
	_, err = u.client.PutObject(ctx, u.bucketName, s3ObjectName, file, fileInfo.Size(),
		minio.PutObjectOptions{ContentType: "audio/wav"})
	if err != nil {
		return fmt.Errorf("failed to upload file to S3: %w", err)
	}

	return nil
}

// CleanupDirectory removes a local directory and all its contents
func (u *S3Uploader) CleanupDirectory(dirPath string) error {
	// Check if directory exists
	if _, err := os.Stat(dirPath); os.IsNotExist(err) {
		return nil // Directory doesn't exist, nothing to clean up
	}

	// Remove directory and all its contents
	if err := os.RemoveAll(dirPath); err != nil {
		return fmt.Errorf("failed to clean up directory: %w", err)
	}

	return nil
}
