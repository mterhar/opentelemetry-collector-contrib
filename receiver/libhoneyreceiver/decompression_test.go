// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package libhoneyreceiver

import (
	"bytes"
	"compress/gzip"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecompressBody(t *testing.T) {
	testData := []byte(`[{"data":{"message":"test"},"samplerate":1,"time":"2025-01-01T00:00:00Z"}]`)

	tests := []struct {
		name            string
		contentEncoding string
		setupBody       func() io.ReadCloser
		expectError     bool
		expectType      string
	}{
		{
			name:            "uncompressed",
			contentEncoding: "",
			setupBody: func() io.ReadCloser {
				return io.NopCloser(bytes.NewReader(testData))
			},
			expectError: false,
			expectType:  "none",
		},
		{
			name:            "gzip",
			contentEncoding: "gzip",
			setupBody: func() io.ReadCloser {
				var buf bytes.Buffer
				gw := gzip.NewWriter(&buf)
				_, _ = gw.Write(testData)
				_ = gw.Close()
				return io.NopCloser(&buf)
			},
			expectError: false,
			expectType:  "gzip",
		},
		{
			name:            "zstd",
			contentEncoding: "zstd",
			setupBody: func() io.ReadCloser {
				encoder, _ := zstd.NewWriter(nil)
				compressed := encoder.EncodeAll(testData, nil)
				return io.NopCloser(bytes.NewReader(compressed))
			},
			expectError: false,
			expectType:  "zstd",
		},
		{
			name:            "unsupported",
			contentEncoding: "brotli",
			setupBody: func() io.ReadCloser {
				return io.NopCloser(bytes.NewReader(testData))
			},
			expectError: true,
			expectType:  "brotli",
		},
		{
			name:            "corrupted_zstd",
			contentEncoding: "zstd",
			setupBody: func() io.ReadCloser {
				return io.NopCloser(bytes.NewReader([]byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x00, 0xff, 0xff}))
			},
			expectError: true,
			expectType:  "zstd",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := tt.setupBody()
			defer body.Close()

			result, detectedType, rawBytes, err := decompressBody(body, tt.contentEncoding, false)

			assert.Equal(t, tt.expectType, detectedType, "Detected type mismatch")
			assert.Nil(t, rawBytes, "rawBytes should be nil when captureRaw is false")

			if tt.expectError {
				assert.Error(t, err, "Expected an error")
			} else {
				require.NoError(t, err, "Unexpected error")
				assert.Equal(t, testData, result, "Decompressed data mismatch")
			}
		})
	}
}

func TestDecompressBodyWithRawCapture(t *testing.T) {
	testData := []byte(`[{"data":{"message":"test"},"samplerate":1}]`)
	encoder, _ := zstd.NewWriter(nil)
	compressed := encoder.EncodeAll(testData, nil)

	body := io.NopCloser(bytes.NewReader(compressed))

	result, detectedType, rawBytes, err := decompressBody(body, "zstd", true)

	require.NoError(t, err)
	assert.Equal(t, "zstd", detectedType)
	assert.Equal(t, testData, result)
	assert.NotNil(t, rawBytes, "rawBytes should be captured when captureRaw is true")
	assert.Greater(t, len(rawBytes), 0, "rawBytes should contain data")

	// Verify it starts with zstd magic number
	assert.Equal(t, byte(0x28), rawBytes[0])
	assert.Equal(t, byte(0xb5), rawBytes[1])
	assert.Equal(t, byte(0x2f), rawBytes[2])
	assert.Equal(t, byte(0xfd), rawBytes[3])
}
