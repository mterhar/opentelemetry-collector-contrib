// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package libhoneyreceiver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/receiver/receivertest"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/libhoneyreceiver/internal/metadata"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/libhoneyreceiver/internal/response"
)

// simulatePanicInMiddleware simulates a panic occurring during HTTP middleware processing,
// similar to what happens with malformed compressed data in the decompressor middleware
func simulatePanicInMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Simulate a panic if the request has a specific header that triggers it
		if r.Header.Get("X-Trigger-Panic") == "true" {
			// This simulates the exact panic from the zstd decoder
			panic("runtime error: slice bounds out of range [32768:0]")
		}
		next(w, r)
	}
}

func TestPanicRecoveryInHandler(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	set := receivertest.NewNopSettings(metadata.Type)

	r, err := newLibhoneyReceiver(cfg, &set)
	require.NoError(t, err)

	// Register consumers
	r.registerTraceConsumer(consumertest.NewNop())
	r.registerLogConsumer(consumertest.NewNop())

	tests := []struct {
		name         string
		triggerPanic bool
		body         []byte
		expectPanic  bool
	}{
		{
			name:         "normal_request_no_panic",
			triggerPanic: false,
			body:         []byte(`[{"data":{"message":"test"},"samplerate":1,"time":"2025-01-01T00:00:00Z"}]`),
			expectPanic:  false,
		},
		{
			name:         "request_triggers_panic_in_middleware",
			triggerPanic: true,
			body:         []byte(`[{"data":{"message":"test"},"samplerate":1,"time":"2025-01-01T00:00:00Z"}]`),
			expectPanic:  false, // Should not panic due to recovery
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create request
			req := httptest.NewRequest(http.MethodPost, "/1/events/test", bytes.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			if tt.triggerPanic {
				req.Header.Set("X-Trigger-Panic", "true")
			}

			w := httptest.NewRecorder()

			// Wrap the handler with middleware that can panic, then wrap with panic recovery
			handlerWithMiddleware := simulatePanicInMiddleware(r.handleEvent)
			safeHandler := r.withPanicRecovery(handlerWithMiddleware)

			// Track if a panic occurs
			panicOccurred := false
			func() {
				defer func() {
					if r := recover(); r != nil {
						panicOccurred = true
					}
				}()
				safeHandler(w, req)
			}()

			// Check that panic was handled appropriately
			assert.Equal(t, tt.expectPanic, panicOccurred, "Panic occurrence mismatch")

			if tt.triggerPanic && !tt.expectPanic {
				// When panic is triggered but recovered, check the response
				assert.Equal(t, http.StatusBadRequest, w.Code, "Should return bad request on panic recovery")

				// Check that a proper libhoney error response was returned
				var errorResp []response.ResponseInBatch
				err := json.Unmarshal(w.Body.Bytes(), &errorResp)
				if assert.NoError(t, err, "Should be able to unmarshal error response") {
					assert.Len(t, errorResp, 1, "Should have one error response")
					if len(errorResp) > 0 {
						assert.Contains(t, errorResp[0].ErrorStr, "decompression", "Error should mention decompression")
						assert.Equal(t, http.StatusBadRequest, errorResp[0].Status, "Error status should be bad request")
					}
				}
			} else if !tt.triggerPanic {
				// Normal request should succeed
				assert.Equal(t, http.StatusOK, w.Code, "Should return OK for normal request")
			}
		})
	}
}

// TestDirectPanicRecovery tests that the withPanicRecovery method correctly recovers from panics
func TestDirectPanicRecovery(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	set := receivertest.NewNopSettings(metadata.Type)

	r, err := newLibhoneyReceiver(cfg, &set)
	require.NoError(t, err)

	// Create a handler that always panics
	panicHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	})

	// Wrap it with panic recovery
	safeHandler := r.withPanicRecovery(panicHandler)

	// Create test request
	req := httptest.NewRequest(http.MethodPost, "/test", bytes.NewReader([]byte("test")))
	req.Header.Set("Content-Encoding", "zstd")
	w := httptest.NewRecorder()

	// This should not panic
	assert.NotPanics(t, func() {
		safeHandler(w, req)
	}, "Handler with panic recovery should not panic")

	// Check that an error response was written
	assert.Equal(t, http.StatusBadRequest, w.Code, "Should return bad request after panic recovery")
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"), "Should set JSON content type")

	// Check the response body
	var errorResp []response.ResponseInBatch
	err = json.Unmarshal(w.Body.Bytes(), &errorResp)
	assert.NoError(t, err, "Should be able to unmarshal error response")
	assert.Len(t, errorResp, 1, "Should have one error response")
	assert.Contains(t, errorResp[0].ErrorStr, "decompression", "Error should mention decompression")
}
