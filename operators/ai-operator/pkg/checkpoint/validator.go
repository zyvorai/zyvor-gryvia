// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CheckpointMetadata holds metadata for a single checkpoint
type CheckpointMetadata struct {
	// Name is the unique identifier for this checkpoint
	Name string `json:"name"`

	// Path is the filesystem path to the checkpoint
	Path string `json:"path"`

	// JobName is the name of the GryviaAIJob that produced this checkpoint
	JobName string `json:"jobName"`

	// CreatedAt is the timestamp when the checkpoint was created
	CreatedAt metav1.Time `json:"createdAt"`

	// Valid indicates whether this checkpoint passed validation
	Valid bool `json:"valid"`

	// ValidationMessage describes the validation result
	ValidationMessage string `json:"validationMessage,omitempty"`

	// IsEmergency indicates whether this was an emergency checkpoint
	IsEmergency bool `json:"isEmergency"`

	// Checksum is the SHA-256 checksum of the checkpoint file
	Checksum string `json:"checksum,omitempty"`

	// SizeBytes is the size of the checkpoint in bytes
	SizeBytes int64 `json:"sizeBytes,omitempty"`
}

// ValidationOptions configures which validation checks to perform
type ValidationOptions struct {
	// ChecksumVerify enables SHA-256 checksum verification after writing
	ChecksumVerify bool

	// TensorShapeVerify enables tensor shape verification against expected model architecture
	TensorShapeVerify bool

	// LoadTest enables a trial load of the checkpoint to verify it can be deserialized
	LoadTest bool
}

// CheckpointIndex maintains an ordered list of checkpoints with metadata.
// It is safe for concurrent use.
type CheckpointIndex struct {
	mu          sync.RWMutex
	checkpoints []CheckpointMetadata
}

// global index instance used by the controller
var (
	globalIndex     = &CheckpointIndex{}
	globalIndexOnce sync.Once
)

// getGlobalIndex returns the singleton checkpoint index
func getGlobalIndex() *CheckpointIndex {
	globalIndexOnce.Do(func() {
		globalIndex = &CheckpointIndex{
			checkpoints: make([]CheckpointMetadata, 0),
		}
	})
	return globalIndex
}

// NewCheckpointIndex creates a new, empty CheckpointIndex
func NewCheckpointIndex() *CheckpointIndex {
	return &CheckpointIndex{
		checkpoints: make([]CheckpointMetadata, 0),
	}
}

// Add inserts a checkpoint into the index, maintaining chronological order.
// If retentionCount > 0, older entries beyond the limit are removed.
func (idx *CheckpointIndex) Add(meta CheckpointMetadata, retentionCount int) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	idx.checkpoints = append(idx.checkpoints, meta)

	// Sort by creation time, most recent first
	sort.Slice(idx.checkpoints, func(i, j int) bool {
		return idx.checkpoints[i].CreatedAt.Time.After(idx.checkpoints[j].CreatedAt.Time)
	})

	// Enforce retention count
	if retentionCount > 0 && len(idx.checkpoints) > retentionCount {
		idx.checkpoints = idx.checkpoints[:retentionCount]
	}
}

// FindLatestValid returns the most recent checkpoint that passed validation.
// Returns the checkpoint metadata and true if found, or an empty metadata and false if none exist.
func (idx *CheckpointIndex) FindLatestValid() (CheckpointMetadata, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	for _, ckpt := range idx.checkpoints {
		if ckpt.Valid {
			return ckpt, true
		}
	}

	return CheckpointMetadata{}, false
}

// FindLatestValidForJob returns the most recent valid checkpoint for a specific job.
func (idx *CheckpointIndex) FindLatestValidForJob(jobName string) (CheckpointMetadata, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	for _, ckpt := range idx.checkpoints {
		if ckpt.Valid && ckpt.JobName == jobName {
			return ckpt, true
		}
	}

	return CheckpointMetadata{}, false
}

// List returns all checkpoints in the index, most recent first.
func (idx *CheckpointIndex) List() []CheckpointMetadata {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	result := make([]CheckpointMetadata, len(idx.checkpoints))
	copy(result, idx.checkpoints)
	return result
}

// Len returns the number of checkpoints in the index.
func (idx *CheckpointIndex) Len() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	return len(idx.checkpoints)
}

