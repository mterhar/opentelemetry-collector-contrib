// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package libhoneyreceiver

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/receiver/receivertest"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/libhoneyreceiver/internal/metadata"
)

func TestPanicRecoveryWithMalformedCompression(t *testing.T) {
	// Create receiver with default config
	cfg := createDefaultConfig().(*Config)
	// Skip this test if HTTP config is not available
	if !cfg.HTTP.HasValue() {
		t.Skip("HTTP config not available, skipping test")
	}
	set := receivertest.NewNopSettings(metadata.Type)

	r, err := newLibhoneyReceiver(cfg, &set)
	require.NoError(t, err)

	// Register consumers
	r.registerTraceConsumer(consumertest.NewNop())
	r.registerLogConsumer(consumertest.NewNop())

	// Start the receiver with real HTTP server
	err = r.Start(context.Background(), componenttest.NewNopHost())
	require.NoError(t, err)
	defer r.Shutdown(context.Background())

	// Wait for server to start
	time.Sleep(100 * time.Millisecond)

	tests := []struct {
		name            string
		body            []byte
		contentType     string
		contentEncoding string
		expectSuccess   bool
	}{
		{
			name: "malformed_zstd_data_that_causes_panic",
			// This is crafted data that will cause the zstd decoder to panic
			// with slice bounds out of range [32768:0]
			body:            []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x58, 0x80, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
			contentType:     "application/json",
			contentEncoding: "zstd",
			expectSuccess:   false, // Should return error but not panic
		},
		{
			name:            "valid_uncompressed_json",
			body:            []byte(`[{"data":{"message":"test"},"samplerate":1,"time":"2025-01-01T00:00:00Z"}]`),
			contentType:     "application/json",
			contentEncoding: "",
			expectSuccess:   true,
		},
		{
			name:            "malformed_gzip_data",
			body:            []byte{0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xff, 0xff, 0xff, 0xff, 0xff},
			contentType:     "application/json",
			contentEncoding: "gzip",
			expectSuccess:   false, // Should return error but not panic
		},
		{
			name:            "malformed_deflate_data",
			body:            []byte{0x78, 0x9c, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
			contentType:     "application/json",
			contentEncoding: "deflate",
			expectSuccess:   false, // Should return error but not panic
		},
		{
			name:            "empty_compressed_data",
			body:            []byte{},
			contentType:     "application/json",
			contentEncoding: "zstd",
			expectSuccess:   false, // Should return error but not panic
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Make HTTP request to the real server
			req, err := http.NewRequest("POST", "http://localhost:8080/1/events/test", bytes.NewReader(tt.body))
			require.NoError(t, err)

			req.Header.Set("Content-Type", tt.contentType)
			if tt.contentEncoding != "" {
				req.Header.Set("Content-Encoding", tt.contentEncoding)
			}

			client := &http.Client{Timeout: 5 * time.Second}

			// This should not panic, even with malformed data
			resp, err := client.Do(req)

			// We should get a response (not panic)
			require.NoError(t, err, "Request should complete without client error")
			require.NotNil(t, resp, "Should get a response even with malformed data")
			defer resp.Body.Close()

			if tt.expectSuccess {
				assert.Equal(t, http.StatusOK, resp.StatusCode, "Expected successful response")
			} else {
				// For malformed data, we expect a bad request or service unavailable response
				assert.True(t, resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusServiceUnavailable,
					"Expected error response for malformed data, got %d", resp.StatusCode)
			}
		})
	}
}