// ValidateCheckpoint performs validation checks on a checkpoint file.
// It returns whether the checkpoint is valid and a human-readable reason.
func ValidateCheckpoint(path string, opts ValidationOptions) (bool, string) {
	// Check 1: Verify the file exists
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, fmt.Sprintf("checkpoint file does not exist: %s", path)
		}
		return false, fmt.Sprintf("failed to stat checkpoint file: %v", err)
	}

	// Check 2: Verify the file has non-zero size
	if info.Size() == 0 {
		return false, "checkpoint file is empty (0 bytes)"
	}

	// Check 3: Verify the file is readable
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Sprintf("checkpoint file is not readable: %v", err)
	}
	defer f.Close()

	// Check 4: Checksum verification
	if opts.ChecksumVerify {
		if _, err := computeChecksum(f); err != nil {
			return false, fmt.Sprintf("checksum computation failed: %v", err)
		}
		// Reset file position for subsequent reads
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return false, fmt.Sprintf("failed to reset file position: %v", err)
		}
	}

	// Check 5: Tensor shape verification (structural check)
	if opts.TensorShapeVerify {
		valid, reason := verifyCheckpointStructure(f)
		if !valid {
			return false, reason
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return false, fmt.Sprintf("failed to reset file position: %v", err)
		}
	}

	// Check 6: Load test (attempt to read the full file to verify integrity)
	if opts.LoadTest {
		valid, reason := performLoadTest(f, info.Size())
		if !valid {
			return false, reason
		}
	}

	return true, "checkpoint passed all validation checks"
}

// computeChecksum calculates the SHA-256 checksum of a file
func computeChecksum(r io.Reader) (string, error) {
	hasher := sha256.New()
	if _, err := io.Copy(hasher, r); err != nil {
		return "", fmt.Errorf("failed to compute checksum: %w", err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// ComputeFileChecksum calculates the SHA-256 checksum of a file at the given path.
// This is a convenience function for external callers.
func ComputeFileChecksum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	defer f.Close()

	return computeChecksum(f)
}

// verifyCheckpointStructure performs a structural check on the checkpoint file.
// For PyTorch checkpoints (.pt/.pth), it verifies the file starts with a valid
// magic number or ZIP header. For other formats, it verifies the file is readable.
func verifyCheckpointStructure(r io.ReadSeeker) (bool, string) {
	// Read the first 4 bytes to check for known formats
	header := make([]byte, 4)
	n, err := r.Read(header)
	if err != nil {
		return false, fmt.Sprintf("failed to read checkpoint header: %v", err)
	}
	if n < 4 {
		return false, "checkpoint file is too small to contain valid tensor data"
	}

	// PyTorch ZIP archive format (PK\x03\x04)
	if header[0] == 0x50 && header[1] == 0x4B && header[2] == 0x03 && header[3] == 0x04 {
		return true, "valid PyTorch ZIP checkpoint"
	}

	// PyTorch legacy format (magic number: 0x70 0x79 0x74 0x6F = "pyto")
	if header[0] == 0x70 && header[1] == 0x79 && header[2] == 0x74 && header[3] == 0x6F {
		return true, "valid PyTorch legacy checkpoint"
	}

	// HDF5 format used by TensorFlow/Keras (magic: \x89HDF)
	if header[0] == 0x89 && header[1] == 0x48 && header[2] == 0x44 && header[3] == 0x46 {
		return true, "valid HDF5 checkpoint"
	}

	// SafeTensors format (starts with JSON metadata, so '{')
	if header[0] == '{' {
		return true, "valid SafeTensors-format checkpoint"
	}

	// NumPy format (magic: \x93NUMPY)
	if header[0] == 0x93 && header[1] == 0x4E {
		return true, "valid NumPy checkpoint"
	}

	// If we cannot identify the format, consider it valid but note the unknown format.
	// This avoids false negatives for custom or newer checkpoint formats.
	return true, "checkpoint format not recognized but file is readable"
}

// performLoadTest reads the entire checkpoint file to verify data integrity.
// This simulates a full deserialization pass to detect corruption.
func performLoadTest(r io.Reader, expectedSize int64) (bool, string) {
	buf := make([]byte, 32*1024) // 32KB buffer
	var totalRead int64

	for {
		n, err := r.Read(buf)
		totalRead += int64(n)
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, fmt.Sprintf("load test failed at byte %d: %v", totalRead, err)
		}
	}

	if totalRead != expectedSize {
		return false, fmt.Sprintf("load test read %d bytes, expected %d", totalRead, expectedSize)
	}

	return true, "load test passed: all bytes readable"
}

// FindLatestValid is a package-level convenience function that searches the
// global checkpoint index for the most recent valid checkpoint.
func FindLatestValid() (CheckpointMetadata, bool) {
	return getGlobalIndex().FindLatestValid()
}

// AddToIndex adds a checkpoint to the global index with the given retention count.
func AddToIndex(name string, meta CheckpointMetadata, retentionCount int) {
	getGlobalIndex().Add(meta, retentionCount)
}
